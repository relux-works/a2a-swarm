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
func TestHookLockTimeout(t *testing.T) {
	e, room := world(t)
	e.hook("alice", "prompt", `{}`).ok(t)
	id := e.hs.inject(room, "@bob:test.local", ping(1))
	before := readFile(t, e.paths().StateFile("alice"))
	release := e.holdStateLock("alice")
	e.configure = func(a *App) { a.hookBudget = 300 * time.Millisecond }
	start := time.Now()
	r := e.hook("alice", "stop", `{"session_id":"s1"}`)
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("hook waited %v for the lock", took)
	}
	if r.code != ExitOK || r.stdout != "" || strings.Count(r.stderr, "\n") != 1 {
		t.Fatalf("lock timeout broke the hook contract: %+v", r)
	}
	tuitestkit.ContainsStr(t, r.stderr, "locked by another a2a process; nothing new was acknowledged")
	if readFile(t, e.paths().StateFile("alice")) != before || e.state("alice").IsAcked(id) {
		t.Fatal("state changed under a held lock")
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
