package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Claim: local target/root tombstones veto reply inference even when the server
// returns stale content, or when the root would otherwise qualify for historical
// fallback. Each refusal is adjacent to the same valid request without that
// tombstone; another room's tombstone and reply-main's unused root stay harmless.
// A fake transport injects mid-read changes synchronously, without timing races.
func TestPostReplyLocalTombstones(t *testing.T) {
	for _, tc := range []struct {
		name, id, during, room string
		rootMissing, replyMain bool
		wantGets               int
	}{
		{name: "target-before-read", id: "$target", wantGets: 0},
		{name: "root-before-read", id: "$root", wantGets: 1},
		{name: "target-during-read", id: "$target", during: "$target", wantGets: 1},
		{name: "root-during-read", id: "$root", during: "$root", wantGets: 2},
		{name: "hidden-root-before-read", id: "$root", rootMissing: true, wantGets: 1},
		{name: "hidden-root-during-read", id: "$root", during: "$root", rootMissing: true, wantGets: 2},
		{name: "target-reply-main", id: "$target", replyMain: true, wantGets: 0},
		{name: "other-room-control", id: "$target", room: "!other:fixture", wantGets: 2},
		{name: "reply-main-root-control", id: "$root", replyMain: true, wantGets: 1},
	} {
		for _, envelope := range []bool{false, true} {
			for _, tombstone := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/envelope=%v/tombstone=%v", tc.name, envelope, tombstone), func(t *testing.T) {
					f := newNamedFixture(t, true)
					redact := func() {
						room := tc.room
						if room == "" {
							room = "!room:fixture"
						}
						mb, err := config.OpenMailbox(f.paths("alpha", false), "alice")
						if err != nil {
							t.Fatal(err)
						}
						if err := mb.Redact([]config.Redaction{{Room: room, EventID: tc.id}}); err != nil {
							t.Fatal(err)
						}
					}
					if tombstone && tc.during == "" {
						redact()
					}
					a := f.app()
					gets, puts := 0, 0
					a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
						if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL || r.Header.Get("Authorization") != "Bearer isolated-alpha" {
							t.Fatal("request escaped selected connection")
						}
						status, body := 200, ""
						switch {
						case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/"):
							gets++
							id := strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/")
							if id != "$target" && id != "$root" {
								t.Fatal("unexpected event read")
							}
							if tombstone && tc.during == id {
								redact()
							}
							content := `{"msgtype":"m.text","body":"[topic] alpha: Root"}`
							if id == "$target" {
								content = `{"msgtype":"m.text","body":"stale child","ai.relux.a2a":{"slug":"alpha"},"m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`
							}
							body = fmt.Sprintf(`{"event_id":%q,"room_id":"!room:fixture","sender":"@peer:fixture","type":"m.room.message","content":%s}`, id, content)
							if id == "$root" && tc.rootMissing {
								status, body = 404, `{"errcode":"M_NOT_FOUND"}`
							}
						case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/send/m.room.message/"):
							puts++
							var raw json.RawMessage
							if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
								t.Fatal(err)
							}
							var m matrix.MessageContent
							if err := json.Unmarshal(raw, &m); err != nil {
								t.Fatal(err)
							}
							var content map[string]json.RawMessage
							if err := json.Unmarshal(raw, &content); err != nil {
								t.Fatal(err)
							}
							h, ok := decodeHeader(m.A2A)
							wantRoot, wantSlug := "$root", "alpha"
							if tc.replyMain {
								wantRoot, wantSlug = "", ""
							}
							if !ok || h.Topic != wantRoot || h.Slug != wantSlug || h.Re != "$target" || h.Kind != "answer" ||
								!reflect.DeepEqual(h.To, []string{"@peer:fixture"}) || m.Mentions == nil || !reflect.DeepEqual(m.Mentions.UserIDs, []string{"@peer:fixture"}) ||
								m.RelatesTo == nil || m.RelatesTo.EventID != wantRoot || m.RelatesTo.InReplyTo == nil || m.RelatesTo.InReplyTo.EventID != "$target" || m.RelatesTo.IsFallingBack {
								t.Fatal("reply routing or addressing changed")
							}
							if envelope {
								var encoded string
								if json.Unmarshal(content[matrix.EnvelopeContentKey], &encoded) != nil || encoded != base64.RawURLEncoding.EncodeToString([]byte("{}\n")) || m.Body != "answer\n" {
									t.Fatal("envelope or body bytes changed")
								}
							}
							body = `{"event_id":"$sent"}`
						default:
							t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
						}
						return &http.Response{StatusCode: status, Request: r, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
					})}
					args := []string{"post", "!room:fixture", "--re", "$target", "--kind", "answer", "--to", "@peer:fixture"}
					if tc.replyMain {
						args = append(args, "--reply-main")
					}
					if envelope {
						args = append(args, "--envelope", "@"+envelopeFile(t, []byte("{}\n")))
					}
					args = append(args, "answer\n")
					code := a.Run(context.Background(), args)
					refuse := tombstone && tc.room == "" && !(tc.replyMain && tc.id == "$root")
					wantGets := tc.wantGets
					if !tombstone {
						wantGets = 2
						if tc.replyMain {
							wantGets = 1
						}
					}
					if gets != wantGets {
						t.Fatalf("event reads=%d, want %d", gets, wantGets)
					}
					if refuse {
						if code != ExitError || puts != 0 || f.out.Len() != 0 || f.err.String() != "a2a: --re event is locally redacted; refusing reply routing\n" {
							t.Fatalf("wrong refusal: exit=%d puts=%d out=%q err=%q", code, puts, f.out.String(), f.err.String())
						}
					} else if code != ExitOK || puts != 1 || strings.TrimSpace(f.out.String()) != "$sent" {
						t.Fatalf("valid control failed: exit=%d puts=%d err=%s", code, puts, f.err.String())
					}
				})
			}
		}
	}
}

// Claim: --topic slug does not require the reply child to occur in local or
// listed history. A pre-join child fetched from the exact selected-room endpoint
// supplies m.thread membership; the listing independently maps the slug to that
// root, even when the root's exact read is historically refused. Header-only
// slug/topic claims, wrong-room children and conflicting discovered roots still
// refuse. Each negative is paired with the valid version of its own fixture.
// This models returned evidence, not a live server's history-access policy.
func TestPostReplyPrejoinChildSlug(t *testing.T) {
	for _, mode := range []string{"readable-root", "hidden-root", "header-only", "wrong-room", "conflicting-root", "undiscoverable-root"} {
		for _, valid := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/valid=%v", mode, valid), func(t *testing.T) {
				f := newNamedFixture(t, true)
				// An unscoped follow and a foreign-room membership cannot establish
				// either slug agreement or membership of the selected target.
				if err := config.SaveState(f.paths("alpha", false), "alice", config.State{
					Threads: []string{"$root"}, ThreadEvents: []config.ThreadEvent{{Room: "!other:fixture", EventID: "$target", Root: "$foreign"}},
				}); err != nil {
					t.Fatal(err)
				}
				a := f.app()
				gets, scans, puts := 0, 0, 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL || r.Header.Get("Authorization") != "Bearer isolated-alpha" {
						t.Fatal("request escaped selected connection")
					}
					status, body := 200, ""
					switch {
					case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/"):
						gets++
						id := strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/")
						if id != "$target" && id != "$root" {
							t.Fatal("followed another root")
						}
						content := `{"msgtype":"m.text","body":"[topic] alpha: Root"}`
						room := "!room:fixture"
						if id == "$target" {
							content = `{"msgtype":"m.text","body":"pre-join child","ai.relux.a2a":{"slug":"alpha","topic":"$untrusted"},"m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`
							if !valid && mode == "header-only" {
								content = `{"msgtype":"m.text","body":"pre-join child","ai.relux.a2a":{"slug":"alpha","topic":"$root"}}`
							}
							if !valid && mode == "wrong-room" {
								room = "!foreign:fixture"
							}
						}
						body = fmt.Sprintf(`{"event_id":%q,"room_id":%q,"sender":"@peer:fixture","type":"m.room.message","origin_server_ts":1,"content":%s}`, id, room, content)
						if id == "$root" && mode == "hidden-root" {
							status, body = 403, `{"errcode":"M_FORBIDDEN"}`
						}
					case r.Method == "GET" && (r.URL.Path == "/_matrix/client/v1/rooms/!room:fixture/threads" || r.URL.Path == "/_matrix/client/v3/rooms/!room:fixture/messages"):
						scans++
						root := "$root"
						if !valid && mode == "conflicting-root" {
							root = "$other"
						}
						// The pre-join child is deliberately absent from all discovery.
						body = fmt.Sprintf(`{"chunk":[{"event_id":%q,"room_id":"!room:fixture","sender":"@peer:fixture","type":"m.room.message","content":{"msgtype":"m.text","body":"[topic] alpha: Root"}}]}`, root)
						if !valid && mode == "undiscoverable-root" {
							body = `{"chunk":[]}`
						}
					case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/send/m.room.message/"):
						puts++
						var m matrix.MessageContent
						if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
							t.Fatal(err)
						}
						h, ok := decodeHeader(m.A2A)
						if !ok || h.Topic != "$root" || h.Slug != "alpha" || h.Re != "$target" || m.RelatesTo == nil || m.RelatesTo.EventID != "$root" || m.RelatesTo.InReplyTo == nil || m.RelatesTo.InReplyTo.EventID != "$target" || m.RelatesTo.IsFallingBack {
							t.Fatal("lost pre-join child's validated thread")
						}
						body = `{"event_id":"$sent"}`
					default:
						t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
					}
					return &http.Response{StatusCode: status, Request: r, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				code := a.Run(context.Background(), []string{"post", "!room:fixture", "--topic", "alpha", "--re", "$target", "answer"})
				refuse := !valid && mode != "readable-root" && mode != "hidden-root"
				if refuse {
					if code != ExitError || puts != 0 || f.out.Len() != 0 {
						t.Fatalf("invalid evidence accepted: exit=%d puts=%d", code, puts)
					}
				} else if code != ExitOK || puts != 1 || gets != 2 || scans != 2 {
					t.Fatalf("valid pre-join child refused: exit=%d puts=%d gets=%d scans=%d err=%s", code, puts, gets, scans, f.err.String())
				}
			})
		}
	}
}
