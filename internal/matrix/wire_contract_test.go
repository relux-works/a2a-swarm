package matrix

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func wireClient(body string, status int) *Client {
	c := New("http://synthetic.invalid", "secret-test-token")
	c.HTTP = &http.Client{Transport: evidenceTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: int64(len(body)), Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	return c
}

func TestGenericACKContract(t *testing.T) {
	for _, shape := range []string{"put", "send", "txn", "resolve", "own-put", "direct-put"} {
		for _, tc := range []struct {
			name, body string
			valid      bool
		}{
			{"valid", `{"event_id":"$opaque:server","room_id":"!room:server","future":{"n":9007199254740993}}`, true},
			{"valid-opaque", `{"event_id":"$opaque_-+/=","room_id":"!room:server"}`, true},
			{"missing", `{}`, false}, {"empty", `{"event_id":"","room_id":""}`, false},
			{"prefix", `{"event_id":"$ack","room_id":"!room:server"} {}`, false},
			{"partial", `{"event_id":"$ack","room_id":"!room:server"`, false},
			{"case", `{"EVENT_ID":"$ack","ROOM_ID":"!room:server"}`, false},
			{"substitution", `{"event_id":"$ack","EVENT_ID":"$other","room_id":"!room:server","ROOM_ID":"!other:server"}`, true},
			{"duplicate", `{"event_id":"$ack","event_id":"$other","room_id":"!room:server"}`, false},
			{"nested-duplicate", `{"event_id":"$ack","room_id":"!room:server","future":{"x":1,"x":2}}`, false},
			{"surrogate", `{"event_id":"$ack","room_id":"!room:server","future":"\ud800"}`, false},
			{"utf8", "{\"event_id\":\"$ack\",\"room_id\":\"!room:server\",\"future\":\"\xff\"}", false},
			{"type", `{"event_id":3,"room_id":3}`, false}, {"null", `{"event_id":null,"room_id":null}`, false},
			{"id-grammar", `{"event_id":"ack","room_id":"room"}`, false},
		} {
			t.Run(shape+"/"+tc.name, func(t *testing.T) {
				c := wireClient(tc.body, 200)
				var id string
				var err error
				switch shape {
				case "put":
					id, err = c.PutState(context.Background(), "!r:s", "s", "k", nil)
				case "send":
					id, err = c.SendMessage(context.Background(), "!r:s", nil)
				case "txn":
					id, err = c.SendMessageTxn(context.Background(), "!r:s", "txn", nil)
				case "resolve":
					id, err = c.ResolveAlias(context.Background(), "#r:s")
				case "own-put":
					id, err = c.ForOwnStatus().PutState(context.Background(), "!r:s", "s", "k", nil)
				case "direct-put":
					id, err = c.WithoutRedirects().PutState(context.Background(), "!r:s", "s", "k", nil)
				}
				if (err == nil) != tc.valid || (!tc.valid && id != "") {
					t.Fatalf("ACK verdict id=%q err=%v wantValid=%v", id, err, tc.valid)
				}
				if tc.valid && id != "$opaque:server" && id != "$opaque_-+/=" && id != "$ack" && id != "!room:server" {
					t.Fatalf("validated exact value replaced: %q", id)
				}
			})
		}
	}
}

func TestControlIdentifierLength(t *testing.T) {
	for _, method := range []string{"put", "send", "txn", "resolve", "join", "create"} {
		for _, encoding := range []string{"ascii", "utf8"} {
			for _, size := range []int{254, 255, 256} {
				t.Run(fmt.Sprintf("%s/%s/%d", method, encoding, size), func(t *testing.T) {
					id := "$" + strings.Repeat("a", size-1)
					if encoding == "utf8" {
						id = "$" + strings.Repeat("é", (size-1)/2) + strings.Repeat("a", (size-1)%2)
					}
					field := "event_id"
					if method == "resolve" || method == "join" || method == "create" {
						field, id = "room_id", "!"+id[1:]
					}
					c := wireClient(fmt.Sprintf(`{%q:%q}`, field, id), 200)
					ctx := context.Background()
					var got string
					var err error
					switch method {
					case "put":
						got, err = c.PutState(ctx, "!r:s", "s", "k", nil)
					case "send":
						got, err = c.SendMessage(ctx, "!r:s", nil)
					case "txn":
						got, err = c.SendMessageTxn(ctx, "!r:s", "txn", nil)
					case "resolve":
						got, err = c.ResolveAlias(ctx, "#r:s")
					case "join":
						got, err = c.Join(ctx, "!r:s")
					case "create":
						got, err = c.CreateRoom(ctx, CreateRoomRequest{})
					}
					valid := size <= 255
					if len(id) != size || (err == nil) != valid || (valid && got != id) || (!valid && got != "") {
						t.Fatalf("identifier bytes=%d valid=%t got=%q err=%v", len(id), valid, got, err)
					}
				})
			}
		}
	}
}

func TestGenericErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		valid      bool
	}{
		{"valid", `{"errcode":"M_FORBIDDEN","error":"denied"}`, true},
		{"case", `{"ERRCODE":"M_FORBIDDEN"}`, false},
		{"utf8", "{\"errcode\":\"M_FORBIDDEN\",\"error\":\"\xff\"}", false},
		{"duplicate", `{"errcode":"M_UNKNOWN","errcode":"M_FORBIDDEN"}`, false},
		{"trailing", `{"errcode":"M_FORBIDDEN"} {}`, false},
		{"partial", `{"errcode":"M_FORBIDDEN",`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wireClient(tc.body, 403).PutState(context.Background(), "!r:s", "s", "k", nil)
			var me *Error
			if !errors.As(err, &me) || (me.ErrCode == "M_FORBIDDEN") != tc.valid {
				t.Fatalf("error verdict %v", err)
			}
		})
	}
}

func TestBulkPeerContentCompatibility(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(fmt.Sprint(bad), func(t *testing.T) {
			content := `{"body":"ordinary","msgtype":"m.text"}`
			want := "ordinary"
			if bad {
				content = `{"body":"first","body":"last\ud800","msgtype":"m.text"}`
				want = "last\ufffd"
			}
			event := `{"event_id":"$event","type":"m.room.message","sender":"@peer:s","content":` + content + `}`
			for _, method := range []string{"sync", "messages", "redactions", "messages-to", "threads", "replies", "event", "state", "room-state", "state-events", "account-data", "presence"} {
				t.Run(method, func(t *testing.T) {
					body := event
					switch method {
					case "sync":
						body = `{"next_batch":"cursor-after","rooms":{"join":{"!r:s":{"timeline":{"events":[` + event + `]}}}}}`
					case "messages", "redactions", "messages-to", "threads", "replies":
						body = `{"chunk":[` + event + `],"end":"cursor-after"}`
					case "room-state", "state-events":
						body = `[` + event + `]`
					case "state", "account-data":
						body = content
					case "presence":
						if bad {
							body = `{"presence":"online","status_msg":"first","status_msg":"last\ud800"}`
						} else {
							body = `{"presence":"online","status_msg":"ordinary"}`
						}
					}
					c := wireClient(body, 200)
					ctx := context.Background()
					var ev Event
					var err error
					var page Page
					switch method {
					case "sync":
						var out SyncResponse
						out, err = c.Sync(ctx, "cursor-before", 0, "")
						if err == nil {
							if out.NextBatch != "cursor-after" || len(out.Rooms.Join["!r:s"].Timeline.Events) != 1 {
								t.Fatal("sync event/cursor lost")
							}
							ev = out.Rooms.Join["!r:s"].Timeline.Events[0]
						}
					case "messages":
						page, err = c.Messages(ctx, "!r:s", "before", "b", 1)
					case "redactions":
						page, err = c.MessagesWithRedactions(ctx, "!r:s", "before", "b", 1)
					case "messages-to":
						page, err = c.MessagesTo(ctx, "!r:s", "before", "after", 1)
					case "threads":
						page, err = c.Threads(ctx, "!r:s", "before", 1)
					case "replies":
						page, err = c.ThreadReplies(ctx, "!r:s", "$root", "before", "b", 1)
					case "event":
						ev, err = c.GetEvent(ctx, "!r:s", "$event")
					case "state", "account-data":
						var m MessageContent
						if method == "state" {
							err = c.GetState(ctx, "!r:s", "s", "k", &m)
						} else {
							err = c.GetAccountData(ctx, "@u:s", "type", &m)
						}
						if err != nil || m.Body != want {
							t.Fatalf("bulk content: %q %v", m.Body, err)
						}
						return
					case "presence":
						var p Presence
						p, err = c.GetPresence(ctx, "@u:s")
						if err != nil || p.StatusMsg != want {
							t.Fatalf("presence compatibility: %+v %v", p, err)
						}
						return
					default:
						var all []Event
						if method == "room-state" {
							all, err = c.GetRoomState(ctx, "!r:s")
						} else {
							all, err = c.GetStateEvents(ctx, "!r:s", "m.room.message")
						}
						if err == nil && len(all) == 1 {
							ev = all[0]
						}
					}
					if err != nil {
						t.Fatal(err)
					}
					if page.Chunk != nil {
						if len(page.Chunk) != 1 || page.End != "cursor-after" {
							t.Fatal("history lost")
						}
						ev = page.Chunk[0]
					}
					if ev.EventID != "$event" || ev.Message().Body != want {
						t.Fatalf("event lost/changed: %+v body=%q", ev, ev.Message().Body)
					}
				})
			}
		})
	}
}

func TestBulkScopedCopyCompatibility(t *testing.T) {
	for _, scope := range []string{"own", "direct"} {
		t.Run(scope, func(t *testing.T) {
			content := `{"body":"first","body":"last\ud800","msgtype":"m.text"}`
			raw := `{"next_batch":"after","rooms":{"join":{"!r:s":{"timeline":{"events":[{"event_id":"$event","type":"m.room.message","content":` + content + `}]}}}}}`
			c := wireClient(raw, 200)
			if scope == "own" {
				c = c.ForOwnStatus()
			} else {
				c = c.WithoutRedirects()
			}
			out, err := c.Sync(context.Background(), "before", 0, "")
			if err != nil || out.NextBatch != "after" || len(out.Rooms.Join["!r:s"].Timeline.Events) != 1 {
				t.Fatalf("scoped bulk response refused: %v", err)
			}
			if out.Rooms.Join["!r:s"].Timeline.Events[0].Message().Body != "last\ufffd" {
				t.Fatal("legacy peer interpretation changed")
			}
		})
	}
}
