package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The marker (none/body/header) x relation (none/native/thread) cross-product
// keeps marked non-thread targets as roots, and m.thread takes precedence over
// markers. Only --reply-main opts roots/children out of thread routing.
// All lanes preserve addressing, metadata and the direct reply target.
func TestPostReplyMainRouting(t *testing.T) {
	for _, target := range []string{"plain", "native-reply", "body-root", "body-native", "header-root", "header-native", "child", "body-child", "header-child"} {
		for _, forceMain := range []bool{false, true} {
			t.Run(target+"/reply-main="+map[bool]string{false: "false", true: "true"}[forceMain], func(t *testing.T) {
				e, room := world(t)
				root := strings.TrimSpace(e.as("alice", "topic", "new", room, "alpha", "A").ok(t).stdout)
				content := msg("plain target", nil)
				if strings.HasPrefix(target, "body-") {
					content["body"] = "[topic] alpha: A"
				} else if strings.HasPrefix(target, "header-") {
					content[ContentKey] = Header{Type: "topic", Slug: "alpha", Title: "A"}
				}
				if strings.Contains(target, "native") {
					content["m.relates_to"] = map[string]any{"m.in_reply_to": map[string]any{"event_id": "$not-followed"}}
				} else if strings.Contains(target, "child") {
					content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": root}
				}
				re := e.hs.inject(room, "@human:test.local", content)
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
				if !forceMain {
					if strings.Contains(target, "child") {
						wantRoot = root
					} else if strings.HasPrefix(target, "body-") || strings.HasPrefix(target, "header-") {
						wantRoot = re
					}
				}
				wantReads := 1
				if wantRoot != "" && strings.Contains(target, "child") {
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
				wantThreads := []string(nil)
				if wantRoot != "" {
					wantThreads = []string{wantRoot}
				}
				if err != nil || !reflect.DeepEqual(st.Threads, wantThreads) {
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

// Both routing modes reject a self-cycle before PUT or state changes. A valid
// child succeeds in both modes; --reply-main reads only the child, never its root.
func TestPostReplyMainSelfCycle(t *testing.T) {
	for _, forceMain := range []bool{false, true} {
		for _, cyclic := range []bool{true, false} {
			name := "valid-child"
			if cyclic {
				name = "self-cycle"
			}
			t.Run(name+"/reply-main="+map[bool]string{false: "false", true: "true"}[forceMain], func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				before, err := config.LoadState(f.paths("alpha", false), "alice")
				if err != nil {
					t.Fatal(err)
				}
				gets, sends := 0, 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					body := ""
					switch {
					case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/!room:fixture/event/$target":
						gets++
						root := "$root"
						if cyclic {
							root = "$target"
						}
						body = `{"event_id":"$target","room_id":"!room:fixture","sender":"@bob:fixture","origin_server_ts":1000,"unsigned":{},"type":"m.room.message","content":{"msgtype":"m.text","body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"` + root + `"}}}`
					case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/!room:fixture/event/$root":
						if forceMain || cyclic {
							t.Fatal("unexpected root re-read")
						}
						gets++
						body = `{"event_id":"$root","room_id":"!room:fixture","sender":"@bob:fixture","origin_server_ts":999,"unsigned":{},"type":"m.room.message","content":{"msgtype":"m.text","body":"[topic] alpha: A"}}`
					case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/send/m.room.message/"):
						sends++
						var content map[string]any
						if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
							t.Fatal(err)
						}
						want := map[string]any{"m.in_reply_to": map[string]any{"event_id": "$target"}}
						if !forceMain {
							want["rel_type"], want["event_id"] = "m.thread", "$root"
						}
						if !reflect.DeepEqual(content["m.relates_to"], want) {
							t.Fatalf("relation=%#v, want %#v", content["m.relates_to"], want)
						}
						body = `{"event_id":"$sent"}`
					default:
						t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					return waitResponse(r, http.StatusOK, body)
				})}
				args := []string{"--as", "alice", "post", "!room:fixture", "--re", "$target", "answer"}
				if forceMain {
					args = append(args, "--reply-main")
				}
				code := a.Run(context.Background(), args)
				wantReads := 1
				if !cyclic && !forceMain {
					wantReads = 2
				}
				if gets != wantReads {
					t.Fatalf("GET count=%d, want %d", gets, wantReads)
				}
				if cyclic {
					after, err := config.LoadState(f.paths("alpha", false), "alice")
					if code != ExitError || sends != 0 || f.out.Len() != 0 || !strings.Contains(f.err.String(), "cyclic thread relation") ||
						err != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("cycle refusal failed: code=%d sends=%d err=%s", code, sends, f.err.String())
					}
				} else if code != ExitOK || sends != 1 || strings.TrimSpace(f.out.String()) != "$sent" {
					t.Fatalf("valid control failed: code=%d sends=%d err=%s", code, sends, f.err.String())
				}
			})
		}
	}
}

// Wait learns same-room membership from a real thread child. Once muted, a
// plain reply to that child creates no mail/receipt, while an addressed or
// mentioned plain reply in the same sync still rings and reaches the mailbox.
func TestPostReplyMainMutedLearnedMemberWait(t *testing.T) {
	for _, control := range []string{"addressed", "mention"} {
		t.Run(control, func(t *testing.T) {
			e, room := subWorld(t)
			root := strings.TrimSpace(e.as("alice", "topic", "new", room, "alpha", "A").ok(t).stdout)
			e.subscribe("alice", "--room", room, "--kind", "notice")
			// The fake's first sync returns a cursor without message history.
			// Establish it before posting the child so wait ingests the event.
			_, s := notificationApp(t, e)
			if _, err := s.takePosition(context.Background(), time.Time{}); err != nil {
				t.Fatal(err)
			}
			member := strings.TrimSpace(e.as("bob", "post", room, "--topic", root, "member").ok(t).stdout)
			if out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout; !strings.Contains(out, member) {
				t.Fatalf("unmuted member did not ring: %q", out)
			}
			if st := e.state("alice"); st.ThreadEventRoot(room, member) != root {
				t.Fatal("wait did not persist same-room membership")
			}
			e.as("alice", "mute", room, "--topic", root).ok(t)
			cursor := e.state("alice").NextBatch
			suppressed := strings.TrimSpace(e.as("bob", "post", room, "--re", member, "--reply-main", "plain reply").ok(t).stdout)
			args := []string{"post", room, "--re", member, "--reply-main"}
			wantRel := "mention"
			if control == "addressed" {
				args = append(args, "--to", "alice", "addressed reply")
				wantRel = "to"
			} else {
				args = append(args, "ping @alice:test.local mentioned reply")
			}
			ringing := strings.TrimSpace(e.as("bob", args...).ok(t).stdout)
			out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
			if !strings.HasPrefix(out, wantRel+" #proj "+ringing+" ") || strings.Count(out, "\n") != 1 {
				t.Fatalf("muted ordinary reply rang or control was lost: %q", out)
			}
			if _, ok := e.pendingEntry("alice", suppressed); ok || e.received("alice", suppressed) {
				t.Fatal("muted reply created mail or a receipt")
			}
			if entry, ok := e.pendingEntry("alice", ringing); !ok || entry.Rel != wantRel || e.received("alice", ringing) {
				t.Fatal("address/mention control did not remain pending with its relevance")
			}
			if st := e.state("alice"); !st.IsMuted(root) || st.NextBatch == cursor {
				t.Fatal("wait lost the mute or did not advance past suppressed mail")
			}
		})
	}
}
