package cli

import (
	"context"
	"encoding/json"
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

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// The existing private Python/tmux fixture language is reused. Deferral changes
// the actual screen immediately after typing, before Send's second capture.
// No runtime/provider, real mailbox, credential store or live pane is used.
const orphanFixture = `import os,sys,tty,json,select
tty.setraw(0)
root=sys.argv[1]; text=''; submitted=[]; erased=0; rev=0; mode={}
def draw():
    footer='Codex · Ready' if text and not mode.get('hint') else '? for shortcuts'
    if mode.get('busy') or (mode.get('defer') and text): footer='esc to interrupt'
    if mode.get('unknown'): footer='unknown layout'
    suffix='\x1b[2m'+mode.get('dim-text','KEEP')+'\x1b[0m' if mode.get('dim') else ''
    prefix='\x1b[2m'+mode.get('dim-text','KEEP')+'\x1b[0m' if mode.get('dim-prefix') else ''
    if mode.get('unknown'): suffix+='\x1b[21;1Hunknown input'
    composer='› '+prefix+text+suffix
    layout='\x1b[20;1H'+composer+'\x1b[22;1H  '+footer
    if mode.get('claude'):
        rule='─'*100
        top=rule+(' synthetic-session' if mode.get('named') else '')
        layout='\x1b[19;1H'+top+'\x1b[20;1H❯ '+prefix+text+suffix+'\x1b[21;1H'+rule+'\x1b[23;1H  '+footer
    os.write(1,('\x1b[2J\x1b[1;1Ha2a-fixture-frame:'+str(rev)+':end'+layout+'\x1b[20;'+str(3+len(text)+mode.get('cursor',0))+'H').encode())
    with open(root+'/screen.next','w') as f: json.dump(dict(text=text,submitted=submitted,erased=erased,pid=os.getpid(),rev=rev),f)
    os.replace(root+'/screen.next',root+'/screen.json')
draw()
while True:
    if os.path.exists(root+'/control.json'):
        with open(root+'/control.json') as f: request=json.load(f)
        os.unlink(root+'/control.json')
        mode=request; rev=request['rev']; text=request.get('text',text); draw()
    ready,_,_=select.select([0],[],[],0.005)
    if ready:
        for c in os.read(0,4096):
            if c in (13,10): submitted.append(text); text=''
            elif c in (127,8): text=text[:-1]; erased+=1
            else: text+=chr(c)
        draw()
`

type orphanScreen struct {
	Text             string
	Submitted        []string
	Erased, PID, Rev int
}

type orphanDispatchFixture struct {
	t         *testing.T
	e         *env
	a         *App
	s         *session
	dir, room string
	now       time.Time
	rev       int
}

func newOrphanDispatchFixture(t *testing.T) *orphanDispatchFixture {
	t.Helper()
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable; private dispatch lane unrun")
		}
	}
	e, room := world(t)
	e.extra[EnvSessionID] = "owned-thread"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dir, err := os.MkdirTemp("/tmp", "ao-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	script := filepath.Join(dir, "fixture.py")
	if err := os.WriteFile(script, []byte(orphanFixture), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	if out, err := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "python3", script, dir).CombinedOutput(); err != nil {
		t.Fatalf("private tmux: %v %s", err, out)
	}
	f := &orphanDispatchFixture{t: t, e: e, dir: dir, room: room, now: time.Now()}
	state := f.wait(func(s orphanScreen) bool { return s.PID > 1 })
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	serverPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || serverPID <= 1 {
		t.Fatal("missing private server PID")
	}
	t.Cleanup(func() {
		for _, pid := range []int{state.PID, serverPID} {
			p, _ := os.FindProcess(pid)
			_ = p.Signal(syscall.SIGTERM)
		}
	})
	target, err := tmuxwake.Pin(context.Background(), socket, "%0", "codex")
	if err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), state.PID)
	if err != nil {
		t.Fatal(err)
	}
	target.OwnerPID = state.PID
	target.OwnerBirth = birth
	f.a = &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Now: func() time.Time { return f.now }, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client()}
	f.s, err = f.a.session()
	if err != nil {
		t.Fatal(err)
	}
	f.update(func(st *config.State) {
		st.Terminal = &config.TerminalBinding{Generation: "orphan-gen", SessionID: "owned-thread", Host: machineLabel(), OwnerPID: state.PID, OwnerBirth: birth, Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 30}
	})
	f.rendered()
	return f
}

// A fixture JSON acknowledgement follows its PTY write, not tmux rendering.
// Wait for the displayed frame too, outside dispatch/control locks. The marker
// is in row zero, outside the recognized bottom input area; it grants no input
// authority and does not collect product idle qualification samples.
func (f *orphanDispatchFixture) rendered() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	f.wait(func(s orphanScreen) bool {
		if s.Rev != f.rev {
			return false
		}
		out, err := exec.CommandContext(ctx, "tmux", "-S", filepath.Join(f.dir, "socket"), "capture-pane", "-p", "-t", "%0").Output()
		return err == nil && strings.Contains(string(out), "a2a-fixture-frame:"+strconv.Itoa(f.rev)+":end")
	})
}

func (f *orphanDispatchFixture) wait(match func(orphanScreen) bool) orphanScreen {
	f.t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		b, err := os.ReadFile(filepath.Join(f.dir, "screen.json"))
		var s orphanScreen
		if err == nil && json.Unmarshal(b, &s) == nil && match(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("private screen condition not reached")
	return orphanScreen{}
}
func (f *orphanDispatchFixture) control(mode map[string]any) {
	f.t.Helper()
	f.rev++
	mode["rev"] = f.rev
	b, _ := json.Marshal(mode)
	if err := os.WriteFile(filepath.Join(f.dir, "control.next"), b, 0600); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.dir, "control.next"), filepath.Join(f.dir, "control.json")); err != nil {
		f.t.Fatal(err)
	}
	f.wait(func(s orphanScreen) bool { return s.Rev == f.rev })
	f.rendered()
}
func (f *orphanDispatchFixture) update(fn func(*config.State)) {
	f.t.Helper()
	_, err := config.UpdateState(f.e.paths(), "alice", func(st *config.State) error { fn(st); return nil })
	if err != nil {
		f.t.Fatal(err)
	}
}
func (f *orphanDispatchFixture) dispatch() {
	f.t.Helper()
	if err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen"); err != nil {
		f.t.Fatal(err)
	}
}
func (f *orphanDispatchFixture) step() { f.now = f.now.Add(time.Second); f.dispatch() }
func (f *orphanDispatchFixture) mail() string {
	return runtimeMail(f.e, f.room, "SYNTHETIC_ORPHAN_MAIL")
}
func (f *orphanDispatchFixture) binding() *config.TerminalBinding { return f.e.state("alice").Terminal }

// Real dispatch types, defers on the busy post-type screen, then own inbox
// receipts its attempt. Readback/recreated App represents a journal restart.
// Manual fixture clearing precedes one qualified current-mail wake; the
// nonempty orphan is never automatically cleared under keeper o-JKabzo.
func TestTerminalDispatchReceiptedOrphan(t *testing.T) {
	for _, neighbor := range []string{"new-mail", "no-new-then-later", "partial-pruning"} {
		t.Run(neighbor, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			old := f.mail()
			var retained string
			if neighbor == "partial-pruning" {
				retained = f.mail()
			}
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			b := f.binding()
			if !b.Typed || b.NoticeCount != len(b.AttemptIDs) || b.Notice == "" {
				t.Fatalf("missing real typed journal: %+v", b)
			}
			if neighbor == "partial-pruning" {
				_, err := f.s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
					_, err := mb.Receive([]string{old})
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				f.e.as("alice", "inbox").ok(t)
			}
			if !f.e.received("alice", old) {
				t.Fatal("own inbox did not receipt old attempt")
			}
			f.control(map[string]any{})
			clone := *f.a
			f.a = &clone
			if neighbor == "no-new-then-later" {
				f.step()
				s := f.wait(func(s orphanScreen) bool { return true })
				if s.Text == "" || s.Erased != 0 || len(s.Submitted) != 0 || len(f.binding().AttemptIDs) != 0 {
					t.Fatal("no-mail orphan submitted or retained journal")
				}
			}
			// Manual fixture action: recovery is external under keeper o-JKabzo.
			f.control(map[string]any{"text": ""})
			newID := f.mail()
			f.now = f.now.Add(config.NotificationEvery) // uncertainty spends shared spacing, not a receipt
			for i := 0; i < 4; i++ {
				f.step()
			}
			s := f.wait(func(s orphanScreen) bool { return true })
			want := tmuxwake.Notice(1)
			if retained != "" {
				want = tmuxwake.Notice(2)
			}
			if len(s.Submitted) != 1 || s.Submitted[0] != want || s.Text != "" {
				t.Fatalf("orphan suppressed/duplicated current wake: %+v want %q", s, want)
			}
			if f.e.received("alice", newID) || retained != "" && f.e.received("alice", retained) {
				t.Fatal("recovery lost pending mail")
			}
			b = f.binding()
			if b.Typed || len(b.AttemptIDs) != 0 || b.Notice != "" || !slices.Contains(b.Rung, newID) {
				t.Fatalf("recovery journal not settled: %+v", b)
			}
		})
	}
}

// A first idle glimpse, even repeated at the same instant, cannot type or
// reserve an attempt. Busy/unknown observations break qualification.
func TestTerminalDispatchIdleQualification(t *testing.T) {
	for _, reset := range []string{"none", "busy", "unknown", "draft", "paused", "manual", "native", "daemon", "session", "target"} {
		t.Run(reset, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			f.control(map[string]any{"busy": true})
			f.dispatch()
			f.control(map[string]any{})
			f.step()
			f.dispatch()
			assertUntyped := func() {
				t.Helper()
				s := f.wait(func(s orphanScreen) bool { return true })
				b := f.binding()
				if s.Text != "" || len(s.Submitted) != 0 || len(b.Reservations) != 0 || len(b.AttemptIDs) != 0 {
					t.Fatalf("unqualified idle typed or reserved: %+v %+v", s, b)
				}
			}
			assertUntyped()
			if reset != "none" {
				switch reset {
				case "busy", "unknown":
					f.control(map[string]any{reset: true})
				case "draft":
					f.control(map[string]any{"text": "KEEP"})
				case "paused":
					f.update(func(st *config.State) { st.HooksPaused = true })
				case "manual":
					f.update(func(st *config.State) {
						st.Liveness = &config.AgentStatus{State: "waiting-human"}
						st.LivenessSource = "manual"
					})
				case "native":
					f.update(func(st *config.State) { st.Runtime = &codexwake.Binding{} })
				case "daemon":
					f.update(func(st *config.State) { st.Terminal.DaemonBirth = "replacement" })
				case "session":
					f.update(func(st *config.State) { st.Terminal.SessionID = "replacement"; st.Lease.Holder = "replacement" })
				case "target":
					f.update(func(st *config.State) { st.Terminal.Target.SocketID = "replacement" })
				}
				f.step()
				if reset == "daemon" || reset == "session" {
					assertUntyped()
				}
				f.control(map[string]any{"text": ""})
				f.update(func(st *config.State) {
					st.HooksPaused = false
					st.Liveness = nil
					st.LivenessSource = ""
					st.Runtime = nil
					if reset == "target" {
						id, _ := tmuxwake.SocketID(st.Terminal.Target.Socket)
						st.Terminal.Target.SocketID = id
					}
				})
				if reset != "daemon" && reset != "session" {
					f.step()
					assertUntyped()
				}
			}
			f.step()
			s := f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
			if s.Submitted[0] != tmuxwake.Notice(1) || len(f.binding().Reservations) != 1 {
				t.Fatal("qualified idle did not submit once")
			}
		})
	}
}

// Every case reaches production dispatch with later pending mail. The old
// exact-owned positive is now a no-keystroke safety control (keeper o-JKabzo).
// Journals never authorize erase/Enter; pending mail and reservations remain.
func TestTerminalDispatchOrphanOwnership(t *testing.T) {
	for _, name := range []string{"valid", "no-journal", "untyped", "count", "notice", "prefix", "suffix", "dim", "cursor", "unknown", "busy", "owner", "owner-birth", "binding-owner", "pane", "pane-birth", "socket", "host", "stopped", "generation", "paused", "manual", "native", "rate", "missing-lease", "foreign-lease", "expired-lease"} {
		t.Run(name, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			notice := tmuxwake.Notice(1)
			mode := map[string]any{"text": notice}
			if name == "prefix" {
				mode["text"] = "KEEP" + notice
			}
			if name == "suffix" {
				mode["text"] = notice + "KEEP"
			}
			if name == "dim" || name == "unknown" || name == "busy" {
				mode[name] = true
			}
			if name == "cursor" {
				mode["cursor"] = -1
			}
			f.control(mode)
			f.update(func(st *config.State) {
				b := st.Terminal
				b.Typed = true
				b.Notice = notice
				b.NoticeCount = 1
				b.AttemptAt = f.now.Add(-time.Minute)
				b.Retried = true
				b.Reservations = []time.Time{f.now}
				b.Rung = []string{"old-rung"}
				b.AttemptIDs = []string{"receipted-id"}
				switch name {
				case "no-journal":
					b.Typed = false
					b.Notice = ""
					b.NoticeCount = 0
					b.AttemptIDs = nil
				case "untyped":
					b.Typed = false
					b.AttemptIDs = []string{id}
				case "count":
					b.NoticeCount = 2
				case "notice":
					b.Notice = tmuxwake.Notice(2)
				case "owner":
					b.Target.OwnerPID = os.Getpid()
					b.OwnerPID = os.Getpid()
				case "owner-birth":
					b.Target.OwnerBirth = "replacement"
					b.OwnerBirth = "replacement"
				case "binding-owner":
					b.OwnerPID++
				case "pane":
					b.Target.Pane = "%99"
				case "pane-birth":
					b.Target.PaneBirth = "replacement"
				case "socket":
					b.Target.SocketID = "replacement"
				case "host":
					b.Host = "other-host"
				case "stopped":
					b.Stopped = true
				case "generation":
					b.Generation = "replacement"
				case "paused":
					st.HooksPaused = true
				case "manual":
					st.Liveness = &config.AgentStatus{State: "waiting-human"}
					st.LivenessSource = "manual"
				case "native":
					st.Runtime = &codexwake.Binding{}
				case "rate":
					b.MaxWakes = 1
				case "missing-lease":
					st.Lease = nil
				case "foreign-lease":
					st.Lease.Holder = "another-session"
				case "expired-lease":
					st.Lease.Expires = f.now
				}
			})
			for i := 0; i < 4; i++ {
				f.now = f.now.Add(time.Second)
				err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
				if name == "stopped" || name == "generation" {
					if err == nil {
						t.Fatal("retired authority admitted")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			s := f.wait(func(s orphanScreen) bool { return true })
			b := f.binding()
			if s.Text != mode["text"] || s.Erased != 0 || len(s.Submitted) != 0 || len(b.Reservations) != 1 {
				t.Fatalf("forbidden orphan effect: %+v %+v", s, b)
			}
			if f.e.received("alice", id) {
				t.Fatal("orphan path receipted pending mail")
			}
		})
	}
}

// A pause/resume pulse between samples must invalidate qualification even when
// the daemon never observes the paused screen. Public pause commands own this
// reset; a failed authority read/interruption is likewise never an idle sample.
func TestTerminalDispatchQualificationInterruption(t *testing.T) {
	for _, name := range []string{"pause-pulse", "manual-pulse", "cancelled", "generation"} {
		t.Run(name, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			if name == "manual-pulse" {
				f.e.as("alice", "status", "init").ok(t)
			}
			f.dispatch()
			switch name {
			case "pause-pulse":
				f.e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
				f.e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
			case "manual-pulse":
				f.e.as("alice", statusArgs("waiting-human")...).ok(t)
				f.e.as("alice", statusArgs("idle")...).ok(t)
			case "cancelled":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_ = f.a.dispatchTerminal(ctx, f.s, "orphan-gen")
			case "generation":
				f.update(func(st *config.State) { st.Terminal.Generation = "replacement" })
				f.now = f.now.Add(time.Second)
				if err := f.a.dispatchTerminal(context.Background(), f.s, "replacement"); err != nil {
					t.Fatal(err)
				}
			}
			if name != "generation" {
				f.step()
			}
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != "" || len(s.Submitted) != 0 || len(f.binding().Reservations) != 0 {
				t.Fatal("interruption retained idle authority")
			}
		})
	}
}

// Restart the actual consumer loop from the disk journal after the old mail
// was receipted. A real sync error must reset the observed idle continuity;
// recovery uses the same fake Matrix and private tmux as the dispatch tests.
func TestTerminalRunOrphanRestartAndSyncRetry(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	if !f.binding().Typed {
		t.Fatal("missing pre-restart typed journal")
	}
	f.e.as("alice", "inbox").ok(t)
	id := f.mail()
	f.control(map[string]any{"text": ""}) // external manual clearing
	f.a.Now = time.Now
	f.e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", "synthetic interruption")
	initial := *f.binding()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := f.a.runTerminal(ctx, f.s, "orphan-gen", initial); result <- err }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-result:
		case <-time.After(4 * time.Second):
			t.Error("private consumer did not stop")
		}
	})
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "consumer unavailable; retrying" })
	if s := f.wait(func(s orphanScreen) bool { return true }); s.Erased != 0 || len(s.Submitted) != 0 {
		t.Fatal("failed sync touched orphan")
	}
	f.e.hs.mu.Lock()
	delete(f.e.hs.failures, "GET /_matrix/client/v3/sync")
	f.e.hs.mu.Unlock()
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "armed: qualifying idle" })
	if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 0 || len(f.binding().Reservations) != 1 {
		t.Fatal("retry's first idle glimpse spent attempt")
	}
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return slices.Contains(st.Terminal.Rung, id) })
	s := f.wait(func(s orphanScreen) bool { return true })
	if len(s.Submitted) != 1 || s.Submitted[0] != tmuxwake.Notice(1) || f.e.received("alice", id) {
		t.Fatal("restarted consumer lost or duplicated wake")
	}
}

// A failed state read also prevents a durable reset. Restoring the exact stale
// journal cannot revive its pre-failure idle sample: the running consumer must
// write a new first observation before reserving or typing anything.
func TestTerminalRunAuthorityReadFailure(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.a.Now = time.Now
	stderr := &flushBuffer{}
	f.a.Stderr = stderr
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := f.a.runTerminal(ctx, f.s, "orphan-gen", *f.binding()); result <- err }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-result:
		case <-time.After(4 * time.Second):
			t.Error("private consumer did not stop")
		}
	})
	st := awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Idle != nil && st.Terminal.NoticePublication != "" })
	previous := st.Terminal.Idle.At
	path := f.e.paths().StateFile("alice")
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Refuse replacement writes while the deliberately malformed file is read.
	// This keeps the fault from being overwritten by an in-flight publisher.
	stateDir := filepath.Dir(path)
	if err := os.Chmod(stateDir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(stateDir, 0700)
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	f.wait(func(orphanScreen) bool { log, _ := stderr.snapshot(); return strings.Contains(log, "bounded retry") })
	if err := os.Chmod(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, saved, 0600); err != nil {
		t.Fatal(err)
	}
	st = awaitBinding(t, f.e.paths(), func(st config.State) bool {
		return len(st.Terminal.Reservations) > 0 || st.Terminal.Idle != nil && st.Terminal.Idle.At.After(previous)
	})
	if len(st.Terminal.Reservations) != 0 {
		t.Fatal("restored authority reused stale idle observation")
	}
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return len(st.Terminal.Rung) == 1 })
	if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 1 {
		t.Fatal("fresh authority did not wake once")
	}
}
