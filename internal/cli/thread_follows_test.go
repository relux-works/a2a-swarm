package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func threadContent(root, text string) map[string]any {
	return msg(text, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}})
}

// Proves real CLI creator/reply/review/attachment/quote actions do not follow;
// a fresh command and a cursor replay still reject non-addressed thread traffic.
// Fake homeserver plus durable mailbox fixtures do not qualify a live runtime.
func TestExplicitThreadParticipationNeverFollows(t *testing.T) {
	for _, action := range []string{"creator", "answer", "review", "attachment", "quote"} {
		t.Run(action, func(t *testing.T) {
			e, room := world(t)
			root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign thread").ok(t).stdout)
			e.as("alice", "wait", "--timeout", "10ms").ok(t)
			cursor := e.state("alice").NextBatch
			switch action {
			case "creator":
				root = strings.TrimSpace(e.as("alice", "topic", "new", "proj", "own", "Own thread").ok(t).stdout)
			case "answer":
				e.as("alice", "post", "proj", "--topic", root, "--kind", "answer", "assignment answered").ok(t)
			case "review":
				e.as("alice", "post", "proj", "--topic", root, "--kind", "answer", "review verdict: changes requested").ok(t)
			case "attachment":
				file := writeFile(t, t.TempDir(), "review.txt", []byte("fixture review"))
				e.as("alice", "attach", "proj", "--topic", root, file).ok(t)
			case "quote":
				e.as("alice", "post", "proj", "--topic", root, "> reference to another thread $other").ok(t)
			}
			id := e.hs.inject(room, "@bob:test.local", threadContent(root, "future thread-only reply"))
			unrelated := e.hs.inject(room, "@bob:test.local", threadContent("$other", "unrelated thread"))
			for _, replay := range []bool{false, true} {
				if replay {
					if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.NextBatch = cursor; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				control := e.hs.inject(room, "@bob:test.local", msg("valid delivery fence @alice:test.local", nil))
				r := e.as("alice", "wait", "--timeout", "2s").ok(t)
				if !strings.Contains(r.stdout, "mention #proj "+control) || e.mailbox("alice").Has(id) || e.mailbox("alice").Has(unrelated) || len(e.state("alice").ThreadFollows) != 0 {
					t.Fatalf("implicit follow admitted thread-only mail: action=%s replay=%v output=%q", action, replay, r.stdout)
				}
			}
		})
	}
}

// Proves explicit follow delivers exactly one durable entry across repeated
// follows and replay, excludes other roots, and mute excludes future thread mail.
func TestExplicitThreadFollowDelivery(t *testing.T) {
	e, room := world(t)
	var app *App
	e.configure = func(a *App) { app = a }
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.configure = nil
	e.as("alice", "follow", "proj", "--topic", root).ok(t)
	r := e.as("alice", "follow", "proj", "--topic", root).ok(t)
	if !strings.Contains(r.stdout, "already followed") || !strings.Contains(r.stdout, "mute cleared") {
		t.Fatalf("effective follow state not visible: %q", r.stdout)
	}
	cursor := e.state("alice").NextBatch
	id := e.hs.inject(room, "@bob:test.local", threadContent(root, "valid explicit delivery"))
	other := e.hs.inject(room, "@bob:test.local", threadContent("$other", "unrelated"))
	r = e.as("alice", "wait", "--timeout", "2s").ok(t)
	if !strings.Contains(r.stdout, "thread #proj "+id) || e.mailbox("alice").Has(other) {
		t.Fatalf("explicit delivery failed: %q", r.stdout)
	}
	before, err := os.ReadFile(e.paths().MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.NextBatch = cursor; return nil }); err != nil {
		t.Fatal(err)
	}
	session, err := app.session()
	if err != nil {
		t.Fatal(err)
	}
	list, err := session.getSubs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.syncNow(context.Background(), e.state("alice"), list.Subs, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if e.state("alice").NextBatch == cursor {
		t.Fatal("replay was not processed")
	}
	if r := e.as("alice", "wait", "--timeout", "40ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("replay duplicated notice: %q", r.stdout)
	}
	after, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	if !bytes.Equal(before, after) {
		t.Fatal("follow replay duplicated durable mail")
	}
	e.as("alice", "mute", "proj", "--topic", root).ok(t)
	blocked := e.hs.inject(room, "@bob:test.local", threadContent(root, "muted"))
	control := e.hs.inject(room, "@bob:test.local", msg("valid delivery fence @alice:test.local", nil))
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, control) || e.mailbox("alice").Has(blocked) {
		t.Fatalf("mute admitted thread-only mail: %q", r.stdout)
	}
	if !e.mailbox("alice").Has(id) || e.received("alice", id) {
		t.Fatal("mute discarded earlier pending obligation")
	}
}

// Proves direct-to and structured/body mentions deliver without following and
// through mute. Ordinary owner delivery is covered through the active DM scope
// separately; a pin alone grants no delivery authority.
func TestExplicitThreadPriorityControls(t *testing.T) {
	for _, follow := range []bool{false, true} {
		for _, muted := range []bool{false, true} {
			e, room := world(t)
			root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
			e.as("alice", "wait", "--timeout", "10ms").ok(t)
			if follow {
				e.as("alice", "follow", "proj", "--topic", root).ok(t)
			}
			if muted {
				e.as("alice", "mute", "proj", "--topic", root).ok(t)
			}
			for _, tc := range []struct {
				name, sender, want string
				extra              map[string]any
			}{
				{"direct", "@bob:test.local", "to", map[string]any{ContentKey: Header{To: []string{"alice"}}}},
				{"structured-mention", "@bob:test.local", "mention", map[string]any{"m.mentions": map[string]any{"user_ids": []string{"@alice:test.local"}}}},
				{"body-mention", "@bob:test.local", "mention", map[string]any{"body": "cc @alice:test.local"}},
			} {
				content := threadContent(root, tc.name)
				for k, v := range tc.extra {
					content[k] = v
				}
				id := e.hs.inject(room, tc.sender, content)
				r := e.as("alice", "wait", "--timeout", "2s").ok(t)
				if !strings.Contains(r.stdout, tc.want+" #proj "+id) || !e.mailbox("alice").Has(id) {
					t.Fatalf("priority %s follow=%v muted=%v output=%q", tc.name, follow, muted, r.stdout)
				}
			}
			// Projections retain their independent suppression policy.
			projection := threadContent(root, "projection")
			projection[ContentKey] = Header{Type: "projection"}
			id := e.hs.inject(room, "@owner:test.local", projection)
			control := e.hs.inject(room, "@bob:test.local", msg("valid projection fence @alice:test.local", nil))
			if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, control) || e.mailbox("alice").Has(id) {
				t.Fatalf("owner projection unexpectedly delivered: %q", r.stdout)
			}
		}
	}
}

// Proves explicitly scoped account subscriptions retain their selector behavior;
// thread mute excludes a topic subscription, while an independent kind scope passes.
func TestExplicitThreadSubscriptionScopes(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.as("alice", "subscribe", "--room", "proj", "--topic", root).ok(t)
	id := e.hs.inject(room, "@bob:test.local", threadContent(root, "explicit topic subscription"))
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, "sub #proj "+id) {
		t.Fatalf("explicit scope failed: %q", r.stdout)
	}
	e.as("alice", "mute", "proj", "--topic", root).ok(t)
	blocked := e.hs.inject(room, "@bob:test.local", threadContent(root, "muted topic subscription"))
	control := e.hs.inject(room, "@bob:test.local", msg("valid delivery fence @alice:test.local", nil))
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, control) || e.mailbox("alice").Has(blocked) {
		t.Fatalf("muted topic scope delivered: %q", r.stdout)
	}
	e.as("alice", "subscribe", "--room", "proj", "--kind", "decision").ok(t)
	content := threadContent(root, "explicit kind subscription")
	content[ContentKey] = Header{Kind: "decision"}
	id = e.hs.inject(room, "@bob:test.local", content)
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, "sub #proj "+id) {
		t.Fatalf("kind scope lost: %q", r.stdout)
	}
}

// Proves a mute committed between collection and append rejects new thread mail,
// preserves its own intent and leaves previous durable pending mail untouched.
func TestExplicitThreadConcurrentMute(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.as("alice", "follow", "proj", "--topic", root).ok(t)
	id := e.hs.inject(room, "@bob:test.local", threadContent(root, "collected before mute"))
	e.configure = func(a *App) {
		a.beforeDeliver = func() {
			_, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.Mute(root); return nil })
			if err != nil {
				t.Error(err)
			}
		}
	}
	control := e.hs.inject(room, "@bob:test.local", msg("concurrent mute delivery fence @alice:test.local", nil))
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, control) || e.mailbox("alice").Has(id) || !e.state("alice").IsMuted(root) {
		t.Fatalf("concurrent mute lost: %q", r.stdout)
	}
	e.configure = nil
	e.as("alice", "follow", "proj", "--topic", root).ok(t)
	id = e.hs.inject(room, "@bob:test.local", threadContent(root, "valid after explicit re-follow"))
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, id) {
		t.Fatalf("positive control failed: %q", r.stdout)
	}
}

// Proves the shared batch classifier rejects historical own events in backfill
// candidates; it does not simulate provider wake, generation replacement or live bind.
func TestExplicitThreadBatchReplay(t *testing.T) {
	e, room := world(t)
	var a *App
	e.configure = func(app *App) { a = app }
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	own := matrix.Event{EventID: "$own", Sender: s.creds.UserID, Type: "m.room.message"}
	own.Content, _ = json.Marshal(threadContent("$root", "historical own reply"))
	peer := matrix.Event{EventID: "$peer", Sender: "@bob:test.local", Type: "m.room.message"}
	peer.Content, _ = json.Marshal(threadContent("$root", "thread-only peer"))
	var resp matrix.SyncResponse
	// Exercise real collectBatch via JSON to use the public Matrix response schema.
	raw, _ := json.Marshal(map[string]any{"rooms": map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{own, peer}}}}}})
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	for _, st := range []config.State{{}, {Threads: []string{"$root"}}, {Participation: []string{"$root"}}} {
		b, err := s.collectBatch(context.Background(), resp, "", st, nil, nil, map[string]string{})
		if err != nil || len(b.entries) != 0 {
			t.Fatalf("replay learned follow: entries=%v err=%v", b.entries, err)
		}
	}
}

// Proves the first migration leaves an actual mailbox byte-exact, including
// shown and receipted entries, and old thread mail still drains by normal pull.
// The future rejection is checked in a batch with a real addressed control.
func TestExplicitThreadLegacyPendingRecovery(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	old := e.hs.inject(room, "@bob:test.local", threadContent(root, "already durable legacy thread mail"))
	ev, ok := e.hs.eventByID(old)
	if !ok {
		t.Fatal("fixture event missing")
	}
	raw, _ := json.Marshal(ev)
	_, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error {
		s.Threads, s.ThreadPolicyVersion, s.ThreadMigration = []string{root}, 0, nil
		s.LegacyPending = json.RawMessage(fmt.Sprintf(`{"events":[%q]}`, old))
		s.LegacyAcked = []string{"$already-receipted"}
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		if _, err := mb.Append([]config.MailEntry{
			{ID: old, Kind: config.MailEvent, Room: room, Label: "#proj", Rel: "thread", Sender: ev.Sender, Event: raw},
			{ID: "$shown-old", Kind: config.MailEvent, Room: room, Rel: "thread", Event: raw},
			{ID: "$already-receipted", Kind: config.MailEvent, Room: room, Rel: "to", Event: raw},
		}); err != nil {
			return err
		}
		if err := mb.MarkShown([]string{"$shown-old"}); err != nil {
			return err
		}
		_, err = mb.Receive([]string{"$already-receipted"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	r := e.as("alice", "mute", "proj", "--topic", root).ok(t)
	after, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	if !bytes.Equal(before, after) || !strings.Contains(r.stderr, "unknown legacy roots excluded") || !strings.Contains(r.stderr, "2 durable pending retained") {
		t.Fatalf("migration was destructive or invisible: %q", r.stderr)
	}
	st := e.state("alice")
	if len(st.LegacyPending) == 0 || len(st.LegacyAcked) != 1 || len(st.ThreadFollows) != 0 || len(st.ThreadMigration.Unknown) != 1 {
		t.Fatal("legacy state relabeled or lost")
	}
	r = e.as("alice", "wait", "--timeout", "2s").ok(t)
	if !strings.Contains(r.stdout, old) || e.received("alice", old) || !e.received("alice", "$already-receipted") {
		t.Fatalf("legacy pending did not recover: %q", r.stdout)
	}
	future := e.hs.inject(room, "@bob:test.local", threadContent(root, "future rejected after cutover"))
	control := e.hs.inject(room, "@bob:test.local", msg("new direct control @alice:test.local", nil))
	r = e.as("alice", "wait", "--timeout", "2s").ok(t)
	if !strings.Contains(r.stdout, control) || e.mailbox("alice").Has(future) || e.received("alice", old) || e.received("alice", "$shown-old") {
		t.Fatalf("future acceptance or old obligation corruption: %q", r.stdout)
	}
	e.as("alice", "ack", old).ok(t)
	if !e.received("alice", old) || e.received("alice", "$shown-old") {
		t.Fatal("normal disposition did not remain independent")
	}
}

// Proves existing Claude/Codex hook adapters use the shared eligibility and
// retain pending mail. These fake hooks are not deployed-provider conformance;
// Muse and native wake/bind are outside this source fixture's qualification.
func TestExplicitThreadHookEligibility(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			clock := e.clock()
			root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
			const input = `{"session_id":"explicit-thread-fixture"}`
			e.hookRT(runtime, "alice", "prompt", input).ok(t)
			e.as("alice", "post", "proj", "--topic", root, "my answer is not consent").ok(t)
			blocked := e.hs.inject(room, "@bob:test.local", threadContent(root, "no follow"))
			if r := e.hookRT(runtime, "alice", "prompt", input).ok(t); r.stdout != "" || e.mailbox("alice").Has(blocked) || e.state("alice").Wake.Attempt != nil {
				t.Fatalf("hook implicit eligibility: %q", r.stdout)
			}
			e.as("alice", "follow", "proj", "--topic", root).ok(t)
			allowed := e.hs.inject(room, "@bob:test.local", threadContent(root, "explicit follow"))
			clock.add(30 * time.Second)
			if r := e.hookRT(runtime, "alice", "prompt", input).ok(t); !strings.Contains(r.stdout, "thread: 1") || !e.mailbox("alice").Has(allowed) || e.received("alice", allowed) {
				t.Fatalf("hook positive failed: %q", r.stdout)
			}
			e.as("alice", "mute", "proj", "--topic", root).ok(t)
			blocked = e.hs.inject(room, "@bob:test.local", threadContent(root, "muted"))
			control := e.hs.inject(room, "@bob:test.local", toMsg("bob", "answer", "addressed despite mute"))
			clock.add(30 * time.Second)
			if r := e.hookRT(runtime, "alice", "prompt", input).ok(t); !strings.Contains(r.stdout, "to: 1") || e.mailbox("alice").Has(blocked) || !e.mailbox("alice").Has(control) || e.received("alice", allowed) {
				t.Fatalf("hook mute/direct control failed: %q", r.stdout)
			}
		})
	}
}

// Proves a concurrently muted topic subscription creates neither mail nor an
// until-kind closure; explicit re-follow restores delivery and normal closure.
func TestExplicitThreadConcurrentSubscriptionMute(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	sub, _ := e.subscribe("alice", "--room", "proj", "--topic", root, "--until-kind", "answer")
	content := threadContent(root, "closing event collected before mute")
	content[ContentKey] = Header{Kind: "answer"}
	blocked := e.hs.inject(room, "@bob:test.local", content)
	control := e.hs.inject(room, "@bob:test.local", msg("valid fence @alice:test.local", nil))
	e.configure = func(a *App) {
		a.beforeDeliver = func() {
			if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.Mute(root); return nil }); err != nil {
				t.Error(err)
			}
		}
	}
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, control) || e.mailbox("alice").Has(blocked) {
		t.Fatalf("concurrent topic mute admitted mail: %q", r.stdout)
	}
	if !containsSub(e.serverSubs("alice").Subs, sub) {
		t.Fatal("rejected event closed subscription")
	}
	e.configure = nil
	e.as("alice", "follow", "proj", "--topic", root).ok(t)
	allowed := e.hs.inject(room, "@bob:test.local", content)
	if r := e.as("alice", "wait", "--timeout", "2s").ok(t); !strings.Contains(r.stdout, allowed) || containsSub(e.serverSubs("alice").Subs, sub) {
		t.Fatalf("valid delivery/closure failed: %q", r.stdout)
	}
}

func containsSub(subs []Sub, id string) bool {
	for _, sub := range subs {
		if sub.ID == id {
			return true
		}
	}
	return false
}

// Proves limited-timeline backfill and a fresh replay recover addressed mail
// without relearning own historical posts; rejected thread traffic stays readable.
func TestExplicitThreadLimitedHistoryReplay(t *testing.T) {
	e, room := world(t)
	e.hs.syncLimit = 3
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	cursor := e.state("alice").NextBatch
	e.hs.inject(room, "@alice:test.local", threadContent(root, "old own answer from another machine"))
	rejected := e.hs.inject(room, "@bob:test.local", threadContent(root, "not addressed"))
	early := e.hs.inject(room, "@bob:test.local", msg("early valid @alice:test.local", nil))
	for i := 0; i < 10; i++ {
		e.hs.inject(room, "@bob:test.local", threadContent(root, "thread-only flood"))
	}
	before := e.hs.CallCount(messagesRoute)
	for _, replay := range []bool{false, true} {
		if replay {
			if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.NextBatch = cursor; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		control := e.hs.inject(room, "@bob:test.local", msg("late valid @alice:test.local", nil))
		r := e.as("alice", "wait", "--timeout", "2s").ok(t)
		if !strings.Contains(r.stdout, control) || (!replay && !strings.Contains(r.stdout, early)) || e.mailbox("alice").Has(rejected) || len(e.state("alice").ThreadFollows) != 0 {
			t.Fatalf("backfill relearned own follow or broke delivery: replay=%v output=%q", replay, r.stdout)
		}
	}
	if e.hs.CallCount(messagesRoute) <= before {
		t.Fatal("limited history path not exercised")
	}
	history := e.as("alice", "read", "proj", "--topic", root, "--json").ok(t).stdout
	if !strings.Contains(history, rejected) {
		t.Fatal("rejected mail was not retrievable from room history")
	}
}
