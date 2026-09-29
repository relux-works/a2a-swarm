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

// Proves relevance-first backfill (defect 3 of
// research/waking-sessions-converged.md: the old backfill kept only the newest
// 2000 events before classifying, so a flood pushed an addressed message out of
// reach): an addressed message before a flood of 5000 irrelevant events and one
// after it are both delivered, the doorbell and inbox report no gap, and the
// mailbox holds only the two relevant events (the flood takes no space).
func TestBackfillFloodKeepsAddressed(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	early := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local before the flood", nil))
	for i := 0; i < 5000; i++ {
		e.hs.inject(room, "@bob:test.local", msg("flood", nil))
	}
	late := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local after the flood", nil))
	bell := bellContext(t, e.hook("alice", "prompt", `{"session_id":"s"}`).ok(t).stdout)
	if bell != "a2a: 2 new peer events (mention: 2) from bob"+bellTail {
		t.Fatalf("doorbell = %q", bell)
	}
	if n := len(e.mailbox("alice").Pending()); n != 2 {
		t.Fatalf("mailbox holds %d entries", n)
	}
	out := e.as("alice", "inbox").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "id="+early)
	tuitestkit.ContainsStr(t, out, "id="+late)
	tuitestkit.NotContainsStr(t, out, "gap")
}

// Proves a stretch the backfill cannot reach is surfaced, never skipped
// silently: when the server stops paging above the previous position (a
// purged history) and when the page bound is hit, the mailbox gets a gap
// record that the doorbell counts and wait and inbox print ("gap: events
// between X and Y could not be checked"), while the addressed message the
// pages did reach is still delivered (positive control); the cut early
// message is not claimed as delivered.
func TestBackfillGapSurfaced(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(e *env, floor int)
	}{
		{"server stops paging", func(e *env, floor int) { e.hs.historyFloor = floor }},
		{"page bound", func(e *env, _ int) { e.configure = func(a *App) { a.backfillPages = 2 } }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			e.hs.syncLimit = 5
			e.as("alice", "inbox").ok(t)
			early := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local cut off", nil))
			floor := len(e.hs.roomMessages(room))
			for i := 0; i < 250; i++ {
				e.hs.inject(room, "@bob:test.local", msg("noise", nil))
			}
			late := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local reachable", nil))
			tc.setup(e, floor)
			bell := bellContext(t, e.hook("alice", "prompt", `{"session_id":"s"}`).ok(t).stdout)
			if bell != "a2a: 1 new peer event (mention: 1) from bob; 1 gap: some events could not be checked"+bellTail {
				t.Fatalf("doorbell = %q", bell)
			}
			w := e.as("alice", "wait", "--timeout", "1s").ok(t).stdout
			tuitestkit.ContainsStr(t, w, "gap #proj gap:"+room)
			tuitestkit.ContainsStr(t, w, "mention #proj "+late)
			out := e.as("alice", "inbox").ok(t).stdout
			tuitestkit.ContainsStr(t, out, "--- event 1/2: gap room=#proj id=gap:"+room)
			tuitestkit.ContainsStr(t, out, inboxIndent+"gap: events between ")
			tuitestkit.ContainsStr(t, out, " could not be checked; read them with a2a read #proj\n")
			tuitestkit.ContainsStr(t, out, "id="+late)
			tuitestkit.NotContainsStr(t, out, early)
		})
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
