package matrix

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// Scoped no-follow must stop every standard redirect for PUT and resolution,
// regardless of a caller policy. The original client still follows normally.
// This is synthetic HTTP evidence, not an assertion about server honesty.
func TestDirectOnlyRedirects(t *testing.T) {
	for _, code := range []int{0, 301, 302, 303, 307, 308} {
		for _, operation := range []string{"put", "resolve"} {
			t.Run(fmt.Sprintf("%d/%s", code, operation), func(t *testing.T) {
				var original, target, callbacks atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/target" {
						target.Add(1)
					} else {
						original.Add(1)
						if code != 0 {
							w.Header().Set("Location", "/target")
							w.WriteHeader(code)
							return
						}
					}
					fmt.Fprint(w, `{"event_id":"$direct","room_id":"!direct:synthetic"}`)
				}))
				defer srv.Close()
				c := New(srv.URL, "")
				c.HTTP.CheckRedirect = func(*http.Request, []*http.Request) error { callbacks.Add(1); return nil }
				call := func(client *Client) error {
					if operation == "put" {
						_, err := client.PutState(context.Background(), "!own:synthetic", "status", "@own:synthetic", map[string]string{"state": "working"})
						return err
					}
					_, err := client.ResolveAlias(context.Background(), "#status:synthetic")
					return err
				}
				err := call(c.WithoutRedirects())
				if (err == nil) != (code == 0) || original.Load() != 1 || target.Load() != 0 || callbacks.Load() != 0 {
					t.Fatalf("scoped redirect refusal: code=%d operation=%s err=%v original=%d target=%d callbacks=%d", code, operation, err, original.Load(), target.Load(), callbacks.Load())
				}
				if err = call(c); err != nil {
					t.Fatal(err)
				}
				if code != 0 && (target.Load() != 1 || callbacks.Load() != 1) {
					t.Fatal("unscoped caller redirect policy changed")
				}
			})
		}
	}
}

// Direct-only ACKs require the original response request, not a synthetic
// transport's different/missing provenance. Direct genuine 2xx remains valid.
func TestDirectOnlyResponseOrigin(t *testing.T) {
	for _, kind := range []string{"direct", "different-url", "missing-request", "informational"} {
		t.Run(kind, func(t *testing.T) {
			c := New("http://synthetic.invalid", "")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				responseRequest := r
				if kind == "different-url" {
					responseRequest = r.Clone(r.Context())
					u := *r.URL
					u.Path = "/other"
					responseRequest.URL = &u
				}
				if kind == "missing-request" {
					responseRequest = nil
				}
				status := 200
				if kind == "informational" {
					status = 101
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: -1, Request: responseRequest, Body: io.NopCloser(strings.NewReader(`{"event_id":"$synthetic"}`))}, nil
			})}
			id, err := c.WithoutRedirects().PutState(context.Background(), "!own:synthetic", "status", "@own:synthetic", map[string]string{"state": "working"})
			if kind == "direct" {
				if err != nil || id != "$synthetic" {
					t.Fatal("direct ACK origin control failed")
				}
				return
			}
			if err == nil || id != "" {
				t.Fatalf("nonoriginal ACK accepted: kind=%s id=%q err=%v", kind, id, err)
			}
		})
	}
}

// The generic client retains its caller redirect policy, but a redirected
// refusal is still not evidence for the initial request. Own writers use the
// stricter no-follow copy, tested above and through the CLI regressions.
func TestPutStateGenericRedirectRefusal(t *testing.T) {
	for _, kind := range []string{"direct", "redirect302"} {
		t.Run(kind, func(t *testing.T) {
			var target atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					target.Add(1)
				} else if kind != "direct" {
					w.Header().Set("Location", "/target")
					w.WriteHeader(302)
					return
				}
				w.WriteHeader(403)
				fmt.Fprint(w, `{"errcode":"M_FORBIDDEN"}`)
			}))
			defer srv.Close()
			c := New(srv.URL, "")
			_, err := c.PutState(context.Background(), "!own:synthetic", "status", "@own:synthetic", map[string]string{"state": "working"})
			me, ok := err.(*Error)
			if !ok || me.Status != 403 || me.ErrCode != "M_FORBIDDEN" {
				t.Fatal("generic refusal fixture failed")
			}
			if me.OriginalRequest != (kind == "direct") {
				t.Fatalf("redirected GET refusal provenance: kind=%s original=%t", kind, me.OriginalRequest)
			}
			if kind != "direct" && target.Load() != 1 {
				t.Fatal("generic redirect target not driven")
			}
		})
	}
}
