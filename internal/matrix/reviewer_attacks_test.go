package matrix

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type reviewerCloseCancel struct {
	io.Reader
	cancel context.CancelFunc
}

func (b reviewerCloseCancel) Close() error { b.cancel(); return nil }
func TestReviewerCloseCancellation(t *testing.T) {
	for _, method := range []string{"put", "txn", "resolve", "error"} {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			raw := `{"event_id":"$real","room_id":"!real:server"}`
			status := 200
			if method == "error" {
				raw = `{"errcode":"M_FORBIDDEN"}`
				status = 403
			}
			c := wireClient(raw, status)
			c.HTTP.Transport = evidenceTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: int64(len(raw)), Request: r, Body: reviewerCloseCancel{strings.NewReader(raw), cancel}}, nil
			})
			var err error
			switch method {
			case "put":
				_, err = c.PutState(ctx, "!r:s", "s", "k", nil)
			case "txn":
				_, err = c.SendMessageTxn(ctx, "!r:s", "t", nil)
			case "resolve":
				_, err = c.ResolveAlias(ctx, "#r:s")
			case "error":
				_, err = c.PutState(ctx, "!r:s", "s", "k", nil)
			}
			if ctx.Err() == nil {
				t.Fatal("cancel fixture did not trigger")
			}
			if method == "error" {
				if IsCode(err, "M_FORBIDDEN") {
					t.Fatalf("cancelled response remains definitive: %v", err)
				}
			} else if err == nil {
				t.Fatal("cancelled response accepted")
			}
		})
	}
}

func TestReviewerIdentitySchema(t *testing.T) {
	for _, method := range []string{"register", "login", "whoami"} {
		for _, id := range []string{"@u:s", "not-a-user-id", "@", "@u"} {
			t.Run(method+"/"+id, func(t *testing.T) {
				raw := `{"user_id":"` + id + `","device_id":"device","access_token":"token"}`
				c := wireClient(raw, 200)
				var e error
				switch method {
				case "register":
					_, e = c.Register(t.Context(), "u", "password", "reg", "device")
				case "login":
					_, e = c.Login(t.Context(), "@u:s", "password", "device")
				case "whoami":
					_, e = c.Whoami(t.Context())
				}
				if (e == nil) != (id == "@u:s") {
					t.Fatalf("identity schema user_id=%q err=%v", id, e)
				}
			})
		}
	}
}
func TestReviewerUIARequiredFlows(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"genuine", `{"session":"s","flows":[{"stages":["m.login.dummy"]}]}`, true},
		{"missing-flows", `{"session":"s"}`, false},
		{"case-flows", `{"session":"s","FLOWS":[{"stages":["m.login.dummy"]}]}`, false},
		{"missing-stages", `{"session":"s","flows":[{}]}`, false},
		{"case-stages", `{"session":"s","flows":[{"STAGES":["m.login.dummy"]}]}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exists, err := wireClient(tc.raw, 401).UserExists(t.Context(), "candidate")
			if (err == nil) != tc.valid || exists {
				t.Fatalf("incomplete UIA classified as free: exists=%v err=%v wantValid=%v", exists, err, tc.valid)
			}
		})
	}
}
