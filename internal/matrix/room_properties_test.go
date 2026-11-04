package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type propertyTransport func(*http.Request) (*http.Response, error)

func (f propertyTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func propertyReply(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r, Header: make(http.Header)}
}

// Proves invalid typed inputs cannot reach transport and valid server forms
// remain compatible with the existing Matrix grammar, including explicit ports.
func TestRoomPropertiesValidation(t *testing.T) {
	for _, via := range [][]string{{"example.invalid"}, {"127.0.0.1:8448"}, {"[2001:db8::1]:8448", "EXAMPLE.invalid"}} {
		if !ValidateParentVia(via) {
			t.Fatalf("valid via rejected: %v", via)
		}
	}
	c := New("https://fixture.invalid", "fixture-token")
	c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid input reached transport")
		return nil, nil
	})
	for _, via := range [][]string{nil, {""}, {"https://foreign.invalid"}, {"bad/server"}, {"[not-ip]"}, {"host:abc"}} {
		if ValidateParentVia(via) {
			t.Fatalf("invalid via accepted: %v", via)
		}
		if _, err := c.SetRoomParent(context.Background(), "!child", "!parent", via, false); err == nil {
			t.Fatal("invalid parent accepted")
		}
	}
	if _, err := c.SetRoomParent(context.Background(), "!same", "!same", []string{"fixture.invalid"}, false); err == nil {
		t.Fatal("self-parent accepted")
	}
	for _, id := range []string{"", "!", "#alias", "!" + strings.Repeat("x", 255), "!\xff"} {
		if _, err := c.RoomAvatar(context.Background(), id); err == nil {
			t.Fatal("invalid read ID")
		}
		if _, err := c.SetRoomAvatar(context.Background(), id, "mxc://media.invalid/a"); err == nil {
			t.Fatal("invalid set ID")
		}
	}
	if _, err := c.SetRoomAvatar(context.Background(), "!child", "https://media.invalid/a"); err == nil {
		t.Fatal("invalid MXC")
	}
}

type propertyCloseBody struct {
	io.Reader
	closeErr error
}

func (b propertyCloseBody) Close() error { return b.closeErr }

// Proves incomplete/close-failed or non-original responses cannot attest state
// or a definite zero-effects refusal. No redirect endpoint is contacted.
func TestRoomPropertiesTransportEvidence(t *testing.T) {
	for _, kind := range []string{"short", "close-failed", "invalid-utf8", "surrogate", "redirect", "non-original-token"} {
		t.Run(kind, func(t *testing.T) {
			c := New("https://fixture.invalid", "fixture-token")
			calls := 0
			c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls != 1 {
					t.Fatal("retried or followed redirect")
				}
				body := `{"event_id":"$ack"}`
				resp := propertyReply(r, 200, body)
				switch kind {
				case "short":
					resp.ContentLength++
				case "close-failed":
					resp.Body = propertyCloseBody{strings.NewReader(body), errors.New("owned close failure")}
				case "invalid-utf8":
					resp = propertyReply(r, 200, "{\"event_id\":\"$\xff\"}")
				case "surrogate":
					resp = propertyReply(r, 200, `{"event_id":"$\udfff"}`)
				case "redirect":
					resp = propertyReply(r, 307, `{}`)
					resp.Header.Set("Location", "https://fixture.invalid/other")
				case "non-original-token":
					resp = propertyReply(r, 401, `{"errcode":"M_UNKNOWN_TOKEN"}`)
					copy := *r
					u := *r.URL
					u.Path = "/other"
					copy.URL = &u
					resp.Request = &copy
				}
				return resp, nil
			})
			got, err := c.SetRoomAvatar(context.Background(), "!room", "mxc://media.invalid/a")
			if err == nil || got.Outcome != "UNKNOWN" || got.Acknowledged || got.EventID != "" || calls != 1 {
				t.Fatalf("result %+v %v calls%d", got, err, calls)
			}
		})
	}
}

// Proves the two typed operations use only their pinned exact key, preserve
// explicit false and via order, and require an ACK plus full-content readback.
// These are owned transport fixtures, not live server or UI evidence.
func TestRoomPropertiesSet(t *testing.T) {
	for _, kind := range []string{"avatar", "parent-true", "parent-false"} {
		t.Run(kind, func(t *testing.T) {
			path, body := "/_matrix/client/v3/rooms/!room/state/m.room.avatar/", `{"url":"mxc://media.invalid/avatar"}`
			if kind != "avatar" {
				path = "/_matrix/client/v3/rooms/!room/state/m.space.parent/!parent:foreign.invalid"
				body = `{"via":["foreign.invalid","[::1]:8448"],"canonical":` + strings.TrimPrefix(kind, "parent-") + `}`
			}
			var calls []string
			c := New("https://fixture.invalid", "fixture-token")
			c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
				calls = append(calls, r.Method+" "+r.URL.Path)
				if r.URL.Path != path || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Fatalf("wrong target/authority: %s", r.URL)
				}
				if r.Method == "PUT" {
					data, _ := io.ReadAll(r.Body)
					want, _ := strictJSON([]byte(body))
					got, _ := strictJSON(data)
					if !reflect.DeepEqual(want, got) {
						t.Fatalf("wrong content: %s", data)
					}
					return propertyReply(r, 200, `{"event_id":"$ack"}`), nil
				}
				return propertyReply(r, 200, body), nil
			})
			var got StateWriteResult
			var err error
			if kind == "avatar" {
				got, err = c.SetRoomAvatar(context.Background(), "!room", "mxc://media.invalid/avatar")
			} else {
				got, err = c.SetRoomParent(context.Background(), "!room", "!parent:foreign.invalid", []string{"foreign.invalid", "[::1]:8448"}, kind == "parent-true")
			}
			if err != nil || got.Outcome != "VERIFIED" || !got.Acknowledged || got.EventID != "$ack" || got.Readback != "match" || got.HTTPStatus != 0 || got.ErrCode != "" {
				t.Fatalf("result %+v, %v", got, err)
			}
			if !reflect.DeepEqual(calls, []string{"PUT " + path, "GET " + path}) {
				t.Fatalf("extra/missing effects: %v", calls)
			}
		})
	}
}

// Proves only the three existing strict original-request refusal pairs expose
// bounded diagnostics, without raw server text, extra requests or an ACK.
// Fixtures exercise both property writers, not live membership or state power.
func TestRoomPropertiesRefusalDiagnostics(t *testing.T) {
	const private = "private-server-error-and-token"
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"forbidden", `{"errcode":"M_FORBIDDEN","error":"` + private + `"}`, "M_FORBIDDEN", 403},
		{"unknown-token", `{"errcode":"M_UNKNOWN_TOKEN","error":"` + private + `"}`, "M_UNKNOWN_TOKEN", 401},
		{"missing-token", `{"errcode":"M_MISSING_TOKEN","error":"` + private + `"}`, "M_MISSING_TOKEN", 401},
		{"wrong-forbidden-status", `{"errcode":"M_FORBIDDEN"}`, "", 401},
		{"wrong-token-status", `{"errcode":"M_UNKNOWN_TOKEN"}`, "", 403},
		{"unknown-code", `{"errcode":"` + private + `"}`, "", 403},
		{"missing-code", `{"error":"` + private + `"}`, "", 403},
		{"duplicate-code", `{"errcode":"M_FORBIDDEN","errcode":"M_FORBIDDEN"}`, "", 403},
		{"malformed", `{"errcode":"M_FORBIDDEN"`, "", 403},
		{"oversize", `{"errcode":"M_FORBIDDEN","error":"` + strings.Repeat("x", roomPropertyLimit) + `"}`, "", 403},
		{"redirect", `{"errcode":"M_FORBIDDEN"}`, "", 307},
		{"non-original", `{"errcode":"M_FORBIDDEN"}`, "", 403},
		{"short", `{"errcode":"M_FORBIDDEN"}`, "", 403},
		{"close-failed", `{"errcode":"M_FORBIDDEN"}`, "", 403},
		{"transport", "", "", 0},
	} {
		for _, parent := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/parent=%t", tc.name, parent), func(t *testing.T) {
				c := New("https://fixture.invalid", "fixture-token")
				calls := 0
				c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					path := "/_matrix/client/v3/rooms/!room/state/m.room.avatar/"
					if parent {
						path = "/_matrix/client/v3/rooms/!room/state/m.space.parent/!parent"
					}
					if calls != 1 || r.Method != http.MethodPut || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer fixture-token" {
						t.Fatal("refusal changed authority or caused an extra effect")
					}
					if tc.name == "transport" {
						return nil, errors.New(private)
					}
					resp := propertyReply(r, tc.status, tc.body)
					switch tc.name {
					case "redirect":
						resp.Header.Set("Location", "https://foreign.invalid/")
					case "non-original":
						other := *r
						u := *r.URL
						u.Path = "/other"
						other.URL = &u
						resp.Request = &other
					case "short":
						resp.ContentLength++
					case "close-failed":
						resp.Body = propertyCloseBody{strings.NewReader(tc.body), errors.New(private)}
					}
					return resp, nil
				})
				var got StateWriteResult
				var err error
				if parent {
					got, err = c.SetRoomParent(context.Background(), "!room", "!parent", []string{"fixture.invalid"}, true)
				} else {
					got, err = c.SetRoomAvatar(context.Background(), "!room", "mxc://media.invalid/avatar")
				}
				wantOutcome, wantStatus := "UNKNOWN", 0
				if tc.code != "" {
					wantOutcome, wantStatus = "REFUSED", tc.status
				}
				if err == nil || got.Outcome != wantOutcome || got.HTTPStatus != wantStatus || got.ErrCode != tc.code || got.Acknowledged || got.EventID != "" || got.Readback != "not-attempted" || calls != 1 {
					t.Fatalf("bounded refusal diagnostic mismatch: result=%+v error=%v calls=%d", got, err, calls)
				}
				raw, err := json.Marshal(got)
				if err != nil || strings.Contains(string(raw), private) {
					t.Fatalf("private response escaped: %s %v", raw, err)
				}
				var fields map[string]any
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				_, statusPresent := fields["http_status"]
				_, codePresent := fields["errcode"]
				if statusPresent != (tc.code != "") || codePresent != statusPresent {
					t.Fatalf("unverified diagnostic was serialized: %s", raw)
				}
			})
		}
	}
}

// Proves uncertain writes/readbacks are never retried or called verified;
// a validated original refusal stays distinct from potentially applied writes.
func TestRoomPropertiesUncertain(t *testing.T) {
	for _, tc := range []struct {
		name, ack, read, outcome, readback string
		status, calls                      int
		acknowledged                       bool
	}{
		{"lost-ack", "network", "", "UNKNOWN", "not-attempted", 0, 1, false},
		{"missing-ack", `{}`, "", "UNKNOWN", "not-attempted", 200, 1, false},
		{"upload-only", `{"content_uri":"mxc://media.invalid/avatar"}`, "", "UNKNOWN", "not-attempted", 200, 1, false},
		{"duplicate-ack", `{"event_id":"$a","event_id":"$b"}`, "", "UNKNOWN", "not-attempted", 200, 1, false},
		{"permission", `{"errcode":"M_FORBIDDEN","error":"private"}`, "", "REFUSED", "not-attempted", 403, 1, false},
		{"token", `{"errcode":"M_UNKNOWN_TOKEN","error":"private"}`, "", "REFUSED", "not-attempted", 401, 1, false},
		{"malformed-refusal", `{"errcode":"M_FORBIDDEN","errcode":"M_FORBIDDEN"}`, "", "UNKNOWN", "not-attempted", 403, 1, false},
		{"server-error", `{"errcode":"M_UNKNOWN"}`, "", "UNKNOWN", "not-attempted", 500, 1, false},
		{"non200-ack", `{"event_id":"$ack"}`, "", "UNKNOWN", "not-attempted", 201, 1, false},
		{"mismatch", `{"event_id":"$ack"}`, `{"url":"mxc://media.invalid/other"}`, "UNKNOWN", "mismatch", 200, 2, true},
		{"extra-content", `{"event_id":"$ack"}`, `{"url":"mxc://media.invalid/avatar","info":{}}`, "UNKNOWN", "mismatch", 200, 2, true},
		{"unavailable", `{"event_id":"$ack"}`, "network", "UNKNOWN", "unavailable", 200, 2, true},
		{"malformed-read", `{"event_id":"$ack"}`, `{"url":null}`, "UNKNOWN", "unavailable", 200, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New("https://fixture.invalid", "fixture-token")
			calls := 0
			c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Path != "/_matrix/client/v3/rooms/!room/state/m.room.avatar/" || calls > tc.calls {
					t.Fatalf("forbidden extra effect %s %s", r.Method, r.URL)
				}
				body, status := tc.ack, tc.status
				if calls == 2 {
					body, status = tc.read, 200
				}
				if body == "network" {
					return nil, errors.New("owned connection interruption")
				}
				return propertyReply(r, status, body), nil
			})
			got, err := c.SetRoomAvatar(context.Background(), "!room", "mxc://media.invalid/avatar")
			if err == nil || got.Outcome != tc.outcome || got.Readback != tc.readback || got.Acknowledged != tc.acknowledged || calls != tc.calls {
				t.Fatalf("got %+v, %v, calls%d", got, err, calls)
			}
			if tc.acknowledged && got.EventID != "$ack" {
				t.Fatal("lost acknowledged event id")
			}
		})
	}
}

// Proves extension syntax is accepted for reads, explicit canonical absence is
// preserved, and malformed/incomplete state is not an empty-success projection.
func TestRoomPropertiesRead(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		parent     bool
		status     int
		valid      bool
	}{
		{"avatar", `{"url":"mxc://media.invalid/a","info":{"w":1}}`, false, 200, true},
		{"empty-avatar", `{}`, false, 200, true},
		{"absent", `{"errcode":"M_NOT_FOUND"}`, false, 404, true},
		{"parent-no-canonical", `{"via":["foreign.invalid"]}`, true, 200, true},
		{"parent-false", `{"via":["foreign.invalid"],"canonical":false}`, true, 200, true},
		{"removed-parent", `{}`, true, 200, true},
		{"duplicate", `{"url":"mxc://media.invalid/a","url":"mxc://media.invalid/b"}`, false, 200, false},
		{"null", `{"url":null}`, false, 200, false},
		{"trailing", `{} {}`, false, 200, false},
		{"canonical-string", `{"via":["foreign.invalid"],"canonical":"false"}`, true, 200, false},
		{"via-null", `{"via":null}`, true, 200, false},
		{"bad-via", `{"via":["https://foreign.invalid"]}`, true, 200, false},
		{"wrong-mxc", `{"url":"https://media.invalid/a"}`, false, 200, false},
		{"status-only-absence", `{}`, false, 404, false},
		{"non200", `{}`, false, 201, false},
		{"oversize", `{"x":"` + strings.Repeat("x", 65536) + `"}`, false, 200, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New("https://fixture.invalid", "fixture-token")
			calls := 0
			c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" {
					t.Fatal("read mutated state")
				}
				return propertyReply(r, tc.status, tc.body), nil
			})
			var err error
			if tc.parent {
				var got ParentState
				got, err = c.RoomParent(context.Background(), "!room", "!parent")
				if tc.name == "parent-no-canonical" && got.Canonical != nil {
					t.Fatal("invented canonical")
				}
				if tc.name == "parent-false" && (got.Canonical == nil || *got.Canonical) {
					t.Fatal("lost false")
				}
			} else {
				var got AvatarState
				got, err = c.RoomAvatar(context.Background(), "!room")
				if tc.name == "absent" && got.Present {
					t.Fatal("absence became present")
				}
			}
			if (err == nil) != tc.valid || calls != 1 {
				t.Fatalf("err%v calls%d", err, calls)
			}
		})
	}
}

// Proves the exact-key read uses the same complete original-response boundary
// as the ACK, and keeps an acknowledged write UNKNOWN on readback failure.
func TestRoomPropertiesReadEvidence(t *testing.T) {
	for _, afterACK := range []bool{false, true} {
		for _, kind := range []string{"short", "close-failed", "read-failed", "redirect", "non-original", "invalid-extension"} {
			t.Run(fmt.Sprintf("ack-%t/%s", afterACK, kind), func(t *testing.T) {
				c := New("https://fixture.invalid", "fixture-token")
				calls := 0
				c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
					calls++
					if r.URL.Path != "/_matrix/client/v3/rooms/!room/state/m.room.avatar/" {
						t.Fatal("wrong state key")
					}
					if afterACK && calls == 1 {
						if r.Method != "PUT" {
							t.Fatal("missing PUT")
						}
						return propertyReply(r, 200, `{"event_id":"$ack"}`), nil
					}
					if r.Method != "GET" || calls > 2 {
						t.Fatal("unexpected mutation/retry")
					}
					body := `{"url":"mxc://media.invalid/a"}`
					resp := propertyReply(r, 200, body)
					switch kind {
					case "short":
						resp.ContentLength++
					case "close-failed":
						resp.Body = propertyCloseBody{strings.NewReader(body), errors.New("owned close failure")}
					case "read-failed":
						resp.Body = propertyReadFailure{}
					case "redirect":
						resp.StatusCode = 307
						resp.Header.Set("Location", "https://fixture.invalid/other")
					case "non-original":
						copy := *r
						u := *r.URL
						u.Path = "/other"
						copy.URL = &u
						resp.Request = &copy
					case "invalid-extension":
						resp = propertyReply(r, 200, `{"url":"mxc://media.invalid/a","extension":{"x":1,"x":2}}`)
					}
					return resp, nil
				})
				if afterACK {
					got, err := c.SetRoomAvatar(context.Background(), "!room", "mxc://media.invalid/a")
					if err == nil || got.Outcome != "UNKNOWN" || !got.Acknowledged || got.EventID != "$ack" || got.Readback != "unavailable" || calls != 2 {
						t.Fatalf("result %+v %v calls%d", got, err, calls)
					}
				} else {
					got, err := c.RoomAvatar(context.Background(), "!room")
					if err == nil || got.Present || calls != 1 {
						t.Fatalf("read %+v %v calls%d", got, err, calls)
					}
				}
			})
		}
	}
}

type propertyReadFailure struct{}

func (propertyReadFailure) Read([]byte) (int, error) { return 0, errors.New("owned read failure") }
func (propertyReadFailure) Close() error             { return nil }

// Proves canonical false, via order and complete parent content are material to
// set verification. Nearby exact true/false controls are TestRoomPropertiesSet.
func TestRoomPropertiesParentMismatch(t *testing.T) {
	for _, body := range []string{
		`{"via":["a.invalid","b.invalid"],"canonical":true}`,
		`{"via":["b.invalid","a.invalid"],"canonical":false}`,
		`{"via":["a.invalid","b.invalid"]}`,
		`{"via":["a.invalid","b.invalid"],"canonical":false,"extra":1}`,
	} {
		c := New("https://fixture.invalid", "fixture-token")
		calls := 0
		c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.URL.Path != "/_matrix/client/v3/rooms/!child/state/m.space.parent/!parent" {
				t.Fatal("wrong target/key")
			}
			if calls == 1 && r.Method == "PUT" {
				return propertyReply(r, 200, `{"event_id":"$ack"}`), nil
			}
			if calls != 2 || r.Method != "GET" {
				t.Fatal("extra effect")
			}
			return propertyReply(r, 200, body), nil
		})
		got, err := c.SetRoomParent(context.Background(), "!child", "!parent", []string{"a.invalid", "b.invalid"}, false)
		if err == nil || got.Outcome != "UNKNOWN" || got.Readback != "mismatch" || !got.Acknowledged || got.EventID != "$ack" || calls != 2 {
			t.Fatalf("result %+v %v calls%d", got, err, calls)
		}
	}
}

// Proves an oversized explicit server list stops before HTTP with a usable
// uncertainty record, rather than emitting empty outcome/readback fields.
func TestRoomPropertiesContentLimit(t *testing.T) {
	c := New("https://fixture.invalid", "fixture-token")
	c.HTTP.Transport = propertyTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("oversized content reached HTTP")
		return nil, nil
	})
	via := make([]string, roomPropertyLimit/2)
	for i := range via {
		via[i] = "example.invalid"
	}
	got, err := c.SetRoomParent(context.Background(), "!child", "!parent", via, false)
	if err == nil || got.Outcome != "UNKNOWN" || got.Readback != "not-attempted" || got.Acknowledged {
		t.Fatalf("result %+v %v", got, err)
	}
}

// Proves retaining original token-refusal evidence preserves the established
// ErrAuth classification and diagnostic text for existing client callers.
func TestRoomPropertiesAuthCompatibility(t *testing.T) {
	c := New("https://fixture.invalid", "fixture-token")
	c.HTTP.Transport = propertyTransport(func(r *http.Request) (*http.Response, error) {
		return propertyReply(r, 401, `{"errcode":"M_UNKNOWN_TOKEN","error":"never print this"}`), nil
	})
	_, err := c.RoomAvatar(context.Background(), "!room")
	var me *Error
	if !errors.Is(err, ErrAuth) || !errors.As(err, &me) || !me.OriginalRequest || err.Error() != "access token rejected by the homeserver (M_UNKNOWN_TOKEN)" {
		t.Fatalf("auth compatibility %v", err)
	}
}
