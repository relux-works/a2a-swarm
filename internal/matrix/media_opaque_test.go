package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpaqueRedirectsAndIdentityEncoding(t *testing.T) {
	for _, status := range []int{200, 300, 301, 302, 303, 304, 305, 306, 307, 308} {
		for _, remote := range []bool{false, true} {
			t.Run(fmt.Sprintf("%d/remote=%v", status, remote), func(t *testing.T) {
				var target, callbacks, guard atomic.Int32
				other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					target.Add(1)
					if r.Header.Get("Authorization") != "" {
						t.Error("credential forwarded")
					}
					fmt.Fprint(w, "bytes")
				}))
				defer other.Close()
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/target" {
						target.Add(1)
						fmt.Fprint(w, "bytes")
						return
					}
					if r.URL.Path != "/_matrix/client/v1/media/download/foreign.invalid/media" || r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Authorization") != "Bearer synthetic" {
						t.Error("wrong homeserver endpoint/headers")
					}
					if status != 200 {
						location := "/target"
						if remote {
							location = other.URL
						}
						w.Header().Set("Location", location)
						w.WriteHeader(status)
						return
					}
					fmt.Fprint(w, "bytes")
				}))
				defer srv.Close()
				c := New(srv.URL, "synthetic")
				c.BeforeRequest = func() error { guard.Add(1); return nil }
				c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { callbacks.Add(1); return nil }
				rc, err := c.DownloadOpaque(context.Background(), "mxc://foreign.invalid/media")
				if status == 200 {
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(rc)
					rc.Close()
					if err != nil || string(data) != "bytes" {
						t.Fatal("direct valid neighbour")
					}
				} else if !errors.Is(err, ErrOpaqueRedirect) {
					t.Fatalf("redirect %d accepted/error=%v", status, err)
				}
				if target.Load() != 0 || callbacks.Load() != 0 || guard.Load() != 1 {
					t.Fatalf("redirect/guard target=%d callbacks=%d guard=%d", target.Load(), callbacks.Load(), guard.Load())
				}
			})
		}
	}
}

type opaqueBody struct{ read, closed bool }

func (b *opaqueBody) Read([]byte) (int, error) { b.read = true; return 0, io.EOF }
func (b *opaqueBody) Close() error             { b.closed = true; return nil }
func TestOpaqueEncodingAndResponseOrigin(t *testing.T) {
	for _, kind := range []string{"clear", "identity", "gzip", "lowercase-gzip", "deflate", "combined", "uncompressed", "missing-request", "foreign-response", "mutated-request", "wrong-method", "status403", "informational", "guard"} {
		t.Run(kind, func(t *testing.T) {
			body := &opaqueBody{}
			c := New("http://synthetic.invalid", "synthetic")
			called := false
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.Header.Get("Accept-Encoding") != "identity" {
					t.Error("identity header absent before transport")
				}
				resp := &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1, Body: body, Request: r}
				switch kind {
				case "identity":
					resp.Header.Set("Content-Encoding", "identity")
				case "gzip", "deflate":
					resp.Header.Set("Content-Encoding", kind)
				case "lowercase-gzip":
					resp.Header["content-encoding"] = []string{"gzip"}
				case "combined":
					resp.Header.Set("Content-Encoding", "identity, gzip")
				case "uncompressed":
					resp.Uncompressed = true
				case "missing-request":
					resp.Request = nil
				case "foreign-response":
					resp.Request = r.Clone(r.Context())
					u := *r.URL
					u.Host = "foreign.invalid"
					resp.Request.URL = &u
				case "mutated-request":
					r.URL.Host = "foreign.invalid"
				case "wrong-method":
					resp.Request = r.Clone(r.Context())
					resp.Request.Method = "POST"
				case "status403":
					resp.StatusCode = 403
				case "informational":
					resp.StatusCode = 101
				}
				return resp, nil
			})}
			if kind == "guard" {
				c.BeforeRequest = func() error { return errors.New("synthetic secret must not escape") }
			}
			rc, err := c.DownloadOpaque(context.Background(), "mxc://foreign.invalid/media")
			if kind == "clear" || kind == "identity" {
				if err != nil {
					t.Fatal(err)
				}
				rc.Close()
				return
			}
			if err == nil || rc != nil {
				t.Fatal("invalid response accepted")
			}
			if body.read || kind != "guard" && !body.closed || kind == "guard" && called {
				t.Fatalf("rejected body read=%v closed=%v called=%v", body.read, body.closed, called)
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("arbitrary guard error escaped")
			}
		})
	}
}

func TestOpaqueTenMinuteClampAndShorterContext(t *testing.T) {
	for _, duration := range []time.Duration{time.Hour, time.Second} {
		t.Run(duration.String(), func(t *testing.T) {
			c := New("http://synthetic.invalid", "")
			parent, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			var requestContext context.Context
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				requestContext = r.Context()
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > min(duration, MediaRequestTimeout) {
					t.Error("transfer deadline not clamped")
				}
				if time.Until(deadline) < min(duration, MediaRequestTimeout)-time.Second/2 {
					t.Error("unexpected deadline surgery")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Request: r, Body: io.NopCloser(strings.NewReader("x"))}, nil
			})}
			rc, err := c.DownloadOpaque(parent, "mxc://foreign.invalid/media")
			if err != nil {
				t.Fatal(err)
			}
			rc.Close()
			if requestContext.Err() != context.Canceled {
				t.Fatal("Close did not release request")
			}
		})
	}
}

func TestGetMediaEventExactSelection(t *testing.T) {
	valid := `{"event_id":"$native","sender":"@human:synthetic","room_id":"!room:synthetic","type":"m.room.message","content":{"msgtype":"m.file","url":"mxc://foreign.invalid/media"}}`
	for _, kind := range []string{"valid", "encrypted", "event", "sender", "room", "room-null", "duplicate", "alias", "unicode", "redacted", "unsigned-null", "content-null", "content-over-limit", "filtered"} {
		t.Run(kind, func(t *testing.T) {
			raw := valid
			switch kind {
			case "encrypted":
				raw = strings.Replace(raw, "m.room.message", "m.room.encrypted", 1)
			case "event":
				raw = strings.Replace(raw, "$native", "$other", 1)
			case "sender":
				raw = strings.Replace(raw, "@human:synthetic", "human", 1)
			case "room":
				raw = strings.Replace(raw, "!room:synthetic", "!other:synthetic", 1)
			case "room-null":
				raw = strings.Replace(raw, `"room_id":"!room:synthetic"`, `"room_id":null`, 1)
			case "duplicate":
				raw = strings.Replace(raw, `"event_id":"$native"`, `"event_id":"$other","event_id":"$native"`, 1)
			case "alias":
				raw = strings.Replace(raw, `"sender":`, `"Sender":`, 1)
			case "unicode":
				raw = strings.Replace(raw, `@human:synthetic`, `@human\ud800:synthetic`, 1)
			case "redacted":
				raw = strings.Replace(raw, `"content":`, `"unsigned":{"redacted_because":{}},"content":`, 1)
			case "unsigned-null":
				raw = strings.Replace(raw, `"content":`, `"unsigned":null,"content":`, 1)
			case "content-null":
				raw = strings.Replace(raw, `{"msgtype":"m.file","url":"mxc://foreign.invalid/media"}`, `null`, 1)
			case "content-over-limit":
				raw = strings.Replace(raw, `"msgtype":"m.file"`, `"msgtype":"m.file","extra":"`+strings.Repeat("x", 65536)+`"`, 1)
			}
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/_matrix/client/v3/rooms/!room:synthetic/event/$native" {
					t.Error("wrong selected endpoint")
				}
				fmt.Fprint(w, raw)
			}))
			defer srv.Close()
			c := New(srv.URL, "")
			if kind == "filtered" {
				c.FilterEvents = func(_ string, evs []Event) ([]Event, error) { evs[0] = evs[0].WithoutContent(); return evs, nil }
			}
			ev, err := c.GetMediaEvent(context.Background(), "!room:synthetic", "$native")
			if kind == "valid" || kind == "encrypted" || kind == "filtered" {
				if err != nil || ev.EventID != "$native" || ev.Sender != "@human:synthetic" {
					t.Fatalf("valid observation: %+v %v", ev, err)
				}
				if kind == "filtered" && !ev.IsRedacted() {
					t.Fatal("local redaction filter bypassed")
				}
			} else if err == nil {
				t.Fatalf("invalid %s selection accepted", kind)
			}
			if calls.Load() != 1 {
				t.Fatal("selection used fallback")
			}
		})
	}
}
