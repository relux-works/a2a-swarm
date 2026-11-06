package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Exercise the selected creation-state endpoint's own transport, including
// complete I/O and original-request provenance, alongside a valid observation.
func TestRequireSpaceTransport(t *testing.T) {
	for _, kind := range []string{"valid", "partial-status", "wrong-method", "missing-request", "binding", "length", "read", "close", "close-cancel"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, guards := 0, 0
			c := New("https://fixture.invalid", "fixture-token")
			c.BeforeRequest = func() error {
				guards++
				if kind == "binding" {
					return errors.New("binding refused")
				}
				return nil
			}
			c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/!Selected:fixture/state/m.room.create/" {
					t.Fatal("creation state selection changed")
				}
				if _, ok := r.Context().Deadline(); !ok {
					t.Fatal("creation lookup has no bound")
				}
				body := `{"type":"m.space","room_version":"12","extension":true}`
				res := &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}
				switch kind {
				case "partial-status":
					res.StatusCode = 206
				case "wrong-method":
					res.Request = r.Clone(r.Context())
					res.Request.Method = "POST"
				case "missing-request":
					res.Request = nil
				case "length":
					res.ContentLength++
				case "read":
					res.Body = verificationReadError{}
				case "close":
					res.Body = verificationBadBody{Reader: strings.NewReader(body)}
				case "close-cancel":
					res.Body = spaceCancelClose{Reader: strings.NewReader(body), cancel: cancel}
				}
				return res, nil
			})}
			err := c.RequireSpace(ctx, "!Selected:fixture")
			wantCalls := 1
			if kind == "binding" {
				wantCalls = 0
			}
			if (err == nil) != (kind == "valid") || calls != wantCalls || guards != 1 {
				t.Fatalf("kind=%s err=%v calls=%d guards=%d", kind, err, calls, guards)
			}
		})
	}
}

type spaceCancelClose struct {
	io.Reader
	cancel context.CancelFunc
}

func (b spaceCancelClose) Close() error { b.cancel(); return nil }
