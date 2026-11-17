package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func threadFileLineCount(t *testing.T, e *env, name string) int {
	t.Helper()
	data, err := os.ReadFile(e.paths().ThreadEventsFile(name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte("\n"))
}

// Claim: a failure after the thread-event append and before the State update
// leaves the cursor unmoved with the learned entries already durable, and
// replaying the same sync then succeeds with the identical window (no
// duplicate lines) and a single mailbox entry.
func TestThreadWindowSyncCrashReplay(t *testing.T) {
	e, room := subWorld(t)
	a, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
	child := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$topic", "$topic")))
	crash := errors.New("injected failure after append")
	a.afterAppend = func() error { return crash }
	if _, err := s.syncNow(ctx, st, rows, time.Time{}); err == nil {
		t.Fatal("injected failure did not fail the sync")
	}
	if got := e.state("alice").NextBatch; got != st.NextBatch {
		t.Fatalf("cursor moved past an unpersisted batch: %q != %q", got, st.NextBatch)
	}
	win := e.threadWindow("alice")
	if win.ThreadEventRoot(room, child) != "$topic" || win.ThreadEventRoot(room, "$topic") != "$topic" {
		t.Fatalf("learned entries not durable before the State update: %+v", win.Entries())
	}
	entriesBefore := win.Entries()
	linesBefore := threadFileLineCount(t, e, "alice")
	if got := len(e.mailbox("alice").Pending()); got != 1 {
		t.Fatalf("pending after failure = %d, want the appended child once", got)
	}
	a.afterAppend = nil
	st, err = s.syncNow(ctx, st, rows, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if st.NextBatch == "" || e.state("alice").NextBatch != st.NextBatch {
		t.Fatal("replayed sync did not advance the cursor")
	}
	if got := e.threadWindow("alice").Entries(); !reflect.DeepEqual(got, entriesBefore) {
		t.Fatalf("replay changed the window: %+v != %+v", got, entriesBefore)
	}
	if got := threadFileLineCount(t, e, "alice"); got != linesBefore {
		t.Fatalf("replay appended duplicate lines: %d != %d", got, linesBefore)
	}
	if got := len(e.mailbox("alice").Pending()); got != 1 {
		t.Fatalf("pending after replay = %d, want 1", got)
	}
}

// Claim: when the thread-event append refuses inside the delivery update,
// the State update does not run, the cursor does not move and no mail is
// appended; beside it, the same sync with a healthy file learns the window,
// delivers the entry and advances the cursor.
func TestThreadWindowAppendFailureKeepsCursor(t *testing.T) {
	e, room := subWorld(t)
	a, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
	e.seedThreadEvents("alice", config.ThreadEvent{Room: room, EventID: "$seed", Root: "$seed"})
	child := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$topic", "$topic")))
	path := e.paths().ThreadEventsFile("alice")
	a.beforeDeliver = func() {
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.syncNow(ctx, st, rows, time.Time{}); err == nil {
		t.Fatal("sync succeeded despite a refused thread-event append")
	}
	if got := e.state("alice").NextBatch; got != st.NextBatch {
		t.Fatalf("append failure moved the cursor: %q != %q", got, st.NextBatch)
	}
	if got := len(e.mailbox("alice").Pending()); got != 0 {
		t.Fatalf("append failure still appended mail: %d pending", got)
	}
	a.beforeDeliver = nil
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = s.syncNow(ctx, st, rows, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if e.state("alice").NextBatch != st.NextBatch || st.NextBatch == "" {
		t.Fatal("healthy control did not advance the cursor")
	}
	win := e.threadWindow("alice")
	if win.ThreadEventRoot(room, child) != "$topic" || win.ThreadEventRoot(room, "$seed") != "$seed" {
		t.Fatalf("healthy control window = %+v", win.Entries())
	}
	if got := len(e.mailbox("alice").Pending()); got != 1 {
		t.Fatalf("healthy control pending = %d, want 1", got)
	}
}

// refThreadWindow re-implements the pre-change in-State window algorithm
// (first observation wins, cap 2000 dropping the oldest, newest-first
// lookup) as the golden reference for the file-backed window.
type refThreadWindow struct{ entries []config.ThreadEvent }

func (r *refThreadWindow) root(room, id string) string {
	for i := len(r.entries) - 1; i >= 0; i-- {
		if e := r.entries[i]; e.Room == room && e.EventID == id {
			return e.Root
		}
	}
	return ""
}

func (r *refThreadWindow) remember(e config.ThreadEvent) {
	if e.Room == "" || e.EventID == "" || e.Root == "" || r.root(e.Room, e.EventID) != "" {
		return
	}
	r.entries = append(r.entries, e)
	if len(r.entries) > 2000 {
		r.entries = r.entries[len(r.entries)-2000:]
	}
}

func (r *refThreadWindow) hasRoot(room, root string) bool {
	for _, e := range r.entries {
		if e.Room == room && e.Root == root {
			return true
		}
	}
	return false
}

func plainReplyEvent(t *testing.T, target string) matrix.Event {
	t.Helper()
	c := kindMsg("bob", "decision", "plain reply")
	c["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": target}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return matrix.Event{EventID: "$reply", Type: "m.room.message", Sender: "@bob:test.local", Content: raw, OriginServerTS: 1000}
}

// Claim (golden comparison): over one learning sequence with duplicates,
// cross-room ids, invalid entries and overflow past the cap, the in-memory
// and file-backed windows retain exactly the reference entries, reply-to
// roots match the reference for every probe, root presence (mute
// resolution) matches, and a muted learned reply is suppressed while an
// unknown-target control still delivers.
func TestThreadWindowGoldenComparison(t *testing.T) {
	e, _ := world(t)
	var sequence []config.ThreadEvent
	for i := 0; i < 2106; i++ {
		room := "!a:test.local"
		if i%3 == 0 {
			room = "!b:test.local"
		}
		sequence = append(sequence, config.ThreadEvent{Room: room, EventID: fmt.Sprintf("$e%d", i), Root: fmt.Sprintf("$root%d", i%37)})
		if i%97 == 0 {
			sequence = append(sequence, config.ThreadEvent{Room: room, EventID: fmt.Sprintf("$e%d", i), Root: "$replacement"})
		}
		if i%211 == 0 {
			sequence = append(sequence, config.ThreadEvent{Room: "", EventID: "$invalid", Root: "$root0"})
		}
	}
	ref := &refThreadWindow{}
	mem := config.NewThreadEventWindow()
	for _, entry := range sequence {
		ref.remember(entry)
		mem.RememberThreadEvent(entry)
	}
	for i := 0; i < len(sequence); i += 300 {
		end := min(i+300, len(sequence))
		if _, err := config.AppendThreadEvents(e.paths(), "alice", sequence[i:end]); err != nil {
			t.Fatal(err)
		}
	}
	fileWin, err := config.OpenThreadEvents(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(mem.Entries(), ref.entries) {
		t.Fatal("in-memory window differs from the reference entries")
	}
	if !reflect.DeepEqual(fileWin.Entries(), ref.entries) {
		t.Fatalf("file-backed window differs from the reference: len=%d want %d", fileWin.Len(), len(ref.entries))
	}
	for _, room := range []string{"!a:test.local", "!b:test.local", "!unknown:test.local"} {
		for i := 0; i < 2106; i += 53 {
			id := fmt.Sprintf("$e%d", i)
			want := ref.root(room, id)
			if got := fileWin.ThreadEventRoot(room, id); got != want {
				t.Fatalf("root(%s,%s) = %q, reference %q", room, id, got, want)
			}
			ev := plainReplyEvent(t, id)
			if got := deliveryTopicRoot(ev, room, fileWin); got != want {
				t.Fatalf("reply-to root(%s,%s) = %q, reference %q", room, id, got, want)
			}
		}
		for _, root := range []string{"$root0", "$root36", "$replacement", "$absent"} {
			if got, want := fileWin.HasRoot(room, root), ref.hasRoot(room, root); got != want {
				t.Fatalf("hasRoot(%s,%s) = %v, reference %v", room, root, got, want)
			}
		}
	}
	// Muted known reply suppressed; unknown-target reply delivered (control).
	knownRoom, knownID := "!a:test.local", ""
	for i := 2105; i >= 0 && knownID == ""; i-- {
		if id := fmt.Sprintf("$e%d", i); ref.root(knownRoom, id) != "" {
			knownID = id
		}
	}
	mutedRoot := ref.root(knownRoom, knownID)
	st := config.State{Muted: []string{mutedRoot}}
	rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: knownRoom, Kinds: []string{"decision"}}}}
	if rel, _, _ := classify(plainReplyEvent(t, knownID), knownRoom, "@alice:test.local", "alice", st, fileWin, rows); rel != "" {
		t.Fatalf("muted learned reply classified %q, want suppressed", rel)
	}
	if rel, _, _ := classify(plainReplyEvent(t, "$never-learned"), knownRoom, "@alice:test.local", "alice", st, fileWin, rows); rel != relSub {
		t.Fatalf("unknown-target control classified %q, want %q", rel, relSub)
	}
}
