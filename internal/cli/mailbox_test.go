package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// addLines counts the mailbox "add" lines of id in the file (duplicates on disk).
func (e *env) addLines(name, id string) int {
	e.t.Helper()
	data, err := os.ReadFile(e.paths().MailboxFile(name))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Count(string(data), `"op":"add","entry":{"id":"`+id+`"`)
}

// Proves the hook never acknowledges an earlier wait batch (defect 1 of
// research/waking-sessions-converged.md): after a wait printed an event, a
// hook rings for it (it is still unreceived) and receipts nothing, and inbox
// prints it; only after that inbox is it gone (positive control: inbox
// receipts, the next inbox and a new session's hook find nothing).
func TestHookNeverAcksWaitBatch(t *testing.T) {
	e, room := world(t)
	offset := time.Duration(0)
	e.now = func() time.Time { return time.Now().Add(offset) }
	e.sequentialSessions()
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", ping(1))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "mention #proj "+id)
	for _, ev := range []string{"prompt", "stop", "session-start"} {
		e.hook("alice", ev, `{"session_id":"s-`+ev+`"}`).ok(t)
		if e.received("alice", id) {
			t.Fatalf("hook %s receipted the wait's event", ev)
		}
	}
	offset += 30 * time.Second
	tuitestkit.ContainsStr(t, bellContext(t, e.hook("alice", "prompt", `{"session_id":"fresh"}`).ok(t).stdout), "a2a: 1 new peer event (mention: 1)")
	e.as("alice", "wait", "--timeout", "10ms").ok(t) // another wait receipts nothing either
	out := e.as("alice", "inbox").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "id="+id)
	if !e.received("alice", id) {
		t.Fatal("inbox did not receipt")
	}
	if r := e.as("alice", "inbox").ok(t); r.stdout != "no new peer events\n" {
		t.Fatalf("second inbox = %q", r.stdout)
	}
	if r := e.hook("alice", "prompt", `{"session_id":"later"}`).ok(t); r.stdout != "" {
		t.Fatalf("rang for a received event: %q", r.stdout)
	}
}

// Proves watch loses nothing when its reader dies after the pipe write
// (defect 2 of research/waking-sessions-converged.md: the old watch committed
// the cursor and acked after writing to stdout): the watch printed the event
// and was killed, the event is still unreceived, and the next inbox prints it
// and receipts it; after that a new watch prints nothing for it (positive
// control: shown entries do not re-wake watch).
func TestWatchKilledReaderLosesNothing(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	out := &flushBuffer{}
	e.configure = func(a *App) { a.Stdout = out }
	ctx, cancel := context.WithCancel(context.Background())
	calls := e.hs.CallCount("GET /_matrix/client/v3/sync")
	done := make(chan result, 1)
	go func() { done <- e.runCtx(ctx, "--as", "alice", "watch", "--json") }()
	e.syncCalls(t, calls+1)
	id := e.hs.inject(room, "@bob:test.local", ping(1))
	deadline := time.Now().Add(5 * time.Second)
	for text, _ := out.snapshot(); !strings.Contains(text, id); text, _ = out.snapshot() {
		if time.Now().After(deadline) {
			t.Fatalf("watch never printed %s", id)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // the reader and the watch die right after the write
	recv(t, done)
	e.configure = nil
	if e.received("alice", id) {
		t.Fatal("watch receipted what it wrote")
	}
	tuitestkit.ContainsStr(t, e.as("alice", "inbox").ok(t).stdout, "id="+id)
	if !e.received("alice", id) {
		t.Fatal("inbox did not receipt")
	}
	if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("received event printed again: %q", r.stdout)
	}
}

// Proves no starvation (defect 4 of research/waking-sessions-converged.md: a
// stable backlog larger than a batch plus the 1000-id ack ring could starve
// the last events): a backlog of 1100 addressed events drains completely
// through repeated `inbox --limit 20`, in order, none lost and none printed
// twice; the 56th inbox prints "no new peer events".
func TestBacklogDrainsWithoutStarvation(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	var want []string
	for i := 0; i < 1100; i++ {
		want = append(want, e.hs.inject(room, "@bob:test.local", ping(i)))
	}
	idRE := regexp.MustCompile(`id=(\$ev\d+)`)
	var got []string
	for run := 0; run < 55; run++ {
		out := e.as("alice", "inbox", "--limit", "20").ok(t).stdout
		ids := idRE.FindAllStringSubmatch(out, -1)
		if len(ids) != 20 {
			t.Fatalf("run %d printed %d events", run, len(ids))
		}
		for _, m := range ids {
			got = append(got, m[1])
		}
	}
	if r := e.as("alice", "inbox", "--limit", "20").ok(t); r.stdout != "no new peer events\n" {
		t.Fatalf("after draining: %q", r.stdout)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("drained %d events, want %d in order", len(got), len(want))
	}
	if n := len(e.mailbox("alice").Pending()); n != 0 {
		t.Fatalf("%d entries left", n)
	}
}

// Proves persist-before-advance at the crash points of the sync step: a crash
// right before the append (nothing durable) leaves the cursor unmoved, so the
// next inbox still delivers the event; a crash right after the append and
// before the cursor advance leaves the cursor unmoved too, and the re-synced
// batch is deduplicated (one "add" line in the mailbox) and delivered once.
func TestCrashAroundAppend(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(a *App)
		appended  bool
	}{
		{"before append", func(a *App) { a.beforeAppend = func() error { return errors.New("killed") } }, false},
		{"after append", func(a *App) { a.afterAppend = func() error { return errors.New("killed") } }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "inbox").ok(t)
			cursor := e.state("alice").NextBatch
			id := e.hs.inject(room, "@bob:test.local", ping(1))
			e.configure = tc.configure
			e.as("alice", "inbox").failsWith(t, ExitError, "killed")
			e.configure = nil
			if st := e.state("alice"); st.NextBatch != cursor {
				t.Fatal("the cursor moved although the batch was not persisted")
			}
			if _, ok := e.pendingEntry("alice", id); ok != tc.appended {
				t.Fatalf("entry in the mailbox = %v, want %v", ok, tc.appended)
			}
			out := e.as("alice", "inbox").ok(t).stdout
			if strings.Count(out, "id="+id) != 1 || e.addLines("alice", id) != 1 {
				t.Fatalf("delivered %d times, %d add lines: %q", strings.Count(out, "id="+id), e.addLines("alice", id), out)
			}
			if e.state("alice").NextBatch == cursor {
				t.Fatal("the cursor did not move after a successful persist")
			}
			if r := e.as("alice", "inbox").ok(t); r.stdout != "no new peer events\n" {
				t.Fatalf("printed again: %q", r.stdout)
			}
		})
	}
}

// Proves the mailbox file is 0600 and that a symlinked mailbox is refused:
// inbox exits 1 naming the refusal, the cursor does not move and the link
// target stays untouched, and a hook stays silent on stdout; once the link is
// replaced by a regular file the event is delivered (positive control).
func TestMailboxModeAndSymlinkRefused(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	e.hs.inject(room, "@bob:test.local", ping(0))
	e.as("alice", "inbox").ok(t)
	path := e.paths().MailboxFile("alice")
	if st, err := os.Lstat(path); err != nil || st.Mode().Perm() != 0o600 || !st.Mode().IsRegular() {
		t.Fatalf("mailbox mode: %v, %v", st, err)
	}
	cursor := e.state("alice").NextBatch
	target := e.dir + "/elsewhere.jsonl"
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Remove(path)
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	id := e.hs.inject(room, "@bob:test.local", ping(1))
	e.as("alice", "inbox").failsWith(t, ExitError, "not a regular file")
	if r := e.hook("alice", "prompt", `{}`); r.code != ExitOK || r.stdout != "" || !strings.Contains(r.stderr, "not a regular file") {
		t.Fatalf("hook with a symlinked mailbox: %+v", r)
	}
	if data, _ := os.ReadFile(target); len(data) != 0 || e.state("alice").NextBatch != cursor {
		t.Fatal("wrote through the symlink or moved the cursor")
	}
	os.Remove(path)
	tuitestkit.ContainsStr(t, e.as("alice", "inbox").ok(t).stdout, "id="+id)
}

// Proves the migration from a pre-mailbox state file: ids of the legacy acked
// ring are never delivered again, and the events of a legacy pending batch
// (printed by an old wait, never acknowledged) are delivered, since the cursor
// never moved past them; recovery fields retire only after the IDs are durable.
func TestLegacyStateMigration(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	cursor := e.state("alice").NextBatch
	acked := e.hs.inject(room, "@bob:test.local", ping(1))
	pending := e.hs.inject(room, "@bob:test.local", ping(2))
	legacy := config.State{NextBatch: cursor, LegacyAcked: []string{acked},
		LegacyPending: []byte(fmt.Sprintf(`{"next_batch":"s99","events":[%q]}`, pending))}
	if err := config.SaveState(e.paths(), "alice", legacy); err != nil {
		t.Fatal(err)
	}
	out := e.as("alice", "inbox").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "id="+pending)
	tuitestkit.NotContainsStr(t, out, acked)
	if st := e.state("alice"); len(st.LegacyAcked) != 0 || len(st.LegacyPending) != 0 {
		t.Fatalf("legacy recovery fields not retired after durable recovery: %+v", st)
	}
}
