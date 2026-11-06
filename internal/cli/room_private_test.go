package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

const privateRoomStateRoute = "GET /_matrix/client/v3/rooms/{room}/state"

// Every policy case drives App.Run against fake HTTP, mutating the server's
// stored state AFTER createRoom, not merely inspecting the outgoing request.
func TestRoomPrivateCreation(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.agent("bob")
	e.as("alice", "product", "new", "alpha").ok(t)
	reads := e.hs.CallCount(privateRoomStateRoute)
	out := e.as("alice", "room", "new", "team", "--private", "--product", "alpha", "--name", "Team", "--topic", "Work", "--invite", "bob,alice,bob").ok(t).stdout
	if !strings.Contains(out, "verified private policy #team !teamRoomID") || !strings.Contains(out, "preset private_chat") ||
		!strings.Contains(out, "invited @bob:test.local") || strings.Contains(out, "invited @alice:test.local") {
		t.Fatalf("private creation output: %s", out)
	}
	if e.hs.CallCount(privateRoomStateRoute) <= reads {
		t.Fatal("no actual server policy readback")
	}
	var rule, history, guest map[string]string
	for key, dst := range map[string]*map[string]string{"m.room.join_rules|": &rule, "m.room.history_visibility|": &history, "m.room.guest_access|": &guest} {
		if err := e.hs.muTryState("!teamRoomID", key, dst); err != nil {
			t.Fatal(err)
		}
	}
	if rule["join_rule"] != "invite" || history["history_visibility"] != "joined" || guest["guest_access"] != "forbidden" {
		t.Fatalf("stored policy: %v %v %v", rule, history, guest)
	}
	e.hs.mu.Lock()
	room := e.hs.rooms["!teamRoomID"]
	_, child := e.hs.rooms["!alphaRoomID"].state["m.space.child|!teamRoomID"]
	_, parent := room.state["m.space.parent|!alphaRoomID"]
	if room.members[alice] != "join" || room.members["@bob:test.local"] != "invite" || !child || !parent {
		t.Error("private room membership or existing product links absent")
	}
	e.hs.mu.Unlock()
	if len(e.subsOf("alice")) == 0 {
		t.Fatal("creator was not subscribed")
	}
	before := len(e.hs.createdRooms())
	if again := e.as("alice", "room", "new", "team", "--private").ok(t).stdout; again != "exists #team !teamRoomID\n" || len(e.hs.createdRooms()) != before {
		t.Fatalf("existing qualified room: %q", again)
	}
}

func TestRoomPrivateServerPolicyRefusals(t *testing.T) {
	cases := map[string]map[string]string{
		"valid":           {},
		"public-join":     {"m.room.join_rules|": `{"join_rule":"public"}`},
		"world-history":   {"m.room.history_visibility|": `{"history_visibility":"world_readable"}`},
		"shared-history":  {"m.room.history_visibility|": `{"history_visibility":"shared"}`},
		"invited-history": {"m.room.history_visibility|": `{"history_visibility":"invited"}`},
		"guest-allowed":   {"m.room.guest_access|": `{"guest_access":"can_join"}`},
		"missing-guest":   {"m.room.guest_access|": ""},
		"missing-rule":    {"m.room.join_rules|": ""},
		"null-history":    {"m.room.history_visibility|": `{"history_visibility":null}`},
		"array-rule":      {"m.room.join_rules|": `[]`},
		"encrypted":       {"m.room.encryption|": `{"algorithm":"m.megolm.v1.aes-sha2"}`},
		"missing-power":   {"m.room.power_levels|": ""},
		"redact-lowered":  {"m.room.power_levels|": `{"redact":50}`},
		"power-grant":     {"m.room.power_levels|": `{"redact":100,"users":{"@bob:test.local":100}}`},
		"power-default":   {"m.room.power_levels|": `{"redact":100,"users_default":50}`},
		"state-lowered":   {"m.room.power_levels|": `{"redact":100,"state_default":0}`},
		"control-lowered": {"m.room.power_levels|": `{"redact":100,"events":{"m.room.join_rules":0}}`},
		"power-malformed": {"m.room.power_levels|": `{"redact":"100"}`},
		"power-null":      {"m.room.power_levels|": `{"redact":100,"users":null}`},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.agent("alice")
			e.agent("bob")
			e.as("alice", "product", "new", "alpha").ok(t)
			before := len(e.hs.createdRooms())
			invites := e.hs.CallCount(inviteRoute)
			subs := len(e.subsOf("alice"))
			hits := 0
			e.configure = func(a *App) {
				base := a.HTTP.Transport
				a.HTTP = &http.Client{Transport: roomPolicyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodGet && r.URL.Path == "/_matrix/client/v3/rooms/!teamRoomID/state" {
						hits++
						e.hs.mu.Lock()
						for key, value := range change {
							if value == "" {
								delete(e.hs.rooms["!teamRoomID"].state, key)
							} else {
								e.hs.rooms["!teamRoomID"].state[key] = json.RawMessage(value)
							}
						}
						e.hs.mu.Unlock()
					}
					return base.RoundTrip(r)
				})}
			}
			r := e.as("alice", "room", "new", "team", "--private", "--product", "alpha", "--invite", "bob")
			if hits != 1 || len(e.hs.createdRooms()) != before+1 {
				t.Fatalf("not a created-room readback: hits=%d", hits)
			}
			if name == "valid" {
				r.ok(t)
				return
			}
			r.failsWith(t, ExitError, "created #team !teamRoomID, but not qualified as private; room retained for inspection")
			assertPrivateRefusal(t, e, r, alice, invites, subs)
		})
	}
}

func assertPrivateRefusal(t *testing.T, e *env, r result, alice string, invites, subs int) {
	t.Helper()
	if strings.Contains(r.stdout, "verified private") || strings.Contains(r.stdout, "preset private_chat") || e.hs.CallCount(inviteRoute) != invites || len(e.subsOf("alice")) != subs {
		t.Fatal("unqualified room reported success or triggered invites/subscription")
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	room := e.hs.rooms["!teamRoomID"]
	if room == nil || room.members[alice] != "join" || e.hs.aliases["#team:"+testServer] != "!teamRoomID" {
		t.Fatal("created room was destructively cleaned up")
	}
	if _, linked := room.state["m.space.parent|!alphaRoomID"]; linked {
		t.Fatal("unqualified room attached to product")
	}
	if space := e.hs.rooms["!alphaRoomID"]; space != nil {
		if _, linked := space.state["m.space.child|!teamRoomID"]; linked {
			t.Fatal("unqualified room attached as a child")
		}
	}
	if e.hs.CallCount("DELETE /_matrix/client/v3/directory/room/{alias}") != 0 || e.hs.CallCount("POST /_matrix/client/v3/rooms/{room}/leave") != 0 {
		t.Fatal("destructive cleanup request")
	}
}

func TestRoomPrivateReadbackWireRefusals(t *testing.T) {
	for _, kind := range []string{"valid", "forbidden", "malformed", "trailing", "duplicate-content", "duplicate-state", "wrong-key", "wrong-room", "redirect"} {
		t.Run(kind, func(t *testing.T) {
			e := newEnv(t)
			alice := e.agent("alice")
			hits := 0
			e.configure = func(a *App) {
				base := a.HTTP.Transport
				a.HTTP = &http.Client{Transport: roomPolicyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := base.RoundTrip(r)
					if err != nil || r.URL.Path != "/_matrix/client/v3/rooms/!teamRoomID/state" {
						return resp, err
					}
					hits++
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					switch kind {
					case "forbidden":
						resp.StatusCode = http.StatusForbidden
					case "malformed":
						raw = []byte(`[{`)
					case "trailing":
						raw = append(raw, []byte(` {}`)...)
					case "duplicate-content":
						raw = bytes.Replace(raw, []byte(`"join_rule":"invite"`), []byte(`"join_rule":"public","join_rule":"invite"`), 1)
					case "duplicate-state":
						raw = append(raw[:len(raw)-1], []byte(`,{"type":"m.room.join_rules","state_key":"","content":{"join_rule":"invite"}}]`)...)
					case "wrong-key":
						raw = bytes.ReplaceAll(raw, []byte(`"state_key":""`), []byte(`"state_key":"other"`))
					case "wrong-room":
						raw = bytes.Replace(raw, []byte(`"type":"m.room.join_rules"`), []byte(`"room_id":"!other","type":"m.room.join_rules"`), 1)
					case "redirect":
						resp.StatusCode = http.StatusTemporaryRedirect
						resp.Header.Set("Location", e.hs.srv.URL+"/unexpected-policy-redirect")
					}
					resp.Body = io.NopCloser(bytes.NewReader(raw))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
			}
			r := e.as("alice", "room", "new", "team", "--private")
			if hits != 1 {
				t.Fatalf("policy requests %d", hits)
			}
			if kind == "valid" {
				r.ok(t)
				return
			}
			r.failsWith(t, ExitError, "room retained for inspection")
			assertPrivateRefusal(t, e, r, alice, 0, 0)
		})
	}
}

func TestRoomPrivatePublicNeighbor(t *testing.T) {
	var bodies [][]byte
	var outputs []string
	for _, flags := range [][]string{nil, {"--private=false"}} {
		e := newEnv(t)
		e.agent("alice")
		e.configure = func(a *App) {
			base := a.HTTP.Transport
			a.HTTP = &http.Client{Transport: roomPolicyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/createRoom") {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						return nil, err
					}
					r.Body.Close()
					bodies = append(bodies, raw)
					r.Body = io.NopCloser(bytes.NewReader(raw))
				}
				return base.RoundTrip(r)
			})}
		}
		outputs = append(outputs, e.as("alice", append([]string{"room", "new", "team"}, flags...)...).ok(t).stdout)
		if e.hs.CallCount(privateRoomStateRoute) != 0 {
			t.Fatal("default public behavior gained policy reads")
		}
	}
	expected := `{"preset":"public_chat","visibility":"private","room_alias_name":"team","power_level_content_override":{"redact":100,"users":{}}}`
	if len(bodies) != 2 || string(bodies[0]) != expected || !bytes.Equal(bodies[0], bodies[1]) {
		t.Fatalf("public request/output changed: bodies=%q outputs=%q", bodies, outputs)
	}
	// Subscription IDs are freshly random on both paths; pin every other byte.
	creation := "created #team !teamRoomID (preset public_chat, visibility private: not in the room directory; power: redact 100, creator @alice:test.local implicit)\n"
	subscription := regexp.MustCompile(`^subscribed sub-[a-f0-9]{8}\troom=#team kinds=system,owner-needed,decision\n$`)
	for _, out := range outputs {
		if !strings.HasPrefix(out, creation) || !subscription.MatchString(strings.TrimPrefix(out, creation)) {
			t.Fatalf("public output changed: %q", out)
		}
	}
}

func TestRoomPrivateExistingAndNoCreation(t *testing.T) {
	for _, mode := range []string{"existing", "alias-race", "resolve-refused", "create-refused", "usage"} {
		t.Run(mode, func(t *testing.T) {
			e := newEnv(t)
			alice := e.agent("alice")
			if mode == "existing" {
				e.hs.addRoom("team", alice)
			}
			if mode == "alias-race" {
				e.configure = func(a *App) {
					base := a.HTTP.Transport
					a.HTTP = &http.Client{Transport: roomPolicyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
						if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/createRoom") {
							e.hs.addRoom("team", alice)
						}
						return base.RoundTrip(r)
					})}
				}
			}
			if mode == "resolve-refused" {
				e.hs.fail("GET /_matrix/client/v3/directory/room/{alias}", 403, "M_FORBIDDEN", "refused")
			}
			if mode == "create-refused" {
				e.hs.fail(createRoute, 403, "M_FORBIDDEN", "refused")
			}
			alias := "team"
			if mode == "usage" {
				alias = "!invalid"
			}
			r := e.as("alice", "room", "new", alias, "--private")
			if mode == "usage" {
				r.failsWith(t, ExitUsage, "invalid alias")
			} else {
				r.failsWith(t, ExitError, "")
			}
			if len(e.hs.createdRooms()) != 0 || strings.Contains(r.stderr, "room retained for inspection") || len(e.subsOf("alice")) != 0 {
				t.Fatal("no-creation failure confused with created-but-unqualified")
			}
			if mode == "existing" || mode == "alias-race" {
				r.failsWith(t, ExitError, "existing room #team !teamRoomID not qualified as private; room unchanged")
			}
		})
	}
}
