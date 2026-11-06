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

// The bound display envelope is generic, not a verification-message decoder.
// Real loopback HTTP preserves exact opaque content, state keys, and IDs for
// state/encrypted/custom events as well as legacy message content.
func TestRoomEventGenericHTTP(t *testing.T) {
	for _, typ := range []string{"m.room.message", "m.room.member", "m.room.encrypted", "example.custom"} {
		t.Run(typ, func(t *testing.T) {
			content := `{"body":"first","body":"last\ud800","extension":9007199254740993}`
			var calls atomic.Int32
			guards := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/!selected:fixture/event/$selected" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL)
				}
				fmt.Fprintf(w, `{"event_id":"$selected","room_id":"!selected:fixture","type":%q,"sender":"@peer:fixture","state_key":"","content":%s}`, typ, content)
			}))
			defer srv.Close()
			c := New(srv.URL, "fixture-token")
			c.BeforeRequest = func() error { guards++; return nil }
			ev, err := c.GetRoomEvent(context.Background(), "!selected:fixture", "$selected")
			if err != nil || ev.EventID != "$selected" || ev.RoomID != "!selected:fixture" || ev.Type != typ || ev.StateKey == nil || *ev.StateKey != "" || string(ev.Content) != content || calls.Load() != 1 || guards != 1 {
				t.Fatalf("display event lost: %+v err=%v calls=%d guards=%d", ev, err, calls.Load(), guards)
			}
		})
	}
}

type roomEventCloseBody struct {
	io.Reader
	close func() error
}

func (b roomEventCloseBody) Close() error { return b.close() }

// Full entity completion, Close and the existing immutable request guard must
// precede a usable event, including cancellation after a complete JSON body.
func TestRoomEventTransportCompletion(t *testing.T) {
	for _, kind := range []string{"valid", "read-error", "close-error", "cancel-on-close", "short-body", "guard"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := verificationResponse(verificationContent)
			c := New("https://fixture.invalid", "fixture-token")
			calls, closed := 0, 0
			c.BeforeRequest = func() error {
				if kind == "guard" {
					return errors.New("binding refused")
				}
				return nil
			}
			c.HTTP = &http.Client{Transport: verificationTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				var reader io.Reader = strings.NewReader(body)
				if kind == "read-error" {
					reader = verificationReadError{}
				}
				length := int64(len(body))
				if kind == "short-body" {
					length++
				}
				return &http.Response{StatusCode: 200, Request: r, ContentLength: length, Body: roomEventCloseBody{Reader: reader, close: func() error {
					closed++
					if kind == "cancel-on-close" {
						cancel()
					}
					if kind == "close-error" {
						return errors.New("private peer detail")
					}
					return nil
				}}}, nil
			})}
			ev, err := c.GetRoomEvent(ctx, "!selected:fixture", "$selected")
			if kind == "valid" {
				if err != nil || ev.EventID != "$selected" {
					t.Fatalf("valid neighbor: %+v %v", ev, err)
				}
			} else if err == nil || ev.EventID != "" || len(ev.Content) != 0 || strings.Contains(err.Error(), "private peer detail") {
				t.Fatalf("transport failure returned event: %+v %v", ev, err)
			}
			want := 1
			if kind == "guard" {
				want = 0
			}
			if calls != want || closed != want {
				t.Fatalf("calls=%d closed=%d want=%d", calls, closed, want)
			}
		})
	}
}
