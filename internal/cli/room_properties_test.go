package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func propertyCLIReply(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r, Header: make(http.Header)}
}

// Proves exact foreign/own alias routing and opaque IDs, fixed state type/key,
// canonical true AND false, one resolution per alias, and no other effects.
// Fixture authority and transport do not qualify live membership or power.
func TestRoomPropertyCLISet(t *testing.T) {
	for _, tc := range []struct{ name, room, parent, label, canonical string }{
		{"avatar-opaque", "!target", "", "alpha", ""},
		{"avatar-foreign", "#same:foreign.invalid", "", "beta", ""},
		{"avatar-own-alias", "same", "", "alpha", ""},
		{"parent-true", "!target", "!space:foreign.invalid", "alpha", "true"},
		{"parent-false-foreign", "#same:foreign.invalid", "#space:elsewhere.invalid", "beta", "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			before := memberFiles(t, f.root)
			a := f.app()
			aliases, puts, gets := 0, 0, 0
			var content string
			statePath := "/_matrix/client/v3/rooms/!target/state/m.room.avatar/"
			args := []string{"room", "avatar", "set", tc.room, "--expect-room-id", "!target", "--mxc", "mxc://media.invalid/avatar", "--json"}
			if tc.parent != "" {
				statePath = "/_matrix/client/v3/rooms/!target/state/m.space.parent/!space:foreign.invalid"
				args = []string{"room", "parent", "set", tc.room, "--parent", tc.parent, "--expect-child-id", "!target", "--expect-parent-id", "!space:foreign.invalid", "--via", "foreign.invalid", "--via", "[::1]:8448", "--canonical", tc.canonical, "--json"}
			}
			args = append([]string{"--profile", tc.label}, args...)
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles[tc.label].URL || r.Header.Get("Authorization") != "Bearer isolated-"+tc.label || r.URL.RawQuery != "" {
					t.Fatal("authority changed")
				}
				if strings.Contains(r.URL.Path, "/directory/room/") {
					aliases++
					want := "#same:foreign.invalid"
					id := "!target"
					if tc.room == "same" {
						want = "#same:" + tc.label + ".invalid"
					}
					if strings.HasSuffix(r.URL.Path, "#space:elsewhere.invalid") {
						want, id = "#space:elsewhere.invalid", "!space:foreign.invalid"
					}
					if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/directory/room/"+want {
						t.Fatalf("wrong alias %s", r.URL)
					}
					return propertyCLIReply(r, 200, `{"room_id":"`+id+`"}`), nil
				}
				if r.URL.Path != statePath {
					t.Fatalf("forbidden target/key %s", r.URL.Path)
				}
				switch r.Method {
				case "PUT":
					puts++
					b, _ := io.ReadAll(r.Body)
					content = string(b)
					return propertyCLIReply(r, 200, `{"event_id":"$ack"}`), nil
				case "GET":
					gets++
					return propertyCLIReply(r, 200, content), nil
				default:
					t.Fatalf("forbidden method %s", r.Method)
				}
				return nil, nil
			})}
			if code := a.Run(context.Background(), args); code != 0 {
				t.Fatalf("exit%d %s", code, f.err.String())
			}
			wantAliases := 0
			if tc.room[0] != '!' {
				wantAliases++
			}
			if tc.parent != "" && tc.parent[0] != '!' {
				wantAliases++
			}
			if puts != 1 || gets != 1 || aliases != wantAliases || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatalf("extra/missing effects: alias%d PUT%d GET%d", aliases, puts, gets)
			}
			var got map[string]any
			if err := json.Unmarshal(f.out.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["version"] != float64(1) || got["room_id"] != "!target" || got["outcome"] != "VERIFIED" || got["acknowledged"] != true || got["event_id"] != "$ack" || got["readback"] != "match" || got["caller"] != "@alice:"+tc.label+".invalid" {
				t.Fatalf("result %s", f.out.String())
			}
			if got["http_status"] != nil || got["errcode"] != nil {
				t.Fatal("successful set emitted refusal diagnostics")
			}
			var sent map[string]any
			json.Unmarshal([]byte(content), &sent)
			if tc.parent == "" {
				if len(sent) != 1 || sent["url"] != "mxc://media.invalid/avatar" || got["state_key"] != "" {
					t.Fatalf("wrong avatar %v", sent)
				}
			} else if len(sent) != 2 || sent["canonical"] != (tc.canonical == "true") || !reflect.DeepEqual(sent["via"], []any{"foreign.invalid", "[::1]:8448"}) || got["state_key"] != "!space:foreign.invalid" {
				t.Fatalf("wrong parent %v", sent)
			}
		})
	}
}

// Proves JSON and text retain the exact allowlisted refusal pair for either
// SET, with a nonzero exit, no credential mutation, retry or private body text.
func TestRoomPropertyCLIRefusalDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{{403, "M_FORBIDDEN"}, {401, "M_UNKNOWN_TOKEN"}, {401, "M_MISSING_TOKEN"}} {
		for _, parent := range []bool{false, true} {
			for _, asJSON := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/parent=%t/json=%t", tc.code, parent, asJSON), func(t *testing.T) {
					f := newNamedFixture(t, true)
					before := memberFiles(t, f.root)
					a := f.app()
					calls := 0
					path := "/_matrix/client/v3/rooms/!target/state/m.room.avatar/"
					args := []string{"room", "avatar", "set", "!target", "--expect-room-id", "!target", "--mxc", "mxc://media.invalid/avatar"}
					if parent {
						path = "/_matrix/client/v3/rooms/!target/state/m.space.parent/!space"
						args = []string{"room", "parent", "set", "!target", "--parent", "!space", "--expect-child-id", "!target", "--expect-parent-id", "!space", "--via", "foreign.invalid", "--canonical", "true"}
					}
					if asJSON {
						args = append(args, "--json")
					}
					a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
						calls++
						if calls != 1 || r.Method != "PUT" || r.URL.Path != path || r.Header.Get("Authorization") != "Bearer isolated-alpha" {
							t.Fatal("refusal retried or changed target/authority")
						}
						return propertyCLIReply(r, tc.status, `{"errcode":"`+tc.code+`","error":"private-server-secret"}`), nil
					})}
					if exit := a.Run(context.Background(), args); exit != ExitError {
						t.Fatalf("refusal exit=%d", exit)
					}
					if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
						t.Fatal("refusal changed credentials or caused extra effects")
					}
					if strings.Contains(f.out.String()+f.err.String(), "private-server-secret") || strings.Contains(f.out.String()+f.err.String(), "isolated-alpha") {
						t.Fatal("private server or authentication text escaped")
					}
					if asJSON {
						var got map[string]any
						if err := json.Unmarshal(f.out.Bytes(), &got); err != nil {
							t.Fatal(err)
						}
						if got["version"] != float64(1) || got["http_status"] != float64(tc.status) || got["errcode"] != tc.code || got["outcome"] != "REFUSED" || got["acknowledged"] != false || got["readback"] != "not-attempted" || got["content"] != nil || got["event_id"] != nil {
							t.Fatalf("refusal projection mismatch: %s", f.out.String())
						}
					} else if !strings.Contains(f.out.String(), fmt.Sprintf("http_status=%d errcode=%s", tc.status, strconv.QuoteToASCII(tc.code))) || !strings.Contains(f.out.String(), "outcome=REFUSED acknowledged=false") {
						t.Fatalf("text lost bounded refusal pair: %s", f.out.String())
					}
				})
			}
		}
	}
}

// Proves same-name and wrong-ID neighbors stop before any state read/write;
// removing the expected-ID conjunct must make these assertions fail.
func TestRoomPropertyExpectedID(t *testing.T) {
	for _, which := range []string{"avatar", "child", "parent"} {
		t.Run(which, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			before := memberFiles(t, f.root)
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || !strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/directory/room/") {
					t.Fatal("wrong target reached state endpoint")
				}
				return propertyCLIReply(r, 200, `{"room_id":"!other-same-name"}`), nil
			})}
			args := []string{"room", "avatar", "set", "#same:foreign.invalid", "--expect-room-id", "!intended", "--mxc", "mxc://media.invalid/avatar", "--json"}
			if which != "avatar" {
				child, parent := "#same:foreign.invalid", "!space"
				if which == "parent" {
					child, parent = "!intended", "#space:foreign.invalid"
				}
				args = []string{"room", "parent", "set", child, "--parent", parent, "--expect-child-id", "!intended", "--expect-parent-id", "!space", "--via", "foreign.invalid", "--canonical", "false", "--json"}
			}
			if code := a.Run(context.Background(), args); code == 0 || !strings.Contains(f.err.String(), "differs from expected ID") || f.out.Len() != 0 || calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
				t.Fatalf("not a zero-write target refusal: code%d out%s err%s", code, f.out.String(), f.err.String())
			}
		})
	}
}

// Proves missing/ambiguous write inputs refuse before credentials or HTTP;
// successful true/false controls are covered above, so reject-all is insufficient.
func TestRoomPropertyUsage(t *testing.T) {
	avatar := []string{"room", "avatar", "set", "!target", "--expect-room-id", "!target", "--mxc", "mxc://media.invalid/a"}
	parent := []string{"room", "parent", "set", "!target", "--parent", "!space", "--expect-child-id", "!target", "--expect-parent-id", "!space", "--via", "foreign.invalid", "--canonical", "false"}
	cases := [][]string{
		{"room", "avatar", "set", "!target"},
		{"room", "avatar", "set", "!target", "--mxc", "mxc://media.invalid/a"},
		{"room", "parent", "set", "!target", "--parent", "!space"},
		append(append([]string{}, avatar...), "--mxc", "mxc://media.invalid/b"),
		append(append([]string{}, avatar...), "--expect-room-id", "!target"),
		append(append([]string{}, avatar...), "--json", "--json"),
		append(append([]string{}, avatar[:6]...), "--mxc", "https://media.invalid/a"),
		append(append([]string{}, parent...), "--canonical", "true"),
		append(append([]string{}, parent...), "--parent", "!other"),
		append(append([]string{}, parent[:len(parent)-2]...), "--canonical", "1"),
		append(append([]string{}, parent[:len(parent)-2]...), "--canonical", ""),
		parent[:len(parent)-2],
		{"room", "avatar", "get", "!target", "--state-key", "other"},
		{"room", "parent", "get", "!target"},
	}
	for i, args := range cases {
		f := newNamedFixture(t, true)
		a := f.app()
		if code := a.Run(context.Background(), args); code != ExitUsage || f.effects() != 0 || f.out.Len() != 0 {
			t.Fatalf("case%d code%d effects%d out%s err%s", i, code, f.effects(), f.out.String(), f.err.String())
		}
	}
}

// Proves an acknowledged lost/mismatched readback retains event_id, never
// retries PUT, and a selected connection change cannot yield VERIFIED.
func TestRoomPropertyCLIUnknown(t *testing.T) {
	for _, kind := range []string{"mismatch", "selected-change", "lost-ack", "upload-only"} {
		t.Run(kind, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			puts, gets := 0, 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/_matrix/client/v3/rooms/!target/state/m.room.avatar/" {
					t.Fatal("wrong effect")
				}
				if r.Method == "PUT" {
					puts++
					if kind == "selected-change" {
						c := f.r.Profiles["alpha"]
						c.URL += "/changed"
						f.r.Profiles["alpha"] = c
						f.save()
					}
					if kind == "lost-ack" {
						return propertyCLIReply(r, 200, `{`), nil
					}
					if kind == "upload-only" {
						return propertyCLIReply(r, 200, `{"content_uri":"mxc://media.invalid/a"}`), nil
					}
					return propertyCLIReply(r, 200, `{"event_id":"$ack"}`), nil
				}
				gets++
				return propertyCLIReply(r, 200, `{"url":"mxc://media.invalid/other"}`), nil
			})}
			code := a.Run(context.Background(), []string{"room", "avatar", "set", "!target", "--expect-room-id", "!target", "--mxc", "mxc://media.invalid/a", "--json"})
			var got map[string]any
			json.Unmarshal(f.out.Bytes(), &got)
			acked := kind == "mismatch" || kind == "selected-change"
			wantGets := 0
			if kind == "mismatch" {
				wantGets = 1
			}
			if code != ExitError || got["outcome"] != "UNKNOWN" || got["acknowledged"] != acked || puts != 1 || gets != wantGets {
				t.Fatalf("code%d result%s PUT%d GET%d", code, f.out.String(), puts, gets)
			}
			if got["http_status"] != nil || got["errcode"] != nil {
				t.Fatal("uncertain write emitted refusal diagnostics")
			}
			if acked && got["event_id"] != "$ack" {
				t.Fatal("ACK was lost")
			}
		})
	}
}

// Proves read output is a bounded projection: omitted canonical remains absent,
// extensions/auth text stay out, and no write or unrelated state read occurs.
func TestRoomPropertyCLIGet(t *testing.T) {
	for _, parent := range []bool{false, true} {
		f := newNamedFixture(t, true)
		a := f.app()
		before := memberFiles(t, f.root)
		calls := 0
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			path, body := "/_matrix/client/v3/rooms/!target/state/m.room.avatar/", `{"url":"mxc://media.invalid/a","private":"discarded"}`
			if parent {
				path, body = "/_matrix/client/v3/rooms/!target/state/m.space.parent/!space", `{"via":["foreign.invalid"],"private":"discarded"}`
			}
			if r.Method != "GET" || r.URL.Path != path {
				t.Fatal("forbidden read/write")
			}
			return propertyCLIReply(r, 200, body), nil
		})}
		args := []string{"room", "avatar", "get", "!target", "--json"}
		if parent {
			args = []string{"room", "parent", "get", "!target", "--parent", "!space", "--json"}
		}
		if code := a.Run(context.Background(), args); code != 0 || calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
			t.Fatalf("code%d calls%d %s", code, calls, f.err.String())
		}
		for _, forbidden := range []string{"canonical", "private", "discarded", "isolated-"} {
			if strings.Contains(f.out.String(), forbidden) {
				t.Fatalf("invented/private data %s", f.out.String())
			}
		}
	}
}

// Proves opaque identifier controls cannot inject text output rows; JSON keeps
// their exact bytes. Both use the same fixed escaped state endpoint.
func TestRoomPropertyTextIdentifiers(t *testing.T) {
	id := "!historical\n\x1b[31m/path"
	for _, asJSON := range []bool{false, true} {
		f := newNamedFixture(t, true)
		a := f.app()
		calls := 0
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "GET" || r.URL.EscapedPath() != "/_matrix/client/v3/rooms/%21historical%0A%1B%5B31m%2Fpath/state/m.room.avatar/" {
				t.Fatalf("wrong encoded path %s", r.URL.EscapedPath())
			}
			return propertyCLIReply(r, 200, `{}`), nil
		})}
		args := []string{"room", "avatar", "get", id}
		if asJSON {
			args = append(args, "--json")
		}
		if code := a.Run(context.Background(), args); code != 0 || calls != 1 {
			t.Fatalf("code%d calls%d %s", code, calls, f.err.String())
		}
		if asJSON {
			var got map[string]any
			if err := json.Unmarshal(f.out.Bytes(), &got); err != nil || got["room_id"] != id {
				t.Fatalf("identifier changed: %s", f.out.String())
			}
		} else if strings.Count(f.out.String(), "\n") != 1 || strings.Contains(f.out.String(), "\x1b") || !strings.Contains(f.out.String(), strconv.QuoteToASCII(id)) {
			t.Fatalf("unsafe text %q", f.out.String())
		}
	}
}
