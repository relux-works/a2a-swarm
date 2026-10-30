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

// mailbox loads name's durable mailbox.
func (e *env) mailbox(name string) *config.Mailbox {
	e.t.Helper()
	mb, err := config.OpenMailbox(e.paths(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return mb
}

// received reports whether id left name's mailbox by a receipt.
func (e *env) received(name, id string) bool { return e.mailbox(name).Received(id) }

// pendingEntry returns the unreceived mailbox entry id (ok false when there is none).
func (e *env) pendingEntry(name, id string) (config.MailEntry, bool) {
	for _, en := range e.mailbox(name).Pending() {
		if en.ID == id {
			return en, true
		}
	}
	return config.MailEntry{}, false
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

// Proves wait persists before it prints and receipts nothing (defect 1 of
// research/waking-sessions-converged.md: the old wait acknowledged the previous
// batch implicitly): after a wait printed an event, the cursor has moved (the
// event is durable), the entry is still unreceived but marked shown, so the
// next wait blocks instead of printing it again; only `ack all-shown` receipts it.
func TestWaitMarksShownNeverReceipts(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	cursor := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local one", nil))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stderr, "a2a ack")
	en, ok := e.pendingEntry("alice", id)
	if e.state("alice").NextBatch == cursor || !ok || !en.Shown {
		t.Fatalf("after delivery: entry %+v (%v)", en, ok)
	}
	if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("shown entry printed again: %q", r.stdout)
	}
	if e.received("alice", id) {
		t.Fatal("a wait receipted")
	}
	tuitestkit.ContainsStr(t, e.as("alice", "ack", "all-shown").ok(t).stdout, "acked 1 events")
	if !e.received("alice", id) {
		t.Fatal("ack all-shown did not receipt")
	}
}

// Proves at-least-once delivery when the output fails: a wait whose stdout
// write fails exits 1 and marks nothing shown (the event is in the mailbox),
// so the next wait prints the same event again.
func TestWaitRedeliversAfterWriteFailure(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local must not be lost", nil))
	e.configure = func(a *App) { a.Stdout = failWriter{} }
	e.as("alice", "wait", "--timeout", "2s").failsWith(t, ExitError, "broken pipe")
	e.configure = nil
	if en, ok := e.pendingEntry("alice", id); !ok || en.Shown {
		t.Fatalf("after a failed write: %+v (%v)", en, ok)
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

// Proves a received event is never delivered twice, even when the cursor is
// rolled back behind it (a restored state file, an old token): the receipt
// index deduplicates the re-synced event, while a new event in the same batch
// is delivered. `ack` receipts by id; bare `ack`, `ack all` and a non-id are
// usage errors (the old implicit "ack the pending batch" is gone).
func TestAckedEventNeverDeliveredTwice(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	old := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local once", nil))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, id)
	tuitestkit.ContainsStr(t, e.as("alice", "ack", id).ok(t).stdout, "acked 1 events")
	tuitestkit.ContainsStr(t, e.as("alice", "ack", "all-shown").ok(t).stdout, "nothing pending")
	tuitestkit.ContainsStr(t, e.as("alice", "ack", id, "$never").ok(t).stdout, "acked 0 events (2 not pending)")
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = old; return nil }); err != nil {
		t.Fatal(err)
	}
	id2 := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local second", nil))
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	tuitestkit.ContainsStr(t, out, id2)
	tuitestkit.NotContainsStr(t, out, id+" ")
	calls := len(e.hs.Calls())
	e.as("alice", "ack").failsWith(t, ExitUsage, "usage: a2a ack")
	e.as("alice", "ack", "all").failsWith(t, ExitUsage, "not a mailbox id")
	e.as("alice", "ack", "extra").failsWith(t, ExitUsage, "not a mailbox id")
	if len(e.hs.Calls()) != calls {
		t.Fatal("a usage error contacted the server")
	}
}

// Proves watch streams every delivered event as its own flushed line without
// exiting, stops cleanly (exit 0) when its context is cancelled, and marks
// what it printed shown without receipting it: a later wait delivers nothing
// again, the entries stay unreceived.
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
	// Watch marks a printed event shown only after persisting its batch, so b
	// shown means the batch of b was committed. Waiting for a further sync
	// request instead raced: the request after b may already be counted when
	// the count is read, and the one after it comes only after the long poll.
	deadline = time.Now().Add(testDeadline)
	for en, ok := e.pendingEntry("alice", b); !ok || !en.Shown; en, ok = e.pendingEntry("alice", b) {
		if time.Now().After(deadline) {
			t.Fatalf("watch never committed %s: %+v %v", b, en, ok)
		}
		time.Sleep(5 * time.Millisecond)
	}
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
	for _, id := range []string{a, b} {
		if en, ok := e.pendingEntry("alice", id); !ok || !en.Shown {
			t.Fatalf("watch did not mark %s shown (or receipted it): %+v %v", id, en, ok)
		}
	}
	if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("committed events delivered again: %q", r.stdout)
	}
}

// Proves watch refuses bad input before any request, and a watch whose output
// fails exits 1 without marking anything shown, so wait delivers the event again.
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
	e.as("alice", "watch").failsWith(t, ExitError, "nothing was marked shown")
	e.configure = nil
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, id)
}
