package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
