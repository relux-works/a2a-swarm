package cli

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Plain targets (including native replies) emit only m.in_reply_to; roots and
// server-thread children retain m.thread unless --reply-main explicitly opts out.
// All lanes preserve addressing, metadata and the direct reply target.
func TestPostReplyMainRouting(t *testing.T) {
	for _, target := range []string{"plain", "native-reply", "root", "child"} {
		for _, forceMain := range []bool{false, true} {
			t.Run(target+"/reply-main="+map[bool]string{false: "false", true: "true"}[forceMain], func(t *testing.T) {
				e, room := world(t)
				root := strings.TrimSpace(e.as("alice", "topic", "new", room, "alpha", "A").ok(t).stdout)
				child := strings.TrimSpace(e.as("alice", "post", room, "--topic", root, "child").ok(t).stdout)
				plain := e.hs.inject(room, "@human:test.local", msg("human question", nil))
				native := e.hs.inject(room, "@human:test.local", msg("native reply", map[string]any{
					"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": plain}},
				}))
				re := map[string]string{"plain": plain, "native-reply": native, "root": root, "child": child}[target]
				args := []string{"post", room, "--re", re, "--to", "@alice:test.local", "--kind", "question",
					"--reply-by", "2026-11-09T12:00:00+02:00", "--trunk", "abcdef1", "--subject", "PR#331", "answer"}
				if forceMain {
					args = append(args, "--reply-main")
				}
				get, send := "GET /_matrix/client/v3/rooms/{room}/event/{event}", "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
				beforeGet, beforeSend := e.hs.CallCount(get), e.hs.CallCount(send)
				id := strings.TrimSpace(e.as("bob", args...).ok(t).stdout)
				ev, exists := e.hs.eventByID(id)
				if !exists || ev.RoomID != room || e.hs.CallCount(send)-beforeSend != 1 {
					t.Fatal("reply was not exactly one send in the selected room")
				}
				wantRoot := ""
				if !forceMain && (target == "root" || target == "child") {
					wantRoot = root
				}
				wantReads := 1
				if wantRoot != "" && target == "child" {
					wantReads = 2
				}
				if e.hs.CallCount(get)-beforeGet != wantReads {
					t.Fatalf("reply read count: got %d, want %d", e.hs.CallCount(get)-beforeGet, wantReads)
				}
				wantRelation := map[string]any{"m.in_reply_to": map[string]any{"event_id": re}}
				if wantRoot != "" {
					wantRelation["rel_type"], wantRelation["event_id"] = "m.thread", wantRoot
				}
				if got := decodeContent(t, ev)["m.relates_to"]; !reflect.DeepEqual(got, wantRelation) {
					t.Fatalf("relation = %#v, want %#v", got, wantRelation)
				}
				m := ev.Message()
				h, ok := decodeHeader(m.A2A)
				wantSlug := ""
				if wantRoot != "" {
					wantSlug = "alpha"
				}
				if !ok || h.Topic != wantRoot || h.Slug != wantSlug || h.Re != re || h.Kind != "question" || h.ReplyBy != "2026-11-09T10:00:00Z" ||
					h.Trunk != "abcdef1" || h.Subject != "PR#331" || !reflect.DeepEqual(h.To, []string{"alice"}) ||
					m.Mentions == nil || !reflect.DeepEqual(m.Mentions.UserIDs, []string{"@alice:test.local"}) ||
					!strings.Contains(m.Body, "- re: "+re+"\n") {
					t.Fatalf("reply lost metadata: %+v", h)
				}
				st, err := config.LoadState(e.paths(), "bob")
				if err != nil || st.HasThread(root) != (wantRoot != "") || st.HasThread(plain) || st.HasThread(native) {
					t.Fatalf("reply recorded incorrect participation: %+v, %v", st, err)
				}
			})
		}
	}
}

// Missing/foreign targets and unthreaded header topic claims are refused without
// PUT or state changes in both modes. A plain same-room reply succeeds after each.
func TestPostReplyMainTargetRefusals(t *testing.T) {
	for _, forceMain := range []bool{false, true} {
		for _, target := range []string{"missing", "foreign", "header-claim"} {
			t.Run(target+"/reply-main="+map[bool]string{false: "false", true: "true"}[forceMain], func(t *testing.T) {
				e, room := world(t)
				plain := e.hs.inject(room, "@human:test.local", msg("plain", nil))
				foreign := e.hs.inject("!elsewhere:test.local", "@human:test.local", msg("foreign", nil))
				claim := e.hs.inject(room, "@human:test.local", msg("claim", map[string]any{
					ContentKey: Header{Type: "message", Topic: "$unrelated"},
				}))
				re := map[string]string{"missing": "$missing", "foreign": foreign, "header-claim": claim}[target]
				beforeState, err := config.LoadState(e.paths(), "bob")
				if err != nil {
					t.Fatal(err)
				}
				send := "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
				beforeSend := e.hs.CallCount(send)
				args := []string{"post", room, "--re", re, "answer"}
				if forceMain {
					args = append(args, "--reply-main")
				}
				wantError := "M_NOT_FOUND"
				if target == "header-claim" {
					wantError = "header topic claim"
				}
				r := e.as("bob", args...).failsWith(t, ExitError, wantError)
				afterState, err := config.LoadState(e.paths(), "bob")
				if r.stdout != "" || e.hs.CallCount(send) != beforeSend || err != nil || !reflect.DeepEqual(beforeState, afterState) {
					t.Fatal("refused reply sent or changed participation")
				}
				e.as("bob", "post", room, "--re", plain, "valid").ok(t)
				if e.hs.CallCount(send)-beforeSend != 1 {
					t.Fatal("plain same-room control did not send")
				}
			})
		}
	}
}

// Conflicting topic/main selection and --reply-main without a target are usage
// errors before any HTTP. The same CLI accepts --reply-main with a valid target.
func TestPostReplyMainUsage(t *testing.T) {
	for _, flags := range [][]string{
		{"--topic", "$root", "--re", "$target", "--reply-main"},
		{"--topic=", "--re", "$target", "--reply-main"},
		{"--topic", "$root", "--reply-main"},
		{"--reply-main"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			requests := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				requests++
				t.Fatalf("usage error reached HTTP: %s %s", r.Method, r.URL.Path)
				return nil, nil
			})}
			args := append([]string{"--as", "alice", "post", "!room:fixture", "answer"}, flags...)
			if code := a.Run(context.Background(), args); code != ExitUsage || requests != 0 || f.out.Len() != 0 ||
				!strings.Contains(f.err.String(), "--reply-main") {
				t.Fatalf("usage not refused locally: code=%d err=%s", code, f.err.String())
			}
			e, room := world(t)
			plain := e.hs.inject(room, "@human:test.local", msg("plain", nil))
			e.as("bob", "post", room, "--re", plain, "--reply-main", "valid").ok(t)
		})
	}
}

// A generated main reply to a plain event is ordinary room mail even with an
// unrelated followed thread and cross-room membership. Same-room learned
// membership is the positive control: only it enables plain-reply thread delivery.
func TestPostReplyMainDelivery(t *testing.T) {
	e, room := world(t)
	plain := e.hs.inject(room, "@human:test.local", msg("human question", nil))
	id := strings.TrimSpace(e.as("bob", "post", room, "--re", plain, "answer").ok(t).stdout)
	ev, exists := e.hs.eventByID(id)
	if !exists {
		t.Fatal("main reply not sent")
	}
	for _, knownRoom := range []string{"", "!other:test.local", room} {
		t.Run("known-room="+knownRoom, func(t *testing.T) {
			st := config.State{Threads: []string{"$unrelated", plain}}
			if knownRoom != "" {
				st.RememberThreadEvent(config.ThreadEvent{Room: knownRoom, EventID: plain, Root: "$unrelated"})
			}
			wantRoot := ""
			wantRel := relevance("")
			if knownRoom == room {
				wantRoot, wantRel = "$unrelated", relThread
			}
			if root := deliveryTopicRoot(ev, room, st); root != wantRoot {
				t.Fatalf("delivery root=%q, want %q", root, wantRoot)
			}
			rel, learned, matched := classify(ev, room, "@alice:test.local", "alice", st, nil)
			if rel != wantRel || learned != "" || len(matched) != 0 {
				t.Fatalf("thread delivery = %q, %q, %v", rel, learned, matched)
			}
			if knownRoom != room {
				// A room subscription still delivers ordinary main-timeline mail.
				subs := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, AllEvents: true}}}
				rel, learned, matched = classify(ev, room, "@alice:test.local", "alice", st, subs)
				if rel != relSub || learned != "" || !reflect.DeepEqual(matched, []string{"room"}) {
					t.Fatal("ordinary room subscription did not deliver main reply")
				}
				// Topic subscriptions cannot manufacture membership either.
				subs = []Sub{{ID: "topic", SubSelector: SubSelector{Room: room, Topic: "$unrelated"}}}
				rel, _, matched = classify(ev, room, "@alice:test.local", "alice", st, subs)
				if rel != "" || len(matched) != 0 {
					t.Fatal("main reply matched an unrelated topic subscription")
				}
			}
		})
	}
}
