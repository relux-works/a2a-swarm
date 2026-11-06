//go:build darwin || linux

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// An actual exec of an atomically replaced CLI keeps PID/birth, generation,
// lease ownership, pane and pending mailbox, becomes the scanner's current
// image, and delivers retained mail afterwards. Lock exclusion is sampled
// across exec; the adoption unit test checks the inherited file description.
// Uses only a private terminal and fake homeserver on CI, no provider session.
// The child advances only the refresh throttle clock; real safety, probing,
// exec and delivery clocks remain intact. PendingCost checks the 30s boundary.
func TestTerminalRefreshDetachedLifecycle(t *testing.T) {
	isolateBindTest(t)
	testTerminalRefreshDetachedLifecycle(t, "direct")
}

// Real consumers launched through a symlink refresh after atomic target
// replacement or symlink retargeting, retaining the lifecycle guarantees above.
func TestTerminalRefreshSymlinkLifecycle(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"replace-target", "retarget"} {
		t.Run(mode, func(t *testing.T) { testTerminalRefreshDetachedLifecycle(t, mode) })
	}
}

// Wire the existing private clock/safety seams into the real child through a
// build overlay, never a production flag or environment override. Each poll
// advances past the safety throttle (and eventually the probe retry cooldown).
// The safety wrapper records an actual rejection, not a guessed cycle count.
func buildRefreshLifecycleCLI(t *testing.T, exe string) {
	t.Helper()
	source, err := filepath.Abs("bind_refresh.go")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = "r.initializePath()\n\treturn r"
	const replacement = `r.initializePath()
	clock := time.Now()
	safeVerdict := false
	r.now = func() time.Time {
		clock = clock.Add(31 * time.Second)
		return clock
	}
	r.safety = func(ctx context.Context, a *App, s *session, generation string) (string, bool, error) {
		id, safe, err := terminalRefreshAuthority(ctx, a, s, generation)
		safeVerdict = err == nil && safe
		if err == nil && !safe {
			fmt.Fprintln(a.Stderr, "test refresh: unsafe candidate deferred")
		}
		return id, safe, err
	}
	r.exec = func(path string, args, env []string) error {
		if !safeVerdict {
			fmt.Fprintln(os.Stderr, "test refresh: unsafe exec")
		}
		return syscall.Exec(path, args, env)
	}
	return r`
	if strings.Count(string(raw), anchor) != 1 {
		t.Fatal("setup: nonunique refresh clock overlay anchor")
	}
	dir := t.TempDir()
	patched := filepath.Join(dir, "bind_refresh.go")
	if err := os.WriteFile(patched, []byte(strings.Replace(string(raw), anchor, replacement, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	overlay, err := json.Marshal(map[string]any{"Replace": map[string]string{source: patched}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "overlay.json")
	if err := os.WriteFile(path, overlay, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-overlay", path, "-o", exe, "../../cmd/a2a").CombinedOutput(); err != nil {
		t.Fatalf("setup: build CLI: %v %s", err, out)
	}
}

func testTerminalRefreshDetachedLifecycle(t *testing.T, mode string) {
	t.Helper()
	if testing.Short() {
		t.Skip("detached executable integration")
	}
	f := newOrphanDispatchFixture(t)
	f.a.Now = time.Now
	exe := filepath.Join(f.dir, "a2a")
	buildRefreshLifecycleCLI(t, exe)
	// A cold build must not spend the consumer's lifecycle deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	launch := exe
	if mode != "direct" {
		launch = filepath.Join(t.TempDir(), "a2a")
		if err := os.Symlink(exe, launch); err != nil {
			t.Fatal(err)
		}
	}
	// Keep mail pending across exec without terminal effects, then resume it.
	f.update(func(st *config.State) { st.HooksPaused = true })
	id := f.mail()
	ready, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	logPath := filepath.Join(f.e.dir, "state", "alice.binding.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	child := exec.CommandContext(ctx, launch, "--as", "alice", "bind", "--run-generation", "orphan-gen")
	child.Env = detachedConsumerEnv(append(os.Environ(), "A2A_SECRET_STORE=file", "A2A_MATRIX_URL="+f.e.hs.srv.URL,
		"A2A_MATRIX_SERVER_NAME="+testServer, "OPENAI_API_KEY=synthetic-never-inherit"), f.e.dir, "owned-thread")
	child.ExtraFiles = []*os.File{writer}
	child.Stdout = log
	child.Stderr = log
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	err = config.WithRuntimeLock(f.e.paths(), "alice", "control", time.Now().Add(3*time.Second), func() error {
		if err := child.Start(); err != nil {
			return err
		}
		birth, err := tmuxwake.Birth(ctx, child.Process.Pid)
		if err != nil {
			return err
		}
		_, err = config.UpdateState(f.e.paths(), "alice", func(st *config.State) error {
			st.Terminal.DaemonPID = child.Process.Pid
			st.Terminal.DaemonBirth = birth
			return nil
		})
		return err
	})
	writer.Close()
	if child.Process != nil {
		t.Cleanup(func() { child.Process.Kill(); child.Wait() })
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := ready.SetReadDeadline(time.Now().Add(8 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(ready)
	if err != nil || string(got) != "ready\n" {
		t.Fatalf("setup: child not armed: %v", err)
	}
	before := awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "suppressed: manual gate" })
	mailbox := readFile(t, f.s.paths.MailboxFile("alice"))
	// An unsafe attempt must defer the actual new executable, even while paused.
	f.update(func(st *config.State) {
		st.Terminal.AttemptIDs = []string{id}
		st.Terminal.Notice = tmuxwake.Notice(1)
		st.Terminal.Typed = true
	})
	raw, err := os.ReadFile(exe)
	if err != nil {
		t.Fatal(err)
	}
	next := exe + ".next"
	if err := os.WriteFile(next, raw, 0700); err != nil {
		t.Fatal(err)
	}
	if mode == "retarget" {
		link := launch + ".next"
		if err := os.Symlink(next, link); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(link, launch); err != nil {
			t.Fatal(err)
		}
		exe = next
	} else {
		if err := os.Rename(next, exe); err != nil {
			t.Fatal(err)
		}
	}
	candidate, err := observeTerminalExecutable(exe)
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(testDeadline)
	for !strings.Contains(readFile(t, logPath), "test refresh: unsafe candidate deferred\n") && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	logs := readFile(t, logPath)
	if !strings.Contains(logs, "test refresh: unsafe candidate deferred\n") {
		t.Fatalf("candidate never reached unsafe refresh gate: context=%v log=%s", ctx.Err(), logs)
	}
	if strings.Contains(logs, "binding re-exec:") {
		t.Fatal("unsafe journal crossed exec")
	}
	f.update(func(st *config.State) {
		st.Terminal.AttemptIDs = nil
		st.Terminal.Notice = ""
		st.Terminal.Typed = false
	})
	// Probe the same flock repeatedly across the transition. Any release allows
	// a second consumer and invalidates the preservation claim.
	until = time.Now().Add(45 * time.Second)
	for time.Now().Before(until) {
		err := config.WithRuntimeLock(f.e.paths(), "alice", "consumer", time.Now(), func() error { return nil })
		if !errors.Is(err, config.ErrLockTimeout) {
			t.Fatalf("consumer lock lost across exec: %v", err)
		}
		current, err := bindingImage(ctx, child.Process.Pid)
		if err == nil && current == candidate.Image && strings.Count(readFile(t, logPath), "binding re-exec: executable changed\n") == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Count(readFile(t, logPath), "binding re-exec: executable changed\n") != 1 {
		t.Fatalf("replacement did not produce one exec: context=%v log=%s", ctx.Err(), readFile(t, logPath))
	}
	// The unsafe acknowledgement precedes poll's return. Retain an exec-boundary
	// witness too, so clearing the journal cannot hide an in-flight unsafe exec.
	if strings.Contains(readFile(t, logPath), "test refresh: unsafe exec\n") {
		t.Fatal("unsafe journal crossed exec")
	}
	current, err := bindingImage(ctx, child.Process.Pid)
	if err != nil || current != candidate.Image {
		t.Fatal("scanner still sees old image")
	}
	// A command launched from the new image now sees a same-image binding rather
	// than ErrBindingOldImage; the ordinary wait-while-bound fence still applies.
	duplicate := exec.CommandContext(ctx, exe, "--as", "alice", "wait", "--timeout", "1ms")
	duplicate.Env = child.Env
	out, err := duplicate.CombinedOutput()
	if err == nil || !strings.Contains(string(out), ErrWaitWhileBound.Error()) || strings.Contains(string(out), ErrBindingOldImage.Error()) {
		t.Fatalf("wrong post-exec admission: %v %s", err, out)
	}
	after := f.e.state("alice")
	b, old := after.Terminal, before.Terminal
	birth, err := tmuxwake.Birth(ctx, child.Process.Pid)
	if err != nil || birth != old.DaemonBirth || b.DaemonPID != old.DaemonPID || b.DaemonBirth != old.DaemonBirth || b.Generation != old.Generation || b.OwnerPID != old.OwnerPID || b.OwnerBirth != old.OwnerBirth || b.Target != old.Target || b.SessionID != old.SessionID || b.Stopped {
		t.Fatal("exec changed process or binding identity")
	}
	if after.Lease == nil || after.Lease.Holder != before.Lease.Holder || after.Lease.Host != before.Lease.Host || after.Lease.PID != before.Lease.PID || !after.Lease.Expires.After(time.Now()) {
		t.Fatal("exec lost lease")
	}
	if mailbox != readFile(t, f.s.paths.MailboxFile("alice")) || f.e.received("alice", id) {
		t.Fatal("exec changed mailbox or receipted mail")
	}
	for _, kv := range processEnviron(t, child.Process.Pid) {
		if strings.Contains(kv, "synthetic-never-inherit") {
			t.Fatal("exec widened environment")
		}
	}
	f.update(func(st *config.State) { st.HooksPaused = false })
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return slices.Contains(st.Terminal.Rung, id) })
	f.control(map[string]any{})
	screen := f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
	if screen.Submitted[0] != tmuxwake.Notice(1) || f.e.received("alice", id) {
		t.Fatal("post-exec consumer failed delivery contract")
	}
	count := f.e.hs.SyncCount("@alice:" + testServer)
	until = time.Now().Add(testDeadline)
	for f.e.hs.SyncCount("@alice:"+testServer) < count+3 && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if f.e.hs.SyncCount("@alice:"+testServer) < count+3 {
		t.Fatalf("replacement consumer stopped cycling: context=%v log=%s", ctx.Err(), readFile(t, logPath))
	}
	if strings.Count(readFile(t, logPath), "binding re-exec: executable changed\n") != 1 {
		t.Fatal("replacement image entered exec loop")
	}
}

// A real Send is blocked inside its literal tmux command, after reservation.
// No refresh callback is observed while that command blocks. After release,
// the callback observes a completed journal and the notice submits once.
func TestTerminalRefreshWaitsForSend(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	f.a.Now = time.Now
	id := f.mail()
	// Establish the first qualified observation before entering the loop.
	f.dispatch()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(f.dir, "send-gate")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := filepath.Join(dir, "entered"), filepath.Join(dir, "release")
	script := "#!/usr/bin/env python3\nimport os,sys,time\nif '-l' in sys.argv:\n open(" + strconv.Quote(entered) + ",'w').close()\n while not os.path.exists(" + strconv.Quote(release) + "): time.sleep(0.01)\nos.execv(" + strconv.Quote(real) + ",[" + strconv.Quote(real) + "]+sys.argv[1:])\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	reached := make(chan struct{}, 1)
	done := make(chan error, 1)
	initial := *f.binding()
	go func() {
		_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", initial, func() {
			if _, err := os.Stat(entered); err == nil {
				if _, err := os.Stat(release); errors.Is(err, os.ErrNotExist) {
					select {
					case reached <- struct{}{}:
					default:
					}
				}
			}
			st, err := config.LoadState(f.s.paths, f.s.name)
			if err == nil && st.Terminal != nil && slices.Contains(st.Terminal.Rung, id) {
				cancel()
			}
		})
		done <- err
	}()
	until := time.Now().Add(6 * time.Second)
	for time.Now().Before(until) {
		if _, err := os.Stat(entered); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(entered); err != nil {
		t.Fatal("setup: Send did not reach literal command")
	}
	select {
	case <-reached:
		t.Fatal("refresh ran during Send")
	default:
	}
	b := f.binding()
	if len(b.AttemptIDs) != 1 || f.e.state("alice").Notification.Reservation == nil {
		t.Fatal("setup: missing mid-Send journal")
	}
	if err := os.WriteFile(release, nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("Send did not drain")
	}
	if ctx.Err() != context.Canceled {
		t.Fatal("safe cycle never reached refresh")
	}
	if !slices.Contains(f.binding().Rung, id) {
		t.Fatal("Send did not finish before refresh")
	}
}

// A persistent Matrix sync error does not strand an otherwise safe old image;
// the callback reaches exec only after bounded error handling releases locks.
func TestTerminalRefreshDuringSyncFailure(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	f.a.Now = time.Now
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f.update(func(st *config.State) { st.Terminal.DaemonPID = os.Getpid(); st.Terminal.DaemonBirth = birth })
	f.e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", "synthetic persistent sync failure")
	path, _ := refreshExecutable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	calls := 0
	r := &terminalRefresh{path: path, exec: func(string, []string, []string) error {
		calls++
		if err := config.WithRuntimeLock(f.s.paths, f.s.name, "control", time.Now(), func() error { return nil }); err != nil {
			t.Error("sync-error refresh held control")
		}
		cancel()
		return syscall.ENOEXEC
	}}
	matureRefresh(t, r)
	err = config.WithConsumerLock(f.s.paths, f.s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() { r.poll(ctx, f.a, f.s, "orphan-gen", lock) })
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || f.binding().Outcome != "consumer unavailable; retrying" {
		t.Fatal("safe sync-error cycle stranded old executable")
	}
}

// A failed exec returns to the same consumer loop: retained mail is delivered
// afterwards, while the unchanged failed candidate is attempted only once.
func TestTerminalRefreshExecFailureContinues(t *testing.T) {
	isolateBindTest(t)
	testTerminalRefreshRefusalContinues(t, false)
}

// An old native candidate rejects the hidden flag: no exec occurs, one probe
// refusal is logged, and the same consumer delivers retained mail afterwards.
func TestTerminalRefreshOldCandidateContinues(t *testing.T) {
	isolateBindTest(t)
	testTerminalRefreshRefusalContinues(t, true)
}

func testTerminalRefreshRefusalContinues(t *testing.T, old bool) {
	t.Helper()
	f := newOrphanDispatchFixture(t)
	f.a.Now = time.Now
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f.update(func(st *config.State) { st.Terminal.DaemonPID = os.Getpid(); st.Terminal.DaemonBirth = birth })
	id := f.mail()
	path, _ := refreshExecutable(t)
	if old {
		next := filepath.Join(filepath.Dir(path), "old")
		if err := os.Rename(path, next); err != nil {
			t.Fatal(err)
		}
		path = next
	}
	var log strings.Builder
	f.a.Stderr = &log
	probes := 0
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	calls := 0
	r := &terminalRefresh{path: path,
		probe: func(ctx context.Context, path string, env []string) error {
			probes++
			return probeTerminalConsumer(ctx, path, env)
		},
		exec: func(string, []string, []string) error { calls++; return syscall.EACCES }}
	matureRefresh(t, r)
	err = config.WithConsumerLock(f.s.paths, f.s.name, time.Now().Add(time.Second), 0, func(lock *os.File) error {
		_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() {
			r.poll(ctx, f.a, f.s, "orphan-gen", lock)
			st, err := config.LoadState(f.s.paths, f.s.name)
			if err == nil && st.Terminal != nil && slices.Contains(st.Terminal.Rung, id) {
				cancel()
			}
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	st := f.e.state("alice")
	wantCalls := 1
	if old {
		wantCalls = 0
	}
	if probes != 1 || calls != wantCalls || !slices.Contains(st.Terminal.Rung, id) || st.Terminal.DaemonPID != os.Getpid() || st.Terminal.DaemonBirth != birth || st.Lease == nil || st.Lease.Holder != "owned-thread" || !st.Lease.Expires.After(time.Now()) || f.e.received("alice", id) {
		t.Fatal("exec failure stopped, duplicated or changed the existing consumer")
	}
	if old && strings.Count(log.String(), "binding re-exec refused: protocol incompatible\n") != 1 {
		t.Fatal("old candidate refusal missing or repeated")
	}
	screen := f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
	if screen.Submitted[0] != tmuxwake.Notice(1) {
		t.Fatal("old consumer did not deliver after exec failure")
	}
}
