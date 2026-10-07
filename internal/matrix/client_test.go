package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// Proves stage selection: the first unfinished supported stage of a fully
// supported flow is chosen, and flows needing unsupported stages are skipped.
func TestNextStage(t *testing.T) {
	flows := func(stages ...[]string) uiaResponse {
		var u uiaResponse
		for _, s := range stages {
			u.Flows = append(u.Flows, struct {
				Stages []string `json:"stages"`
			}{s})
		}
		return u
	}
	u := flows([]string{"m.login.recaptcha"}, []string{"m.login.registration_token", "m.login.dummy"})
	if s, ok := nextStage(u); !ok || s != "m.login.registration_token" {
		t.Fatalf("got %q %v", s, ok)
	}
	u.Completed = []string{"m.login.registration_token"}
	if s, ok := nextStage(u); !ok || s != "m.login.dummy" {
		t.Fatalf("got %q %v", s, ok)
	}
	if _, ok := nextStage(flows([]string{"m.login.email.identity"})); ok {
		t.Fatal("unsupported flow accepted")
	}
}

// Proves UserExists reads the first registration step: M_USER_IN_USE is a
// taken name, a 401 UIA challenge (with a session) is a free one, and anything
// else (a 401 without a session, a server error) is an error, never "free".
func TestUserExists(t *testing.T) {
	for _, c := range []struct {
		name, body   string
		status       int
		exists, fail bool
	}{
		{"taken", `{"errcode":"M_USER_IN_USE","error":"taken"}`, 400, true, false},
		{"free", `{"session":"s","flows":[{"stages":["m.login.registration_token"]}]}`, 401, false, false},
		{"401 without session", `{"errcode":"M_FORBIDDEN"}`, 401, false, true},
		{"server error", `{"errcode":"M_UNKNOWN"}`, 500, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				if r.URL.Path != "/_matrix/client/v3/register" || req["username"] != "bob" || req["auth"] != nil {
					t.Errorf("unexpected probe %s %v", r.URL.Path, req)
				}
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			exists, err := New(srv.URL, "").UserExists(context.Background(), "bob")
			if exists != c.exists || (err != nil) != c.fail {
				t.Fatalf("UserExists = %v, %v", exists, err)
			}
		})
	}
}

// Proves a server that needs registration_token then dummy is walked through
// both stages, and that a server that never accepts the token fails instead of looping.
func TestRegisterStages(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req RegisterRequest
		json.NewDecoder(r.Body).Decode(&req)
		flows := `"flows":[{"stages":["m.login.registration_token","m.login.dummy"]}],"session":"s"`
		switch {
		case req.Auth == nil:
			w.WriteHeader(401)
			w.Write([]byte(`{` + flows + `}`))
		case req.Auth["type"] == "m.login.registration_token" && req.Auth["token"] == "good":
			w.WriteHeader(401)
			w.Write([]byte(`{` + flows + `,"completed":["m.login.registration_token"]}`))
		case req.Auth["type"] == "m.login.dummy":
			w.Write([]byte(`{"user_id":"@u:s","access_token":"t","device_id":"d"}`))
		default:
			w.WriteHeader(401)
			w.Write([]byte(`{` + flows + `,"completed":[]}`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "")
	resp, err := c.Register(context.Background(), "u", "p", "good", "dev")
	if err != nil || resp.UserID != "@u:s" || calls != 3 {
		t.Fatalf("resp=%+v err=%v calls=%d", resp, err, calls)
	}
	calls = 0
	_, err = c.Register(context.Background(), "u", "p", "bad", "dev")
	if err == nil || !strings.Contains(err.Error(), "registration token rejected") || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	_, err = c.Register(context.Background(), "u", "p", "", "dev")
	if err == nil || !strings.Contains(err.Error(), "requires a registration token") {
		t.Fatalf("err=%v", err)
	}
}

// Proves error mapping: M_UNKNOWN_TOKEN becomes ErrAuth without the server's
// message (which may echo the token); other errors keep errcode and status.
func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			w.Write([]byte(`{"errcode":"M_UNKNOWN_TOKEN","error":"bad token ` + r.Header.Get("Authorization") + `"}`))
			return
		}
		w.WriteHeader(404)
		w.Write([]byte(`{"errcode":"M_NOT_FOUND","error":"nope"}`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "leaky-token").Whoami(context.Background())
	if !errors.Is(err, ErrAuth) || strings.Contains(err.Error(), "leaky-token") {
		t.Fatalf("err = %v", err)
	}
	_, err = New(srv.URL, "good").ResolveAlias(context.Background(), "#x:s")
	if !errors.Is(err, ErrUnknownAlias) {
		t.Fatalf("err = %v", err)
	}
	_, err = New(srv.URL, "good").JoinedRooms(context.Background())
	if !IsCode(err, "M_NOT_FOUND") || !IsStatus(err, 404) || !strings.Contains(err.Error(), "status 404") {
		t.Fatalf("err = %v", err)
	}
}

// Proves Sync sends since, timeout (ms), and filter, and gives the request
// enough client-side time beyond the server-side timeout.
func TestSyncQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("since") != "s1" || q.Get("timeout") != "50" || q.Get("filter") != "{}" {
			w.WriteHeader(400)
			return
		}
		time.Sleep(60 * time.Millisecond)
		w.Write([]byte(`{"next_batch":"s2"}`))
	}))
	defer srv.Close()
	resp, err := New(srv.URL, "t").Sync(context.Background(), "s1", 50*time.Millisecond, "{}")
	if err != nil || resp.NextBatch != "s2" {
		t.Fatalf("resp=%+v err=%v", resp, err)
	}
}

// Proves path segments are escaped, so aliases with '#' and ':' reach the server intact.
func TestPathEscaping(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.EscapedPath()
		w.Write([]byte(`{"room_id":"!r"}`))
	}))
	defer srv.Close()
	id, err := New(srv.URL, "t").ResolveAlias(context.Background(), "#a2a-swarm:a2a.local")
	if err != nil || id != "!r" || got != "/_matrix/client/v3/directory/room/%23a2a-swarm:a2a.local" {
		t.Fatalf("id=%q err=%v path=%q", id, err, got)
	}
}

// Proves thread helpers read m.relates_to and the unsigned thread count.
func TestEventHelpers(t *testing.T) {
	ev := Event{Content: json.RawMessage(`{"body":"x","m.relates_to":{"rel_type":"m.thread","event_id":"$r"}}`),
		Unsigned: json.RawMessage(`{"m.relations":{"m.thread":{"count":3}}}`)}
	if ev.ThreadRoot() != "$r" || ev.ThreadCount() != 3 {
		t.Fatalf("root=%q count=%d", ev.ThreadRoot(), ev.ThreadCount())
	}
	plain := Event{Content: json.RawMessage(`{"body":"x","m.relates_to":{"m.in_reply_to":{"event_id":"$q"}}}`)}
	if plain.ThreadRoot() != "" || plain.ThreadCount() != -1 {
		t.Fatal("non-thread event misread")
	}
}

// Proves CreateRoom sends the documented body (preset, visibility, alias
// local part, power level override) to POST /createRoom and returns room_id;
// a taken alias surfaces as M_ROOM_IN_USE, and a 200 without room_id is an
// error, never an empty id.
func TestCreateRoom(t *testing.T) {
	var got map[string]any
	status, body := 200, `{"room_id":"!new:s"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/_matrix/client/v3/createRoom" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		got = nil
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	c := New(srv.URL, "t")
	req := CreateRoomRequest{Preset: "public_chat", Visibility: "private", RoomAliasName: "proj",
		PowerLevelContentOverride: map[string]any{"redact": 100, "users": map[string]int{"@k:s": 100}}}
	id, err := c.CreateRoom(context.Background(), req)
	if err != nil || id != "!new:s" {
		t.Fatalf("id=%q err=%v", id, err)
	}
	pl, _ := json.Marshal(got["power_level_content_override"])
	if got["preset"] != "public_chat" || got["visibility"] != "private" || got["room_alias_name"] != "proj" ||
		string(pl) != `{"redact":100,"users":{"@k:s":100}}` || got["invite"] != nil || got["name"] != nil {
		t.Fatalf("body = %v", got)
	}
	status, body = 400, `{"errcode":"M_ROOM_IN_USE","error":"taken"}`
	if _, err := c.CreateRoom(context.Background(), req); !IsCode(err, "M_ROOM_IN_USE") {
		t.Fatalf("err = %v", err)
	}
	status, body = 200, `{}`
	if id, err := c.CreateRoom(context.Background(), req); err == nil || id != "" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

// Proves incremental message readers explicitly request redactions while
// existing message-only callers retain their filter and pagination options.
func TestMessagesRedactionFilter(t *testing.T) {
	for _, redactions := range []bool{false, true} {
		t.Run(fmt.Sprint(redactions), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				var filter struct {
					Types []string `json:"types"`
				}
				_ = json.Unmarshal([]byte(q.Get("filter")), &filter)
				want := []string{"m.room.message"}
				if redactions {
					want = append(want, "m.room.redaction")
				}
				if !slices.Equal(filter.Types, want) || q.Get("from") != "cursor" || q.Get("dir") != "b" || q.Get("limit") != "100" {
					t.Errorf("bad history request: %v", q)
				}
				w.Write([]byte(`{"chunk":[],"end":"next"}`))
			}))
			defer srv.Close()
			c := New(srv.URL, "t")
			var page Page
			var err error
			if redactions {
				page, err = c.MessagesWithRedactions(context.Background(), "!room", "cursor", "b", 100)
			} else {
				page, err = c.Messages(context.Background(), "!room", "cursor", "b", 100)
			}
			if err != nil || page.End != "next" {
				t.Fatalf("pagination lost: %+v %v", page, err)
			}
		})
	}
}
