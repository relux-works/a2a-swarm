//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// holdStateLock takes the flock another a2a process would hold on name's
// state and returns its release function.
func (e *env) holdStateLock(name string) func() {
	e.t.Helper()
	path := filepath.Join(filepath.Dir(e.paths().StateFile(name)), name+".lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		e.t.Fatal(err)
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }
}

// Proves the hooks' bounded lock wait: while another process holds the state
// lock past the hook budget, a hook exits 0 within about the budget with empty
// stdout and one stderr line, and records nothing (no rung set, nothing
// acknowledged, state file unchanged); after the lock is released the same
// hook rings (positive control). inbox keeps the blocking lock: it waits for
// the release and then prints.
//
// The marked row is an identity that has bound before: its lease and cursor
// are in the sidecar, and the typed operations on them wait for the same lock.
// Regression witness: those waits had no limit, and the hook of a marked
// identity waited for as long as the lock was held. The row also compares the
// sidecar's bytes: the refused hook left no reservation behind.
func TestHookLockTimeout(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(markedRow(marked), func(t *testing.T) { hookLockTimeout(t, marked) })
	}
}

func hookLockTimeout(t *testing.T, marked bool) {
	e, room := world(t)
	e.sequentialSessions()
	e.hook("alice", "prompt", `{}`).ok(t)
	if marked {
		markFixtureIdentity(t, e, "alice")
	}
	id := e.hs.inject(room, "@bob:test.local", ping(1))
	before := readFile(t, e.paths().StateFile("alice"))
	sidecarBefore := ""
	if marked {
		sidecarBefore = readFile(t, e.paths().BindPartFile("alice"))
	}
	release := e.holdStateLock("alice")
	e.configure = func(a *App) { a.hookBudget = 300 * time.Millisecond }
	start := time.Now()
	// The hook runs beside a watchdog: a hook that waits for the lock without
	// limit would otherwise hold this test until the binary's own timeout.
	hooked := make(chan result, 1)
	go func() { hooked <- e.hook("alice", "stop", `{"session_id":"s1"}`) }()
	var r result
	select {
	case r = <-hooked:
	case <-time.After(10 * time.Second):
		release()
		<-hooked
		t.Fatal("the hook was still waiting for the State lock after 10s")
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("hook waited %v for the lock", took)
	}
	if r.code != ExitOK || r.stdout != "" || strings.Count(r.stderr, "\n") != 1 {
		t.Fatalf("lock timeout broke the hook contract: %+v", r)
	}
	tuitestkit.ContainsStr(t, r.stderr, "locked by another a2a process; nothing new was acknowledged")
	if readFile(t, e.paths().StateFile("alice")) != before || e.received("alice", id) {
		t.Fatal("state changed under a held lock")
	}
	if marked && readFile(t, e.paths().BindPartFile("alice")) != sidecarBefore {
		t.Fatal("the sidecar changed under a held lock")
	}
	done := make(chan result, 1)
	go func() { done <- e.as("alice", "inbox") }()
	select {
	case r := <-done:
		t.Fatalf("inbox did not wait for the lock: %+v", r)
	case <-time.After(150 * time.Millisecond):
	}
	release()
	tuitestkit.ContainsStr(t, recv(t, done).ok(t).stdout, "id="+id)
	e.configure = nil
	e.hs.inject(room, "@bob:test.local", ping(2))
	tuitestkit.ContainsStr(t, stopReason(t, e.hook("alice", "stop", `{"session_id":"s1"}`).ok(t).stdout), "1 new peer event")
}
