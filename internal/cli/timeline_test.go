package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

const messagesRoute = "GET /_matrix/client/v3/rooms/{room}/messages"

// Proves wait still reports an addressed message that the server cut from a
// limited /sync timeline (backfilled via /messages up to the previous
// position), reports it once, and never reports messages from before that
// position.
func TestWaitBackfillsLimitedTimeline(t *testing.T) {
	e, room := world(t)
	e.hs.syncLimit = 5
	e.hs.inject(room, "@bob:test.local", msg("@alice:test.local before my position", nil))
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local early in the flood", nil))
	for i := 0; i < 12; i++ {
		e.hs.inject(room, "@bob:test.local", msg(fmt.Sprintf("chatter %d", i), nil))
	}
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "mention #proj "+id)
	tuitestkit.NotContainsStr(t, r.stdout, "before my position")
	if strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("want exactly one line: %q", r.stdout)
	}
	if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("re-emitted after backfill: %q", r.stdout)
	}
}

// Proves a failed backfill makes wait exit 1 without advancing its position,
// so the next wait still reports the cut message.
func TestWaitBackfillFailureKeepsPosition(t *testing.T) {
	e, room := world(t)
	e.hs.syncLimit = 3
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local cut", nil))
	for i := 0; i < 5; i++ {
		e.hs.inject(room, "@bob:test.local", msg("noise", nil))
	}
	e.hs.fail(messagesRoute, 500, "M_UNKNOWN", "backfill down")
	e.as("alice", "wait", "--timeout", "2s").failsWith(t, ExitError, "backfill down")
	e.hs.mu.Lock()
	delete(e.hs.failures, messagesRoute)
	e.hs.mu.Unlock()
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, id)
}

// Proves more than maxBackfill skipped events are bounded: wait warns on
// stderr about the gap instead of paging forever, and still reports the
// newest addressed message.
func TestWaitBackfillBounded(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	for i := 0; i < maxBackfill+150; i++ {
		e.hs.inject(room, "@bob:test.local", msg("flood", nil))
	}
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local after the flood", nil))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, id)
	tuitestkit.ContainsStr(t, r.stderr, "older ones were not checked")
	if n := e.hs.CallCount(messagesRoute); n > maxBackfill/100+1 {
		t.Fatalf("backfill pages = %d", n)
	}
}

// Proves the projector projects typed messages that a limited timeline cut,
// in order and exactly once.
func TestProjectBackfillsLimitedTimeline(t *testing.T) {
	e, room, system := projWorld(t)
	e.hs.syncLimit = 4
	e.project().ok(t)
	a := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "first"))
	b := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "owner-needed", "second"))
	for i := 0; i < 10; i++ {
		e.hs.inject(room, "@bob:test.local", msg("noise", nil))
	}
	e.project().ok(t)
	got := e.projections(system)
	if len(got) != 2 || projectionOf(t, got[0]).OriginEvent != a || projectionOf(t, got[1]).OriginEvent != b {
		t.Fatalf("projections = %d", len(got))
	}
}
