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

func TestControlEndpointPartition(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, good string
		call       func(*Client) error
	}{
		{"register", `{"user_id":"@u:s","access_token":"token","device_id":"device"}`, func(c *Client) error { _, e := c.Register(ctx, "u", "p", "r", "d"); return e }},
		{"login", `{"user_id":"@u:s","access_token":"token","device_id":"device"}`, func(c *Client) error { _, e := c.Login(ctx, "@u:s", "p", "d"); return e }},
		{"whoami", `{"user_id":"@u:s"}`, func(c *Client) error { _, e := c.Whoami(ctx); return e }},
		{"joined", `{"joined_rooms":["!room:s"]}`, func(c *Client) error { _, e := c.JoinedRooms(ctx); return e }},
		{"join", `{"room_id":"!room:s"}`, func(c *Client) error { _, e := c.Join(ctx, "!room:s"); return e }},
		{"create", `{"room_id":"!room:s"}`, func(c *Client) error { _, e := c.CreateRoom(ctx, CreateRoomRequest{}); return e }},
		{"delete-alias", `{}`, func(c *Client) error { return c.DeleteAlias(ctx, "#a:s") }},
		{"leave", `{}`, func(c *Client) error { return c.Leave(ctx, "!room:s") }},
		{"invite", `{}`, func(c *Client) error { return c.Invite(ctx, "!room:s", "@u:s") }},
		{"presence", `{}`, func(c *Client) error { return c.SetPresence(ctx, "@u:s", Presence{}) }},
		{"put-account", `{}`, func(c *Client) error { return c.PutAccountData(ctx, "@u:s", "type", nil) }},
		{"deactivate", `{"id_server_unbind_result":"no-support"}`, func(c *Client) error { return c.Deactivate(ctx, "@u:s", "p") }},
		{"media-config", `{"m.upload.size":123}`, func(c *Client) error { _, e := c.MediaConfig(ctx); return e }},
		{"upload", `{"content_uri":"mxc://s/media"}`, func(c *Client) error {
			_, e := c.Upload(ctx, "name", "text/plain", strings.NewReader("x"), 1)
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, kind := range []string{"valid", "trailing", "duplicate", "case-required", "wrong-type", "missing"} {
				if (kind == "case-required" || kind == "wrong-type" || kind == "missing") && (tc.name == "deactivate" || tc.name == "media-config" || tc.good == `{}`) {
					continue
				}
				t.Run(kind, func(t *testing.T) {
					body := tc.good
					switch kind {
					case "trailing":
						body += " {}"
					case "duplicate":
						body = strings.TrimSuffix(body, "}") + `,"x":1,"x":2}`
					case "case-required":
						body = strings.ToUpper(body)
					case "wrong-type":
						body = `{"user_id":3,"access_token":3,"device_id":3,"room_id":3,"joined_rooms":3,"content_uri":3}`
					case "missing":
						body = `{}`
					}
					err := tc.call(wireClient(body, 200))
					if (err == nil) != (kind == "valid") {
						t.Fatalf("control route kind=%s err=%v", kind, err)
					}
				})
			}
		})
	}
}

func TestACKCompletionAndReceipt(t *testing.T) {
	for _, shape := range []string{"put", "send", "txn"} {
		for _, kind := range []string{"known", "chunked", "zero-length", "short", "long", "read-error", "cancel-eof", "cancel-read", "cancel-before", "trailing"} {
			t.Run(shape+"/"+kind, func(t *testing.T) {
				raw := `{"event_id":"$receipt","unknown":{"number":9007199254740993}}`
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c := wireClient(raw, 200)
				var observed *evidenceBody
				c.HTTP.Transport = evidenceTransport(func(r *http.Request) (*http.Response, error) {
					body := raw
					if kind == "trailing" {
						body += " {}"
					}
					observed = &evidenceBody{Reader: strings.NewReader(body)}
					length := int64(len(body))
					switch kind {
					case "chunked":
						length = -1
					case "zero-length":
						length = 0
					case "short":
						length--
					case "long":
						length++
					case "read-error":
						observed.readErr = io.ErrUnexpectedEOF
					case "cancel-eof":
						observed.cancel = cancel
					}
					var reader io.ReadCloser = observed
					if kind == "cancel-read" {
						reader = &cancelReadBody{body: strings.NewReader(body), cancel: cancel}
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: length, Request: r, Body: reader}, nil
				})
				if kind == "cancel-before" {
					cancel()
				}
				var ack ACKReceipt
				var err error
				switch shape {
				case "put":
					ack, err = c.PutStateReceipt(ctx, "!r:s", "s", "k", nil)
				case "send":
					ack, err = c.SendMessageReceipt(ctx, "!r:s", nil)
				case "txn":
					ack, err = c.SendMessageTxnReceipt(ctx, "!r:s", "txn", nil)
				}
				valid := kind == "known" || kind == "chunked"
				if (err == nil) != valid {
					t.Fatalf("completion verdict ack=%+v err=%v", ack, err)
				}
				if !valid {
					if ack.EventID() != "" || len(ack.Raw()) != 0 {
						t.Fatal("failed response returned usable receipt")
					}
					return
				}
				if ack.EventID() != "$receipt" || string(ack.Raw()) != raw || ack.Status() != 200 {
					t.Fatalf("receipt binding: %+v", ack)
				}
				if kind == "chunked" && ack.ContentLength() != -1 || kind == "known" && ack.ContentLength() != int64(len(raw)) {
					t.Fatal("lost length fact")
				}
				copy := ack.Raw()
				copy[0] = '!'
				if string(ack.Raw()) != raw {
					t.Fatal("receipt raw aliases caller memory")
				}
			})
		}
	}
}

type cancelReadBody struct {
	body   *strings.Reader
	cancel context.CancelFunc
}

func (b *cancelReadBody) Read(p []byte) (int, error) { n, e := b.body.Read(p); b.cancel(); return n, e }
func (b *cancelReadBody) Close() error               { return nil }

func TestErrorCompletionAndUIA(t *testing.T) {
	for _, kind := range []string{"valid", "read-error", "cancel-eof", "length", "at-cap", "below-cap"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			raw := `{"errcode":"M_FORBIDDEN","error":"server secret"}`
			if kind == "at-cap" {
				raw += strings.Repeat(" ", maxErrorBody-len(raw))
			}
			if kind == "below-cap" {
				raw += strings.Repeat(" ", maxErrorBody-1-len(raw))
			}
			c := wireClient(raw, 403)
			c.HTTP.Transport = evidenceTransport(func(r *http.Request) (*http.Response, error) {
				b := &evidenceBody{Reader: strings.NewReader(raw)}
				n := int64(len(raw))
				switch kind {
				case "read-error":
					b.readErr = io.ErrUnexpectedEOF
				case "cancel-eof":
					b.cancel = cancel
				case "length":
					n++
				}
				return &http.Response{StatusCode: 403, Header: make(http.Header), ContentLength: n, Request: r, Body: b}, nil
			})
			_, err := c.PutState(ctx, "!r:s", "s", "k", nil)
			var me *Error
			if !errors.As(err, &me) {
				t.Fatal(err)
			}
			valid := kind == "valid" || kind == "below-cap"
			if (me.ErrCode == "M_FORBIDDEN") != valid {
				t.Fatalf("error completion trusted: %+v", me)
			}
			if strings.Contains(err.Error(), "server secret") {
				t.Fatal("raw server message leaked")
			}
		})
	}
	for _, method := range []string{"register", "probe", "deactivate"} {
		for _, kind := range []string{"valid", "case", "duplicate", "trailing", "surrogate"} {
			t.Run(method+"/"+kind, func(t *testing.T) {
				challenge := `{"session":"session","flows":[{"stages":["m.login.dummy","m.login.password"]}]}`
				if method == "register" {
					challenge = `{"session":"session","flows":[{"stages":["m.login.dummy"]}]}`
				}
				if method == "deactivate" {
					challenge = `{"session":"session","flows":[{"stages":["m.login.password"]}]}`
				}
				switch kind {
				case "case":
					challenge = strings.Replace(challenge, "session", "SESSION", 1)
				case "duplicate":
					challenge = strings.TrimSuffix(challenge, "}") + `,"session":"other"}`
				case "trailing":
					challenge += " {}"
				case "surrogate":
					challenge = strings.TrimSuffix(challenge, "}") + `,"future":"\ud800"}`
				}
				calls := 0
				c := wireClient("", 200)
				c.HTTP.Transport = evidenceTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					body, status := challenge, 401
					if calls > 1 {
						status = 200
						body = `{"user_id":"@u:s","device_id":"d","access_token":"t"}`
					}
					return &http.Response{StatusCode: status, Header: make(http.Header), ContentLength: int64(len(body)), Request: r, Body: io.NopCloser(strings.NewReader(body))}, nil
				})
				var err error
				switch method {
				case "register":
					_, err = c.Register(context.Background(), "u", "p", "token", "device")
				case "probe":
					_, err = c.UserExists(context.Background(), "u")
				case "deactivate":
					err = c.Deactivate(context.Background(), "@u:s", "p")
				}
				if (err == nil) != (kind == "valid") {
					t.Fatalf("UIA %s %s calls=%d err=%v", method, kind, calls, err)
				}
				want := 1
				if kind == "valid" && method != "probe" {
					want = 2
				}
				if calls != want {
					t.Fatalf("UIA raw reinterpretation calls=%d want=%d", calls, want)
				}
			})
		}
	}
	// The Error.Raw compatibility field is diagnostic only; UIA reads the bound tree.
	c := wireClient(`{"session":"valid","flows":[]}`, 401)
	_, err := c.PutState(context.Background(), "!r:s", "s", "k", nil)
	var me *Error
	if !errors.As(err, &me) {
		t.Fatal(err)
	}
	me.Raw = []byte(`{"session":"forged"}`)
	var u uiaResponse
	if !me.uia(&u) || u.Session != "valid" {
		t.Fatal("UIA reparsed mutable Raw")
	}
	for _, status := range []int{401, 403, 404, 500} {
		for _, code := range []string{"M_UNKNOWN_TOKEN", "M_FORBIDDEN"} {
			t.Run(fmt.Sprintf("error-%d-%s", status, code), func(t *testing.T) {
				c := wireClient(fmt.Sprintf(`{"errcode":%q,"error":"secret"}`, code), status)
				_, err := c.Sync(context.Background(), "", 0, "")
				if status == 401 && code == "M_UNKNOWN_TOKEN" {
					if !errors.Is(err, ErrAuth) {
						t.Fatal(err)
					}
				} else if !IsCode(err, code) || !IsStatus(err, status) {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestStrictAliasAbsence(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		absent    bool
	}{
		{"valid", `{"errcode":"M_NOT_FOUND","error":"absent"}`, true},
		{"missing", `{}`, false}, {"case", `{"ERRCODE":"M_NOT_FOUND"}`, false},
		{"partial", `{"errcode":"M_NOT_FOUND",`, false},
		{"duplicate", `{"errcode":"M_NOT_FOUND","errcode":"M_NOT_FOUND"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wireClient(tc.raw, 404).ResolveAlias(context.Background(), "#a:s")
			if errors.Is(err, ErrUnknownAlias) != tc.absent {
				t.Fatalf("absence inferred from invalid body: %v", err)
			}
		})
	}
}

func TestUploadResponseSizeNeighbors(t *testing.T) {
	for _, size := range []int{maxErrorBody - 1, maxErrorBody, maxErrorBody + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			raw := `{"content_uri":"mxc://s/media"}`
			raw += strings.Repeat(" ", size-len(raw))
			uri, err := wireClient(raw, 200).Upload(context.Background(), "file", "text/plain", strings.NewReader("x"), 1)
			if (err == nil) != (size <= maxErrorBody) || size <= maxErrorBody && uri != "mxc://s/media" {
				t.Fatalf("size=%d uri=%q err=%v", size, uri, err)
			}
		})
	}
}

func TestAllEndpointErrorPartition(t *testing.T) {
	ctx := context.Background()
	calls := map[string]func(*Client) error{
		"register":     func(c *Client) error { _, e := c.Register(ctx, "u", "p", "r", "d"); return e },
		"probe":        func(c *Client) error { _, e := c.UserExists(ctx, "u"); return e },
		"login":        func(c *Client) error { _, e := c.Login(ctx, "@u:s", "p", "d"); return e },
		"deactivate":   func(c *Client) error { return c.Deactivate(ctx, "@u:s", "p") },
		"whoami":       func(c *Client) error { _, e := c.Whoami(ctx); return e },
		"joined":       func(c *Client) error { _, e := c.JoinedRooms(ctx); return e },
		"resolve":      func(c *Client) error { _, e := c.ResolveAlias(ctx, "#r:s"); return e },
		"delete-alias": func(c *Client) error { return c.DeleteAlias(ctx, "#r:s") },
		"join":         func(c *Client) error { _, e := c.Join(ctx, "!r:s"); return e },
		"leave":        func(c *Client) error { return c.Leave(ctx, "!r:s") },
		"invite":       func(c *Client) error { return c.Invite(ctx, "!r:s", "@u:s") },
		"create":       func(c *Client) error { _, e := c.CreateRoom(ctx, CreateRoomRequest{}); return e },
		"state":        func(c *Client) error { var m map[string]any; return c.GetState(ctx, "!r:s", "s", "k", &m) },
		"state-events": func(c *Client) error { _, e := c.GetStateEvents(ctx, "!r:s", "s"); return e },
		"room-state":   func(c *Client) error { _, e := c.GetRoomState(ctx, "!r:s"); return e },
		"put":          func(c *Client) error { _, e := c.PutState(ctx, "!r:s", "s", "k", nil); return e },
		"send":         func(c *Client) error { _, e := c.SendMessage(ctx, "!r:s", nil); return e },
		"txn":          func(c *Client) error { _, e := c.SendMessageTxnReceipt(ctx, "!r:s", "txn", nil); return e },
		"messages":     func(c *Client) error { _, e := c.Messages(ctx, "!r:s", "", "b", 1); return e },
		"redactions":   func(c *Client) error { _, e := c.MessagesWithRedactions(ctx, "!r:s", "", "b", 1); return e },
		"messages-to":  func(c *Client) error { _, e := c.MessagesTo(ctx, "!r:s", "", "to", 1); return e },
		"threads":      func(c *Client) error { _, e := c.Threads(ctx, "!r:s", "", 1); return e },
		"replies":      func(c *Client) error { _, e := c.ThreadReplies(ctx, "!r:s", "$r", "", "b", 1); return e },
		"event":        func(c *Client) error { _, e := c.GetEvent(ctx, "!r:s", "$e"); return e },
		"sync":         func(c *Client) error { _, e := c.Sync(ctx, "", 0, ""); return e },
		"set-presence": func(c *Client) error { return c.SetPresence(ctx, "@u:s", Presence{}) },
		"presence":     func(c *Client) error { _, e := c.GetPresence(ctx, "@u:s"); return e },
		"account":      func(c *Client) error { var m map[string]any; return c.GetAccountData(ctx, "@u:s", "t", &m) },
		"put-account":  func(c *Client) error { return c.PutAccountData(ctx, "@u:s", "t", nil) },
		"media-config": func(c *Client) error { _, e := c.MediaConfig(ctx); return e },
		"upload":       func(c *Client) error { _, e := c.Upload(ctx, "f", "text/plain", strings.NewReader("x"), 1); return e },
		"download":     func(c *Client) error { _, e := c.Download(ctx, "mxc://s/media"); return e },
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			for _, kind := range []string{"valid", "case", "duplicate", "utf8", "trailing"} {
				t.Run(kind, func(t *testing.T) {
					raw := `{"errcode":"M_FORBIDDEN","error":"denied"}`
					switch kind {
					case "case":
						raw = `{"ERRCODE":"M_FORBIDDEN"}`
					case "duplicate":
						raw = `{"errcode":"M_UNKNOWN","errcode":"M_FORBIDDEN"}`
					case "utf8":
						raw = "{\"errcode\":\"M_FORBIDDEN\",\"error\":\"\xff\"}"
					case "trailing":
						raw += " {}"
					}
					err := call(wireClient(raw, 403))
					var me *Error
					if !errors.As(err, &me) || (me.ErrCode == "M_FORBIDDEN") != (kind == "valid") {
						t.Fatalf("shared error partition method=%s kind=%s err=%v", name, kind, err)
					}
				})
			}
		})
	}
}

func TestBulkDownloadBytes(t *testing.T) {
	raw := "\xff" + `{"x":"\ud800","x":2}`
	body, err := wireClient(raw, 200).Download(context.Background(), "mxc://s/media")
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	b, err := io.ReadAll(body)
	if err != nil || string(b) != raw {
		t.Fatalf("bulk media changed %q %v", b, err)
	}
}
