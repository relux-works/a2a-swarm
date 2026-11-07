package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func firstSyncJoin(user, id string) matrix.Event {
	return matrix.Event{EventID: id, Type: "m.room.member", Sender: user, StateKey: &user, Content: json.RawMessage(`{"membership":"join"}`)}
}

func firstSyncMessage(id, sender string, content map[string]any) matrix.Event {
	raw, _ := json.Marshal(content)
	return matrix.Event{EventID: id, Type: "m.room.message", Sender: sender, Content: raw}
}

func firstSyncFixture(t *testing.T, e *env, room string, limited bool, prev string, events ...matrix.Event) {
	t.Helper()
	var resp matrix.SyncResponse
	raw, err := json.Marshal(map[string]any{"next_batch": fmt.Sprintf("s%d", len(e.hs.events)), "rooms": map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": events, "limited": limited, "prev_batch": prev}}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	e.hs.initialDelivery = &resp
}

// A post-join instruction rings once, remains pending until inbox receipts it,
// and neither a second sync nor repeating the first batch duplicates it.
// A pre-join instruction and our own post-join message are excluded controls.
func TestFirstSyncDeliveryOnce(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	before := firstSyncMessage("$before", "@bob:test.local", ping(0))
	after := firstSyncMessage("$after", "@bob:test.local", toMsg("bob", "question", "FIRST_INSTRUCTION"))
	own := firstSyncMessage("$own", "@alice:test.local", ping(2))
	firstSyncFixture(t, e, room, false, "", before, firstSyncJoin("@alice:test.local", "$join"), after, own)
	bell := bellContext(t, e.hook("alice", "prompt", `{"session_id":"first"}`).ok(t).stdout)
	if bell != "a2a: 1 new peer event (to: 1; kinds: question) from bob"+bellTail {
		t.Fatalf("doorbell: %q", bell)
	}
	if pending := e.mailbox("alice").Pending(); len(pending) != 1 || pending[0].ID != "$after" || pending[0].Shown || e.received("alice", "$after") {
		t.Fatalf("pending: %+v", pending)
	}
	if r := e.hook("alice", "prompt", `{"session_id":"first"}`).ok(t); r.stdout != "" {
		t.Fatalf("second sync rang: %+v", r)
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	out := e.as("alice", "inbox").ok(t).stdout
	if strings.Count(out, "FIRST_INSTRUCTION") != 1 || strings.Contains(out, "$before") || strings.Contains(out, "$own") || !e.received("alice", "$after") {
		t.Fatalf("inbox: %q", out)
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "inbox").ok(t)
	if len(e.mailbox("alice").Pending()) != 0 {
		t.Fatal("receipted initial event returned")
	}
	if e.hs.CallCount(messagesRoute) != 0 {
		t.Fatal("complete join timeline crawled history")
	}
}

// Limited first sync uses backward history and stops on the own join page,
// excluding older mail, another user's join, and a join-to-join profile update.
func TestFirstSyncLimitedStopsAtJoin(t *testing.T) {
	e, room := world(t)
	post := firstSyncMessage("$post", "@bob:test.local", ping(1))
	cut := firstSyncMessage("$cut", "@bob:test.local", ping(2))
	before := firstSyncMessage("$before", "@bob:test.local", ping(0))
	profile := firstSyncJoin("@alice:test.local", "$profile")
	profile.Unsigned = json.RawMessage(`{"prev_content":{"membership":"join"}}`)
	firstSyncFixture(t, e, room, true, "tail", post)
	e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {
		"tail":      {page: matrix.Page{Chunk: []matrix.Event{profile, firstSyncJoin("@bob:test.local", "$other"), cut}, End: "join-page"}},
		"join-page": {page: matrix.Page{Chunk: []matrix.Event{firstSyncJoin("@alice:test.local", "$join"), before}, End: "forbidden"}},
		"forbidden": {failure: &fakeFailure{status: 500, errcode: "M_UNKNOWN", message: "paged before join"}},
	}}
	out := e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(out, "id=$post") || !strings.Contains(out, "id=$cut") || strings.Index(out, "id=$cut") > strings.Index(out, "id=$post") || strings.Contains(out, "$before") || strings.Contains(out, "gap") {
		t.Fatalf("inbox: %q", out)
	}
	if n := e.hs.CallCount(messagesRoute); n != 2 {
		t.Fatalf("history requests=%d", n)
	}
	for _, raw := range e.hs.rawURLs {
		u, _ := url.Parse(raw)
		if strings.HasSuffix(u.Path, "/messages") && (u.Query().Get("dir") != "b" || u.Query().Get("limit") != "100" || u.Query().Get("to") != "") {
			t.Fatalf("history query: %s", raw)
		}
	}
}

// At the page/event bound an unproven tail becomes a durable, receiptable gap,
// while reaching the join on the last permitted page delivers its valid tail.
func TestFirstSyncHistoryBound(t *testing.T) {
	for _, found := range []bool{true, false} {
		t.Run(fmt.Sprint(found), func(t *testing.T) {
			e, room := world(t)
			e.extra[EnvSessionID] = "bound"
			firstSyncFixture(t, e, room, true, "p0", firstSyncMessage("$tail", "@bob:test.local", ping(1)))
			pages := map[string]fakeMessagePage{}
			for i := 0; i < firstSyncMaxPages; i++ {
				chunk := make([]matrix.Event, backfillPageSize)
				for j := range chunk {
					chunk[j] = firstSyncMessage(fmt.Sprintf("$noise-%d-%d", i, j), "@bob:test.local", msg("noise", nil))
				}
				if found && i == firstSyncMaxPages-1 {
					chunk[len(chunk)-1] = firstSyncJoin("@alice:test.local", "$join")
				}
				pages[fmt.Sprintf("p%d", i)] = fakeMessagePage{page: matrix.Page{Chunk: chunk, End: fmt.Sprintf("p%d", i+1)}}
			}
			e.hs.messagePages = map[string]map[string]fakeMessagePage{room: pages}
			bell := bellContext(t, e.hook("alice", "prompt", `{"session_id":"bound"}`).ok(t).stdout)
			pending := e.mailbox("alice").Pending()
			if len(pending) != 1 || e.hs.CallCount(messagesRoute) != firstSyncMaxPages {
				t.Fatalf("pending=%+v calls=%d", pending, e.hs.CallCount(messagesRoute))
			}
			if found {
				if pending[0].ID != "$tail" || !strings.Contains(bell, "1 new peer event") || strings.Contains(bell, "gap") {
					t.Fatalf("valid boundary: %+v %q", pending, bell)
				}
			} else {
				if pending[0].Kind != config.MailGap || pending[0].From != "own join" || pending[0].To == "" || !strings.Contains(bell, "1 gap") {
					t.Fatalf("missing gap: %+v %q", pending, bell)
				}
				out := e.as("alice", "inbox").ok(t).stdout
				if !strings.Contains(out, "gap:") || strings.Contains(out, "id=$tail") || !e.received("alice", pending[0].ID) {
					t.Fatalf("gap receipt: %q", out)
				}
			}
		})
	}
}

// Missing, stalled and oversized history cannot prove a join boundary. Each
// produces a gap without delivering the tail; the neighboring valid page can.
func TestFirstSyncHistoryRefusals(t *testing.T) {
	for _, mode := range []string{"valid", "complete-no-join", "missing-token", "exhausted", "stalled", "oversized", "oversized-tail"} {
		t.Run(mode, func(t *testing.T) {
			e, room := world(t)
			token := "tail"
			if mode == "missing-token" {
				token = ""
			}
			firstSyncFixture(t, e, room, true, token, firstSyncMessage("$tail", "@bob:test.local", ping(1)))
			jr := e.hs.initialDelivery.Rooms.Join[room]
			if mode == "complete-no-join" {
				jr.Timeline.Limited = false
			}
			if mode == "oversized-tail" {
				jr.Timeline.Events = make([]matrix.Event, backfillPageSize+1)
			}
			e.hs.initialDelivery.Rooms.Join[room] = jr
			page := matrix.Page{Chunk: []matrix.Event{firstSyncJoin("@alice:test.local", "$join")}}
			switch mode {
			case "exhausted":
				page.Chunk = nil
			case "stalled":
				page.Chunk = nil
				page.End = "tail"
			case "oversized":
				page.Chunk = make([]matrix.Event, backfillPageSize+1)
			}
			e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {"tail": {page: page}}}
			e.hook("alice", "prompt", `{"session_id":"refusal"}`).ok(t)
			pending := e.mailbox("alice").Pending()
			want := config.MailGap
			if mode == "valid" {
				want = config.MailEvent
			}
			if len(pending) != 1 || pending[0].Kind != want {
				t.Fatalf("pending: %+v", pending)
			}
		})
	}
}

// An existing cursor still consumes live traffic (including a rejoin) without
// the initial history crawl; a first sync after rejoin cuts at the latest join.
func TestFirstSyncExistingCursorAndRejoin(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			e, room := world(t)
			if existing {
				e.as("alice", "inbox").ok(t)
			}
			before := e.hs.inject(room, "@bob:test.local", ping(0))
			e.hs.mu.Lock()
			e.hs.appendLocked(room, "@alice:test.local", "m.room.member", map[string]string{"membership": "join"})
			e.hs.events[len(e.hs.events)-1].StateKey = new(string)
			*e.hs.events[len(e.hs.events)-1].StateKey = "@alice:test.local"
			e.hs.mu.Unlock()
			after := e.hs.inject(room, "@bob:test.local", ping(1))
			firstSyncFixture(t, e, room, true, "must-not-page",
				firstSyncJoin("@alice:test.local", "$old-join"), firstSyncMessage(before, "@bob:test.local", ping(0)), firstSyncJoin("@alice:test.local", "$rejoin"), firstSyncMessage(after, "@bob:test.local", ping(1)))
			out := e.as("alice", "inbox").ok(t).stdout
			if !strings.Contains(out, "id="+after) || strings.Contains(out, "id="+before) != existing || strings.Contains(out, "gap") || e.hs.CallCount(messagesRoute) != 0 {
				t.Fatalf("existing=%t output=%q", existing, out)
			}
		})
	}
}

// First-sync subscription mail follows the normal topic/mute and real-sender
// rules: subscribed control arrives, muted/unsubscribed and forged sender do not.
// Mandatory principal and direct-address delivery retain their normal bypasses.
func TestFirstSyncDeliveryRules(t *testing.T) {
	e, room := world(t)
	subID := "allowed-topic"
	e.hs.setAccountData("@alice:test.local", SubsType, SubList{V: 1, Subs: []Sub{
		{ID: subID, SubSelector: SubSelector{Room: room, Topic: "$allowed", Kinds: []string{"decision"}}, CreatedAt: legacyCreatedAt},
		{ID: "muted-topic", SubSelector: SubSelector{Room: room, Topic: "$muted", Kinds: []string{"decision"}}, CreatedAt: legacyCreatedAt},
		{ID: "sender", SubSelector: SubSelector{Room: room, From: "@bob:test.local"}, CreatedAt: legacyCreatedAt},
	}})
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Muted = []string{"$muted"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := ownerWrite(e.t.Context(), ownerFiles(e.paths(), "alice").path("principals.json"), principalConfig{V: 1, Principals: []string{"@principal:authority.invalid"}}); err != nil {
		t.Fatal(err)
	}
	events := []matrix.Event{firstSyncJoin("@alice:test.local", "$join")}
	for _, topic := range []string{"allowed", "muted", "unsubscribed"} {
		content := kindMsg("carol", "decision", "ambient decision")
		content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": "$" + topic}
		events = append(events, firstSyncMessage("$"+topic+"-reply", "@carol:test.local", content))
	}
	events = append(events, firstSyncMessage("$sender", "@bob:test.local", msg("plain sender control", nil)), firstSyncMessage("$forged", "@carol:test.local", msg("Signed: @bob:test.local", nil)), firstSyncMessage("$principal", "@principal:authority.invalid", msg("principal instruction", nil)), firstSyncMessage("$direct", "@carol:test.local", toMsg("carol", "question", "addressed")))
	firstSyncFixture(t, e, room, false, "", events...)
	e.hook("alice", "prompt", `{"session_id":"rules"}`).ok(t)
	pending := e.mailbox("alice").Pending()
	if len(pending) != 4 {
		t.Fatalf("pending: %+v", pending)
	}
	for _, id := range []string{"$allowed-reply", "$sender", "$principal", "$direct"} {
		if _, ok := e.pendingEntry("alice", id); !ok {
			t.Fatalf("missing %s", id)
		}
	}
	for _, id := range []string{"$muted-reply", "$unsubscribed-reply", "$forged"} {
		if _, ok := e.pendingEntry("alice", id); ok {
			t.Fatalf("forbidden %s", id)
		}
	}
	if en, _ := e.pendingEntry("alice", "$allowed-reply"); en.Sub != subID {
		t.Fatalf("subscription: %+v", en)
	}
}

// The history budget is shared across rooms, not reset per room: after one
// room exhausts it, another limited room gaps without HTTP, while a complete
// joined tail still supplies its valid control.
func TestFirstSyncHistoryBudgetShared(t *testing.T) {
	e, room := world(t)
	second := e.hs.addRoom("zz-limited", "@alice:test.local", "@bob:test.local")
	complete := e.hs.addRoom("zz-valid", "@alice:test.local", "@bob:test.local")
	firstSyncFixture(t, e, room, true, "p0", firstSyncMessage("$unproven", "@bob:test.local", ping(0)))
	resp := e.hs.initialDelivery
	jr := resp.Rooms.Join[room]
	jr.Timeline.PrevBatch = "forbidden"
	resp.Rooms.Join[second] = jr
	jr.Timeline.Limited = false
	jr.Timeline.Events = []matrix.Event{firstSyncJoin("@alice:test.local", "$join"), firstSyncMessage("$valid", "@bob:test.local", ping(1))}
	resp.Rooms.Join[complete] = jr
	pages := map[string]fakeMessagePage{}
	for i := 0; i < firstSyncMaxPages; i++ {
		pages[fmt.Sprintf("p%d", i)] = fakeMessagePage{page: matrix.Page{End: fmt.Sprintf("p%d", i+1)}}
	}
	e.hs.messagePages = map[string]map[string]fakeMessagePage{room: pages, second: {"forbidden": {failure: &fakeFailure{status: 500, errcode: "M_UNKNOWN", message: "budget reset"}}}}
	e.hook("alice", "prompt", `{"session_id":"shared"}`).ok(t)
	pending := e.mailbox("alice").Pending()
	if len(pending) != 3 || e.hs.CallCount(messagesRoute) != firstSyncMaxPages {
		t.Fatalf("pending=%+v calls=%d", pending, e.hs.CallCount(messagesRoute))
	}
	gaps := 0
	for _, en := range pending {
		if en.Kind == config.MailGap {
			gaps++
		}
	}
	if _, ok := e.pendingEntry("alice", "$valid"); !ok || gaps != 2 {
		t.Fatalf("shared budget: %+v", pending)
	}
}

// Cancellation reaches both history HTTP and redaction corroboration before
// the cursor/mailbox changes; nearby uninterrupted controls deliver normally.
// The transport inspects the real request context, with no timing sleeps.
func TestFirstSyncHTTPCancellation(t *testing.T) {
	for _, stage := range []string{"history", "redaction"} {
		for _, interrupt := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/%t", stage, interrupt), func(t *testing.T) {
				e, room := world(t)
				valid := firstSyncMessage("$valid", "@bob:test.local", ping(1))
				path := "/messages"
				if stage == "history" {
					firstSyncFixture(t, e, room, true, "tail", valid)
					e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {"tail": {page: matrix.Page{Chunk: []matrix.Event{firstSyncJoin("@alice:test.local", "$join")}}}}}
				} else {
					target := e.hs.inject(room, "@bob:test.local", msg("target", nil))
					path = "/event/" + target
					red := matrix.Event{EventID: "$redaction", Type: "m.room.redaction", Sender: "@carol:test.local", Redacts: target, Content: json.RawMessage(`{}`)}
					firstSyncFixture(t, e, room, false, "", firstSyncJoin("@alice:test.local", "$join"), red, valid)
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				requests := 0
				e.configure = func(a *App) {
					base := a.HTTP.Transport
					if base == nil {
						base = http.DefaultTransport
					}
					a.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, path) {
							requests++
							deadline, ok := r.Context().Deadline()
							if !ok || time.Until(deadline) > firstSyncTimeout {
								t.Fatal("first-sync HTTP has no bounded deadline")
							}
							if interrupt {
								cancel()
								select {
								case <-r.Context().Done():
								default:
									t.Fatal("request escaped first-sync cancellation")
								}
								return nil, context.Canceled
							}
						}
						return base.RoundTrip(r)
					})}
				}
				r := e.runCtx(ctx, "--as", "alice", "inbox")
				if requests == 0 {
					t.Fatal("HTTP path was not exercised")
				}
				if interrupt {
					if r.code != ExitError || e.state("alice").NextBatch != "" || len(e.mailbox("alice").Pending()) != 0 || e.received("alice", "$valid") {
						t.Fatalf("partial baseline after cancellation: %+v", r)
					}
				} else if r.code != ExitOK || !strings.Contains(r.stdout, "id=$valid") || !e.received("alice", "$valid") || e.state("alice").NextBatch == "" {
					t.Fatalf("valid HTTP control: %+v", r)
				}
			})
		}
	}
}

// First-sync recovery honors wait's existing --all diagnostic rule, while the
// neighboring ordinary wait excludes unaddressed, unsubscribed post-join mail.
func TestFirstSyncWaitAll(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprint(all), func(t *testing.T) {
			e, room := world(t)
			firstSyncFixture(t, e, room, false, "", firstSyncJoin("@alice:test.local", "$join"), firstSyncMessage("$ambient", "@bob:test.local", msg("ambient", nil)))
			args := []string{"wait", "--room", "proj", "--timeout", "1ms"}
			if all {
				args = append(args, "--all")
			}
			out := e.as("alice", args...).ok(t).stdout
			if strings.Contains(out, "$ambient") != all || len(e.mailbox("alice").Pending()) != map[bool]int{false: 0, true: 1}[all] || e.received("alice", "$ambient") {
				t.Fatalf("all=%t output=%q", all, out)
			}
		})
	}
}
