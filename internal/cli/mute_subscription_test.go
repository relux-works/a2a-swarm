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
		{name: "room-sender", root: "$muted", subs: []Sub{{ID: "sender", SubSelector: SubSelector{Room: room, From: "@bob:test.local"}}}},
		{name: "room-topic", root: "$muted", subs: []Sub{{ID: "topic", SubSelector: SubSelector{Room: room, Topic: "$muted"}}}},
		{name: "mixed-matches", root: "$muted", subs: []Sub{roomSub, ownerSub}, rel: relSub, ids: []string{"owner"}},
		{name: "all-events", root: "$muted", subs: []Sub{{ID: "diagnostic", SubSelector: SubSelector{Room: room, AllEvents: true}}}, rel: relSub, ids: []string{"diagnostic"}},
		{name: "projection-matching-unchanged", root: "$muted", subs: []Sub{roomSub}, edit: func(c map[string]any) {
			h := c[ContentKey].(Header)
			h.Type = "projection"
			h.To = []string{"alice"}
			c[ContentKey] = h
		}, rel: relSub, ids: []string{"room"}},
		{name: "projection-no-direct-shortcut", root: "$muted", edit: func(c map[string]any) {
			h := c[ContentKey].(Header)
			h.Type = "projection"
			h.To = []string{"alice"}
			c[ContentKey] = h
		}},
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
	for _, route := range []string{"suppressed", "direct", "mention", "roomless", "all-events", "all"} {
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
