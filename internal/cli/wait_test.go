package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// waitAsync starts `a2a wait` in the background and returns a channel with its result.
func (e *env) waitAsync(name string, args ...string) <-chan result {
	ch := make(chan result, 1)
	go func() { ch <- e.as(name, append([]string{"wait"}, args...)...) }()
	return ch
}

// syncCalls waits until the fake server has seen at least n /sync requests, so
// the test injects events only after `wait` has taken its position.
// testDeadline bounds every wait-until in these tests. It is a ceiling for a
// slow or shared CI host (the 5 s it replaced failed under load and on Linux),
// never an expectation: a passing run returns as soon as its condition holds.
const testDeadline = 60 * time.Second

func (e *env) syncCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for e.hs.CallCount("GET /_matrix/client/v3/sync") < n {
		if time.Now().After(deadline) {
			t.Fatalf("wait never reached sync #%d", n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// syncedParked waits until user has made sync request #n while the wait
// behind done stays parked. A wait that returns first (it woke on something it
// must not) fails at once with its output, instead of timing out on the count.
func (e *env) syncedParked(t *testing.T, user string, n int, done <-chan result, what string) {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for e.hs.SyncCount(user) < n {
		select {
		case r := <-done:
			t.Fatalf("%s: wait returned instead of staying parked: code %d %q", what, r.code, r.stdout)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: wait never reached sync #%d", what, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
	select {
	case r := <-done:
		t.Fatalf("%s: wait returned instead of staying parked: code %d %q", what, r.code, r.stdout)
	default:
	}
}

func recv(t *testing.T, ch <-chan result) result {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(testDeadline):
		t.Fatal("wait did not return")
		return result{}
	}
}

func msg(body string, extra map[string]any) map[string]any {
	m := map[string]any{"msgtype": "m.text", "body": body}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

// Proves wait exits 0 with a single "timeout" line when nothing arrives, and
// that the first run takes a position without replaying room history.
func TestWaitTimeout(t *testing.T) {
	e, room := world(t)
	e.hs.inject(room, "@bob:test.local", msg("old news for @alice:test.local", nil))
	start := time.Now()
	r := e.as("alice", "wait", "--timeout", "80ms").ok(t)
	if r.stdout != "timeout\n" {
		t.Fatalf("stdout = %q", r.stdout)
	}
	if d := time.Since(start); d < 80*time.Millisecond || d > 3*time.Second {
		t.Fatalf("returned after %v", d)
	}
	st, _ := config.LoadState(e.paths(), "alice")
	if st.NextBatch == "" {
		t.Fatal("next_batch not persisted")
	}
}

// Proves each relevance rule wakes wait: a mention of my user id, my name in
// "to", and a reply in a thread I posted in. My own messages and unrelated
// chatter do not.
func TestWaitRelevance(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "500ms").ok(t) // take a position

	cases := []struct {
		name    string
		sender  string
		content map[string]any
		want    string
	}{
		{"to", "@bob:test.local", msg("- from: bob\n\nhello there", map[string]any{ContentKey: Header{V: 1, Type: "message", From: "bob", To: []string{"alice"}, Kind: "request", Slug: "alpha"}}), "to #proj"},
		{"mention metadata", "@bob:test.local", msg("ping", map[string]any{"m.mentions": map[string]any{"user_ids": []string{"@alice:test.local"}}}), "mention #proj"},
		{"mention in body", "@bob:test.local", msg("hey @alice:test.local look", nil), "mention #proj"},
		{"thread", "@bob:test.local", msg("reply in alpha", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}), "thread #proj"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Noise first: unrelated chatter and my own message must not wake me.
			e.hs.inject(room, "@bob:test.local", msg("unrelated chatter", nil))
			e.hs.inject(room, "@alice:test.local", msg("my own note @alice:test.local", nil))
			id := e.hs.inject(room, tc.sender, tc.content)
			r := e.as("alice", "wait", "--timeout", "2s").ok(t)
			lines := tuitestkit.LinesFromStr(strings.TrimSpace(r.stdout))
			if len(lines) != 1 {
				t.Fatalf("lines = %q", lines)
			}
			tuitestkit.ContainsStr(t, lines[0], tc.want+" "+id+" from=bob")
		})
	}
}

// Proves wait blocks until a relevant event arrives while it is waiting, then
// prints it and exits.
func TestWaitWakesOnLiveEvent(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "500ms").ok(t)
	calls := e.hs.CallCount("GET /_matrix/client/v3/sync")
	ch := e.waitAsync("alice", "--timeout", "10s")
	e.syncCalls(t, calls+1)
	e.hs.inject(room, "@bob:test.local", msg("unrelated", nil))
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local wake up", nil))
	r := recv(t, ch).ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "mention #proj "+id)
	tuitestkit.NotContainsStr(t, r.stdout, "unrelated")
}

// Proves next_batch is persisted: after wait reported an event, a restarted
// wait never reports it again (it times out instead), and a newer event is
// still delivered.
func TestWaitNoReemitAfterRestart(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "500ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local first", nil))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, id)
	r = e.as("alice", "wait", "--timeout", "60ms").ok(t)
	if r.stdout != "timeout\n" {
		t.Fatalf("restart re-emitted: %q", r.stdout)
	}
	id2 := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local second", nil))
	r = e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, id2)
	tuitestkit.NotContainsStr(t, r.stdout, id+" ")
}

// Proves invites wake wait, both pending on the very first run and arriving later.
func TestWaitInvite(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "invite", "proj", "carol").ok(t)
	r := e.as("carol", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "invite #proj !projRoomID from=alice")

	e.hs.addRoom("side", "@bob:test.local")
	e.as("bob", "invite", "side", "carol").ok(t)
	r = e.as("carol", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "invite #side !sideRoomID from=bob")
	tuitestkit.NotContainsStr(t, r.stdout, "#proj")
}

// Proves a thread I joined by posting (learned from my own event in the sync
// stream, not from local state) wakes me on the next reply.
func TestWaitLearnsThreadsFromSync(t *testing.T) {
	e, room := world(t)
	root := e.hs.inject(room, "@bob:test.local", msg("[topic] beta: B", nil))
	e.as("alice", "wait", "--timeout", "500ms").ok(t)
	// alice posts from "another machine": inject directly, so local state does not know.
	e.hs.inject(room, "@alice:test.local", msg("my reply", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}))
	e.as("alice", "wait", "--timeout", "30ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("answer to alice", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "thread #proj "+id)
}

// Proves --room restricts which rooms can wake me, without losing what it
// filters out: a `--room proj` wait processes the lobby mention without waking
// (checked after the wait has synced past it, not after a wall-clock timeout),
// the proj mention then wakes it, and a later wait that includes the lobby
// prints the lobby mention from the mailbox (the old wait moved the shared
// cursor past it and it was lost).
func TestWaitRoomFilter(t *testing.T) {
	e, room := world(t)
	lobby := "!lobbyRoomID"
	e.as("alice", "wait", "--timeout", "500ms").ok(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	alice := "@alice:test.local"
	calls := e.hs.SyncCount(alice)
	done := make(chan result, 1)
	go func() { done <- e.runCtx(ctx, "--as", "alice", "wait", "--room", "proj", "--timeout", "0") }()
	e.syncedParked(t, alice, calls+1, done, "parked")
	lid := e.hs.inject(lobby, "@bob:test.local", msg("@alice:test.local in lobby", nil))
	e.syncedParked(t, alice, calls+2, done, "filtered lobby mention") // its batch was processed
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local in proj", nil))
	r := recv(t, done)
	tuitestkit.ContainsStr(t, r.stdout, "mention #proj "+id)
	if strings.Contains(r.stdout, lid) {
		t.Fatalf("--room proj printed the lobby mention: %q", r.stdout)
	}
	r = e.as("alice", "wait", "--room", "#proj", "--room", "lobby", "--timeout", "10s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "mention #lobby "+lid)
}

// Proves doorbell lines never carry more than 200 characters of body, never
// contain control characters, and hide the protocol header lines.
func TestWaitSnippetBounded(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "500ms").ok(t)
	long := "- from: bob\n- to: alice\n\n" + "\x1b[2J\x1b[31mALERT\x07\r\n" + strings.Repeat("y", 1000)
	e.hs.inject(room, "@bob:test.local", msg(long, map[string]any{ContentKey: Header{V: 1, Type: "message", From: "bob", To: []string{"alice"}, Kind: "notice"}}))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	line := strings.TrimSuffix(r.stdout, "\n")
	if strings.Contains(line, "\n") {
		t.Fatalf("multi-line output: %q", r.stdout)
	}
	_, body, ok := strings.Cut(line, ": ")
	if !ok || utf8.RuneCountInString(body) > SnippetMax {
		t.Fatalf("body excerpt is %d chars: %q", utf8.RuneCountInString(body), body)
	}
	for _, r := range line {
		if isUnsafeRune(r) {
			t.Fatalf("control character %U in %q", r, line)
		}
	}
	tuitestkit.NotContainsStr(t, line, "- from:")
	tuitestkit.ContainsStr(t, line, "[31mALERT")
}

// Proves invalid flags are usage errors and that an interrupted wait returns
// promptly with an error instead of hanging.
func TestWaitNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "-1s").failsWith(t, ExitUsage, "--timeout must not be negative")
	e.as("alice", "wait", "--timeout", "soon").failsWith(t, ExitUsage, "invalid value")
	e.as("alice", "wait", "extra").failsWith(t, ExitUsage, "usage")
	e.as("alice", "wait", "--timeout", "500ms").ok(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	r := e.runCtx(ctx, "--as", "alice", "wait", "--timeout", "1m")
	if r.code != ExitError || time.Since(start) > 3*time.Second {
		t.Fatalf("interrupted wait: code %d after %v", r.code, time.Since(start))
	}
}

// Proves the pure relevance classifier over a table of events, including the
// negative controls (own events, non-messages, other threads).
func TestClassify(t *testing.T) {
	me := "@alice:test.local"
	st := config.State{Threads: []string{"$mine"}}
	ev := func(sender, typ string, content map[string]any) matrix.Event {
		raw, _ := json.Marshal(content)
		return matrix.Event{EventID: "$e", Type: typ, Sender: sender, Content: raw}
	}
	thread := func(root string) map[string]any {
		return map[string]any{"body": "x", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}
	}
	cases := []struct {
		name    string
		ev      matrix.Event
		rel     relevance
		learned string
	}{
		{"to by name", ev("@bob:test.local", "m.room.message", map[string]any{"body": "x", ContentKey: map[string]any{"to": []string{"alice"}}}), relTo, ""},
		{"to by user id", ev("@bob:test.local", "m.room.message", map[string]any{"body": "x", ContentKey: map[string]any{"to": []string{me}}}), relTo, ""},
		{"to someone else", ev("@bob:test.local", "m.room.message", map[string]any{"body": "x", ContentKey: map[string]any{"to": []string{"carol", "all"}}}), "", ""},
		{"mentions", ev("@bob:test.local", "m.room.message", map[string]any{"body": "x", "m.mentions": map[string]any{"user_ids": []string{me}}}), relMention, ""},
		{"body mention", ev("@bob:test.local", "m.room.message", map[string]any{"body": "cc " + me}), relMention, ""},
		{"similar name is not a mention", ev("@bob:test.local", "m.room.message", map[string]any{"body": "cc @alicex:test.local and @alice:test.localhost"}), "", ""},
		{"mention before punctuation", ev("@bob:test.local", "m.room.message", map[string]any{"body": "@alice:test.localhost, then " + me + "."}), relMention, ""},
		{"my thread", ev("@bob:test.local", "m.room.message", thread("$mine")), relThread, ""},
		{"other thread", ev("@bob:test.local", "m.room.message", thread("$other")), "", ""},
		{"own reply learns thread", ev(me, "m.room.message", thread("$new")), "", "$new"},
		{"own topic learns root", ev(me, "m.room.message", map[string]any{"body": "[topic] x: y"}), "", "$e"},
		{"own mention ignored", ev(me, "m.room.message", map[string]any{"body": me}), "", ""},
		{"non-message ignored", ev("@bob:test.local", "m.reaction", map[string]any{"body": me}), "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rel, learned, _ := classify(tc.ev, "!r", me, "alice", st, nil)
			if rel != tc.rel || learned != tc.learned {
				t.Fatalf("classify = (%q, %q), want (%q, %q)", rel, learned, tc.rel, tc.learned)
			}
		})
	}
}
