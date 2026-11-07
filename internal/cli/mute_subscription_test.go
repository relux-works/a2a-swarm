package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func mutedSubscriptionEvent(root, kind string) matrix.Event {
	c := kindMsg("bob", kind, "ordinary decision")
	if root != "" {
		c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root}
	}
	raw, _ := json.Marshal(c)
	return matrix.Event{EventID: "$event", Type: "m.room.message", Sender: "@bob:test.local", Content: raw, OriginServerTS: 1000}
}

// Every negative has a nearby allowed route. Match IDs are part of the
// assertion: a suppressed match must not terminate a one-shot subscription.
func TestMutedRoomSubscriptionClassify(t *testing.T) {
	const room = "!room:test.local"
	roomSub := Sub{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"system", "owner-needed", "decision"}}}
	ownerSub := Sub{ID: "owner", SubSelector: SubSelector{From: "@bob:test.local"}}
	state := config.State{Muted: []string{"$muted"}}
	tests := []struct {
		name string
		root string
		room string
		subs []Sub
		edit func(map[string]any)
		rel  relevance
		ids  []string
	}{
		{name: "muted-room", root: "$muted", subs: []Sub{roomSub}},
		{name: "neighbor-root", root: "$neighbor", subs: []Sub{roomSub}, rel: relSub, ids: []string{"room"}},
		{name: "same-slug-other-room", root: "$neighbor", room: "!other:test.local", subs: []Sub{{ID: "other", SubSelector: SubSelector{Room: "!other:test.local", Kinds: []string{"decision"}}}}, rel: relSub, ids: []string{"other"}},
		{name: "muted-root-itself", subs: []Sub{roomSub}},
		{name: "fresh-root-same-slug", subs: []Sub{roomSub}, rel: relSub, ids: []string{"room"}},
		{name: "direct-name", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) { h := c[ContentKey].(Header); h.To = []string{"alice"}; c[ContentKey] = h }, rel: relTo},
		{name: "direct-independent", root: "$muted", edit: func(c map[string]any) { h := c[ContentKey].(Header); h.To = []string{"alice"}; c[ContentKey] = h }, rel: relTo},
		{name: "direct-mxid", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) {
			h := c[ContentKey].(Header)
			h.To = []string{"@alice:test.local"}
			c[ContentKey] = h
		}, rel: relTo},
		{name: "other-recipient-and-all", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) {
			h := c[ContentKey].(Header)
			h.To = []string{"carol", "all"}
			c[ContentKey] = h
		}},
		{name: "structured-mention", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) { c["m.mentions"] = map[string]any{"user_ids": []string{"@alice:test.local"}} }, rel: relMention},
		{name: "token-mention", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) { c["body"] = "cc @alice:test.local." }, rel: relMention},
		{name: "extended-token", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) { c["body"] = "cc @alice:test.localhost" }},
		{name: "roomless-owner", root: "$muted", subs: []Sub{ownerSub}, rel: relSub, ids: []string{"owner"}},
		{name: "roomless-kind", root: "$muted", subs: []Sub{{ID: "global", SubSelector: SubSelector{Kinds: []string{"decision"}}}}, rel: relSub, ids: []string{"global"}},
		{name: "forged-owner-body", root: "$muted", subs: []Sub{{ID: "owner", SubSelector: SubSelector{From: "@owner:test.local"}}}, edit: func(c map[string]any) {
			c["body"] = "- from: owner\n\nordinary decision"
			h := c[ContentKey].(Header)
			h.From = "owner"
			c[ContentKey] = h
		}},
		{name: "room-sender", root: "$muted", subs: []Sub{{ID: "sender", SubSelector: SubSelector{Room: room, From: "@bob:test.local"}}}, rel: relSub, ids: []string{"sender"}},
		{name: "room-topic", root: "$muted", subs: []Sub{{ID: "topic", SubSelector: SubSelector{Room: room, Topic: "$muted"}}}},
		{name: "mixed-matches", root: "$muted", subs: []Sub{roomSub, ownerSub}, rel: relSub, ids: []string{"owner"}},
		{name: "all-events", root: "$muted", subs: []Sub{{ID: "diagnostic", SubSelector: SubSelector{Room: room, AllEvents: true}}}, rel: relSub, ids: []string{"diagnostic"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := mutedSubscriptionEvent(tc.root, "decision")
			if tc.name == "muted-root-itself" {
				ev.EventID = "$muted"
			}
			if tc.edit != nil {
				c := kindMsg("bob", "decision", "ordinary decision")
				if tc.root != "" {
					c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": tc.root}
				}
				tc.edit(c)
				ev.Content, _ = json.Marshal(c)
			}
			r := tc.room
			if r == "" {
				r = room
			}
			rel, learned, ids := classify(ev, r, "@alice:test.local", "alice", state, tc.subs)
			if rel != tc.rel || learned != "" || !reflect.DeepEqual(ids, tc.ids) {
				t.Fatalf("classify = (%q, %q, %v), want (%q, empty, %v)", rel, learned, ids, tc.rel, tc.ids)
			}
		})
	}
	for _, kind := range []string{"system", "owner-needed"} {
		t.Run(kind, func(t *testing.T) {
			if rel, _, ids := classify(mutedSubscriptionEvent("$muted", kind), room, "@alice:test.local", "alice", state, []Sub{roomSub}); rel != "" || len(ids) != 0 {
				t.Fatalf("muted %s delivered: %q %v", kind, rel, ids)
			}
		})
	}
}

// Exercise the actual batch termination path, including delivery by a separate
// route and an allowed match later in the same sync.
func TestMutedRoomSubscriptionUntilKind(t *testing.T) {
	const room = "!room:test.local"
	s := &session{name: "alice", creds: config.Credentials{UserID: "@alice:test.local"}, client: &matrix.Client{}}
	for _, route := range []string{"suppressed", "direct", "mention", "roomless", "room-sender", "all-events", "all"} {
		t.Run(route, func(t *testing.T) {
			subs := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}, UntilKind: "decision"}}}
			ev := mutedSubscriptionEvent("$muted", "decision")
			all := map[string]bool{}
			wantClose := []string(nil)
			wantEntries := 0
			switch route {
			case "direct", "mention":
				c := kindMsg("bob", "decision", "ordinary decision")
				c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": "$muted"}
				if route == "direct" {
					h := c[ContentKey].(Header)
					h.To = []string{"alice"}
					c[ContentKey] = h
				} else {
					c["body"] = "@alice:test.local"
				}
				ev.Content, _ = json.Marshal(c)
				wantEntries = 1
			case "roomless":
				subs = append(subs, Sub{ID: "owner", SubSelector: SubSelector{From: "@bob:test.local", UntilKind: "decision"}})
				wantClose, wantEntries = []string{"owner"}, 1
			case "room-sender":
				subs = append(subs, Sub{ID: "sender", SubSelector: SubSelector{Room: room, From: "@bob:test.local", UntilKind: "decision"}})
				wantClose, wantEntries = []string{"sender"}, 1
			case "all-events":
				subs = append(subs, Sub{ID: "diagnostic", SubSelector: SubSelector{Room: room, AllEvents: true, UntilKind: "decision"}})
				wantClose, wantEntries = []string{"diagnostic"}, 1
			case "all":
				all[room], wantEntries = true, 1
			}
			collect := func(events ...matrix.Event) batch {
				t.Helper()
				raw, err := json.Marshal(map[string]any{"rooms": map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": events}}}}})
				if err != nil {
					t.Fatal(err)
				}
				var resp matrix.SyncResponse
				if err := json.Unmarshal(raw, &resp); err != nil {
					t.Fatal(err)
				}
				b, err := s.collectBatch(context.Background(), resp, "before", config.State{Muted: []string{"$muted"}}, subs, all, map[string]string{room: "#room"})
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			b := collect(ev)
			if len(b.entries) != wantEntries || !reflect.DeepEqual(b.close, wantClose) {
				t.Fatalf("entries=%v close=%v, want %d %v", b.entries, b.close, wantEntries, wantClose)
			}
			neighbor := mutedSubscriptionEvent("$neighbor", "decision")
			neighbor.EventID = "$neighbor-event"
			late := neighbor
			late.EventID = "$late"
			b = collect(ev, neighbor, late)
			wantClose = append(wantClose, "room")
			wantBatchEntries := wantEntries + 1
			if route == "all" {
				wantBatchEntries++
			}
			if len(b.entries) != wantBatchEntries || !reflect.DeepEqual(b.close, wantClose) {
				t.Fatalf("same-batch entries=%v close=%v, want %d %v", b.entries, b.close, wantBatchEntries, wantClose)
			}
		})
	}
}

// Mute affects future classification only. Existing pending mail survives a
// state reload, receives exactly once, and is not replayed after follow.
func TestMutedRoomSubscriptionPendingAndFollow(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.subscribe("alice", "--room", "proj", "--kind", "decision")
	content := func() map[string]any {
		c := kindMsg("bob", "decision", "decision in alpha")
		c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root}
		return c
	}
	before := e.hs.inject(room, "@bob:test.local", content())
	e.as("alice", "wait", "--timeout", "2s").ok(t)
	cursor := e.state("alice").NextBatch
	e.as("alice", "mute", "proj", "--topic", "alpha").ok(t)
	e.as("alice", "mute", "proj", "--topic", "alpha").ok(t)
	if st := e.state("alice"); !st.IsMuted(root) || st.NextBatch != cursor {
		t.Fatalf("mute changed cursor or was not persisted: %+v", st)
	}
	if _, ok := e.pendingEntry("alice", before); !ok || e.received("alice", before) {
		t.Fatal("mute deleted or receipted pending entry")
	}
	e.as("alice", "post", "proj", "--topic", "alpha", "own reply").ok(t)
	suppressed := e.hs.inject(room, "@bob:test.local", content())
	if out := e.as("alice", "wait", "--timeout", "80ms").ok(t).stdout; out != "timeout\n" {
		t.Fatalf("muted subscription rang: %q", out)
	}
	if _, ok := e.pendingEntry("alice", suppressed); ok || e.received("alice", suppressed) {
		t.Fatal("suppression created pending mail or receipt")
	}
	if !strings.Contains(e.as("alice", "inbox").ok(t).stdout, before) {
		t.Fatal("pending-before-mute entry unavailable")
	}
	if !e.received("alice", before) {
		t.Fatal("successful inbox did not receipt pending entry")
	}
	if strings.Contains(e.as("alice", "inbox").ok(t).stdout, before) {
		t.Fatal("pending-before-mute entry retrieved twice")
	}
	if out := e.as("alice", "ack", before).ok(t).stdout; !strings.Contains(out, "acked 0 events") {
		t.Fatal(out)
	}
	e.as("alice", "follow", "proj", "--topic", "alpha").ok(t)
	e.as("alice", "follow", "proj", "--topic", "alpha").ok(t)
	after := e.hs.inject(room, "@bob:test.local", content())
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	if !strings.Contains(out, after) || strings.Contains(out, before) || strings.Contains(out, suppressed) {
		t.Fatalf("follow replayed history or lost future match: %q", out)
	}
}

// Limited sync must classify backfilled originals with the same selector,
// retain neighboring traffic, and advance the cursor without inventing receipts.
func TestMutedRoomSubscriptionBackfill(t *testing.T) {
	e, room := subWorld(t)
	e.hs.syncLimit = 2
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.subscribe("alice", "--room", "proj", "--kind", "owner-needed")
	e.as("alice", "mute", "proj", "--topic", "alpha").ok(t)
	cursor := e.state("alice").NextBatch
	content := func(thread string, direct bool) map[string]any {
		c := kindMsg("bob", "owner-needed", "ordinary request")
		c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": thread}
		if direct {
			h := c[ContentKey].(Header)
			h.To = []string{"alice"}
			c[ContentKey] = h
		}
		return c
	}
	suppressed := e.hs.inject(room, "@bob:test.local", content(root, false))
	neighbor := e.hs.inject(room, "@bob:test.local", content("$neighbor", false))
	for i := 0; i < 4; i++ {
		e.hs.inject(room, "@bob:test.local", msg("unrelated chatter", nil))
	}
	direct := e.hs.inject(room, "@bob:test.local", content(root, true))
	e.hs.inject(room, "@bob:test.local", msg("unrelated tail", nil))
	calls := e.hs.CallCount(messagesRoute)
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	if !strings.Contains(out, neighbor) || !strings.Contains(out, direct) || strings.Contains(out, suppressed) || strings.Count(out, "\n") != 2 {
		t.Fatalf("backfill delivered wrong events: %q", out)
	}
	if e.hs.CallCount(messagesRoute) <= calls || e.state("alice").NextBatch == cursor {
		t.Fatal("backfill or cursor advancement was not exercised")
	}
	if _, ok := e.pendingEntry("alice", suppressed); ok || e.received("alice", suppressed) {
		t.Fatal("suppressed backfill created mail or receipt")
	}
	if out := e.as("alice", "wait", "--timeout", "60ms").ok(t).stdout; out != "timeout\n" {
		t.Fatalf("backfill replayed events: %q", out)
	}
}

// Client messages have no a2a header, no direct-address field and no mention.
// The exact event sender, not any sender-like body text, selects delivery.
func TestMutedSenderSubscriptionHeaderless(t *testing.T) {
	const room = "!room:test.local"
	base := Sub{ID: "sender", SubSelector: SubSelector{Room: room, From: "@bob:test.local"}}
	for _, tc := range []struct {
		name string
		edit func(*Sub, *matrix.Event)
		want bool
	}{
		{name: "room-sender", want: true},
		{name: "roomless-sender", edit: func(s *Sub, _ *matrix.Event) { s.Room = "" }, want: true},
		{name: "sender-topic", edit: func(s *Sub, _ *matrix.Event) { s.Topic = "$muted" }, want: true},
		{name: "sender-kind-needs-header", edit: func(s *Sub, _ *matrix.Event) { s.Kinds = []string{"decision"} }},
		{name: "sender-subtype-needs-header", edit: func(s *Sub, _ *matrix.Event) { s.Subtypes = []string{"notice"} }},
		{name: "other-sender-same-body", edit: func(_ *Sub, e *matrix.Event) { e.Sender = "@carol:test.local" }},
		{name: "other-sender-server", edit: func(_ *Sub, e *matrix.Event) { e.Sender = "@bob:other.test" }},
		{name: "other-room", edit: func(s *Sub, _ *matrix.Event) { s.Room = "!other:test.local" }},
		{name: "other-topic", edit: func(s *Sub, _ *matrix.Event) { s.Topic = "$other" }},
		{name: "subject-match", edit: func(s *Sub, _ *matrix.Event) { s.Subject = "TASK-1" }, want: true},
		{name: "subject-mismatch", edit: func(s *Sub, _ *matrix.Event) { s.Subject = "TASK-2" }},
		{name: "before-creation", edit: func(s *Sub, _ *matrix.Event) { s.CreatedAt = "1970-01-01T00:00:02Z" }},
		{name: "at-creation", edit: func(s *Sub, _ *matrix.Event) { s.CreatedAt = "1970-01-01T00:00:01Z" }, want: true},
		{name: "expired", edit: func(s *Sub, _ *matrix.Event) { s.Until = "1970-01-01T00:00:00Z" }},
		{name: "unexpired", edit: func(s *Sub, _ *matrix.Event) { s.Until = "1970-01-01T00:00:02Z" }, want: true},
		{name: "before-since", edit: func(s *Sub, _ *matrix.Event) { s.Since = "$since"; s.SinceTS = 2000 }},
		{name: "after-since", edit: func(s *Sub, _ *matrix.Event) { s.Since = "$since"; s.SinceTS = 999 }, want: true},
		{name: "self-event", edit: func(s *Sub, e *matrix.Event) { s.From = "@alice:test.local"; e.Sender = s.From }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := base
			raw, err := json.Marshal(msg("TASK-1: plain client reply from bob", map[string]any{
				"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$muted"},
			}))
			if err != nil {
				t.Fatal(err)
			}
			ev := matrix.Event{EventID: "$client", Type: "m.room.message", Sender: "@bob:test.local", Content: raw, OriginServerTS: 1000}
			if tc.edit != nil {
				tc.edit(&sub, &ev)
			}
			if _, ok := decodeHeader(ev.Message().A2A); ok || ev.Message().Mentions != nil || strings.Contains(ev.Message().Body, "@alice") {
				t.Fatal("fixture must be a headerless, unmentioned client message")
			}
			rel, _, ids := classify(ev, room, "@alice:test.local", "alice", config.State{Muted: []string{"$muted"}}, []Sub{sub})
			if tc.want {
				if rel != relSub || !reflect.DeepEqual(ids, []string{"sender"}) {
					t.Fatalf("sender delivery lost: %q %v", rel, ids)
				}
			} else if rel != "" || len(ids) != 0 {
				t.Fatalf("nonmatching sender row delivered: %q %v", rel, ids)
			}
		})
	}
}

// Use the projector itself: real projections are unthreaded, with topic_root
// only in their header. Muting that origin root cannot promote or hide a mirror.
func TestMutedSubscriptionRealProjection(t *testing.T) {
	origin := mutedSubscriptionEvent("$muted", "decision")
	h, _ := decodeHeader(origin.Message().A2A)
	h.To = []string{"alice"}
	content, err := projection(origin, h, "!origin:test.local", "#origin", "test.local", "", "")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	ev := matrix.Event{EventID: "$mirror", Type: "m.room.message", Sender: "@projector:test.local", Content: raw, OriginServerTS: 1000}
	var projected Projection
	encoded, _ := json.Marshal(content[ContentKey])
	if err := json.Unmarshal(encoded, &projected); err != nil {
		t.Fatal(err)
	}
	if ev.ThreadRoot() != "" || projected.TopicRoot != "$muted" {
		t.Fatal("not the actual unthreaded projection shape")
	}
	for _, tc := range []struct {
		name string
		subs []Sub
		ids  []string
	}{
		{name: "room-kind", subs: []Sub{{ID: "room", SubSelector: SubSelector{Room: "!system:test.local", Kinds: []string{"decision"}}}}, ids: []string{"room"}},
		{name: "all-events", subs: []Sub{{ID: "all", SubSelector: SubSelector{Room: "!system:test.local", AllEvents: true}}}, ids: []string{"all"}},
		{name: "roomless-no-duplicate", subs: []Sub{{ID: "global", SubSelector: SubSelector{Kinds: []string{"decision"}}}}},
		{name: "no-direct-shortcut"},
		{name: "origin-topic-is-not-thread", subs: []Sub{{ID: "topic", SubSelector: SubSelector{Room: "!system:test.local", Topic: "$muted"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, state := range []config.State{{}, {Muted: []string{"$muted"}}} {
				rel, learned, ids := classify(ev, "!system:test.local", "@alice:test.local", "alice", state, tc.subs)
				want := relevance("")
				if len(tc.ids) > 0 {
					want = relSub
				}
				if rel != want || learned != "" || !reflect.DeepEqual(ids, tc.ids) {
					t.Fatalf("projection = %q %q %v, want %q %v", rel, learned, ids, want, tc.ids)
				}
			}
		})
	}
}

// Exercise persistence before cursor advancement for real headerless client
// replies, including limited-sync backfill and a failed output/retry.
func TestMutedSenderSubscriptionMail(t *testing.T) {
	for _, mode := range []string{"timeline", "backfill", "output-failure"} {
		t.Run(mode, func(t *testing.T) {
			e, room := subWorld(t)
			if mode == "backfill" {
				e.hs.syncLimit = 2
			}
			root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
			e.as("alice", "wait", "--timeout", "10ms").ok(t)
			senderID, _ := e.subscribe("alice", "--room", "proj", "--from", "bob")
			e.subscribe("alice", "--room", "proj", "--kind", "decision")
			e.as("alice", "mute", "proj", "--topic", root).ok(t)
			cursor := e.state("alice").NextBatch
			reply := map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}
			client := msg("plain client reply", reply)
			if _, ok := client[ContentKey]; ok {
				t.Fatal("client fixture has an a2a header")
			}
			id := e.hs.inject(room, "@bob:test.local", client)
			forged := e.hs.inject(room, "@carol:test.local", msg("plain client reply from bob", reply))
			ambient := kindMsg("carol", "decision", "ambient room-kind traffic")
			ambient["m.relates_to"] = reply["m.relates_to"]
			suppressed := e.hs.inject(room, "@carol:test.local", ambient)
			for i := 0; i < 4; i++ {
				e.hs.inject(room, "@carol:test.local", msg("unrelated tail", nil))
			}
			calls := e.hs.CallCount(messagesRoute)
			if mode == "output-failure" {
				e.configure = func(a *App) { a.Stdout = failWriter{} }
				e.as("alice", "wait", "--timeout", "2s").failsWith(t, ExitError, "broken pipe")
				e.configure = nil
				if en, ok := e.pendingEntry("alice", id); !ok || en.Shown || e.received("alice", id) {
					t.Fatalf("failed output lost pending mail: %+v %v", en, ok)
				}
			}
			out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
			if !strings.Contains(out, "sub #proj "+id) || strings.Count(out, "\n") != 1 {
				t.Fatalf("wrong client delivery: %q", out)
			}
			en, ok := e.pendingEntry("alice", id)
			if !ok || !en.Shown || en.Sub != senderID || e.received("alice", id) || e.state("alice").NextBatch == cursor {
				t.Fatalf("mail/cursor invariant: %+v %v", en, ok)
			}
			if mode == "backfill" && e.hs.CallCount(messagesRoute) <= calls {
				t.Fatal("backfill was not exercised")
			}
			for _, ignored := range []string{forged, suppressed} {
				if _, ok := e.pendingEntry("alice", ignored); ok || e.received("alice", ignored) {
					t.Fatalf("nonmatch created mail/receipt: %s", ignored)
				}
			}
			if out := e.as("alice", "wait", "--timeout", "60ms").ok(t).stdout; out != "timeout\n" {
				t.Fatalf("client event duplicated after cursor: %q", out)
			}
			e.as("alice", "follow", "proj", "--topic", root).ok(t)
			if _, ok := e.pendingEntry("alice", id); !ok {
				t.Fatal("follow removed pending client mail")
			}
			if out := e.as("alice", "inbox").ok(t).stdout; !strings.Contains(out, id) {
				t.Fatal("pending client mail unavailable after follow")
			}
			if !e.received("alice", id) {
				t.Fatal("inbox did not receipt client mail")
			}
			if out := e.as("alice", "inbox").ok(t).stdout; strings.Contains(out, id) {
				t.Fatal("client mail received twice")
			}
		})
	}
}

// Adding a topic-only one-shot does not unmute it. Even a separately delivered
// answer cannot close the suppressed row; following permits the next answer.
func TestMutedTopicOneShot(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.as("alice", "mute", "proj", "--topic", root).ok(t)
	r := e.as("alice", "subscribe", "--room", "proj", "--topic", root, "--until-kind", "answer").ok(t)
	line := subLineRE.FindStringSubmatch(strings.TrimSpace(r.stdout))
	if line == nil || !strings.Contains(r.stderr, "topic is muted") || !strings.Contains(r.stderr, "a2a follow") {
		t.Fatalf("muted one-shot needs a warning and normal subscription output: %+v", r)
	}
	id := line[1]
	answer := func(direct bool) map[string]any {
		c := kindMsg("bob", "answer", "answer in muted topic")
		c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root}
		if direct {
			h := c[ContentKey].(Header)
			h.To = []string{"alice"}
			c[ContentKey] = h
		}
		return c
	}
	suppressed := e.hs.inject(room, "@bob:test.local", answer(false))
	if out := e.as("alice", "wait", "--timeout", "60ms").ok(t).stdout; out != "timeout\n" {
		t.Fatalf("muted one-shot rang: %q", out)
	}
	if list := e.serverSubs("alice"); list.find(id) < 0 {
		t.Fatal("suppressed answer closed one-shot")
	}
	direct := e.hs.inject(room, "@bob:test.local", answer(true))
	if out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout; !strings.Contains(out, direct) {
		t.Fatalf("direct answer lost: %q", out)
	}
	if list := e.serverSubs("alice"); list.find(id) < 0 {
		t.Fatal("separate direct delivery closed suppressed one-shot")
	}
	e.as("alice", "follow", "proj", "--topic", root).ok(t)
	allowed := e.hs.inject(room, "@bob:test.local", answer(false))
	if out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout; !strings.Contains(out, allowed) || strings.Contains(out, suppressed) {
		t.Fatalf("follow lost future answer or replayed suppressed one: %q", out)
	}
	if list := e.serverSubs("alice"); list.find(id) >= 0 {
		t.Fatal("delivered answer did not close one-shot")
	}
	if _, ok := e.pendingEntry("alice", suppressed); ok || e.received("alice", suppressed) {
		t.Fatal("suppression created mail/receipt")
	}
}

func TestMutedTopicSubscriptionWarningExemptions(t *testing.T) {
	for _, mode := range []string{"sender", "all-events", "unmuted"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := subWorld(t)
			root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
			args := []string{"subscribe", "--room", "proj", "--topic", root, "--until-kind", "answer"}
			if mode != "unmuted" {
				e.as("alice", "mute", "proj", "--topic", root).ok(t)
			}
			switch mode {
			case "sender":
				args = append(args, "--from", "bob")
			case "all-events":
				args = append(args, "--all-events")
			}
			r := e.as("alice", args...).ok(t)
			if strings.Contains(r.stderr, "topic is muted") {
				t.Fatalf("warned about exempt subscription: %s", r.stderr)
			}
			if mode != "unmuted" && !e.state("alice").IsMuted(root) {
				t.Fatal("subscription unexpectedly removed mute")
			}
		})
	}
}
