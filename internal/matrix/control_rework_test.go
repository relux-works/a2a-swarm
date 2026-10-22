package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

type reworkCloseBody struct {
	io.Reader
	close func() error
}

func (b reworkCloseBody) Close() error { return b.close() }

// Strict bodies close exactly once before fields attest an outcome. Unknown
// length with clean EOF and clean close remains valid.
func TestControlCloseBoundary(t *testing.T) {
	for _, method := range []string{"put", "txn", "resolve", "error", "upload", "whoami", "probe"} {
		for _, effect := range []string{"clean", "cancel", "failure"} {
			t.Run(method+"/"+effect, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				raw := `{"event_id":"$real","room_id":"!real:s","user_id":"@u:s","content_uri":"mxc://s/media"}`
				status := 200
				if method == "error" {
					raw, status = `{"errcode":"M_FORBIDDEN"}`, 403
				}
				if method == "probe" {
					raw, status = `{"session":"s","flows":[{"stages":["m.login.dummy"]}]}`, 401
				}
				closes := 0
				c := wireClient(raw, status)
				c.HTTP.Transport = evidenceTransport(func(r *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: -1, Request: r, Body: reworkCloseBody{strings.NewReader(raw), func() error {
						closes++
						if effect == "cancel" {
							cancel()
						}
						if effect == "failure" {
							return errors.New("synthetic secret close error")
						}
						return nil
					}}}, nil
				})
				var err error
				switch method {
				case "put", "error":
					_, err = c.PutState(ctx, "!r:s", "s", "k", nil)
				case "txn":
					_, err = c.SendMessageTxn(ctx, "!r:s", "t", nil)
				case "resolve":
					_, err = c.ResolveAlias(ctx, "#r:s")
				case "upload":
					_, err = c.Upload(ctx, "f", "text/plain", strings.NewReader("x"), 1)
				case "whoami":
					_, err = c.Whoami(ctx)
				case "probe":
					_, err = c.UserExists(ctx, "candidate")
				}
				if closes != 1 {
					t.Fatalf("body closed %d times", closes)
				}
				if method == "error" {
					if IsCode(err, "M_FORBIDDEN") != (effect == "clean") {
						t.Fatalf("denial trust: %v", err)
					}
				} else if (err == nil) != (effect == "clean") {
					t.Fatalf("success trust: %v", err)
				}
				if err != nil && strings.Contains(err.Error(), "synthetic secret") {
					t.Fatal("close error leaked")
				}
			})
		}
	}
}

func TestControlIdentityComponents(t *testing.T) {
	for _, method := range []string{"register", "login", "whoami"} {
		for _, id := range []string{"@u:s", "@Historical_UPPER/+=:server:8448", "@u:[::1]:8448", "@:s", "@u:"} {
			t.Run(method+"/"+id, func(t *testing.T) {
				c := wireClient(`{"user_id":"`+id+`","access_token":"token","device_id":"device"}`, 200)
				var err error
				var got string
				switch method {
				case "register":
					v, e := c.Register(t.Context(), "u", "p", "r", "d")
					got, err = v.UserID, e
				case "login":
					v, e := c.Login(t.Context(), "@u:s", "p", "d")
					got, err = v.UserID, e
				case "whoami":
					v, e := c.Whoami(t.Context())
					got, err = v.UserID, e
				}
				valid := id != "@:s" && id != "@u:"
				if (err == nil) != valid || (valid && got != id) {
					t.Fatalf("id=%q got=%q err=%v", id, got, err)
				}
			})
		}
	}
}
