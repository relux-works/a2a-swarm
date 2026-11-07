package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// These HTTP cases prove historical root refusals preserve the child's routing
// and slug, exact explicit roots need no second lookup, and readable roots keep
// supplying their own slug. Invalid/missing child headers supply no slug.
// Negative cases prove other root failures and unreadable children send nothing;
// an undiscoverable explicit slug cannot be established by the child's header.
func TestPostReplyHistoricalRoot(t *testing.T) {
	const child = `{"msgtype":"m.text","body":"[topic] body-slug: not a root","ai.relux.a2a":{"v":1,"type":"message","slug":"child-slug","topic":"$untrusted"},"m.relates_to":{"rel_type":"m.thread","event_id":"$root"}}`
	const root = `{"msgtype":"m.text","body":"[topic] root-slug: Root"}`
	for _, tc := range []struct {
		name, mode, topic, header, wantSlug, wantError string
	}{
		{name: "root-404", mode: "404", wantSlug: "child-slug"},                                                      // A missing root preserves the child's relation and slug.
		{name: "root-403", mode: "403", wantSlug: "child-slug"},                                                      // A forbidden historical root has the same fallback.
		{name: "explicit-root", mode: "404", topic: "$root", wantSlug: "child-slug"},                                 // The exact matching root ID passes without another lookup.
		{name: "explicit-root-403", mode: "403", topic: "$root", wantSlug: "child-slug"},                             // Exact root agreement also passes a 403.
		{name: "readable-root", wantSlug: "root-slug"},                                                               // A readable root still supplies the slug, overriding the child.
		{name: "missing-header", mode: "404", header: "missing"},                                                     // Body text cannot supply the fallback slug.
		{name: "invalid-header", mode: "404", header: "invalid"},                                                     // An undecodable header supplies no slug.
		{name: "invalid-slug", mode: "404", header: "slug"},                                                          // An invalid header slug is discarded.
		{name: "undiscoverable-slug", mode: "404", topic: "child-slug", wantError: "topic \"child-slug\" not found"}, // A child slug alone cannot resolve an explicit topic.
		{name: "other-topic", mode: "404", topic: "$other", wantError: "omit --re"},                                  // A different explicit root refuses with citation guidance.
		{name: "child-404", mode: "child-404", wantError: "M_NOT_FOUND"},                                             // Missing children never reach a root read or send.
		{name: "root-500", mode: "500", wantError: "HTTP 500"},                                                       // Server failures are not historical refusals.
		{name: "500-forbidden-code", mode: "500-forbidden", wantError: "status 500"},                                 // An allowed code on a server-error status cannot enable fallback.
		{name: "404-wrong-code", mode: "404-wrong-code", wantError: "HTTP 404"},                                      // Status alone cannot enable fallback.
		{name: "malformed-refusal", mode: "malformed-refusal", wantError: "HTTP 404"},                                // A partial error body cannot enable fallback.
		{name: "unbound-refusal", mode: "unbound-refusal", wantError: "M_NOT_FOUND"},                                 // A refusal must belong to the original request.
		{name: "root-timeout", mode: "timeout", wantError: "deadline exceeded"},                                      // A transport deadline failure sends nothing (no wall-clock wait).
		{name: "root-transport", mode: "transport", wantError: "synthetic transport failure"},                        // Other transport failures send nothing.
		{name: "redacted-root", mode: "redacted", wantError: "redacted"},                                             // Returned redacted roots remain refused.
		{name: "related-root", mode: "related", wantError: "itself related"},                                         // Returned related roots remain refused without following another link.
		{name: "malformed-root", mode: "malformed", wantError: "invalid verification"},                               // Invalid root JSON remains refused.
		{name: "ambiguous-root", mode: "ambiguous", wantError: "ambiguous --re message content"},                     // Case-alias body ambiguity remains refused.
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			gets, sends, scans := 0, 0, 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["alpha"].URL || r.Header.Get("Authorization") != "Bearer isolated-alpha" {
					t.Fatal("request escaped selected authenticated connection")
				}
				status, body := http.StatusOK, ""
				original := r
				switch {
				case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/"):
					gets++
					id := strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/event/")
					content := root
					if id == "$target" {
						content = child
						switch tc.header {
						case "missing":
							content = strings.Replace(content, `"ai.relux.a2a":{"v":1,"type":"message","slug":"child-slug","topic":"$untrusted"},`, "", 1)
						case "invalid":
							content = strings.Replace(content, `"v":1`, `"v":"bad"`, 1)
						case "slug":
							content = strings.Replace(content, "child-slug", "invalid slug", 1)
						}
					} else if id == "$root" {
						switch tc.mode {
						case "404", "unbound-refusal":
							status, body = 404, `{"errcode":"M_NOT_FOUND"}`
						case "403":
							status, body = 403, `{"errcode":"M_FORBIDDEN"}`
						case "500":
							status, body = 500, `{"errcode":"M_UNKNOWN"}`
						case "500-forbidden":
							status, body = 500, `{"errcode":"M_FORBIDDEN"}`
						case "404-wrong-code":
							status, body = 404, `{"errcode":"M_UNKNOWN"}`
						case "malformed-refusal":
							status, body = 404, `{"errcode":"M_NOT_FOUND",`
						case "timeout":
							return nil, context.DeadlineExceeded
						case "transport":
							return nil, errors.New("synthetic transport failure")
						case "related":
							content = `{"body":"root","m.relates_to":{"rel_type":"m.thread","event_id":"$other"}}`
						case "ambiguous":
							content = `{"body":"root","BODY":"other"}`
						}
						if tc.mode == "unbound-refusal" {
							original = nil
						}
					} else if id != "$other" {
						t.Fatalf("unexpected event read %q", id)
					}
					if id == "$target" && tc.mode == "child-404" {
						status, body = 404, `{"errcode":"M_NOT_FOUND"}`
					}
					if body == "" {
						ev := map[string]any{"event_id": id, "room_id": "!room:fixture", "sender": "@peer:remote.invalid", "type": "m.room.message", "content": json.RawMessage(content)}
						if id == "$root" && tc.mode == "redacted" {
							ev["unsigned"] = map[string]any{"redacted_because": map[string]any{}}
						}
						data, err := json.Marshal(ev)
						if err != nil {
							t.Fatal(err)
						}
						body = string(data)
						if id == "$root" && tc.mode == "malformed" {
							body = `{"event_id":`
						}
					}
				case r.Method == "GET" && (r.URL.Path == "/_matrix/client/v1/rooms/!room:fixture/threads" || r.URL.Path == "/_matrix/client/v3/rooms/!room:fixture/messages"):
					scans++
					body = `{"chunk":[]}`
				case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/!room:fixture/send/m.room.message/"):
					sends++
					var m matrix.MessageContent
					if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
						t.Fatal(err)
					}
					h, ok := decodeHeader(m.A2A)
					if !ok || h.Topic != "$root" || h.Re != "$target" || h.Slug != tc.wantSlug || h.Kind != "answer" ||
						m.RelatesTo == nil || m.RelatesTo.RelType != "m.thread" || m.RelatesTo.EventID != "$root" ||
						m.RelatesTo.IsFallingBack || m.RelatesTo.InReplyTo == nil || m.RelatesTo.InReplyTo.EventID != "$target" {
						t.Fatalf("incorrect sent thread/header: %+v %+v", h, m.RelatesTo)
					}
					body = `{"event_id":"$sent"}`
				default:
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				return &http.Response{StatusCode: status, Request: original, Header: make(http.Header), ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			args := []string{"--as", "alice", "post", "!room:fixture", "--re", "$target", "--kind", "answer", "answer"}
			if tc.topic != "" {
				args = append(args, "--topic", tc.topic)
			}
			code := a.Run(context.Background(), args)
			if tc.wantError == "" {
				if code != ExitOK || sends != 1 || strings.TrimSpace(f.out.String()) != "$sent" {
					t.Fatalf("post failed: code=%d sends=%d err=%s", code, sends, f.err.String())
				}
			} else if code != ExitError || sends != 0 || f.out.Len() != 0 || !strings.Contains(f.err.String(), tc.wantError) {
				t.Fatalf("wrong refusal: code=%d sends=%d out=%q err=%s", code, sends, f.out.String(), f.err.String())
			}
			wantGets, wantScans := 2, 0
			if tc.mode == "child-404" {
				wantGets = 1
			}
			if tc.topic == "$other" {
				wantGets++
			}
			if tc.topic == "child-slug" {
				wantScans = 2
			}
			if gets != wantGets || scans != wantScans {
				t.Fatalf("unexpected reads: events=%d scans=%d", gets, scans)
			}
		})
	}
}
