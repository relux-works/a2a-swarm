package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// failWriter rejects every write, like a closed pipe.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// flushBuffer is a goroutine-safe output that counts Flush calls.
type flushBuffer struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	flushes int
}

func (b *flushBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *flushBuffer) Flush() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flushes++
	return nil
}

func (b *flushBuffer) snapshot() (string, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String(), b.flushes
}

func (e *env) state(name string) config.State {
	e.t.Helper()
	st, err := config.LoadState(e.paths(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

// Proves a wait with no matching events blocks instead of returning empty:
// with --timeout it prints only "timeout" after the full timeout, and with
// --timeout 0 it keeps blocking through non-matching traffic until a
// matching event arrives, which it prints without any timeout line.
func TestWaitBlocksWithoutMatch(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.hs.inject(room, "@bob:test.local", msg("not for alice", nil))
	start := time.Now()
	r := e.as("alice", "wait", "--timeout", "150ms").ok(t)
	if r.stdout != "timeout\n" || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("wait returned %q after %v", r.stdout, time.Since(start))
	}

	calls := e.hs.CallCount("GET /_matrix/client/v3/sync")
	ch := e.waitAsync("alice", "--timeout", "0")
	e.syncCalls(t, calls+1)
	e.hs.inject(room, "@bob:test.local", msg("still not for alice", nil))
	select {
	case r := <-ch:
		t.Fatalf("--timeout 0 returned without a match: %+v", r)
	case <-time.After(300 * time.Millisecond):
	}
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local now", nil))
	r = recv(t, ch).ok(t)
	if r.stdout == "" || strings.Contains(r.stdout, "timeout") || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("stdout = %q", r.stdout)
	}
	tuitestkit.ContainsStr(t, r.stdout, "mention #proj "+id)
}

// Proves wait prints first and records the batch as pending only afterwards:
// the cursor does not move until the next wait acknowledges the batch, which
// then moves it to the batch's next_batch and records the delivered ids.
func TestWaitPendingUntilAck(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	cursor := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local one", nil))
	e.as("alice", "wait", "--timeout", "2s").ok(t)
	st := e.state("alice")
	if st.NextBatch != cursor || st.Pending == nil || len(st.Pending.Events) != 1 || st.Pending.Events[0] != id {
		t.Fatalf("after delivery: %+v", st)
	}
	e.as("alice", "wait", "--timeout", "20ms").ok(t)
	st = e.state("alice")
	if st.Pending != nil || st.NextBatch == cursor || !st.IsAcked(id) {
		t.Fatalf("after ack: %+v", st)
	}
}

// Proves at-least-once delivery when the output fails: a wait whose stdout
// write fails exits 1, saves no pending batch and leaves the cursor where it
// was, so the next wait delivers the same event again.
func TestWaitRedeliversAfterWriteFailure(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	cursor := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local must not be lost", nil))
	e.configure = func(a *App) { a.Stdout = failWriter{} }
	e.as("alice", "wait", "--timeout", "2s").failsWith(t, ExitError, "broken pipe")
	e.configure = nil
	if st := e.state("alice"); st.Pending != nil || st.NextBatch != cursor {
		t.Fatalf("state moved after a failed write: %+v", st)
	}
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "mention #proj "+id)
}

// Proves at-least-once delivery across a crash between print and save (test
// hook): the printed batch is not pending, so a restarted wait delivers it
// again; after that wait completes normally, it is not delivered a third time.
func TestWaitRedeliversAfterCrashBeforeSave(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local survive a crash", nil))
	e.configure = func(a *App) { a.afterPrint = func() error { return errors.New("killed") } }
	r := e.as("alice", "wait", "--timeout", "2s").failsWith(t, ExitError, "killed")
	tuitestkit.ContainsStr(t, r.stdout, id)
	e.configure = nil
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "mention #proj "+id)
	if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("delivered a third time: %q", r.stdout)
	}
}

// Proves an acknowledged event is never delivered twice, even when the cursor
// is rolled back behind it (a restored state file, an old token): the acked
// ring filters it, while a new event in the same batch is delivered.
func TestAckedEventNeverDeliveredTwice(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	old := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local once", nil))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, id)
	tuitestkit.ContainsStr(t, e.as("alice", "ack").ok(t).stdout, "acked 1 events")
	tuitestkit.ContainsStr(t, e.as("alice", "ack", "all").ok(t).stdout, "nothing pending")
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = old; return nil }); err != nil {
		t.Fatal(err)
	}
	id2 := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local second", nil))
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	tuitestkit.ContainsStr(t, out, id2)
	tuitestkit.NotContainsStr(t, out, id+" ")
	e.as("alice", "ack", "extra").failsWith(t, ExitUsage, "usage")
}

// Proves watch streams every delivered event as its own flushed line without
// exiting, stops cleanly (exit 0) when its context is cancelled, and commits
// the cursor after each batch: a later wait delivers nothing again.
func TestWatch(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	out := &flushBuffer{}
	e.configure = func(a *App) { a.Stdout = out }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := e.hs.CallCount("GET /_matrix/client/v3/sync")
	done := make(chan result, 1)
	go func() { done <- e.runCtx(ctx, "--as", "alice", "watch") }()
	e.syncCalls(t, calls+1)
	e.hs.inject(room, "@bob:test.local", msg("noise", nil))
	a := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local first", nil))
	e.syncCalls(t, calls+2)
	b := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local second", nil))
	deadline := time.Now().Add(5 * time.Second)
	for {
		text, _ := out.snapshot()
		if strings.Contains(text, b) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("watch never printed %s: %q", b, text)
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.syncCalls(t, e.hs.CallCount("GET /_matrix/client/v3/sync")+1) // the batch of b was committed
	select {
	case r := <-done:
		t.Fatalf("watch exited on its own: %+v", r)
	default:
	}
	cancel()
	r := recv(t, done)
	if r.code != ExitOK {
		t.Fatalf("watch exit %d: %s", r.code, r.stderr)
	}
	e.configure = nil
	text, flushes := out.snapshot()
	lines := tuitestkit.LinesFromStr(strings.TrimSpace(text))
	if len(lines) != 2 || !strings.Contains(lines[0], "mention #proj "+a) || !strings.Contains(lines[1], "mention #proj "+b) || flushes != 2 {
		t.Fatalf("lines = %q, flushes = %d", lines, flushes)
	}
	st := e.state("alice")
	if st.Pending != nil || !st.IsAcked(a) || !st.IsAcked(b) {
		t.Fatalf("watch did not commit: %+v", st)
	}
	if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("committed events delivered again: %q", r.stdout)
	}
}

// Proves watch refuses bad input before any request, and a watch whose output
// fails exits 1 without committing, so wait delivers the event again.
func TestWatchNegative(t *testing.T) {
	e, room := world(t)
	before := len(e.hs.Calls())
	e.as("alice", "watch", "extra").failsWith(t, ExitUsage, "usage")
	e.as("alice", "watch", "--all").failsWith(t, ExitUsage, "--all needs at least one --room")
	e.as("alice", "watch", "--timeout", "1s").failsWith(t, ExitUsage, "flag provided but not defined")
	if n := len(e.hs.Calls()); n != before {
		t.Fatalf("usage errors reached the server: %d requests", n-before)
	}
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local keep me", nil))
	e.configure = func(a *App) { a.Stdout = failWriter{} }
	e.as("alice", "watch").failsWith(t, ExitError, "not committed")
	e.configure = nil
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, id)
}
