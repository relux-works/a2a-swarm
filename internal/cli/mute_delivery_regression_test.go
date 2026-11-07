package cli

import (
	"bytes"
	"context"
	"encoding/json"
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

// A forged header cannot suppress a room subscription or create a topic/follow
// match. Each forged claim is paired with a real m.thread control, including a
// conflicting header. Read's root comes only from the explicit server relation.
func TestMuteDeliveryIgnoresHeaderClaims(t *testing.T) {
	const room = "!room:test.local"
	for _, route := range []string{"room", "topic", "follow"} {
		t.Run(route, func(t *testing.T) {
			for _, relation := range []string{"", "reply", "$topic", "$neighbor"} {
				ev := headerTopicDecision(t, "$topic", relation)
				st := config.State{}
				var rows []Sub
				want := relevance("")
				switch route {
				case "room":
					st.Muted = []string{"$topic"}
					rows = []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
					if relation != "$topic" {
						want = relSub
					}
				case "topic":
					rows = []Sub{{ID: "topic", SubSelector: SubSelector{Room: room, Topic: "$topic"}}}
					if relation == "$topic" {
						want = relSub
					}
				case "follow":
					st.Threads = []string{"$topic", "$child"} // Unscoped follow state proves no reply membership.
					if relation == "$topic" {
						want = relThread
					}
				}
				rel, learned, ids := classify(ev, room, "@alice:test.local", "alice", st, rows)
				var wantIDs []string
				if want == relSub {
					wantIDs = []string{rows[0].ID}
				}
				if rel != want || learned != "" || !reflect.DeepEqual(ids, wantIDs) {
					t.Fatalf("route=%s relation=%q: got %q %q %v, want %q", route, relation, rel, learned, ids, want)
				}
				root := ""
				if strings.HasPrefix(relation, "$") {
					root = relation
				}
				if got := eventRecord("message", "#room", room, ev, "test.local", false).Root; got != root {
					t.Fatalf("read root=%q, want %q", got, root)
				}
			}
		})
	}
}

// Plain replies inherit only previously learned same-room membership, even
// without a header. Unknown and cross-room targets remain ordinary subscribed
// mail; valid muted targets suppress, and an unmuted target delivers/follows.
func TestMuteKnownReplyMembership(t *testing.T) {
	const room = "!room:test.local"
	for _, knownRoom := range []string{"", "!other:test.local", room} {
		for _, muted := range []bool{false, true} {
			for _, route := range []string{"room", "topic", "follow"} {
				for _, header := range []bool{false, true} {
					ev := headerTopicDecision(t, "$forged", "reply")
					if !header {
						c := eventContent(t, ev)
						delete(c, ContentKey)
						ev.Content, _ = json.Marshal(c)
					}
					st := config.State{Threads: []string{"$topic"}}
					if knownRoom != "" {
						st.RememberThreadEvent(config.ThreadEvent{Room: knownRoom, EventID: "$child", Root: "$topic"})
					}
					if muted {
						st.Muted = []string{"$topic"}
					}
					var rows []Sub
					want := relevance("")
					switch route {
					case "room":
						rows = []Sub{{ID: "room", SubSelector: SubSelector{Room: room, AllEvents: false}}}
						want = relSub
						if muted && knownRoom == room {
							want = ""
						}
					case "topic":
						rows = []Sub{{ID: "topic", SubSelector: SubSelector{Room: room, Topic: "$topic"}}}
						if !muted && knownRoom == room {
							want = relSub
						}
					case "follow":
						if !muted && knownRoom == room {
							want = relThread
						}
					}
					rel, _, _ := classify(ev, room, "@alice:test.local", "alice", st, rows)
					if rel != want {
						t.Fatalf("knownRoom=%q muted=%t route=%s header=%t: got %q want %q", knownRoom, muted, route, header, rel, want)
					}
				}
			}
		}
	}
}

// Muting a real Matrix thread suppresses ordinary rows while sender rows,
// all-events, direct addresses, mentions, principals and projections retain
// their independent delivery. Each route has an unmuted positive control.
func TestMuteThreadExemptions(t *testing.T) {
	const room = "!room:test.local"
	for _, route := range []string{"ordinary", "sender", "all-events", "roomless", "direct", "mention", "principal", "projection", "topic-sender"} {
		t.Run(route, func(t *testing.T) {
			for _, relation := range []string{"$topic", "reply"} {
				for _, muted := range []bool{true, false} {
					ev := headerTopicDecision(t, "$topic", relation)
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
					if relation == "reply" {
						st.RememberThreadEvent(config.ThreadEvent{Room: room, EventID: "$child", Root: "$topic"})
					}
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
		ev := headerTopicDecision(t, root, root)
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

// Both hook output paths stay silent for a muted Matrix-thread reply and ring
// for its unmuted neighbor. Counts and subsequent inbox receipts exclude only
// the suppressed event; no terminal, process, sleeps or live server is used.
func TestMuteThreadHookAndInbox(t *testing.T) {
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
				muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "$muted")))
				e.stdin = `{"session_id":"mute-fixture"}`
				args := []string{"hook", hook, "--runtime", runtime}
				if r := e.as("alice", args...).ok(t); r.stdout != "" {
					t.Fatalf("muted reply rang: %+v", r)
				}
				neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
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
	muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "$muted")))
	neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
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

func plainReplyDecision(target string) map[string]any {
	c := kindMsg("bob", "decision", "plain reply")
	c["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": target}}
	return c
}

// A direct reply to a muted root rings while membership is unknown, even after
// observing the root itself. Observing a same-room m.thread child suppresses
// the identical reply shape; neither suppression nor admission creates receipts.
func TestMuteDirectRootReplyNeedsLearnedMembership(t *testing.T) {
	for _, learned := range []bool{false, true} {
		name := "unknown-root"
		if learned {
			name = "learned-root"
		}
		t.Run(name, func(t *testing.T) {
			e, room := subWorld(t)
			_, s := notificationApp(t, e)
			ctx := context.Background()
			st, err := s.takePosition(ctx, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
			root := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "decision", "muted root"))
			notificationState(t, e, func(cur *config.State) { cur.Mute(root) })
			st, err = s.syncNow(ctx, st, rows, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if len(e.state("alice").ThreadEvents) != 0 || len(e.mailbox("alice").Pending()) != 0 {
				t.Fatal("observing the muted root learned membership or admitted mail")
			}
			if learned {
				child := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, root, root)))
				st, err = s.syncNow(ctx, st, rows, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				current := e.state("alice")
				if current.ThreadEventRoot(room, root) != root || current.ThreadEventRoot(room, child) != root || len(e.mailbox("alice").Pending()) != 0 {
					t.Fatal("suppressed m.thread child did not establish same-room root membership")
				}
			}
			reply := e.hs.inject(room, "@bob:test.local", plainReplyDecision(root))
			if _, err := s.syncNow(ctx, st, rows, time.Time{}); err != nil {
				t.Fatal(err)
			}
			entries := e.mailbox("alice").Pending()
			bell := doorbellOf(itemsOf(entries, s.server), s.server, nil)
			if learned {
				if len(entries) != 0 || bell.total != 0 || e.state("alice").ThreadEventRoot(room, reply) != root {
					t.Fatalf("learned root reply was not suppressed/learned: %+v %+v", entries, bell)
				}
			} else if len(entries) != 1 || entries[0].ID != reply || entries[0].Sub != "room" || bell.total != 1 || bell.types["sub"] != 1 || len(e.state("alice").ThreadEvents) != 0 {
				t.Fatalf("unknown root reply must ring without learning membership: %+v %+v", entries, bell)
			}
			if e.received("alice", root) || e.received("alice", reply) {
				t.Fatal("admission/suppression created receipts")
			}
		})
	}
}

// syncNow persists no membership for a header-only muted-topic claim: it and
// its later plain reply ring the room subscription. The same claim with a real
// m.thread relation learns membership and suppresses both events across cycles.
func TestMuteHeaderClaimCannotPoisonPersistedMembership(t *testing.T) {
	for _, relation := range []string{"", "$muted"} {
		name := "header-only"
		if relation != "" {
			name = "server-thread"
		}
		t.Run(name, func(t *testing.T) {
			e, room := subWorld(t)
			_, s := notificationApp(t, e)
			ctx := context.Background()
			st, err := s.takePosition(ctx, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			notificationState(t, e, func(cur *config.State) { cur.Mute("$muted") })
			rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
			claim := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", relation)))
			st, err = s.syncNow(ctx, st, rows, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			check := func(ids ...string) {
				t.Helper()
				entries := e.mailbox("alice").Pending()
				if len(entries) != len(ids) {
					t.Fatalf("pending=%+v, want IDs %v", entries, ids)
				}
				for i, id := range ids {
					if entries[i].ID != id || entries[i].Sub != "room" || entries[i].Rel != string(relSub) {
						t.Fatalf("pending[%d]=%+v, want subscription event %s", i, entries[i], id)
					}
				}
				bell := doorbellOf(itemsOf(entries, s.server), s.server, nil)
				if bell.total != len(ids) || bell.types["sub"] != len(ids) {
					t.Fatalf("wrong subscription ring: %+v", bell)
				}
			}
			current := e.state("alice")
			if relation == "" {
				check(claim)
				if len(current.ThreadEvents) != 0 {
					t.Fatalf("header claim persisted membership: %+v", current.ThreadEvents)
				}
			} else {
				check()
				if current.ThreadEventRoot(room, claim) != "$muted" || current.ThreadEventRoot(room, "$muted") != "$muted" {
					t.Fatal("real server relation did not persist membership")
				}
			}
			reply := e.hs.inject(room, "@bob:test.local", plainReplyDecision(claim))
			if _, err := s.syncNow(ctx, st, rows, time.Time{}); err != nil {
				t.Fatal(err)
			}
			current = e.state("alice")
			if relation == "" {
				check(claim, reply)
				if len(current.ThreadEvents) != 0 {
					t.Fatalf("reply to header-only claim persisted membership: %+v", current.ThreadEvents)
				}
			} else {
				check()
				if current.ThreadEventRoot(room, reply) != "$muted" {
					t.Fatal("plain reply did not inherit persisted server membership")
				}
			}
			if e.received("alice", claim) || e.received("alice", reply) {
				t.Fatal("admission/suppression created receipts")
			}
		})
	}
}

// wait and watch suppress a learned muted plain reply normally but deliver it
// under --all. An unmuted subscribed neighbor rings in both modes; the actual
// consumers print and mark shown without receipting either event.
func TestMuteLearnedReplyWaitWatchAll(t *testing.T) {
	for _, mode := range []string{"wait", "watch"} {
		for _, all := range []bool{false, true} {
			name := mode + "/ordinary"
			if all {
				name = mode + "/all"
			}
			t.Run(name, func(t *testing.T) {
				e, room := subWorld(t)
				a, s := notificationApp(t, e)
				ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
				defer cancel()
				a.ctx = ctx
				st, err := s.takePosition(ctx, time.Time{})
				if err != nil {
					t.Fatal(err)
				}
				row, _ := e.subscribe("alice", "--room", "proj", "--kind", "decision")
				notificationState(t, e, func(cur *config.State) { cur.Mute("$muted") })
				child := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "$muted")))
				if _, err := s.syncNow(ctx, st, e.serverSubs("alice").Subs, time.Time{}); err != nil {
					t.Fatal(err)
				}
				if e.state("alice").ThreadEventRoot(room, child) != "$muted" || len(e.mailbox("alice").Pending()) != 0 {
					t.Fatal("fixture did not learn suppressed child membership")
				}
				reply := e.hs.inject(room, "@bob:test.local", plainReplyDecision(child))
				neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
				var output bytes.Buffer
				a.Stdout = &output
				polls := 0
				base := s.client.HTTP.Transport
				a.HTTP = &http.Client{Transport: muteRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						polls++
						if polls == 2 {
							if mode != "watch" {
								t.Fatal("wait did not ring on the unmuted control")
							}
							cancel()
							return nil, context.Canceled
						}
					}
					return base.RoundTrip(r)
				})}
				args := []string{"--room", "proj", "--json"}
				if all {
					args = append(args, "--all")
				}
				if mode == "wait" {
					err = cmdWait(ctx, a, append(args, "--timeout", "0"))
				} else {
					err = cmdWatch(ctx, a, args)
				}
				if err != nil {
					t.Fatal(err)
				}
				want := []string{neighbor}
				if all {
					want = []string{reply, neighbor}
				}
				lines := strings.Split(strings.TrimSpace(output.String()), "\n")
				entries := e.mailbox("alice").Pending()
				if len(lines) != len(want) || len(entries) != len(want) {
					t.Fatalf("wrong delivery: output=%q pending=%+v", output.String(), entries)
				}
				for i, id := range want {
					var rec EventRecord
					if err := json.Unmarshal([]byte(lines[i]), &rec); err != nil {
						t.Fatal(err)
					}
					wantRel, wantSub := string(relSub), row
					if id == reply {
						wantRel, wantSub = string(relAll), ""
					}
					if rec.EventID != id || rec.Type != wantRel || entries[i].ID != id || entries[i].Rel != wantRel || entries[i].Sub != wantSub || !entries[i].Shown {
						t.Fatalf("wrong printed/shown entry: %+v %+v", rec, entries[i])
					}
				}
				if strings.Contains(output.String(), child) || e.received("alice", reply) || e.received("alice", neighbor) {
					t.Fatal("consumer replayed suppressed child or created receipts")
				}
			})
		}
	}
}

// Repeated policy changes defer normally without partial mail, receipts,
// cursor advancement or one-shot closure. Once policy stabilizes,
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
	deferred, err := s.syncNow(ctx, st, list.Subs, time.Time{})
	if err != nil || deferred.NextBatch != st.NextBatch || !deferred.IsMuted("$muted") {
		t.Fatalf("contention must defer normally with current persisted state: %+v %v", deferred, err)
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

// Observed server relations persist even for suppressed/unmatched events and
// establish same-room plain-reply membership across cycles without following.
// Unknown and cross-room targets retain the valid room-subscription control.
func TestMuteLearnsReplyMembershipAcrossCycles(t *testing.T) {
	e, room := subWorld(t)
	_, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	notificationState(t, e, func(cur *config.State) { cur.Mute("$muted") })
	child := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$forged", "$muted")))
	st, err = s.syncNow(ctx, st, nil, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if st.ThreadEventRoot(room, child) != "$muted" || st.HasThread("$muted") || len(e.mailbox("alice").Pending()) != 0 {
		t.Fatal("unmatched server thread membership was lost or granted delivery/follow")
	}
	reply := func(target string) map[string]any {
		c := kindMsg("bob", "decision", "plain reply")
		c["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": target}}
		return c
	}
	// Another room's observation is not proof about a target in this room.
	notificationState(t, e, func(cur *config.State) {
		cur.RememberThreadEvent(config.ThreadEvent{Room: "!other:test.local", EventID: "$foreign", Root: "$muted"})
	})
	suppressed := e.hs.inject(room, "@bob:test.local", reply(child))
	chain := e.hs.inject(room, "@bob:test.local", reply(suppressed))
	unknown := e.hs.inject(room, "@bob:test.local", reply("$unknown"))
	foreign := e.hs.inject(room, "@bob:test.local", reply("$foreign"))
	rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
	st, err = s.syncNow(ctx, st, rows, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	entries := e.mailbox("alice").Pending()
	if len(entries) != 2 || entries[0].ID != unknown || entries[1].ID != foreign || st.ThreadEventRoot(room, chain) != "$muted" {
		t.Fatalf("wrong membership or delivery: %+v %+v", st.ThreadEvents, entries)
	}
	if e.received("alice", suppressed) || e.received("alice", chain) {
		t.Fatal("suppression wrote receipts")
	}
}

// Three invalidated admissions must not terminate wait, watch or the native
// consumer. The next normal poll uses the unchanged cursor, then admits and
// rings only the unmuted control and closes its one-shot. Admission hooks
// establish ordering; no terminal fixture is used.
func TestMuteContentionConsumersContinue(t *testing.T) {
	for _, mode := range []string{"wait", "watch", "native"} {
		t.Run(mode, func(t *testing.T) {
			var e *env
			var room, generation string
			var native *nativeFixture
			if mode == "native" {
				e, room, native, generation = runtimeWorld(t)
				e.hs.mu.Lock()
				e.hs.liveClock = true
				e.hs.mu.Unlock()
			} else {
				e, room = subWorld(t)
			}
			a, s := notificationApp(t, e)
			ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
			defer cancel()
			a.ctx = ctx
			st, err := s.takePosition(ctx, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			row, _ := e.subscribe("alice", "--room", "proj", "--kind", "decision", "--until-kind", "decision")
			muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "$muted")))
			neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
			attempts, polls := 0, 0
			var output bytes.Buffer
			a.Stdout = &output
			a.beforeDeliver = func() {
				attempts++
				if attempts <= 3 {
					roots := []string{"$muted", "$change-1", "$change-2"}
					notificationState(t, e, func(cur *config.State) { cur.Mute(roots[attempts-1]) })
				} else if attempts != 4 {
					t.Fatal("unexpected extra admission")
				}
			}
			base := s.client.HTTP.Transport
			a.HTTP = &http.Client{Transport: muteRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/sync") {
					polls++
					if polls == 2 {
						current := e.state("alice")
						if attempts != 3 || r.URL.Query().Get("since") != st.NextBatch || current.NextBatch != st.NextBatch || len(current.ThreadEvents) != len(st.ThreadEvents) || len(e.mailbox("alice").Pending()) != 0 || e.serverSubs("alice").find(row) < 0 {
							t.Fatal("contention did not defer atomically to the next normal poll")
						}
					}
					if mode == "watch" && polls == 3 {
						if !strings.Contains(output.String(), neighbor) || strings.Contains(output.String(), muted) {
							t.Fatalf("wrong watch ring: %q", output.String())
						}
						cancel()
						return nil, context.Canceled
					}
				}
				return base.RoundTrip(r)
			})}
			s.client.HTTP = a.HTTP
			switch mode {
			case "wait":
				err = cmdWait(ctx, a, []string{"--timeout", "0", "--json"})
			case "watch":
				err = cmdWatch(ctx, a, []string{"--json"})
			case "native":
				native.onWake = cancel
				err = config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(time.Second), func() error { return a.runNative(ctx, s, generation) })
			}
			if err != nil {
				t.Fatal(err)
			}
			if attempts != 4 || polls < 2 || e.serverSubs("alice").find(row) >= 0 || e.state("alice").NextBatch == st.NextBatch {
				t.Fatalf("consumer failed to retry: attempts=%d polls=%d", attempts, polls)
			}
			if entries := e.mailbox("alice").Pending(); len(entries) != 1 || entries[0].ID != neighbor || e.received("alice", muted) || e.received("alice", neighbor) {
				t.Fatalf("wrong delivery/receipt state: %+v", entries)
			}
			if mode == "native" {
				if len(native.wakes) != 1 {
					t.Fatalf("native wakes=%d", len(native.wakes))
				}
			} else if !strings.Contains(output.String(), neighbor) || strings.Contains(output.String(), muted) {
				t.Fatalf("wrong ring: %q", output.String())
			}
		})
	}
}

// cmdWait survives three invalidated first-sync admissions with an empty cursor
// and no partial mail/membership/one-shot closure. Its next poll repeats initial
// membership recovery and rings only the unmuted post-join subscription control.
func TestMuteContentionFirstSyncRetries(t *testing.T) {
	e, room := subWorld(t)
	a, s := notificationApp(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	a.ctx = ctx
	row, _ := e.subscribe("alice", "--room", "proj", "--kind", "decision", "--until-kind", "decision")
	muted := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$muted", "$muted")))
	id := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
	mutedEvent, ok := e.hs.eventByID(muted)
	if !ok {
		t.Fatal("missing muted post-join fixture")
	}
	ev, ok := e.hs.eventByID(id)
	if !ok {
		t.Fatal("missing post-join fixture")
	}
	before := firstSyncMessage("$before", "@bob:test.local", msg("@alice:test.local pre-join control", nil))
	firstSyncFixture(t, e, room, false, "", before, firstSyncJoin("@alice:test.local", "$join"), mutedEvent, ev)
	if st := e.state("alice"); st.NextBatch != "" || len(st.ThreadEvents) != 0 {
		t.Fatal("fixture already has a cursor or learned membership")
	}
	attempts, polls := 0, 0
	a.beforeDeliver = func() {
		attempts++
		if attempts <= 3 {
			roots := []string{"$muted", "$mute-2", "$mute-3"}
			notificationState(t, e, func(cur *config.State) { cur.Mute(roots[attempts-1]) })
		} else if attempts != 4 {
			t.Fatal("unexpected extra initial admission")
		}
	}
	base := s.client.HTTP.Transport
	a.HTTP = &http.Client{Transport: muteRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/sync") {
			polls++
			if polls > 2 {
				t.Fatal("cmdWait did not ring after initial retry")
			}
			if r.URL.Query().Get("since") != "" || r.URL.Query().Get("filter") != firstDeliveryFilter {
				t.Fatal("initial retry lost the empty cursor or membership recovery filter")
			}
			if polls == 2 {
				current := e.state("alice")
				if attempts != 3 || current.NextBatch != "" || len(current.ThreadEvents) != 0 || len(e.mailbox("alice").Pending()) != 0 || e.serverSubs("alice").find(row) < 0 {
					t.Fatal("first-sync contention did not defer atomically to the next poll")
				}
			}
		}
		return base.RoundTrip(r)
	})}
	var output bytes.Buffer
	a.Stdout = &output
	if err := cmdWait(ctx, a, []string{"--timeout", "0", "--json"}); err != nil {
		t.Fatalf("cmdWait exited with an error during initial contention/retry: %v", err)
	}
	if attempts != 4 || polls != 2 || e.state("alice").NextBatch == "" || e.serverSubs("alice").find(row) >= 0 {
		t.Fatalf("initial retry failed: attempts=%d polls=%d", attempts, polls)
	}
	if entries := e.mailbox("alice").Pending(); len(entries) != 1 || entries[0].ID != id || entries[0].Sub != row || !entries[0].Shown {
		t.Fatalf("initial retry lost post-join control or admitted pre-join mail: %+v", entries)
	}
	var rec EventRecord
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.EventID != id || rec.Type != "sub" || rec.Sub != row || e.received("alice", id) || e.received("alice", muted) || e.received("alice", before.EventID) {
		t.Fatalf("initial retry rang the wrong event or created receipts: %q", output.String())
	}
}
