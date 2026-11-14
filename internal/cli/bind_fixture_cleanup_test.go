//go:build darwin || linux

package cli

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// cleanupFixture is a fixture directory whose state names pid as alice's
// consumer with the given birth, but matches no fixture record, so
// bindFixtureDaemon cannot identify it (the PID 0 path).
func cleanupFixture(t *testing.T, pid int, birth string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fixture")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := config.Paths{Base: filepath.Join(dir, "config")}
	if err := os.MkdirAll(filepath.Join(p.Base, "state"), 0700); err != nil {
		t.Fatal(err)
	}
	st := config.State{Terminal: &config.TerminalBinding{Generation: "unidentified", DaemonPID: pid, DaemonBirth: birth}}
	if err := config.SaveState(p, "alice", st); err != nil {
		t.Fatalf("setup: state: %v", err)
	}
	if got := bindFixtureDaemon(dir, bindFixtureRecord{}); got.PID != 0 {
		t.Fatalf("setup: the planted consumer must be unidentifiable, got pid %d", got.PID)
	}
	return dir
}

// startSleeper starts an own child that the test alone stops.
func startSleeper(t *testing.T, dir string) (*exec.Cmd, bindFixtureProcess) {
	t.Helper()
	cmd := exec.Command("sleep", "30")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	id, err := bindFixtureIdentity(cmd.Process.Pid)
	if err != nil {
		stop()
		t.Fatalf("setup: sleeper identity: %v", err)
	}
	return cmd, id
}

// startReaped starts cmd as an own child and reaps it as soon as it exits,
// so cleanup can observe a signalled child as exited rather than as a zombie.
func startReaped(t *testing.T, cmd *exec.Cmd) bindFixtureProcess {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	id, err := bindFixtureIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("setup: child identity: %v", err)
	}
	return id
}

// exitedIdentity is the identity of an own child that has already exited.
func exitedIdentity(t *testing.T) bindFixtureProcess {
	t.Helper()
	cmd, id := startSleeper(t, t.TempDir())
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return id
}

// fakeConsumer starts an own child whose command line is exactly
// "argv0 --as alice bind --run-generation gen", the shape bindFixtureDaemon
// admits. gen doubles as the sleep duration, so it must be a number.
func fakeConsumer(t *testing.T, argv0, gen string) bindFixtureProcess {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash unavailable")
	}
	cmd := exec.Command("bash", "-c", `exec -a "$0 --as alice bind --run-generation" sleep "$1"`, argv0, gen)
	id := startReaped(t, cmd)
	want := argv0 + " --as alice bind --run-generation " + gen
	until := time.Now().Add(2 * time.Second)
	for bindFixtureCommand(id.PID) != want && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := bindFixtureCommand(id.PID); got != want {
		t.Fatalf("setup: fake consumer command %q, want %q", got, want)
	}
	return id
}

func fixturePathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Proves that reclaimBindFixture does not treat an unidentifiable consumer
// as gone. While a live process the state names (by PID and birth) still
// runs, the fixture stays and that process is not signalled. Once it exits,
// the fixture is removed. Before this change, PID 0 counted as gone and
// RemoveAll ran immediately, which is the absolute_alias cleanup race class.
// Limit: only the alice state slot and the PID plus birth match are covered.
func TestReclaimBindFixtureWaitsForUnidentifiedConsumer(t *testing.T) {
	cmd, id := startSleeper(t, t.TempDir())
	dir := cleanupFixture(t, id.PID, id.Birth)
	if pid, _, obs := bindFixtureNamedConsumer(dir); pid != id.PID || obs != bindFixtureLive {
		t.Fatalf("setup: the state must name the live sleeper, got pid %d observation %d", pid, obs)
	}
	done := make(chan error, 1)
	go func() { done <- reclaimBindFixture(dir, bindFixtureRecord{}) }()
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("reclaim finished while the named consumer was live: %v", err)
	default:
	}
	if !fixturePathExists(dir) {
		t.Fatal("fixture removed while the named consumer was live")
	}
	if processGone(id.PID) {
		t.Fatal("the unidentified consumer was signalled")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reclaim after the consumer exited: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reclaim did not finish after the consumer exited")
	}
	if fixturePathExists(dir) {
		t.Fatal("fixture not removed after the consumer exited")
	}
}

// Proves the bound: a named consumer that outlives the cleanup bound yields
// the named refusal, the fixture is kept, and that process is not killed
// (cleanup has no signal authority over an unidentified process).
func TestReclaimBindFixtureRefusesLiveUnidentifiedConsumer(t *testing.T) {
	_, id := startSleeper(t, t.TempDir())
	dir := cleanupFixture(t, id.PID, id.Birth)
	if pid, _, obs := bindFixtureNamedConsumer(dir); pid != id.PID || obs != bindFixtureLive {
		t.Fatalf("setup: the state must name the live sleeper, got pid %d observation %d", pid, obs)
	}
	err := reclaimBindFixture(dir, bindFixtureRecord{})
	if err == nil || !strings.Contains(err.Error(), "unidentified state consumer pid "+strconv.Itoa(id.PID)+" still live") {
		t.Fatalf("got %v, want the named unidentified-consumer refusal", err)
	}
	if !fixturePathExists(dir) {
		t.Fatal("fixture removed despite the refusal")
	}
	if processGone(id.PID) {
		t.Fatal("the unidentified consumer was killed")
	}
}

// Positive controls for the two refusals above: a named consumer that has
// already exited, and one whose birth does not match the live PID, name
// nothing, and the fixture is removed promptly.
func TestReclaimBindFixtureRemovesWhenNamedConsumerGone(t *testing.T) {
	exited := exec.Command("true")
	if err := exited.Run(); err != nil {
		t.Fatal(err)
	}
	_, live := startSleeper(t, t.TempDir())
	for name, c := range map[string]struct {
		pid   int
		birth string
	}{
		"exited":         {exited.Process.Pid, live.Birth},
		"birth mismatch": {live.PID, "not-" + live.Birth},
	} {
		t.Run(name, func(t *testing.T) {
			dir := cleanupFixture(t, c.pid, c.birth)
			start := time.Now()
			if err := reclaimBindFixture(dir, bindFixtureRecord{}); err != nil {
				t.Fatal(err)
			}
			if fixturePathExists(dir) {
				t.Fatal("fixture not removed")
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Fatalf("reclaim took %v, want prompt removal", d)
			}
		})
	}
	if processGone(live.PID) {
		t.Fatal("a process with a mismatched birth was signalled")
	}
}

// Proves that a removal failure is reported with what remains: the leftover
// entry names, and on Linux the same-user process whose cwd is inside the
// fixture. It also proves that there is no retry that would hide the residue.
// The failure is planted with a 0500 directory, so it is skipped for root.
func TestRemoveBindFixtureDirNamesResidue(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores the planted 0500 directory")
	}
	dir := filepath.Join(t.TempDir(), "fixture")
	locked := filepath.Join(dir, "config", "state", "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "x"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	_, holder := startSleeper(t, filepath.Join(dir, "config", "state"))
	err := removeBindFixtureDir(dir)
	if err == nil {
		t.Fatal("removal of a planted unremovable fixture succeeded")
	}
	msg := err.Error()
	if !strings.Contains(msg, filepath.Join("config", "state", "locked", "x")) {
		t.Fatalf("leftover entry not named: %s", msg)
	}
	want := "holders=[unavailable]"
	if runtime.GOOS == "linux" {
		want = "pid=" + strconv.Itoa(holder.PID) + " comm=sleep via=cwd"
	}
	if !strings.Contains(msg, want) {
		t.Fatalf("holder not reported as %q: %s", want, msg)
	}
	if !fixturePathExists(filepath.Join(locked, "x")) {
		t.Fatal("residue changed after the failure")
	}
}

// Positive control: a removable fixture with nested entries and no holder
// is removed, and no error is reported.
func TestRemoveBindFixtureDirRemovesCleanFixture(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	if err := os.MkdirAll(filepath.Join(dir, "config", "state"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "state", "alice.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := removeBindFixtureDir(dir); err != nil {
		t.Fatal(err)
	}
	if fixturePathExists(dir) {
		t.Fatal("fixture not removed")
	}
}

// Proves that a failed observation is not exit evidence (review R2). With
// ps unavailable, neither a recorded nor a state-named live consumer can be
// observed: cleanup keeps the fixture, returns the named "unobservable"
// refusal, and does not signal the consumer. The working-observer control is
// TestReclaimBindFixtureStopsRecordedConsumer.
func TestReclaimBindFixtureUnobservableConsumerKeepsFixture(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "state named", true: "recorded"}[recorded], func(t *testing.T) {
			_, id := startSleeper(t, t.TempDir())
			dir := cleanupFixture(t, id.PID, id.Birth)
			var r bindFixtureRecord
			if recorded {
				r.Daemon = id
			}
			t.Setenv("PATH", t.TempDir())
			err := reclaimBindFixture(dir, r)
			if err == nil || !strings.Contains(err.Error(), "pid "+strconv.Itoa(id.PID)+" unobservable") {
				t.Fatalf("got %v, want the named unobservable refusal", err)
			}
			if !fixturePathExists(dir) {
				t.Fatal("fixture removed without exit evidence")
			}
			if processGone(id.PID) {
				t.Fatal("an unobservable consumer was signalled")
			}
		})
	}
}

// Positive controls with a working observer. A recorded consumer with no
// state, one that the state also names (the same identity, a single
// obligation), and one that has really exec'd since it was recorded (PID,
// birth and start ticks unchanged), are each stopped through the recorded
// identity, observed exited, and the fixture is removed. Limit: the exec is
// sh into sleep, not the CLI's own re-exec.
func TestReclaimBindFixtureStopsRecordedConsumer(t *testing.T) {
	t.Run("no state", func(t *testing.T) {
		id := startReaped(t, exec.Command("sleep", "30"))
		dir := cleanupFixture(t, id.PID, id.Birth)
		if err := os.Remove(filepath.Join(dir, "config", "state", "alice.json")); err != nil {
			t.Fatal(err)
		}
		if err := reclaimBindFixture(dir, bindFixtureRecord{Daemon: id}); err != nil {
			t.Fatal(err)
		}
		if fixturePathExists(dir) || !processGone(id.PID) {
			t.Fatal("recorded consumer not stopped and fixture not removed")
		}
	})
	t.Run("state names the same identity", func(t *testing.T) {
		id := startReaped(t, exec.Command("sleep", "30"))
		dir := cleanupFixture(t, id.PID, id.Birth)
		if err := reclaimBindFixture(dir, bindFixtureRecord{Daemon: id}); err != nil {
			t.Fatal(err)
		}
		if fixturePathExists(dir) || !processGone(id.PID) {
			t.Fatal("recorded consumer not stopped and fixture not removed")
		}
	})
	t.Run("exec after recording", func(t *testing.T) {
		cmd := exec.Command("sh", "-c", "read ready; exec sleep 30")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		before := startReaped(t, cmd)
		if _, err := input.Write([]byte("ready\n")); err != nil {
			t.Fatal(err)
		}
		_ = input.Close()
		until := time.Now().Add(2 * time.Second)
		for bindFixtureCommand(before.PID) != "sleep 30" && time.Now().Before(until) {
			time.Sleep(10 * time.Millisecond)
		}
		if after, err := bindFixtureIdentity(before.PID); bindFixtureCommand(before.PID) != "sleep 30" || err != nil || after != before {
			t.Fatalf("setup: exec did not keep the identity: before=%+v after=%+v err=%v", before, after, err)
		}
		dir := cleanupFixture(t, before.PID, before.Birth)
		if err := reclaimBindFixture(dir, bindFixtureRecord{Daemon: before}); err != nil {
			t.Fatal(err)
		}
		if fixturePathExists(dir) || !processGone(before.PID) {
			t.Fatal("re-executed recorded consumer not stopped and fixture not removed")
		}
	})
}

// Proves that a state-identified successor does not replace the recorded
// consumer (review R3). The record holds an original live consumer while the
// state admits a different live successor. Both are separate obligations:
// cleanup stops both and removes the fixture only after both have exited.
// Before this change the successor overwrote the record, and the fixture was
// removed while the original was still running.
func TestReclaimBindFixtureKeepsRecordedAndSuccessorConsumers(t *testing.T) {
	argv0 := "/fixture/a2a"
	original := fakeConsumer(t, argv0, "31")
	successor := fakeConsumer(t, argv0, "30")
	fixture, server := exitedIdentity(t), exitedIdentity(t)
	dir := cleanupFixture(t, successor.PID, successor.Birth)
	st := config.State{Terminal: &config.TerminalBinding{Generation: "30", SessionID: "bind-fixture", Host: machineLabel(),
		OwnerPID: fixture.PID, OwnerBirth: fixture.Birth, DaemonPID: successor.PID, DaemonBirth: successor.Birth}}
	st.Terminal.Target.Socket = filepath.Join(dir, "socket")
	st.Terminal.Target.OwnerPID, st.Terminal.Target.OwnerBirth = fixture.PID, fixture.Birth
	if err := config.SaveState(config.Paths{Base: filepath.Join(dir, "config")}, "alice", st); err != nil {
		t.Fatal(err)
	}
	r := bindFixtureRecord{Fixture: fixture, Server: server, Daemon: original, Consumers: []string{argv0}}
	if got := bindFixtureDaemon(dir, r); got != successor {
		t.Fatalf("setup: the state must identify the successor, got %+v", got)
	}
	if err := reclaimBindFixture(dir, r); err != nil {
		t.Fatal(err)
	}
	if !processGone(original.PID) {
		t.Fatal("fixture cleanup finished while the recorded original consumer is still live")
	}
	if !processGone(successor.PID) {
		t.Fatal("fixture cleanup finished while the successor consumer is still live")
	}
	if fixturePathExists(dir) {
		t.Fatal("fixture not removed after both consumers exited")
	}
}

// Proves that only the same identity (PID and birth) merges the recorded and
// the state-named obligations (review R4). A stale record that shares the
// live consumer's PID but carries another birth is exited, and grants no
// signal authority. The state-named live consumer stays a separate
// obligation: the fixture is kept and the consumer is not signalled until it
// exits, and only then is the fixture removed. Before this change the shared
// PID alone discarded the live obligation and the fixture was removed at once.
func TestReclaimBindFixtureStaleRecordSharingPIDKeepsStateObligation(t *testing.T) {
	cmd, live := startSleeper(t, t.TempDir())
	dir := cleanupFixture(t, live.PID, live.Birth)
	stale := bindFixtureProcess{PID: live.PID, Birth: "stale " + live.Birth, Start: live.Start}
	if stale.live() || stale.observe() != bindFixtureExited {
		t.Fatal("setup: the stale record must observe as exited and grant no signal")
	}
	done := make(chan error, 1)
	go func() { done <- reclaimBindFixture(dir, bindFixtureRecord{Daemon: stale}) }()
	time.Sleep(500 * time.Millisecond)
	select {
	case err := <-done:
		t.Fatalf("reclaim finished while the state-named consumer was live: %v", err)
	default:
	}
	if !fixturePathExists(dir) {
		t.Fatal("fixture removed while the state-named consumer was live")
	}
	if processGone(live.PID) {
		t.Fatal("the state-named consumer was signalled through a stale record")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reclaim after the consumer exited: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reclaim did not finish after the consumer exited")
	}
	if fixturePathExists(dir) {
		t.Fatal("fixture not removed after the consumer exited")
	}
}

// Positive control for R4: a stale record (exited, another birth) with no
// state names nothing live, so the fixture is removed promptly. The
// same-identity merge is covered by TestReclaimBindFixtureStopsRecordedConsumer
// ("state names the same identity").
func TestReclaimBindFixtureStaleRecordWithoutStateRemoves(t *testing.T) {
	_, live := startSleeper(t, t.TempDir())
	dir := cleanupFixture(t, live.PID, live.Birth)
	if err := os.Remove(filepath.Join(dir, "config", "state", "alice.json")); err != nil {
		t.Fatal(err)
	}
	stale := bindFixtureProcess{PID: live.PID, Birth: "stale " + live.Birth, Start: live.Start}
	start := time.Now()
	if err := reclaimBindFixture(dir, bindFixtureRecord{Daemon: stale}); err != nil {
		t.Fatal(err)
	}
	if fixturePathExists(dir) {
		t.Fatal("fixture not removed")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("reclaim took %v, want prompt removal", d)
	}
	if processGone(live.PID) {
		t.Fatal("a process sharing only the PID was signalled")
	}
}

// recordingFS records every name the holder scan asks for, so a test can
// prove which /proc files it touched.
type recordingFS struct {
	fs.FS
	mu    sync.Mutex
	names []string
}

func (r *recordingFS) note(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.names = append(r.names, name)
}

func (r *recordingFS) Open(name string) (fs.File, error) { r.note(name); return r.FS.Open(name) }
func (r *recordingFS) Stat(name string) (fs.FileInfo, error) {
	r.note(name)
	return fs.Stat(r.FS, name)
}
func (r *recordingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	r.note(name)
	return fs.ReadDir(r.FS, name)
}
func (r *recordingFS) ReadFile(name string) ([]byte, error) {
	r.note(name)
	return fs.ReadFile(r.FS, name)
}
func (r *recordingFS) ReadLink(name string) (string, error) {
	r.note(name)
	return fs.ReadLink(r.FS, name)
}
func (r *recordingFS) Lstat(name string) (fs.FileInfo, error) {
	r.note(name)
	return fs.Lstat(r.FS, name)
}

// environNames returns every recorded name that is a process environ file.
func (r *recordingFS) environNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var hits []string
	for _, n := range r.names {
		if n == "environ" || strings.HasSuffix(n, "/environ") {
			hits = append(hits, n)
		}
	}
	return hits
}

// Proves that the holder scan never opens or stats any process's environ
// file: on a shared host that holds other agents' secrets. The same scan
// still names a real cwd holder (the control that it ran), and the recorder
// sees an environ read when one happens (the control that the detector
// works). Linux only, where /proc exists.
func TestBindFixtureHoldersNeverReadEnviron(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("no /proc")
	}
	held := t.TempDir()
	_, holder := startSleeper(t, held)
	rec := &recordingFS{FS: os.DirFS("/proc")}
	got := bindFixtureHoldersIn(rec, held)
	if want := "pid=" + strconv.Itoa(holder.PID) + " comm=sleep via=cwd"; !strings.Contains(got, want) {
		t.Fatalf("scan did not name the cwd holder %q: %s", want, got)
	}
	if hits := rec.environNames(); len(hits) != 0 {
		t.Fatalf("holder scan touched environ files: %v", hits)
	}
	probe := &recordingFS{FS: os.DirFS("/proc")}
	_, _ = fs.ReadFile(probe, "self/environ")
	if len(probe.environNames()) == 0 {
		t.Fatal("detector control: an environ read was not recorded")
	}
}
