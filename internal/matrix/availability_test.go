package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"testing"
)

type availabilityRoundTrip func(*http.Request) (*http.Response, error)

func (f availabilityRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestAvailabilityResponseContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   AvailabilityOutcome
	}{
		{"available", 200, `{"available":true}`, AvailabilityAvailable},
		{"extension", 200, `{"available":true,"org.example.extension":{"value":1}}`, AvailabilityAvailable},
		{"taken", 400, `{"errcode":"M_USER_IN_USE","error":"synthetic-peer-secret"}`, AvailabilityTaken},
		{"invalid", 400, `{"errcode":"M_INVALID_USERNAME"}`, AvailabilityInvalid},
		{"reserved", 400, `{"errcode":"M_EXCLUSIVE"}`, AvailabilityReserved},
		{"false-is-not-taken", 200, `{"available":false}`, AvailabilityUnknown},
		{"missing", 200, `{}`, AvailabilityUnknown},
		{"null", 200, `{"available":null}`, AvailabilityUnknown},
		{"string", 200, `{"available":"true"}`, AvailabilityUnknown},
		{"wrong-case", 200, `{"Available":true}`, AvailabilityUnknown},
		{"duplicate", 200, `{"available":false,"available":true}`, AvailabilityUnknown},
		{"duplicate-extension", 200, `{"available":true,"x":{"a":1,"a":2}}`, AvailabilityUnknown},
		{"trailing", 200, `{"available":true}{}`, AvailabilityUnknown},
		{"array", 200, `[true]`, AvailabilityUnknown},
		{"malformed", 200, `{"available":`, AvailabilityUnknown},
		{"success-with-errcode", 200, `{"available":true,"errcode":"M_USER_IN_USE"}`, AvailabilityUnknown},
		{"success-with-error", 200, `{"available":true,"error":"synthetic-peer-secret"}`, AvailabilityUnknown},
		{"wrong-success-status", 201, `{"available":true}`, AvailabilityUnknown},
		{"taken-on-success", 200, `{"errcode":"M_USER_IN_USE"}`, AvailabilityUnknown},
		{"taken-on-server-error", 500, `{"errcode":"M_USER_IN_USE"}`, AvailabilityUnknown},
		{"mixed-error", 400, `{"errcode":"M_USER_IN_USE","available":false}`, AvailabilityUnknown},
		{"bad-error-type", 400, `{"errcode":"M_USER_IN_USE","error":false}`, AvailabilityUnknown},
		{"duplicate-error", 400, `{"errcode":"M_USER_IN_USE","errcode":"M_EXCLUSIVE"}`, AvailabilityUnknown},
		{"unknown-error", 400, `{"errcode":"M_OTHER"}`, AvailabilityUnknown},
		{"malformed-error", 400, `not JSON`, AvailabilityUnknown},
		{"disabled", 403, `{"errcode":"M_FORBIDDEN"}`, AvailabilityUnknown},
		{"forbidden-taken", 403, `{"errcode":"M_USER_IN_USE"}`, AvailabilityUnknown},
		{"unsupported", 404, `{"errcode":"M_NOT_FOUND"}`, AvailabilityUnknown},
		{"method-unsupported", 405, `{}`, AvailabilityUnknown},
		{"rate-limit", 429, `{"errcode":"M_LIMIT_EXCEEDED","retry_after_ms":1}`, AvailabilityUnknown},
		{"server-error", 503, `{"available":true}`, AvailabilityUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls, guards := 0, 0
			jar, _ := cookiejar.New(nil)
			base, _ := url.Parse("https://selected.invalid")
			jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: "synthetic-cookie-secret"}})
			client := New(base.String(), "synthetic-access-secret")
			client.BeforeRequest = func() error { guards++; return nil }
			client.HTTP = &http.Client{Jar: jar, Transport: availabilityRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet || r.URL.Scheme != "https" || r.URL.Host != "selected.invalid" || r.URL.Path != v3+"/register/available" || r.URL.RawQuery != "username=a%2Bb%2Fc" || r.Body != nil || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Errorf("ZERO_MUTATION_GUARD: non-anonymous or unexpected request")
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), ContentLength: int64(len(tc.body)), Request: r, Header: make(http.Header)}, nil
			})}
			out, err := client.Availability(context.Background(), "a+b/c")
			if out.Outcome != tc.want || (err != nil) != (tc.want == AvailabilityUnknown) || out.HTTPStatus != tc.status || calls != 1 || guards != 1 {
				t.Fatalf("out=%+v err=%v calls=%d guards=%d", out, err, calls, guards)
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-") {
				t.Fatal("peer secret escaped")
			}
			if client.Token != "synthetic-access-secret" || client.HTTP.Jar != jar {
				t.Fatal("caller policy changed")
			}
		})
	}
}

type availabilityFaultBody struct {
	io.Reader
	readErr, closeErr error
	closeFunc         func()
	closed            bool
}

func (b *availabilityFaultBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF && b.readErr != nil {
		return n, b.readErr
	}
	return n, err
}
func (b *availabilityFaultBody) Close() error {
	b.closed = true
	if b.closeFunc != nil {
		b.closeFunc()
	}
	return b.closeErr
}

func TestAvailabilityEvidenceFailures(t *testing.T) {
	for _, fault := range []string{"valid", "read", "close", "length", "oversize", "interrupted", "wrong-url", "wrong-method", "missing-request", "transport", "binding"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := &availabilityFaultBody{Reader: strings.NewReader(`{"available":true}`)}
			client := New("https://selected.invalid", "")
			calls := 0
			if fault == "binding" {
				client.BeforeRequest = func() error { return errors.New("synthetic-binding-secret") }
			}
			client.HTTP.Transport = availabilityRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if fault == "transport" {
					return nil, errors.New("synthetic-transport-secret")
				}
				resp := &http.Response{StatusCode: 200, Body: body, ContentLength: 18, Request: r, Header: make(http.Header)}
				switch fault {
				case "read":
					body.readErr = io.ErrUnexpectedEOF
				case "close":
					body.closeErr = errors.New("synthetic-close-secret")
				case "length":
					resp.ContentLength++
				case "oversize":
					body.Reader = strings.NewReader(`{"available":true,"x":"` + strings.Repeat("x", maxErrorBody) + `"}`)
					resp.ContentLength = -1
				case "interrupted":
					body.closeFunc = cancel
				case "wrong-url":
					other := r.Clone(ctx)
					other.URL, _ = url.Parse("https://other.invalid/_matrix/client/v3/register/available?username=alice")
					resp.Request = other
				case "wrong-method":
					other := r.Clone(ctx)
					other.Method = http.MethodPost
					resp.Request = other
				case "missing-request":
					resp.Request = nil
				}
				return resp, nil
			})
			out, err := client.Availability(ctx, "alice")
			if fault == "valid" {
				if err != nil || out.Outcome != AvailabilityAvailable {
					t.Fatalf("valid neighbor: %+v %v", out, err)
				}
			} else if err == nil || out.Outcome != AvailabilityUnknown {
				t.Fatalf("fault accepted: %+v %v", out, err)
			}
			if fault == "interrupted" && !errors.Is(err, context.Canceled) {
				t.Fatal("lost interruption")
			}
			if err != nil && strings.Contains(err.Error(), "synthetic-") {
				t.Fatal("secret escaped")
			}
			if fault == "binding" && calls != 0 {
				t.Fatal("binding refusal had effect")
			}
			if fault != "binding" && fault != "transport" && !body.closed {
				t.Fatal("body not closed")
			}
		})
	}
}

func TestAvailabilityMismatchedError(t *testing.T) {
	client := New("https://selected.invalid", "")
	client.HTTP.Transport = availabilityRoundTrip(func(r *http.Request) (*http.Response, error) {
		other := r.Clone(r.Context())
		other.URL, _ = url.Parse("https://other.invalid/")
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"errcode":"M_USER_IN_USE"}`)), ContentLength: -1, Request: other}, nil
	})
	out, err := client.Availability(context.Background(), "alice")
	if err == nil || out.Outcome != AvailabilityUnknown || out.Reason != "mismatched_response" {
		t.Fatalf("%+v %v", out, err)
	}
}

func TestAvailabilityEarlyRefusals(t *testing.T) {
	for _, endpoint := range []string{"https://synthetic-user:synthetic-password@selected.invalid", "https://selected.invalid/?token=synthetic-secret", "https://selected.invalid/#fragment", "https://selected.invalid/nested", "ftp://selected.invalid", ""} {
		client := New(endpoint, "")
		client.HTTP.Transport = availabilityRoundTrip(func(*http.Request) (*http.Response, error) {
			t.Fatal("invalid endpoint had HTTP effect")
			return nil, nil
		})
		out, err := client.Availability(context.Background(), "alice")
		if err == nil || out.Outcome != AvailabilityUnknown || out.Reason != "invalid_endpoint" {
			t.Fatalf("%+v %v", out, err)
		}
		if strings.Contains(err.Error(), "synthetic-") {
			t.Fatal("endpoint secret escaped")
		}
	}
	client := New("https://selected.invalid", "")
	client.HTTP.Transport = availabilityRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("empty username had HTTP effect")
		return nil, nil
	})
	if out, err := client.Availability(context.Background(), ""); err == nil || out.Outcome != AvailabilityUnknown {
		t.Fatal("empty username accepted")
	}
}
