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
)

type evidenceTransport func(*http.Request) (*http.Response, error)

func (f evidenceTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type evidenceBody struct {
	io.Reader
	cancel  context.CancelFunc
	readErr error
}

func (b *evidenceBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		if b.cancel != nil {
			b.cancel()
		}
		if b.readErr != nil {
			return n, b.readErr
		}
	}
	return n, err
}
func (b *evidenceBody) Close() error { return nil }

// PutState must not promote a valid prefix, read failure, expired read or
// different response URL into direct decoded refusal evidence. The synthetic
// transport controls EOF/cancellation without sleeping; real HTTP cap and
// applied-write recovery are covered by the retained CLI reviewer regressions.
func TestPutStateResponseEvidence(t *testing.T) {
	for _, kind := range []string{"direct", "below-cap", "at-cap", "over-cap", "read-error", "cancel-at-eof", "different-url", "missing-request"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := `{"errcode":"M_FORBIDDEN","error":"denied"}`
			size := len(body)
			switch kind {
			case "below-cap":
				size = maxErrorBody - 1
			case "at-cap":
				size = maxErrorBody
			case "over-cap":
				size = maxErrorBody + 1
			}
			body += strings.Repeat(" ", size-len(body))
			var reads atomic.Int32
			c := New("http://synthetic.invalid", "")
			c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
				reads.Add(1)
				b := &evidenceBody{Reader: strings.NewReader(body)}
				if kind == "read-error" {
					b.readErr = io.ErrUnexpectedEOF
				}
				if kind == "cancel-at-eof" {
					b.cancel = cancel
				}
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
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: b, Request: responseRequest}, nil
			})}
			_, err := c.PutState(ctx, "!room:synthetic", "status", "@own:synthetic", map[string]string{"state": "working"})
			var me *Error
			if !errors.As(err, &me) || me.Status != 403 || reads.Load() != 1 {
				t.Fatalf("response evidence setup: %v reads=%d", err, reads.Load())
			}
			decoded := kind != "at-cap" && kind != "over-cap" && kind != "read-error" && kind != "cancel-at-eof"
			if (me.ErrCode == "M_FORBIDDEN") != decoded {
				t.Fatalf("complete bounded response: kind=%s code=%q", kind, me.ErrCode)
			}
			original := kind != "different-url" && kind != "missing-request"
			if me.OriginalRequest != original {
				t.Fatalf("original response URL: kind=%s original=%t", kind, me.OriginalRequest)
			}
		})
	}
}

// A redirect returning to the original URL still cannot prove rejection of
// the original PUT. The caller's redirect policy remains effective and shared
// clients remain unchanged; every request method and callback count is checked.
func TestPutStateRedirectProvenance(t *testing.T) {
	for _, kind := range []string{"direct", "return-to-original", "stop-policy", "error-policy"} {
		t.Run(kind, func(t *testing.T) {
			var original, other, callbacks atomic.Int32
			var originalURI atomic.Value
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut {
					t.Errorf("redirect changed PUT method: %s", r.Method)
				}
				if r.URL.Path == "/other" {
					other.Add(1)
					w.Header().Set("Location", originalURI.Load().(string))
					w.WriteHeader(307)
					return
				}
				n := original.Add(1)
				if n == 1 {
					originalURI.Store(r.URL.RequestURI())
				}
				if n == 1 && kind != "direct" {
					w.Header().Set("Location", "/other")
					w.WriteHeader(307)
					return
				}
				w.WriteHeader(403)
				fmt.Fprint(w, `{"errcode":"M_FORBIDDEN"}`)
			}))
			defer srv.Close()
			c := New(srv.URL, "")
			policyErr := errors.New("synthetic redirect refusal")
			policy := func(next *http.Request, via []*http.Request) error {
				callbacks.Add(1)
				if kind == "return-to-original" && len(via) == 2 {
					t.Logf("redirect original=%s final=%s", via[0].URL, next.URL)
					if next.URL.String() != via[0].URL.String() {
						t.Error("setup: redirect did not return to exact original URL")
					}
				}
				switch kind {
				case "stop-policy":
					return http.ErrUseLastResponse
				case "error-policy":
					return policyErr
				}
				return nil
			}
			c.HTTP.CheckRedirect = policy
			_, err := c.PutState(context.Background(), "!r", "s", "k", map[string]string{"state": "working"})
			if kind == "error-policy" {
				if !errors.Is(err, policyErr) || original.Load() != 1 || other.Load() != 0 || callbacks.Load() != 1 {
					t.Fatalf("redirect error policy: %v", err)
				}
				return
			}
			var me *Error
			if !errors.As(err, &me) {
				t.Fatalf("redirect error evidence: %v", err)
			}
			wantOriginal := kind == "direct"
			if me.OriginalRequest != wantOriginal {
				t.Fatalf("redirect chain provenance: kind=%s original=%t", kind, me.OriginalRequest)
			}
			switch kind {
			case "direct":
				if me.Status != 403 || me.ErrCode != "M_FORBIDDEN" || original.Load() != 1 || other.Load() != 0 || callbacks.Load() != 0 {
					t.Fatal("direct control lost refusal")
				}
			case "return-to-original":
				if me.Status != 403 || me.ErrCode != "M_FORBIDDEN" || original.Load() != 2 || other.Load() != 1 || callbacks.Load() != 2 {
					t.Fatal("redirect chain was not driven")
				}
			case "stop-policy":
				if me.Status != 307 || original.Load() != 1 || other.Load() != 0 || callbacks.Load() != 1 {
					t.Fatal("redirect stop policy changed")
				}
			}
		})
	}
}
