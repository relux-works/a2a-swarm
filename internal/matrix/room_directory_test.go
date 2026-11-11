package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type directoryTransport func(*http.Request) (*http.Response, error)

func (f directoryTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type directoryBody struct {
	io.Reader
	readError, closeError bool
}

func (b directoryBody) Read(p []byte) (int, error) {
	if b.readError {
		return 0, errors.New("synthetic incomplete body")
	}
	return b.Reader.Read(p)
}
func (b directoryBody) Close() error {
	if b.closeError {
		return errors.New("synthetic close failure")
	}
	return nil
}

// Proves the selected endpoint returns only explicit complete private/public
// observations, with duplicate/case/transport ambiguity and partial bodies refused.
func TestRoomDirectoryVisibility(t *testing.T) {
	for _, name := range []string{"private", "public", "extension", "empty", "null", "array", "unknown", "wrong-type", "case", "shadow", "duplicate", "escaped-duplicate", "trailing", "unicode", "403", "404", "206", "redirect", "network", "read", "close", "short", "oversized", "canceled"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls, guards := 0, 0
			client := New("https://selected.invalid", "synthetic-token")
			client.BeforeRequest = func() error { guards++; return nil }
			client.HTTP = &http.Client{Transport: directoryTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.String() != "https://selected.invalid/_matrix/client/v3/directory/list/room/%21opaque" || r.Header.Get("Authorization") != "Bearer synthetic-token" {
					t.Errorf("wrong selected endpoint/caller: %s %s", r.Method, r.URL)
				}
				status, body := 200, `{"visibility":"private"}`
				switch name {
				case "public":
					body = `{"visibility":"public"}`
				case "extension":
					body = `{"visibility":"private","extension":true}`
				case "empty":
					body = `{}`
				case "null":
					body = `null`
				case "array":
					body = `[]`
				case "unknown":
					body = `{"visibility":"unknown"}`
				case "wrong-type":
					body = `{"visibility":true}`
				case "case":
					body = `{"Visibility":"private"}`
				case "shadow":
					body = `{"visibility":"private","Visibility":"public"}`
				case "duplicate":
					body = `{"visibility":"public","visibility":"private"}`
				case "escaped-duplicate":
					body = `{"visibility":"public","visibilit\u0079":"private"}`
				case "trailing":
					body += `{}`
				case "unicode":
					body = `{"visibility":"private","extension":"\ud800"}`
				case "403":
					status = 403
				case "404":
					status = 404
				case "206":
					status = 206
				case "redirect":
					status = 302
				case "network":
					return nil, errors.New("synthetic unavailable server")
				case "oversized":
					body += strings.Repeat(" ", VerificationEntityLimit)
				case "canceled":
					cancel()
				}
				length := int64(len(body))
				if name == "short" {
					length++
				}
				return &http.Response{StatusCode: status, Body: directoryBody{Reader: strings.NewReader(body), readError: name == "read", closeError: name == "close"}, ContentLength: length, Request: r, Header: http.Header{"Location": []string{"https://other.invalid/directory"}}}, nil
			})}
			got, err := client.GetRoomDirectoryVisibility(ctx, "!opaque")
			valid := name == "private" || name == "public" || name == "extension"
			if (err == nil) != valid || calls != 1 || guards != 1 || (!valid && got != "") {
				t.Fatalf("wrong directory boundary: visibility%q err%v calls%d guards%d", got, err, calls, guards)
			}
			if valid && got != "private" && !(name == "public" && got == "public") {
				t.Fatalf("wrong observed visibility %q", got)
			}
		})
	}
}

// Proves invalid selectors and request-admission refusals perform no network call.
func TestRoomDirectorySelectionRefusals(t *testing.T) {
	client := New("https://selected.invalid", "synthetic-token")
	client.HTTP = &http.Client{Transport: directoryTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("refused directory read reached network")
		return nil, errors.New("unexpected request")
	})}
	for _, room := range []string{"", "invalid", "#alias:fixture.invalid", "!bad room"} {
		if value, err := client.GetRoomDirectoryVisibility(context.Background(), room); err == nil || value != "" {
			t.Fatal("invalid selection admitted")
		}
	}
	client.BeforeRequest = func() error { return errors.New("synthetic admission refusal") }
	if value, err := client.GetRoomDirectoryVisibility(context.Background(), "!opaque"); err == nil || value != "" {
		t.Fatal("request admission bypassed")
	}
}
