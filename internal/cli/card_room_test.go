package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// cardRoomWorld publishes only the caller's isolated fake credentials. Target
// mappings deliberately have no usable credentials in any other profile.
func cardRoomWorld(t *testing.T, ref string) (*env, config.ConnectionRegistry) {
	t.Helper()
	e, _ := world(t)
	root, err := filepath.EvalSymlinks(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.dir = root
	c := config.Connection{ID: "home-id", URL: e.hs.srv.URL, ServerName: testServer, CardRoom: ref, Store: config.CredentialStore{Backend: "file"}}
	r := config.ConnectionRegistry{SchemaVersion: 1, Current: "home", Profiles: map[string]config.Connection{"home": c}}
	saveCardRegistry(t, e, r)
	for _, name := range []string{"alice", "bob"} {
		p, err := c.Paths(config.Paths{Base: e.dir}, name, false)
		if err != nil {
			t.Fatal(err)
		}
		b, err := c.Binding(name, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := config.PublishBoundCredentials(p, name, false, config.Credentials{UserID: b.UserID, Homeserver: c.URL, AccessToken: e.tokens[name], ConnectionBinding: &b}); err != nil {
			t.Fatal(err)
		}
	}
	return e, r
}

func saveCardRegistry(t *testing.T, e *env, r config.ConnectionRegistry) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.dir, "connections.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func putTestCard(t *testing.T, e *env, room, user, body string) {
	t.Helper()
	raw, err := json.Marshal(Card{Name: localpart(user), Format: "text/markdown", Body: body})
	if err != nil {
		t.Fatal(err)
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	e.hs.rooms[room].state[CardEventType+"|"+user] = raw
	if e.hs.rooms[room].stateSenders == nil {
		e.hs.rooms[room].stateSenders = map[string]string{}
	}
	e.hs.rooms[room].stateSenders[CardEventType+"|"+user] = user
}

func assertCardNoMembershipWrites(t *testing.T, e *env) {
	t.Helper()
	for _, route := range []string{joinRoute, inviteRoute, createRoute} {
		tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, route)
	}
}

// Both legacy TestCard and this named-mode control default to home #lobby.
// Explicit aliases and opaque IDs replace the default for both writes and reads.
func TestCardRoomDefaultAndExplicit(t *testing.T) {
	for _, ref := range []string{"", "#cards", "#cards:test.local", "!cardsRoomID"} {
		t.Run("ref="+ref, func(t *testing.T) {
			e, _ := cardRoomWorld(t, ref)
			id := "!lobbyRoomID"
			if ref != "" {
				id = e.hs.addRoom("cards", "@alice:test.local", "@bob:test.local")
			}
			e.stdin = "configured card\n"
			e.as("alice", "card", "set", "--file", "-").ok(t)
			if got := e.as("bob", "card", "get", "alice").ok(t).stdout; got != "configured card\n" {
				t.Fatal(got)
			}
			e.hs.mu.Lock()
			_, present := e.hs.rooms[id].state[CardEventType+"|@alice:test.local"]
			_, inLobby := e.hs.rooms["!lobbyRoomID"].state[CardEventType+"|@alice:test.local"]
			e.hs.mu.Unlock()
			if !present || ref != "" && inLobby {
				t.Fatal("card written outside configured room")
			}
			assertCardNoMembershipWrites(t, e)
		})
	}
}

// A home-room decoy cannot satisfy missing/denied target storage. Repairing the
// exact configured room is the positive control; a missing card remains distinct.
func TestCardRoomUnavailableNoFallback(t *testing.T) {
	for _, ref := range []string{"#missing", "!missingRoomID"} {
		t.Run(ref, func(t *testing.T) {
			e, r := cardRoomWorld(t, ref)
			putTestCard(t, e, "!lobbyRoomID", "@alice:test.local", "wrong fallback")
			e.as("bob", "card", "get", "alice").failsWith(t, ExitError, "card_room_unavailable")
			e.stdin = "do not write"
			e.as("alice", "card", "set", "--file", "-").failsWith(t, ExitError, "card_room_unavailable")
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			assertCardNoMembershipWrites(t, e)
			// Changing public card policy keeps the existing authenticated binding valid.
			c := r.Profiles["home"]
			c.CardRoom = "#cards"
			r.Profiles["home"] = c
			e.hs.addRoom("cards", "@alice:test.local", "@bob:test.local")
			saveCardRegistry(t, e, r)
			e.as("alice", "card", "set", "--file", "-").ok(t)
			e.as("bob", "card", "get", "alice").ok(t)
			e.as("bob", "card", "get", "nobody").failsWith(t, ExitError, "no card for @nobody:test.local in #cards")
		})
	}
}

// The full target server, including a port, selects public metadata. Endpoint
// hostnames, room-ID suffixes, current and unrelated profiles cannot select it.
// A trap endpoint and absent foreign credentials detect profile/token borrowing.
func TestCardRoomCrossServerTargetMapping(t *testing.T) {
	for _, ref := range []string{"", "#cards", "#cards:remote.invalid:8448", "!opaque:else.invalid"} {
		t.Run("ref="+ref, func(t *testing.T) {
			e, r := cardRoomWorld(t, "#homecards")
			home := e.hs.addRoom("homecards", "@alice:test.local", "@bob:test.local")
			target := "remote.invalid:8448"
			user := "@alice:" + target
			putTestCard(t, e, home, user, "wrong home mapping")
			putTestCard(t, e, "!lobbyRoomID", user, "wrong home lobby")
			id := e.hs.addRoom("targetcards", "@bob:test.local")
			alias := "#cards:" + target
			if ref == "" {
				alias = "#lobby:" + target
			}
			e.hs.mu.Lock()
			if strings.HasPrefix(ref, "!") {
				room := e.hs.rooms[id]
				delete(e.hs.rooms, id)
				id = ref
				room.id = id
				e.hs.rooms[id] = room
			}
			e.hs.aliases[alias] = id
			e.hs.mu.Unlock()
			putTestCard(t, e, id, user, "correct remote card")
			var foreignCalls atomic.Int32
			trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				foreignCalls.Add(1)
				http.Error(w, "foreign endpoint must not be used", 500)
			}))
			t.Cleanup(trap.Close)
			r.Profiles["remote"] = config.Connection{ID: "remote-id", URL: trap.URL, ServerName: target, CardRoom: ref, Store: config.CredentialStore{Backend: "keychain", AccessTokenService: "test.foreign.token", PasswordService: "test.foreign.password"}}
			decoy := r.Profiles["remote"]
			decoy.ID = "decoy-id"
			decoy.ServerName = "remote.invalid"
			decoy.CardRoom = "#wrong"
			r.Profiles["decoy"] = decoy
			saveCardRegistry(t, e, r)
			if got := e.as("bob", "card", "get", user).ok(t).stdout; got != "correct remote card\n" {
				t.Fatal(got)
			}
			if foreignCalls.Load() != 0 {
				t.Fatal("used target profile endpoint")
			}
			assertCardNoMembershipWrites(t, e)
			// Localpart shorthand must keep its home-server provenance.
			putTestCard(t, e, home, "@alice:test.local", "correct home card")
			if got := e.as("bob", "card", "get", "alice").ok(t).stdout; got != "correct home card\n" {
				t.Fatal(got)
			}
		})
	}
}

// No target mapping, conflicting mappings and an unavailable mapped alias all
// refuse despite a readable decoy card in home #lobby. Exact repair succeeds.
func TestCardRoomCrossServerClosedMapping(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous", "absent-room"} {
		t.Run(mode, func(t *testing.T) {
			e, r := cardRoomWorld(t, "")
			user := "@alice:remote.invalid"
			putTestCard(t, e, "!lobbyRoomID", user, "wrong fallback")
			remote := config.Connection{ID: "remote-id", URL: e.hs.srv.URL, ServerName: "remote.invalid", CardRoom: "#cards", Store: config.CredentialStore{Backend: "file"}}
			if mode != "missing" {
				r.Profiles["remote"] = remote
			}
			if mode == "ambiguous" {
				other := remote
				other.ID = "other-id"
				other.CardRoom = "#other"
				r.Profiles["other"] = other
			}
			saveCardRegistry(t, e, r)
			e.as("bob", "card", "get", user).failsWith(t, ExitError, "card_room_unavailable")
			if mode != "absent-room" {
				if n := len(e.hs.Calls()); n != 0 {
					t.Fatalf("mapping refusal made %d requests", n)
				}
			}
			assertCardNoMembershipWrites(t, e)
			delete(r.Profiles, "other")
			r.Profiles["remote"] = remote
			id := e.hs.addRoom("targetcards", "@bob:test.local")
			e.hs.mu.Lock()
			e.hs.aliases["#cards:remote.invalid"] = id
			e.hs.mu.Unlock()
			putTestCard(t, e, id, user, "repaired target")
			saveCardRegistry(t, e, r)
			if got := e.as("bob", "card", "get", user).ok(t).stdout; got != "repaired target\n" {
				t.Fatal(got)
			}
			// Equivalent references across two profiles are unambiguous.
			other := remote
			other.ID = "other-id"
			other.CardRoom = "#cards:remote.invalid"
			r.Profiles["other"] = other
			saveCardRegistry(t, e, r)
			e.as("bob", "card", "get", user).ok(t)
		})
	}
}

// A mapping grants no membership. Card operations never autojoin, even public
// rooms. Explicit membership is the successful control; write power still binds.
func TestCardRoomMembershipAndPower(t *testing.T) {
	e, _ := cardRoomWorld(t, "#cards")
	id := e.hs.addRoom("cards", "@alice:test.local")
	e.hs.makePublic(id)
	putTestCard(t, e, id, "@alice:test.local", "private card")
	e.as("bob", "card", "get", "alice").failsWith(t, ExitError, "card_room_unavailable")
	e.stdin = "bob card"
	e.as("bob", "card", "set", "--file", "-").failsWith(t, ExitError, "card_room_unavailable")
	assertCardNoMembershipWrites(t, e)
	e.as("bob", "join", "cards").ok(t)
	e.as("bob", "card", "get", "alice").ok(t)
	e.hs.mu.Lock()
	e.hs.rooms[id].state["m.room.power_levels|"] = json.RawMessage(`{"state_default":50}`)
	e.hs.mu.Unlock()
	e.as("bob", "card", "set", "--file", "-").failsWith(t, ExitError, `power_levels.events["ai.relux.a2a.card"] = 0`)
	e.hs.mu.Lock()
	e.hs.rooms[id].state["m.room.power_levels|"] = json.RawMessage(`{"events":{"ai.relux.a2a.card":0}}`)
	e.hs.mu.Unlock()
	e.as("bob", "card", "set", "--file", "-").ok(t)
}

// Both aliases of one room, direct IDs, and ID-configured rooms are protected.
// Ordinary rooms remain leaveable. Unresolved protection never silently skips.
func TestCardRoomLeaveProtection(t *testing.T) {
	for _, ref := range []string{"#cards", "!cardsRoomID"} {
		t.Run(ref, func(t *testing.T) {
			e, _ := cardRoomWorld(t, ref)
			e.hs.addRoom("cards", "@alice:test.local", "@bob:test.local")
			e.hs.mu.Lock()
			e.hs.aliases["#alternate:test.local"] = "!cardsRoomID"
			e.hs.mu.Unlock()
			for _, arg := range []string{"cards", "#cards:test.local", "!cardsRoomID", "alternate"} {
				e.as("alice", "leave", arg).failsWith(t, ExitUsage, "pass --force")
			}
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/rooms/{room}/leave")
			e.as("alice", "leave", "lobby").ok(t) // old default no longer protected
			e.as("alice", "leave", "alternate", "--force").ok(t)
			e.as("bob", "card", "get", "nobody").failsWith(t, ExitError, "no card for")
		})
	}
	e, _ := cardRoomWorld(t, "#missing")
	e.as("alice", "leave", "!lobbyRoomID").failsWith(t, ExitError, "card_room_unavailable")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/rooms/{room}/leave")
	e.as("alice", "leave", "!lobbyRoomID", "--force").ok(t)
}

// The old flag cannot bypass missing mappings or select a decoy store. It is
// accepted only as an assertion matching the configured reference.
func TestCardRoomOverrideCannotBypassMapping(t *testing.T) {
	e, _ := cardRoomWorld(t, "#cards")
	e.hs.addRoom("cards", "@alice:test.local", "@bob:test.local")
	e.stdin = "own card"
	e.as("alice", "card", "set", "--room", "lobby", "--file", "-").failsWith(t, ExitUsage, "must match")
	e.as("bob", "card", "get", "alice", "--room", "lobby").failsWith(t, ExitUsage, "must match")
	e.as("bob", "card", "get", "@alice:remote.invalid", "--room", "lobby").failsWith(t, ExitError, "card_room_unavailable")
	if n := len(e.hs.Calls()); n != 0 {
		t.Fatalf("override made %d requests", n)
	}
	e.as("alice", "card", "set", "--room", "cards", "--file", "-").ok(t)
	e.as("bob", "card", "get", "alice", "--room", "#cards:test.local").ok(t)
}

// Public card policy may change between invocations without reissuing secrets,
// but a session already using an older snapshot refuses further requests.
func TestCardRoomPolicyChangeFencesSession(t *testing.T) {
	e, r := cardRoomWorld(t, "")
	var out, errout bytes.Buffer
	a := NewApp()
	a.Stdout = &out
	a.Stderr = &errout
	a.as = "alice"
	a.HTTP = e.hs.srv.Client()
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.resolveCardRoom(context.Background(), testServer); err != nil {
		t.Fatal(err)
	}
	c := r.Profiles["home"]
	c.CardRoom = "#cards"
	r.Profiles["home"] = c
	e.hs.addRoom("cards", "@alice:test.local")
	saveCardRegistry(t, e, r)
	before := len(e.hs.Calls())
	if _, _, err := s.resolveCardRoom(context.Background(), testServer); err == nil || !strings.Contains(err.Error(), "selected connection changed") {
		t.Fatalf("stale session = %v", err)
	}
	if len(e.hs.Calls()) != before {
		t.Fatal("stale policy reached server")
	}
	e.stdin = "fresh snapshot"
	e.as("alice", "card", "set", "--file", "-").ok(t)
}

// Onboarding resolves the same configured room, leaves the old default alone,
// and applies normal join authorization. Missing stores prevent subscriptions.
func TestCardRoomOnboarding(t *testing.T) {
	for _, ref := range []string{"#cards", "!cardsRoomID"} {
		t.Run(ref, func(t *testing.T) {
			e, _ := cardRoomWorld(t, ref)
			e.hs.mu.Lock()
			e.hs.rooms["!lobbyRoomID"].members["@alice:test.local"] = "leave"
			e.hs.mu.Unlock()
			run := func() (string, error) {
				var out, errout bytes.Buffer
				a := NewApp()
				a.Stdout = &out
				a.Stderr = &errout
				a.as = "alice"
				a.HTTP = e.hs.srv.Client()
				a.Getenv = func(k string) string {
					if k == config.EnvConfigDir {
						return e.dir
					}
					return ""
				}
				s, err := a.session()
				if err != nil {
					return "", err
				}
				err = a.onboard(context.Background(), s, onboarding{project: "proj", rt: hookRuntime{name: "none", title: "none"}})
				return out.String(), err
			}
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "card_room_unavailable") {
				t.Fatalf("missing room = %v", err)
			}
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, putADRoute)
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, createRoute)
			id := e.hs.addRoom("cards", "@bob:test.local")
			if _, err := run(); err == nil || !strings.Contains(err.Error(), "card_room_unavailable") {
				t.Fatalf("uninvited join = %v", err)
			}
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, putADRoute)
			e.hs.makePublic(id)
			out, err := run()
			if err != nil {
				t.Fatal(err)
			}
			command := "topic new cards join-"
			if strings.HasPrefix(ref, "!") {
				command = "topic new '!cardsRoomID' join-"
			}
			if !strings.Contains(out, "joined ") || !strings.Contains(out, "next steps:") || !strings.Contains(out, command) {
				t.Fatal(out)
			}
			e.hs.mu.Lock()
			joinedCards := e.hs.rooms[id].members["@alice:test.local"] == "join"
			joinedLobby := e.hs.rooms["!lobbyRoomID"].members["@alice:test.local"] == "join"
			e.hs.mu.Unlock()
			if !joinedCards || joinedLobby {
				t.Fatal("onboarding used wrong store")
			}
			found := false
			for _, sub := range e.subsOf("alice") {
				if sub.Room == id {
					found = true
				}
				if sub.Room == "!lobbyRoomID" {
					t.Fatal("subscribed to old default")
				}
			}
			if !found {
				t.Fatal("no configured card-room subscription")
			}
		})
	}
}

// A target mapping is not public accessibility or a reason to join remotely.
// Existing target membership is the positive control; no foreign profile opens.
func TestCardRoomCrossServerAuthorization(t *testing.T) {
	e, r := cardRoomWorld(t, "")
	user := "@alice:remote.invalid"
	id := e.hs.addRoom("targetcards", "@alice:test.local")
	e.hs.makePublic(id)
	putTestCard(t, e, id, user, "target members only")
	e.hs.mu.Lock()
	e.hs.aliases["#cards:remote.invalid"] = id
	e.hs.mu.Unlock()
	r.Profiles["remote"] = config.Connection{ID: "remote-id", URL: e.hs.srv.URL, ServerName: "remote.invalid", CardRoom: "#cards", Store: config.CredentialStore{Backend: "file"}}
	saveCardRegistry(t, e, r)
	e.as("bob", "card", "get", user).failsWith(t, ExitError, "card_room_unavailable")
	assertCardNoMembershipWrites(t, e)
	e.hs.mu.Lock()
	e.hs.rooms[id].members["@bob:test.local"] = "join"
	e.hs.mu.Unlock()
	if got := e.as("bob", "card", "get", user).ok(t).stdout; got != "target members only\n" {
		t.Fatal(got)
	}
	assertCardNoMembershipWrites(t, e)
}

// Struct-field case aliases cannot smuggle a second card store past strict JSON
// admission. Both possible stores are readable, so a permissive decoder would
// return the wrong card instead of failing because a fixture is unavailable.
func TestCardRoomAmbiguousCaseSetting(t *testing.T) {
	e, r := cardRoomWorld(t, "#cards")
	cards := e.hs.addRoom("cards", "@alice:test.local", "@bob:test.local")
	other := e.hs.addRoom("other", "@alice:test.local", "@bob:test.local")
	putTestCard(t, e, cards, "@alice:test.local", "configured card")
	putTestCard(t, e, other, "@alice:test.local", "shadow store card")
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"card_room":"#cards"`), []byte(`"card_room":"#cards","CARD_ROOM":"#other"`), 1)
	if err := os.WriteFile(filepath.Join(e.dir, "connections.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	e.as("bob", "card", "get", "alice").failsWith(t, ExitError, "duplicate keys")
	if len(e.hs.Calls()) != 0 {
		t.Fatal("ambiguous store reached the server")
	}
	saveCardRegistry(t, e, r)
	if got := e.as("bob", "card", "get", "alice").ok(t).stdout; got != "configured card\n" {
		t.Fatal(got)
	}
}
