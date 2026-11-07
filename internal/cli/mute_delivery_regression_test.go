package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func headerTopicDecision(t *testing.T, root, relation string) matrix.Event {
	t.Helper()
	c := kindMsg("bob", "decision", "ordinary decision")
	h := c[ContentKey].(Header)
	h.Topic = root
	c[ContentKey] = h
	if relation == "reply" {
		c["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": "$child"}}
	} else if relation != "" {
		c["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": relation}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return matrix.Event{EventID: "$decision", Type: "m.room.message", Sender: "@bob:test.local", Content: raw, OriginServerTS: 1000}
}

// Plain replies and header-only messages use the same root in read and
// classification. An explicit Matrix thread wins over a conflicting header;
// the unmuted neighbor controls against suppressing every decision.
func TestMuteDeliveryRootAgreesWithRead(t *testing.T) {
	const room = "!room:test.local"
	row := Sub{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"system", "owner-needed", "decision"}}}
	for _, relation := range []string{"reply", "", "$muted", "$neighbor"} {
		for _, topic := range []string{"$muted", "$neighbor"} {
			t.Run(relation+"/"+topic, func(t *testing.T) {
				ev := headerTopicDecision(t, topic, relation)
				root := topic
				if strings.HasPrefix(relation, "$") {
					root = relation
				}
				if got := eventRecord("message", "#room", room, ev, "test.local", false).Root; got != root {
					t.Fatalf("read root=%q, want %q", got, root)
				}
				st := config.State{Muted: []string{"$muted"}, Threads: []string{"$child"}}
				rel, learned, ids := classify(ev, room, "@alice:test.local", "alice", st, []Sub{row})
				if root == "$muted" {
					if rel != "" || learned != "" || len(ids) != 0 {
						t.Fatalf("muted decision delivered: %q %q %v", rel, learned, ids)
					}
				} else if rel != relSub || !reflect.DeepEqual(ids, []string{"room"}) {
					t.Fatalf("neighbor lost: %q %v", rel, ids)
				}
			})
		}
	}
}

// Muting a header-named topic suppresses ordinary rows while sender rows,
// all-events, direct addresses, mentions, principals and projections retain
// their independent delivery. Each route has an unmuted positive control.
func TestMuteHeaderTopicExemptions(t *testing.T) {
	const room = "!room:test.local"
	for _, route := range []string{"ordinary", "sender", "all-events", "roomless", "direct", "mention", "principal", "projection", "topic-sender"} {
		t.Run(route, func(t *testing.T) {
			for _, muted := range []bool{true, false} {
				ev := headerTopicDecision(t, "$topic", "reply")
				row := Sub{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}
				var principals []string
				want := relSub
				switch route {
				case "sender", "topic-sender":
					row.From = ev.Sender
					if route == "topic-sender" {
						row.Topic = "$topic"
					}
				case "all-events":
					row.AllEvents = true
				case "roomless":
					row.Room = ""
				case "direct", "mention", "projection":
					var c map[string]any
					if err := json.Unmarshal(ev.Content, &c); err != nil {
						t.Fatal(err)
					}
					h := c[ContentKey].(map[string]any)
					if route == "direct" {
						h["to"] = []string{"alice"}
						want = relTo
					} else if route == "mention" {
						c["m.mentions"] = map[string]any{"user_ids": []string{"@alice:test.local"}}
						want = relMention
					} else {
						h["type"] = "projection"
					}
					ev.Content, _ = json.Marshal(c)
				case "principal":
					principals = []string{ev.Sender}
				}
				st := config.State{}
				if muted {
					st.Muted = []string{"$topic"}
					if route == "ordinary" {
						want = ""
					}
				}
				rel, _, ids := classifyWithPrincipals(ev, room, "@alice:test.local", "alice", st, []Sub{row}, principals)
				if rel != want {
					t.Fatalf("muted=%v: got %q, want %q", muted, rel, want)
				}
				if muted && (route == "ordinary" || route == "direct" || route == "mention" || route == "principal") && len(ids) != 0 {
					t.Fatalf("suppressed row still matched: %v", ids)
				}
			}
		})
	}
}

// Reuse the bound consumer's session and syncNow path with a deliberately
// stale poll snapshot: a persisted mute takes effect without recreating either.
// This is a delivery-cycle fixture, not a terminal/process lifecycle test.
// The neighbor reaches doorbell/inbox; the muted event creates no mail/receipt.
func TestMuteConsumerReloadWithoutRestart(t *testing.T) {
	e, room := subWorld(t)
	_, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"system", "owner-needed", "decision"}}}}
	ids := make([]string, 0, 2)
	for _, root := range []string{"$muted", "$neighbor"} {
		ev := headerTopicDecision(t, root, "reply")
		var c map[string]any
		if err := json.Unmarshal(ev.Content, &c); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.hs.inject(room, ev.Sender, c))
	}
	// Hold this running poll's response until mute has returned. Channel
	// barriers, rather than sleeps or scheduler timing, establish the ordering.
	ready, release := make(chan struct{}), make(chan struct{})
	base := s.client.HTTP.Transport
	s.client.HTTP = &http.Client{Transport: muteRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := base.RoundTrip(r)
		if strings.HasSuffix(r.URL.Path, "/sync") {
			close(ready)
			select {
			case <-release:
			case <-r.Context().Done():
				if resp != nil {
					resp.Body.Close()
				}
				return nil, r.Context().Err()
			}
		}
		return resp, err
	})}
	pollCtx, cancel := context.WithTimeout(ctx, testDeadline)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.syncNow(pollCtx, st, rows, time.Time{})
		done <- err
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("poll returned before mute: %v", err)
	case <-pollCtx.Done():
		t.Fatal(pollCtx.Err())
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error { cur.Mute("$muted"); return nil }); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := e.pendingEntry("alice", ids[0]); ok || e.received("alice", ids[0]) {
		t.Fatal("muted event created mail/receipt")
	}
	items := itemsOf(e.mailbox("alice").Pending(), s.server)
	if d := doorbellOf(items, s.server, nil); d.total != 1 || d.types["sub"] != 1 {
		t.Fatalf("wrong doorbell: %+v", d)
	}
	if len(items) != 1 || items[0].id != ids[1] || items[0].rec.Root != "$neighbor" {
		t.Fatalf("wrong pending control: %+v", items)
	}
	out := e.as("alice", "inbox", "--json").ok(t).stdout
	if strings.Contains(out, ids[0]) || !strings.Contains(out, ids[1]) || !e.received("alice", ids[1]) || e.received("alice", ids[0]) {
		t.Fatalf("inbox delivery/receipt mismatch: %q", out)
	}
}

type muteRoundTripFunc func(*http.Request) (*http.Response, error)

func (f muteRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Both hook output paths stay silent for a muted header-topic reply and ring
// for its unmuted neighbor. Counts and subsequent inbox receipts exclude only
// the suppressed event; no terminal, process, sleeps or live server is used.
func TestMuteHeaderTopicHookAndInbox(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		for _, hook := range []string{"prompt", "stop"} {
			t.Run(runtime+"/"+hook, func(t *testing.T) {
				e, room := subWorld(t)
				_, s := notificationApp(t, e)
				if _, err := s.takePosition(context.Background(), time.Time{}); err != nil {
					t.Fatal(err)
				}
				e.subscribe("alice", "--room", "proj", "--kind", "system,owner-needed,decision")
				notificationState(t, e, func(st *config.State) { st.Mute("$muted") })
				muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "reply")))
				e.stdin = `{"session_id":"mute-fixture"}`
				args := []string{"hook", hook, "--runtime", runtime}
				if r := e.as("alice", args...).ok(t); r.stdout != "" {
					t.Fatalf("muted reply rang: %+v", r)
				}
				neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "reply")))
				r := e.as("alice", args...).ok(t)
				var bell string
				if hook == "stop" {
					bell = stopReason(t, r.stdout)
				} else {
					bell = bellContext(t, r.stdout)
				}
				if !strings.Contains(bell, "1 new peer event (sub: 1; kinds: decision)") {
					t.Fatalf("wrong doorbell counts: %q", bell)
				}
				out := e.as("alice", "inbox", "--json").ok(t).stdout
				if strings.Contains(out, muted) || !strings.Contains(out, neighbor) || e.received("alice", muted) || !e.received("alice", neighbor) {
					t.Fatalf("wrong inbox events/receipts: %q", out)
				}
			})
		}
	}
}

// A mute arriving after classification invalidates the batch before append
// or one-shot closure. Reclassification retains the unmuted neighboring decision.
func TestMuteBetweenClassificationAndPersistence(t *testing.T) {
	e, room := subWorld(t)
	a, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := e.subscribe("alice", "--room", "proj", "--kind", "decision", "--until-kind", "decision")
	list := e.serverSubs("alice")
	muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "reply")))
	neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "reply")))
	a.beforeDeliver = func() {
		a.beforeDeliver = nil
		if _, err := config.UpdateState(e.paths(), "alice", func(cur *config.State) error { cur.Mute("$muted"); return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.syncNow(ctx, st, list.Subs, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.pendingEntry("alice", muted); ok || e.received("alice", muted) {
		t.Fatal("classification race retained muted event")
	}
	if en, ok := e.pendingEntry("alice", neighbor); !ok || en.Sub != id {
		t.Fatalf("neighbor must deliver and close one-shot: %+v %v", en, ok)
	}
	if list := e.serverSubs("alice"); list.find(id) >= 0 {
		t.Fatal("delivered neighbor did not close one-shot")
	}
}

func eventContent(t *testing.T, ev matrix.Event) map[string]any {
	t.Helper()
	var c map[string]any
	if err := json.Unmarshal(ev.Content, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// Repeated policy changes refuse the entire batch without partial mail,
// receipts, cursor advancement or one-shot closure. Once policy stabilizes,
// the same cursor delivers the neighbor and closes the row normally.
func TestMuteDeliveryContentionIsAtomic(t *testing.T) {
	e, room := subWorld(t)
	a, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := e.subscribe("alice", "--room", "proj", "--kind", "decision", "--until-kind", "decision")
	list := e.serverSubs("alice")
	muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "$muted")))
	neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
	roots := []string{"$muted", "$concurrent-1", "$concurrent-2"}
	changes := 0
	a.beforeDeliver = func() {
		if changes >= len(roots) {
			t.Fatal("unbounded reclassification")
		}
		root := roots[changes]
		changes++
		notificationState(t, e, func(cur *config.State) { cur.Mute(root) })
	}
	if _, err := s.syncNow(ctx, st, list.Subs, time.Time{}); !errors.Is(err, errDeliveryMutesChanged) {
		t.Fatalf("want policy contention refusal, got %v", err)
	}
	if changes != 3 || len(e.mailbox("alice").Pending()) != 0 || e.state("alice").NextBatch != st.NextBatch || e.serverSubs("alice").find(id) < 0 {
		t.Fatal("rejected batch partially delivered, advanced or closed its row")
	}
	a.beforeDeliver = nil
	if _, err := s.syncNow(ctx, st, list.Subs, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if entries := e.mailbox("alice").Pending(); len(entries) != 1 || entries[0].ID != neighbor || entries[0].Sub != id || e.state("alice").NextBatch == st.NextBatch || e.serverSubs("alice").find(id) >= 0 {
		t.Fatalf("stable retry failed: %+v", entries)
	}
	if e.received("alice", muted) || e.received("alice", neighbor) {
		t.Fatal("admission created receipts")
	}
}
