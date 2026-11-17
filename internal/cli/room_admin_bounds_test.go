package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Entry/count/aggregate checks must name their own invariant before secrets or
// HTTP. The 262-byte neighbor also exceeds the retained grammar's maximum;
// the 4096/4097 aggregate pair independently satisfies every entry predicate.
func TestRoomAdminViaRefusals(t *testing.T) {
	for _, tc := range []struct {
		name     string
		via      []string
		location string
	}{
		{"none", nil, "$.via"},
		{"seventeen", strings.Fields(strings.Repeat("server.invalid ", 17)), "$.via"},
		{"entry262", []string{strings.Repeat("h", 256) + ":12345"}, "$.via[0]"},
		{"second-entry", []string{"server.invalid", "/" + adminMarker}, "$.via[1]"},
		{"bad-port", []string{"server.invalid:123456"}, "$.via[0]"},
		{"empty", []string{""}, "$.via[0]"},
		{"aggregate4097", func() []string {
			v := make([]string, 16)
			for i := range v {
				v[i] = strings.Repeat("h", 250) + ":12345"
			}
			v[15] = strings.Repeat("h", 251) + ":12345"
			return v
		}(), "$.via"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			args := adminArgs(true, "remove")
			args[2] = "add"
			for _, via := range tc.via {
				args = append(args, "--via", via)
			}
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(*http.Request) (*http.Response, error) {
				calls++
				t.Fatal("invalid via reached HTTP")
				return nil, nil
			})}
			if code := a.Run(t.Context(), args); code != 2 || f.err.String() != "RULE admin_via: "+tc.location+"\n" || f.out.Len() != 0 || calls != 0 || len(f.store.reads) != 0 {
				t.Fatalf("via invariant exit%d err%q", code, f.err.String())
			}
		})
	}
}

// Inclusive entity limits have one-byte forbidden neighbors. Valid boundaries
// use inert JSON whitespace, so size is the only changing predicate.
func TestRoomAdminEntityBounds(t *testing.T) {
	for _, stage := range []string{"get", "policy", "ack"} {
		for _, over := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/over%t", stage, over), func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				op := "set"
				if stage == "get" {
					op = "get"
				}
				puts, props := 0, 0
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					body := `{"name":"Example"}`
					limit := 65536
					selected := false
					if strings.HasSuffix(r.URL.Path, "/state") {
						body = adminPolicy()
						limit = 1048576
						selected = stage == "policy"
					} else if r.Method == "PUT" {
						puts++
						body = `{"event_id":"$ok"}`
						selected = stage == "ack"
					} else {
						props++
						selected = stage == "get"
					}
					if selected {
						n := limit
						if over {
							n++
						}
						body += strings.Repeat(" ", n-len(body))
					}
					return propertyCLIReply(r, 200, body), nil
				})}
				code := a.Run(t.Context(), append(adminArgs(false, op), "--json"))
				if !over {
					if code != 0 {
						t.Fatalf("inclusive boundary exit%d %s", code, f.err.String())
					}
					return
				}
				if code != 1 {
					t.Fatal("oversize admitted")
				}
				if stage == "policy" {
					if puts != 0 || props != 0 || f.out.Len() != 0 || f.err.String() != "RULE admin_policy: $.policy\n" {
						t.Fatal("policy cap had property effects")
					}
				} else {
					m, e := decodeAdminRecord(f.out.Bytes())
					if e != nil || m["outcome"] != "UNKNOWN" || stage == "ack" && props != 0 {
						t.Fatal("cap became success or extra readback")
					}
				}
			})
		}
	}
}

// Creator/default/version branches are exercised at the new mutation boundary;
// legacy room-power compatibility has its unchanged, broader fixture matrix.
func TestRoomAdminCreatorNeighbors(t *testing.T) {
	for _, tc := range []struct {
		name, version, creator, member, extra string
		allowed                               bool
	}{
		{"v12-joined", "12", "@alice:alpha.invalid", "join", "", true},
		{"v12-left", "12", "@alice:alpha.invalid", "leave", "", false},
		{"v12-additional", "12", "@founder:example.invalid", "join", `"additional_creators":["@alice:alpha.invalid"],`, true},
		{"v11-initial", "11", "@alice:alpha.invalid", "join", "", true},
		{"v10-initial", "10", "@alice:alpha.invalid", "join", `"creator":"@alice:alpha.invalid",`, true},
		{"v1-default", "", "@alice:alpha.invalid", "join", `"creator":"@alice:alpha.invalid",`, true},
		{"unsupported", "13", "@alice:alpha.invalid", "join", "", false},
		{"noncanonical-version", "012", "@alice:alpha.invalid", "join", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			create := tc.extra + `"type":"m.space"`
			if tc.version != "" {
				create += `,"room_version":"` + tc.version + `"`
			}
			body := `[{"type":"m.room.create","state_key":"","sender":"` + tc.creator + `","content":{` + create + `}},{"type":"m.room.member","state_key":"@alice:alpha.invalid","content":{"membership":"` + tc.member + `"}}]`
			f := newNamedFixture(t, true)
			a := f.app()
			puts := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/state") {
					return propertyCLIReply(r, 200, body), nil
				}
				if r.Method == "PUT" {
					puts++
					return propertyCLIReply(r, 200, `{"event_id":"$ok"}`), nil
				}
				return propertyCLIReply(r, 200, `{}`), nil
			})}
			code := a.Run(t.Context(), append(adminArgs(true, "remove"), "--json"))
			if tc.allowed {
				if code != 0 || puts != 1 {
					t.Fatalf("valid creator exit%d puts%d %s", code, puts, f.err.String())
				}
			} else if code != 1 || puts != 0 || f.out.Len() != 0 || f.err.String() != "RULE admin_policy: $.policy\n" {
				t.Fatal("creator policy bypass")
			}
		})
	}
}

// Interpreted getter values have the same byte/control and via boundaries as
// setters, while ordinary opaque extensions remain outside their projection.
func TestRoomAdminInvalidProjectedValues(t *testing.T) {
	for _, child := range []bool{false, true} {
		values := []any{strings.Repeat("x", 256), "\x00", strings.Repeat("é", 128)}
		if child {
			values = []any{[]any{1}, []string{"bad/server"}, strings.Fields(strings.Repeat("server.invalid ", 17))}
		}
		for n, value := range values {
			t.Run(fmt.Sprintf("child%t/%d", child, n), func(t *testing.T) {
				f := newNamedFixture(t, true)
				a := f.app()
				field := "name"
				if child {
					field = "via"
				}
				b, _ := json.Marshal(map[string]any{field: value})
				a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/state") {
						return propertyCLIReply(r, 200, adminPolicy()), nil
					}
					return propertyCLIReply(r, 200, string(b)), nil
				})}
				code := a.Run(t.Context(), append(adminArgs(child, "get"), "--json"))
				m, e := decodeAdminRecord(f.out.Bytes())
				if code != 1 || e != nil || m["outcome"] != "UNKNOWN" || f.err.String() != "RULE admin_state: $.content\n" {
					t.Fatal("invalid projection became a result value")
				}
			})
		}
	}
}

// A real registry change at the entered request guard is a zero-PUT refusal;
// the same drift after acknowledged readback remains UNKNOWN with the real ID.
func TestRoomAdminConnectionDrift(t *testing.T) {
	for _, when := range []string{"guard", "after-ack"} {
		t.Run(when, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			drift := func() { c := f.r.Profiles["alpha"]; c.URL += "/changed"; f.r.Profiles["alpha"] = c; f.save() }
			if when == "guard" {
				a.beforeAdminPropertyRequest = drift
			}
			puts := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/state") {
					return propertyCLIReply(r, 200, adminPolicy()), nil
				}
				if r.Method == "PUT" {
					puts++
					return propertyCLIReply(r, 200, `{"event_id":"$actual"}`), nil
				}
				resp := propertyCLIReply(r, 200, `{"name":"Example"}`)
				resp.Body = adminCloseBody{strings.NewReader(`{"name":"Example"}`), drift}
				return resp, nil
			})}
			code := a.Run(t.Context(), append(adminArgs(false, "set"), "--json"))
			m, e := decodeAdminRecord(f.out.Bytes())
			if code != 1 || e != nil || f.err.String() != "RULE admin_context: $.context\n" {
				t.Fatal("connection drift not safely classified")
			}
			if when == "guard" {
				if puts != 0 || m["outcome"] != "REFUSED" {
					t.Fatal("guard refusal wrote")
				}
			} else if puts != 1 || m["outcome"] != "UNKNOWN" || m["event_id"] != "$actual" || m["readback"] != "unavailable" {
				t.Fatal("late drift erased ACK or verified")
			}
		})
	}
}

// Framing and Close failure are neither absence nor complete content.
func TestRoomAdminReadFailures(t *testing.T) {
	for _, mode := range []string{"length", "close", "read"} {
		t.Run(mode, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				resp := propertyCLIReply(r, 200, `{"name":"Example"}`)
				if mode == "length" {
					resp.ContentLength++
				}
				if mode == "close" {
					resp.Body = adminFailBody{Reader: strings.NewReader(`{"name":"Example"}`)}
				}
				if mode == "read" {
					resp.Body = adminFailBody{Reader: adminFailReader{}}
				}
				return resp, nil
			})}
			if code := a.Run(t.Context(), append(adminArgs(false, "get"), "--json")); code != 1 {
				t.Fatal("failed observation succeeded")
			}
			m, e := decodeAdminRecord(f.out.Bytes())
			if e != nil || m["outcome"] != "UNKNOWN" {
				t.Fatal("failed observation became empty")
			}
		})
	}
}

type adminFailBody struct{ io.Reader }

func (adminFailBody) Close() error { return fmt.Errorf("%s", adminMarker) }

type adminFailReader struct{}

func (adminFailReader) Read([]byte) (int, error) { return 0, fmt.Errorf("%s", adminMarker) }
