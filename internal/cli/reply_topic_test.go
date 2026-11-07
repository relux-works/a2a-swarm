package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// These tests enter through App.Run, with synthetic authenticated HTTP and
// recorded requests, rather than calling the resolver directly.
func TestPostReplyTopicCLI(t *testing.T) {
	for _, target := range []string{"root", "child"} {
		for _, topic := range []string{"", "id", "slug"} {
			t.Run(target+"/"+topic, func(t *testing.T) {
				e, room := world(t)
				root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
				child := strings.TrimSpace(e.as("alice", "post", room, "--topic", root, "question").ok(t).stdout)
				re := root
				wantReads := 1
				if target == "child" {
					re = child
					wantReads = 2
				}
				args := []string{"post", room, "--re", re, "--kind", "answer", "because"}
				if topic == "id" {
					args = append(args, "--topic", root)
				} else if topic == "slug" {
					args = append(args, "--topic", "alpha")
				}
				get := "GET /_matrix/client/v3/rooms/{room}/event/{event}"
				send := "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
				beforeGet, beforeSend := e.hs.CallCount(get), e.hs.CallCount(send)
				beforeThreads := e.hs.CallCount("GET /_matrix/client/v1/rooms/{room}/threads")
				id := strings.TrimSpace(e.as("bob", args...).ok(t).stdout)
				ev, ok := e.hs.eventByID(id)
				if !ok || ev.RoomID != room || e.hs.CallCount(send)-beforeSend != 1 || e.hs.CallCount(get)-beforeGet != wantReads {
					t.Fatal("reply did not use exactly the selected room, bounded reads and one send")
				}
				if topic != "slug" && e.hs.CallCount("GET /_matrix/client/v1/rooms/{room}/threads") != beforeThreads {
					t.Fatal("reply unnecessarily scanned topics")
				}
				m := ev.Message()
				h, _ := decodeHeader(m.A2A)
				if h.Topic != root || h.Slug != "alpha" || h.Re != re || h.Kind != "answer" ||
					m.RelatesTo == nil || m.RelatesTo.EventID != root || m.RelatesTo.RelType != "m.thread" ||
					m.RelatesTo.IsFallingBack || m.RelatesTo.InReplyTo == nil || m.RelatesTo.InReplyTo.EventID != re {
					t.Fatal("reply lost its actual root or direct reply target")
				}
				st, err := config.LoadState(e.paths(), "bob")
				if err != nil || !st.HasThread(root) {
					t.Fatal("actual root not remembered")
				}
			})
		}
	}
}

func TestPostReplyTopicRoomAndConflict(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	beta := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "beta", "B").ok(t).stdout)
	child := strings.TrimSpace(e.as("alice", "post", room, "--topic", root, "question").ok(t).stdout)
	foreign := strings.TrimSpace(e.as("alice", "topic", "new", "lobby", "alpha", "A").ok(t).stdout)
	plain := e.hs.inject(room, "@alice:test.local", map[string]any{"msgtype": "m.text", "body": "plain; claims topic " + root,
		ContentKey: Header{Type: "message", Topic: root}})
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"missing", []string{"--re", "$missing"}, ExitError},
		{"wrong-room", []string{"--re", foreign}, ExitError},
		{"unthreaded", []string{"--re", plain}, ExitError},
		{"topic-id-conflict", []string{"--re", child, "--topic", beta}, ExitError},
		{"topic-slug-conflict", []string{"--re", child, "--topic", "beta"}, ExitError},
		{"topic-child", []string{"--re", child, "--topic", child}, ExitError},
		{"topic-missing", []string{"--re", child, "--topic", "$missing"}, ExitError},
		{"invalid-id", []string{"--re", "not-an-event"}, ExitUsage},
		{"empty-id", []string{"--re", "$"}, ExitUsage},
		{"space-id", []string{"--re", "$not an event"}, ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			send := "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
			before := e.hs.CallCount(send)
			get := "GET /_matrix/client/v3/rooms/{room}/event/{event}"
			directory := "GET /_matrix/client/v3/directory/room/{alias}"
			beforeGet, beforeDirectory := e.hs.CallCount(get), e.hs.CallCount(directory)
			args := append([]string{"post", room}, tc.args...)
			args = append(args, "refuse")
			r := e.as("bob", args...)
			if r.code != tc.code || r.stdout != "" || e.hs.CallCount(send) != before {
				t.Fatalf("refusal failed: exit=%d stdout=%q", r.code, r.stdout)
			}
			// Readable cross-topic conflicts retain refusal and explain citation.
			if strings.HasPrefix(tc.name, "topic-") && strings.HasSuffix(tc.name, "-conflict") &&
				(!strings.Contains(r.stderr, "--re target does not belong to --topic") ||
					!strings.Contains(r.stderr, "omit --re") || !strings.Contains(r.stderr, "message body")) {
				t.Fatal("cross-topic refusal lost citation guidance")
			}
			if tc.code == ExitUsage && (e.hs.CallCount(get) != beforeGet ||
				e.hs.CallCount(directory) != beforeDirectory) {
				t.Fatal("invalid reply arguments reached HTTP")
			}
			// Adjacent valid reply: a refusal must not poison subsequent routing.
			e.as("bob", "post", room, "--topic", root, "--re", child, "valid").ok(t)
		})
	}
}

// A synthetic transport can return malformed and mismatched entities that a
// well-behaved homeserver would never send. Every request is bound to the chosen
// authenticated origin, exact full room ID, method and event path.
func TestPostReplyTopicWireRefusals(t *testing.T) {
	rootContent := `{"msgtype":"m.text","body":"[topic] alpha: A"}`
	childContent := `{"msgtype":"m.text","body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`
	cases := []struct {
		name, target, root, envelope string
		maxGets                      int
	}{
		{"valid", childContent, rootContent, "", 2},
		{"valid-root", rootContent, "", "", 1},
		{"valid-no-room-field", childContent, rootContent, "no-room", 2},
		{"valid-ordinary-root", childContent, `{"msgtype":"m.text","body":"ordinary root"}`, "", 2},
		{"wrong-id", childContent, rootContent, "wrong-id", 1},
		{"missing-event-id", childContent, rootContent, "missing-id", 1},
		{"wrong-full-room", childContent, rootContent, "wrong-room", 1},
		{"room-alias", childContent, rootContent, "alias-room", 1},
		{"room-null", childContent, rootContent, "null-room", 1},
		{"wrong-type", childContent, rootContent, "wrong-type", 1},
		{"missing", childContent, rootContent, "missing", 1},
		{"forbidden", childContent, rootContent, "forbidden", 1},
		{"redirect", childContent, rootContent, "redirect", 1},
		{"unbound-response", childContent, rootContent, "unbound", 1},
		{"other-response-path", childContent, rootContent, "other-path", 1},
		{"redacted", childContent, rootContent, "redacted", 1},
		{"duplicate-relation", `{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$root","event_id":"$other"}}`, rootContent, "", 1},
		{"case-alias-relation", `{"body":"child","M.RELATES_TO":{"rel_type":"m.thread","event_id":"$root"}}`, rootContent, "", 1},
		{"case-alias-id", `{"body":"child","m.relates_to":{"rel_type":"m.thread","EVENT_ID":"$root"}}`, rootContent, "", 1},
		{"relation-null", `{"body":"child","m.relates_to":null}`, rootContent, "", 1},
		{"relation-string", `{"body":"child","m.relates_to":"$root"}`, rootContent, "", 1},
		{"fallback-null", `{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$root","is_falling_back":null}}`, rootContent, "", 1},
		{"relation-empty", `{"body":"child","m.relates_to":{}}`, rootContent, "", 1},
		{"relation-missing-root", `{"body":"child","m.relates_to":{"rel_type":"m.thread"}}`, rootContent, "", 1},
		{"relation-invalid-root", `{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"root"}}`, rootContent, "", 1},
		{"relation-invalid-kind", `{"body":"child","m.relates_to":{"rel_type":"m.replace","event_id":"$root"}}`, rootContent, "", 1},
		{"ordinary-reply", `{"body":"child","m.relates_to":{"m.in_reply_to":{"event_id":"$root"}}}`, rootContent, "", 1},
		{"invalid-in-reply-to", `{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$root","m.in_reply_to":{}}}`, rootContent, "", 1},
		{"self-cycle", `{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$target"}}`, rootContent, "", 1},
		{"two-cycle", childContent, `{"body":"root","m.relates_to":{"rel_type":"m.thread","event_id":"$target"}}`, "", 2},
		{"chain", childContent, `{"body":"root","m.relates_to":{"rel_type":"m.thread","event_id":"$third"}}`, "", 2},
		{"root-wrong-room", childContent, rootContent, "root-wrong-room", 2},
		{"root-wrong-id", childContent, rootContent, "root-wrong-id", 2},
		// A validated child's relation identifies a root hidden by history access.
		{"valid-root-missing", childContent, rootContent, "root-missing", 2},
		{"unthreaded-header-claim", `{"body":"$root","ai.relux.a2a":{"type":"message","topic":"$root"}}`, rootContent, "", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			gets, sends := 0, 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL ||
					r.Header.Get("Authorization") != "Bearer isolated-alpha" || r.URL.RawQuery != "" {
					t.Fatal("request escaped selected authenticated connection")
				}
				status, body := 200, ""
				switch {
				case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/"):
					gets++
					id := strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/")
					if id != "$target" && id != "$root" {
						t.Fatal("followed an extra relation")
					}
					content := tc.target
					if id == "$root" {
						content = tc.root
					}
					ev := map[string]any{"event_id": id, "room_id": "!room:fixture", "sender": "@peer:remote.invalid",
						"type": "m.room.message", "content": json.RawMessage(content)}
					switch tc.envelope {
					case "wrong-id":
						ev["event_id"] = "$wrong"
					case "missing-id":
						delete(ev, "event_id")
					case "wrong-room":
						ev["room_id"] = "!room:other.invalid"
					case "alias-room":
						ev["room_id"] = "#room:fixture"
					case "null-room":
						ev["room_id"] = nil
					case "no-room":
						delete(ev, "room_id")
					case "wrong-type":
						ev["type"] = "m.room.member"
					case "redacted":
						ev["unsigned"] = map[string]any{"redacted_because": map[string]any{}}
					case "root-wrong-room":
						if id == "$root" {
							ev["room_id"] = "!room:elsewhere.invalid"
						}
					case "root-wrong-id":
						if id == "$root" {
							ev["event_id"] = "$elsewhere"
						}
					}
					if tc.envelope == "missing" || (tc.envelope == "root-missing" && id == "$root") {
						status, body = 404, `{"errcode":"M_NOT_FOUND","error":"missing"}`
					} else if tc.envelope == "forbidden" {
						status, body = 403, `{"errcode":"M_FORBIDDEN","error":"refused"}`
					} else if tc.envelope == "redirect" {
						status, body = 302, "{}"
					} else {
						data, err := json.Marshal(ev)
						if err != nil {
							t.Fatal(err)
						}
						body = string(data)
					}
				case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/send/m.room.message/"):
					sends++
					var content matrix.MessageContent
					if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
						t.Fatal(err)
					}
					wantRoot := "$root"
					if tc.name == "valid-root" {
						wantRoot = "$target"
					}
					h, _ := decodeHeader(content.A2A)
					if h.Topic != wantRoot || h.Re != "$target" || content.RelatesTo == nil ||
						content.RelatesTo.EventID != wantRoot || content.RelatesTo.InReplyTo == nil ||
						content.RelatesTo.InReplyTo.EventID != "$target" {
						t.Fatal("wrong wire thread")
					}
					body = `{"event_id":"$sent"}`
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				responseRequest := r
				if tc.envelope == "unbound" {
					responseRequest = nil
				}
				if tc.envelope == "other-path" {
					responseRequest = r.Clone(r.Context())
					u := *r.URL
					u.Path += "/other"
					responseRequest.URL = &u
				}
				return &http.Response{StatusCode: status, Request: responseRequest, Header: make(http.Header),
					ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			code := a.Run(context.Background(), []string{"--as", "alice", "post", "!room:fixture", "--re", "$target", "answer"})
			valid := strings.HasPrefix(tc.name, "valid")
			if valid && (code != ExitOK || sends != 1 || strings.TrimSpace(f.out.String()) != "$sent") {
				t.Fatalf("valid reply failed: code=%d sends=%d err=%s", code, sends, f.err.String())
			}
			if !valid && (code == ExitOK || sends != 0 || f.out.Len() != 0) {
				t.Fatalf("invalid reply sent: code=%d sends=%d", code, sends)
			}
			if gets != tc.maxGets {
				t.Fatalf("GET count %d, want %d", gets, tc.maxGets)
			}
		})
	}
}

func TestTopicNewAddressedCLI(t *testing.T) {
	for _, tc := range []struct {
		name, kind      string
		to              []string
		names, mentions []string
	}{
		{"defaults", "", nil, []string{"all"}, nil},
		{"addressed-question", "question", []string{"bob,,@other:remote.invalid", "all,bob"}, []string{"bob", "@other:remote.invalid", "all", "bob"}, []string{"@bob:test.local", "@other:remote.invalid", "@bob:test.local"}},
		{"empty-addresses", "request", []string{",,", ""}, []string{"all"}, nil},
		{"system", "system", []string{"bob"}, []string{"bob"}, []string{"@bob:test.local"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			e.now = func() time.Time { return now }
			send := "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
			before := e.hs.CallCount(send)
			args := []string{"topic", "new", room, "alpha", "A", "--body", "opener"}
			if tc.kind != "" {
				args = append(args, "--kind", tc.kind)
			}
			for _, to := range tc.to {
				args = append(args, "--to", to)
			}
			id := strings.TrimSpace(e.as("alice", args...).ok(t).stdout)
			ev, exists := e.hs.eventByID(id)
			if !exists || e.hs.CallCount(send)-before != 1 || ev.RoomID != room {
				t.Fatal("opener was not exactly one event")
			}
			m := ev.Message()
			h, ok := decodeHeader(m.A2A)
			wantKind := tc.kind
			if wantKind == "" {
				wantKind = "notice"
			}
			wantReply := ""
			if wantKind == "question" {
				wantReply = "2026-01-02T15:04:05Z"
			}
			if !ok || h.Type != "topic" || h.Slug != "alpha" || h.Title != "A" || h.Kind != wantKind ||
				h.From != "alice" || h.At != "2026-01-02T03:04:05Z" || h.ReplyBy != wantReply ||
				!reflect.DeepEqual(h.To, tc.names) || h.Topic != "" || h.Re != "" ||
				m.RelatesTo != nil || m.Mentions == nil || !reflect.DeepEqual(m.Mentions.UserIDs, tc.mentions) ||
				m.Body != "[topic] alpha: A\n\nopener" {
				t.Fatalf("opener contract changed: header=%+v mentions=%+v", h, m.Mentions)
			}
		})
	}
}

// Invalid new arguments must refuse before even resolving the room. Paired
// post invocations exercise the same existing validation and parsing contract.
func TestTopicNewAddressedRefusesBeforeHTTP(t *testing.T) {
	for _, flags := range [][]string{
		{"--kind", "gossip"}, {"--kind", ""}, {"--to", "bad user"},
		{"--to", "bob", "--to", "bad user"}, {"--to", "@bad user:server"},
	} {
		t.Run(strings.Join(flags, "/"), func(t *testing.T) {
			for _, command := range []string{"topic", "post"} {
				e, _ := world(t)
				calls := 0
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						t.Fatal("invalid arguments reached HTTP")
						return nil, nil
					})}
				}
				args := []string{"topic", "new", "proj", "alpha", "A"}
				if command == "post" {
					args = []string{"post", "proj", "--topic", "$root", "body"}
				}
				r := e.as("alice", append(args, flags...)...)
				if r.code != ExitUsage || calls != 0 || r.stdout != "" {
					t.Fatalf("invalid %s accepted", command)
				}
			}
		})
	}
}

// A selected connection changing after a valid observation must be caught
// before the next root read or send, including when only one read is needed.
func TestPostReplyTopicConnectionChange(t *testing.T) {
	for _, child := range []bool{false, true} {
		name := "root"
		if child {
			name = "child"
		}
		t.Run(name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 || r.Method != "GET" ||
					r.URL.Path != "/_matrix/client/v3/rooms/!room:fixture/event/$target" ||
					r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL ||
					r.Header.Get("Authorization") != "Bearer isolated-alpha" {
					t.Fatal("connection change reached another request")
				}
				content := `{"body":"[topic] alpha: A"}`
				if child {
					content = `{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`
				}
				body := `{"event_id":"$target","room_id":"!room:fixture","sender":"@peer:remote.invalid","type":"m.room.message","content":` + content + `}`
				c := f.r.Profiles["alpha"]
				c.ID = "changed"
				f.r.Profiles["alpha"] = c
				f.save()
				return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)),
					Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			code := a.Run(context.Background(), []string{"post", "!room:fixture", "--re", "$target", "answer"})
			if code == ExitOK || calls != 1 || f.out.Len() != 0 ||
				!strings.Contains(f.err.String(), "selected connection changed") {
				t.Fatal("changed connection was used")
			}
		})
	}
}

func TestPostReplyTopicCancellation(t *testing.T) {
	for _, stage := range []string{"before-read", "after-target"} {
		t.Run(stage, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if stage == "before-read" {
				cancel()
			}
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					return nil, err
				}
				calls++
				if stage == "before-read" || calls != 1 || r.Method != "GET" ||
					r.URL.Path != "/_matrix/client/v3/rooms/!room:fixture/event/$target" {
					t.Fatal("cancellation reached an extra request")
				}
				body := `{"event_id":"$target","sender":"@peer:remote.invalid","type":"m.room.message","content":{"body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}}`
				cancel()
				return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)),
					Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			code := a.Run(ctx, []string{"post", "!room:fixture", "--re", "$target", "answer"})
			wantCalls := 1
			if stage == "before-read" {
				wantCalls = 0
			}
			if code == ExitOK || calls != wantCalls || f.out.Len() != 0 {
				t.Fatal("cancelled command made progress")
			}
		})
	}
}
