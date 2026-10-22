package matrix

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Own status and generic controls share strict interpretation. The scoped
// copy still adds its independent no-redirect policy.
func TestOwnStatusEvidenceScope(t *testing.T) {
	for _, shape := range []string{"put", "resolve"} {
		for _, body := range []string{"{}", `{"event_id":"$id","room_id":"!room:test"} {}`} {
			t.Run(shape+"/"+body, func(t *testing.T) {
				c := New("http://synthetic.invalid", "")
				c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1, Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
				})}
				call := func(c *Client) error {
					if shape == "put" {
						_, err := c.PutState(context.Background(), "!r:test", "status", "@a:test", nil)
						return err
					}
					_, err := c.ResolveAlias(context.Background(), "#status:test")
					return err
				}
				if err := call(c); err == nil {
					t.Fatal("generic missing/partial ACK accepted")
				}
				if err := call(c.ForOwnStatus()); err == nil {
					t.Fatal("scoped missing/partial ACK accepted")
				}
			})
		}
	}
}

// Exercise cancellation at the public scoped shape adapters, before a CLI
// caller's independent pre-send guard can mask a broken response predicate.
// Direct ACK and ordinary500 remain valid siblings; this is synthetic EOF.
func TestOwnStatusResponseCancellation(t *testing.T) {
	for _, shape := range []string{"put", "resolve"} {
		for _, kind := range []string{"direct", "cancel-eof", "server"} {
			t.Run(shape+"/"+kind, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := 0
				c := New("http://synthetic.invalid", "")
				c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					body := `{"event_id":"$ack","room_id":"!room:test"}`
					status := http.StatusOK
					if kind == "server" {
						status, body = http.StatusInternalServerError, `{"errcode":"M_FORBIDDEN"}`
					}
					reader := &evidenceBody{Reader: strings.NewReader(body)}
					if kind == "cancel-eof" {
						reader.cancel = cancel
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), Request: r, ContentLength: int64(len(body)), Body: reader}, nil
				})}
				var err error
				var id string
				if shape == "put" {
					id, err = c.ForOwnStatus().PutState(ctx, "!room:test", "status", "@a:test", nil)
				} else {
					id, err = c.ForOwnStatus().ResolveAlias(ctx, "#status:test")
				}
				if calls != 1 {
					t.Fatalf("unexpected request count: %d", calls)
				}
				if kind == "direct" {
					if err != nil || id == "" {
						t.Fatalf("direct shape response rejected: id=%q err=%v", id, err)
					}
				} else if err == nil || id != "" {
					t.Fatalf("cancelled shape evidence accepted: kind=%s id=%q err=%v", kind, id, err)
				}
				if kind == "cancel-eof" && ctx.Err() != context.Canceled {
					t.Fatal("cancellation did not execute")
				}
			})
		}
	}
}
