package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type memberReadTransport func(*http.Request) (*http.Response, error)

func (f memberReadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func membershipReply(label string, invite bool) string {
	rows := []map[string]any{{"type": "m.room.member", "room_id": "!room:fixture", "state_key": "@alice:" + label + ".invalid", "content": map[string]any{"membership": "join", "displayname": "private-name", "reason": "private-reason"}, "unsigned": map[string]any{"private": "private-history"}}}
	if invite {
		rows = append(rows, map[string]any{"type": "m.room.member", "room_id": "!room:fixture", "state_key": "@guest:external.invalid", "content": map[string]any{"membership": "invite"}})
	}
	b, _ := json.Marshal(map[string]any{"chunk": rows})
	return string(b)
}

func memberFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(root, p)
		files[rel] = fmt.Sprintf("%x", sha256.Sum256(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func seedMemberState(t *testing.T, f *namedFixture) {
	t.Helper()
	for _, label := range []string{"alpha", "beta"} {
		p := f.paths(label, false)
		for _, suffix := range []string{".json", ".mailbox.jsonl", ".lease.json"} {
			path := filepath.Join(p.Base, "state", "alice"+suffix)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("preserved-"+label+suffix), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Proves explicit/current named routing, empty invite success and minimal JSON
// and text projection, with no history/write requests or local file changes.
func TestRoomMembersNamedReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name, label, filter string
		json, invite        bool
	}{
		{"current", "alpha", "join,invite", true, true},
		{"explicit", "beta", "join,invite", true, true},
		{"empty-invite", "beta", "invite", true, false},
		{"text", "alpha", "join", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			seedMemberState(t, f)
			a := f.app()
			before := memberFiles(t, f.root)
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles[tc.label].URL || r.URL.Path != "/_matrix/client/v3/rooms/!room:fixture/members" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "Bearer isolated-"+tc.label {
					t.Fatalf("unexpected authority/endpoint %s %s", r.Method, r.URL)
				}
				body := membershipReply(tc.label, tc.invite)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
			})}
			args := []string{"room", "members", "!room:fixture", "--membership", tc.filter}
			if tc.label == "beta" {
				args = append([]string{"--profile", "beta"}, args...)
			}
			if tc.json {
				args = append(args, "--json")
			}
			if code := a.Run(context.Background(), args); code != 0 {
				t.Fatalf("exit%d: %s", code, f.err.String())
			}
			if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatal("read mutated files/store or made extra requests")
			}
			wantRef := config.SecretReference{Service: f.r.Profiles[tc.label].Store.AccessTokenService, Account: "@alice:" + tc.label + ".invalid"}
			if len(f.store.reads) != 1 || f.store.reads[0] != wantRef {
				t.Fatalf("cross-profile lookup: %+v", f.store.reads)
			}
			text := f.out.String()
			for _, secret := range []string{"private-name", "private-reason", "private-history", "isolated-", "access_token", "displayname", "unsigned"} {
				if strings.Contains(text, secret) {
					t.Fatalf("extra/private data escaped: %s", secret)
				}
			}
			if tc.json {
				var got struct {
					RoomID, Caller, Filter, Completeness string
					Members                              []matrix.RoomMember
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal(f.out.Bytes(), &raw); err != nil {
					t.Fatal(err)
				}
				if len(raw) != 6 {
					t.Fatalf("unexpected output fields: %v", raw)
				}
				json.Unmarshal(raw["room_id"], &got.RoomID)
				json.Unmarshal(raw["caller"], &got.Caller)
				json.Unmarshal(raw["membership_filter"], &got.Filter)
				json.Unmarshal(raw["completeness"], &got.Completeness)
				json.Unmarshal(raw["members"], &got.Members)
				if got.Caller != "@alice:"+tc.label+".invalid" || got.RoomID != "!room:fixture" || got.Filter != tc.filter || got.Completeness != "complete-response" {
					t.Fatalf("record: %+v", got)
				}
				if tc.name == "empty-invite" && (len(got.Members) != 0 || string(raw["members"]) != "[]") {
					t.Fatalf("empty success: %s", text)
				}
				if tc.invite && len(got.Members) != 2 {
					t.Fatalf("principals: %+v", got.Members)
				}
			} else if !strings.Contains(text, "count=1\njoin\t@alice:alpha.invalid\n") {
				t.Fatalf("text: %s", text)
			}
		})
	}
}

// Proves all failures leave stdout empty and files unchanged. A valid request
// above prevents an implementation that rejects everything satisfying this.
func TestRoomMembersRefusals(t *testing.T) {
	for _, kind := range []string{"permission", "auth", "network", "malformed", "late-malformed-row", "incomplete", "archived", "selected-change", "current-unrelated", "invalid-filter", "missing-room", "missing-metadata", "binding-mismatch"} {
		t.Run(kind, func(t *testing.T) {
			f := newNamedFixture(t, kind != "missing-metadata")
			seedMemberState(t, f)
			if kind == "binding-mismatch" {
				c := f.r.Profiles["alpha"]
				c.Store.AccessTokenService += ".changed"
				f.r.Profiles["alpha"] = c
				f.save()
			}
			a := f.app()
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || !strings.HasSuffix(r.URL.Path, "/members") {
					t.Fatalf("forbidden request %s %s", r.Method, r.URL.Path)
				}
				if kind == "network" {
					return nil, errors.New("synthetic network unavailable")
				}
				body := membershipReply("alpha", false)
				status := 200
				switch kind {
				case "permission":
					status = 403
					body = `{"errcode":"M_FORBIDDEN","error":"private-error"}`
				case "auth":
					status = 401
					body = `{"errcode":"M_UNKNOWN_TOKEN","error":"private-error"}`
				case "malformed":
					body = `{"chunk":null}`
				case "late-malformed-row":
					body = strings.TrimSuffix(body, "]}") + `,{}]}`
				case "incomplete":
					status = 206
				case "archived":
					body = strings.Replace(body, `"join"`, `"leave"`, 1)
				case "selected-change":
					c := f.r.Profiles["alpha"]
					c.Store.AccessTokenService += ".changed"
					f.r.Profiles["alpha"] = c
					f.save()
				case "current-unrelated":
					f.r.Current = "beta"
					c := f.r.Profiles["beta"]
					c.Store.AccessTokenService += ".changed"
					f.r.Profiles["beta"] = c
					f.save()
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
			})}
			args := []string{"room", "members", "!room:fixture", "--json"}
			if kind == "invalid-filter" {
				args = append(args, "--membership", "leave")
			}
			if kind == "missing-room" {
				args = []string{"room", "members"}
			}
			before := memberFiles(t, f.root)
			code := a.Run(context.Background(), args)
			after := memberFiles(t, f.root)
			if kind == "selected-change" || kind == "current-unrelated" {
				delete(before, "connections.json")
				delete(after, "connections.json")
			}
			if !reflect.DeepEqual(before, after) || len(f.store.creates) != 0 {
				t.Fatal("read changed protected files/store")
			}
			if kind == "current-unrelated" {
				if code != 0 || f.out.Len() == 0 {
					t.Fatalf("unrelated edit broke selected read: %d %s", code, f.err.String())
				}
				return
			}
			if code == 0 || f.out.Len() != 0 {
				t.Fatalf("failure printed roster: exit%d %s %s", code, f.out.String(), f.err.String())
			}
			if kind == "missing-metadata" || kind == "invalid-filter" || kind == "missing-room" || kind == "binding-mismatch" {
				if calls != 0 || len(f.store.reads) != 0 {
					t.Fatal("early refusal performed lookup/request")
				}
			}
			for _, secret := range []string{"private-error", "private-name", "isolated-"} {
				if strings.Contains(f.err.String(), secret) {
					t.Fatal("private diagnostic escaped")
				}
			}
			want := map[string]string{"permission": "permission denied", "auth": "access token", "network": "request failed", "malformed": "malformed membership", "late-malformed-row": "malformed membership", "incomplete": "incomplete membership", "archived": "current roster unverified", "selected-change": "selected connection changed"}[kind]
			if want != "" && !strings.Contains(f.err.String(), want) {
				t.Fatalf("missing error class %q: %s", want, f.err.String())
			}
		})
	}
}

// Proves the no-write request instrument detects an intentionally planted
// forbidden write in the same synthetic transport; no live server is used.
func TestRoomMembersReadInstrumentControl(t *testing.T) {
	writes := 0
	reads := 0
	c := matrix.New("http://synthetic.invalid", "synthetic-token")
	c.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
		body := membershipReply("alpha", false)
		if r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/members") {
			reads++
		} else {
			writes++
			body = `{"event_id":"$plant"}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
	})}
	if _, e := c.CurrentRoomMembers(context.Background(), "!room:fixture", "@alice:alpha.invalid"); e != nil || reads != 1 || writes != 0 {
		t.Fatalf("valid instrument: %v %d %d", e, reads, writes)
	}
	if _, e := c.PutState(context.Background(), "!room:fixture", "synthetic.plant", "", map[string]any{}); e != nil {
		t.Fatal(e)
	}
	if writes != 1 {
		t.Fatal("instrument missed planted forbidden write")
	}
}

// Proves registry-free callers retain the existing session/alias routing and
// make only the alias lookup plus membership GET, without local state changes.
func TestRoomMembersLegacyAlias(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	calls := 0
	e.configure = func(a *App) {
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			body := ""
			switch {
			case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/directory/room/#project:"+testServer:
				body = `{"room_id":"!room:fixture","servers":[]}`
			case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/!room:fixture/members":
				body = strings.ReplaceAll(membershipReply("alpha", false), "alpha.invalid", testServer)
			default:
				t.Fatalf("forbidden request: %s %s", r.Method, r.URL.Path)
			}
			if r.Header.Get("Authorization") != "Bearer "+e.tokens["alice"] {
				t.Fatal("wrong legacy authority")
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
		})}
	}
	before := memberFiles(t, e.dir)
	r := e.run("--as", "alice", "room", "members", "project", "--membership", "invite", "--json")
	if r.code != 0 || calls != 2 || !strings.Contains(r.stdout, `"members":[]`) {
		t.Fatalf("legacy read: %+v calls%d", r, calls)
	}
	if !reflect.DeepEqual(before, memberFiles(t, e.dir)) {
		t.Fatal("legacy read changed local files")
	}
}
