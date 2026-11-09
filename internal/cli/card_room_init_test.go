package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func powerTree(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var out map[string]any
	if err := d.Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func cardInitFixture(t *testing.T) (*env, string, json.RawMessage) {
	t.Helper()
	e, _ := cardRoomWorld(t, "#cards")
	id := e.hs.addRoom("cards", "@alice:test.local", "@bob:test.local")
	raw := json.RawMessage(`{"users":{"@alice:test.local":100},"users_default":0,"events_default":0,"state_default":50,"invite":50,"ban":75,"kick":70,"redact":100,"notifications":{"room":60},"events":{"m.room.power_levels":100,"ai.relux.a2a.card":50,"m.room.encryption":100,"other.event":73},"extension":{"exact":9007199254740993,"nested":[null,true,"unchanged",1.234567890123456789]}}`)
	e.hs.mu.Lock()
	e.hs.rooms[id].state["m.room.power_levels|"] = raw
	e.hs.mu.Unlock()
	return e, id, raw
}

// Exactly one leaf changes, including when unknown extension fields and numbers
// exceed float64 precision. Members cannot init, can publish after an admin's
// init, and cannot overwrite another member's @-prefixed state key.
func TestCardRoomInitOnlyCardPowerAndIdempotent(t *testing.T) {
	e, id, before := cardInitFixture(t)
	e.stdin = "bob own card"
	e.as("bob", "card", "set", "--file", "-").failsWith(t, ExitError, "card_room_unavailable")
	count := len(e.hs.CallsFor("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}"))
	e.as("bob", "card", "room", "init").failsWith(t, ExitError, "card_room_init_forbidden")
	if len(e.hs.CallsFor("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")) != count {
		t.Fatal("member init sent a write")
	}
	e.as("alice", "card", "room", "init").ok(t)
	e.hs.mu.Lock()
	after := append([]byte(nil), e.hs.rooms[id].state["m.room.power_levels|"]...)
	e.hs.mu.Unlock()
	want := powerTree(t, before)
	want["events"].(map[string]any)[CardEventType] = json.Number("0")
	if !reflect.DeepEqual(powerTree(t, after), want) {
		t.Fatalf("changed more than card power: %s", after)
	}
	count = len(e.hs.CallsFor("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}"))
	e.as("alice", "card", "room", "init").ok(t)
	e.as("bob", "card", "room", "init").failsWith(t, ExitError, "card_room_init_forbidden")
	if len(e.hs.CallsFor("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")) != count {
		t.Fatal("idempotent or forbidden init wrote state")
	}
	e.as("bob", "card", "set", "--file", "-").ok(t)
	if got := e.as("alice", "card", "get", "bob").ok(t).stdout; got != "bob own card\n" {
		t.Fatal(got)
	}
	// Direct fake-server negative control. The CLI has no input for a state key.
	c := matrix.New(e.hs.srv.URL, e.tokens["bob"])
	c.HTTP = e.hs.srv.Client()
	if _, err := c.PutState(context.Background(), id, CardEventType, "@alice:test.local", Card{Body: "forged"}); !matrix.IsCode(err, "M_FORBIDDEN") {
		t.Fatalf("forged state-key write = %v", err)
	}
	e.as("bob", "card", "set", "--file", "-", "--state-key", "@alice:test.local").failsWith(t, ExitUsage, "flag provided but not defined")
	e.hs.mu.Lock()
	_, forged := e.hs.rooms[id].state[CardEventType+"|@alice:test.local"]
	e.hs.mu.Unlock()
	if forged {
		t.Fatal("another user's card was changed")
	}
	assertCardNoMembershipWrites(t, e)
}

func TestCardRoomInitExplicitRoom(t *testing.T) {
	for _, ref := range []string{"#other", "!otherRoomID"} {
		t.Run(ref, func(t *testing.T) {
			e, id, before := cardInitFixture(t)
			other := e.hs.addRoom("other", "@alice:test.local")
			e.hs.mu.Lock()
			e.hs.rooms[other].state["m.room.power_levels|"] = append([]byte(nil), before...)
			e.hs.mu.Unlock()
			e.as("alice", "card", "room", "init", "--room", ref).ok(t)
			e.hs.mu.Lock()
			unchanged := bytes.Equal(e.hs.rooms[id].state["m.room.power_levels|"], before)
			changed := powerTree(t, e.hs.rooms[other].state["m.room.power_levels|"])
			e.hs.mu.Unlock()
			if !unchanged || changed["events"].(map[string]any)[CardEventType] != json.Number("0") {
				t.Fatal("explicit init retargeted")
			}
			e.as("alice", "card", "room", "init", "--room", ref).ok(t)
			assertCardNoMembershipWrites(t, e)
		})
	}
}

// Refusal controls include inability to write PL, inability to lower the old
// card level, unknown/malformed authority and server rejection after preflight.
func TestCardRoomInitClosedAuthority(t *testing.T) {
	for name, raw := range map[string]string{
		"no-power":          `{"users":{"@alice:test.local":49},"events":{"m.room.power_levels":50}}`,
		"cannot-lower-card": `{"users":{"@alice:test.local":50},"events":{"m.room.power_levels":50,"ai.relux.a2a.card":100}}`,
		"bad-events":        `{"users":{"@alice:test.local":100},"events":null}`,
		"bad-users":         `{"users":false}`,
		"bad-level":         `{"users":{"@alice:test.local":1.5}}`,
		"duplicate-level":   `{"users":{"@alice:test.local":100,"@alice:test.local":0}}`,
	} {
		t.Run(name, func(t *testing.T) {
			e, id, valid := cardInitFixture(t)
			e.hs.mu.Lock()
			e.hs.rooms[id].state["m.room.power_levels|"] = json.RawMessage(raw)
			e.hs.mu.Unlock()
			e.as("alice", "card", "room", "init").failsWith(t, ExitError, "card_room_init_forbidden")
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			e.hs.mu.Lock()
			e.hs.rooms[id].state["m.room.power_levels|"] = valid
			e.hs.mu.Unlock()
			e.as("alice", "card", "room", "init").ok(t)
		})
	}
	e, id, before := cardInitFixture(t)
	e.hs.mu.Lock()
	e.hs.forbidden["m.room.power_levels"] = true
	e.hs.mu.Unlock()
	e.as("alice", "card", "room", "init").failsWith(t, ExitError, "card_room_init_forbidden")
	e.hs.mu.Lock()
	unchanged := bytes.Equal(e.hs.rooms[id].state["m.room.power_levels|"], before)
	e.hs.forbidden["m.room.power_levels"] = false
	e.hs.mu.Unlock()
	if !unchanged {
		t.Fatal("server refusal changed power levels")
	}
	e.as("alice", "card", "room", "init").ok(t)
}

// The local preflight supports explicit PL grants/defaults, legacy integer
// strings and v12's implicit creator/additional-creator authority. Unknown or
// malformed versions cannot manufacture that authority.
func TestCardRoomInitPowerRules(t *testing.T) {
	for _, tc := range []struct {
		name, create, power, sender string
		allowed                     bool
	}{
		{"explicit", `{}`, `{"users":{"@alice:test.local":60},"events":{"m.room.power_levels":60,"ai.relux.a2a.card":60}}`, "", true},
		{"defaults", `{}`, `{"users_default":70,"state_default":60}`, "", true},
		{"legacy-strings", `{"room_version":"9"}`, `{"users":{"@alice:test.local":"100"},"state_default":"50"}`, "", true},
		{"modern-strings", `{"room_version":"10"}`, `{"users":{"@alice:test.local":"100"}}`, "", false},
		{"creator-v11-not-implicit", `{"room_version":"11","creator":"@alice:test.local"}`, `{}`, "@alice:test.local", false},
		{"creator-v12", `{"room_version":"12"}`, `{"events":{"m.room.power_levels":100,"ai.relux.a2a.card":100}}`, "@alice:test.local", true},
		{"additional-v12", `{"room_version":"12","additional_creators":["@alice:test.local"]}`, `{}`, "@other:test.local", true},
		{"missing-v12-creator", `{"room_version":"12"}`, `{}`, "", false},
		{"future-version", `{"room_version":"13"}`, `{"users":{"@alice:test.local":100}}`, "@alice:test.local", false},
		{"malformed-version", `{"room_version":12}`, `{}`, "@alice:test.local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			have, need, old, _, err := cardRoomInitPower(powerTree(t, []byte(tc.power)), powerTree(t, []byte(tc.create)), tc.sender, "@alice:test.local")
			allowed := err == nil && have >= need && have >= old && have >= 0
			if allowed != tc.allowed {
				t.Fatalf("allowed=%v have=%d need=%d old=%d err=%v", allowed, have, need, old, err)
			}
		})
	}
}

// A forged or missing sender cannot make a remote/local card body printable.
// Restoring the exact owner sender succeeds; names inside content are no proof.
func TestCardRoomOwnerMismatch(t *testing.T) {
	for _, sender := range []string{"@bob:test.local", "@alice:other.invalid", ""} {
		t.Run("sender="+sender, func(t *testing.T) {
			e, _ := cardRoomWorld(t, "")
			user := "@alice:test.local"
			putTestCard(t, e, "!lobbyRoomID", user, "must not leak")
			e.hs.mu.Lock()
			e.hs.rooms["!lobbyRoomID"].stateSenders[CardEventType+"|"+user] = sender
			e.hs.mu.Unlock()
			r := e.as("bob", "card", "get", "alice").failsWith(t, ExitError, "card_owner_mismatch")
			if r.stdout != "" || strings.Contains(r.stderr, "must not leak") {
				t.Fatal("forged card body escaped")
			}
			e.hs.mu.Lock()
			e.hs.rooms["!lobbyRoomID"].stateSenders[CardEventType+"|"+user] = user
			e.hs.mu.Unlock()
			if got := e.as("bob", "card", "get", "alice").ok(t).stdout; got != "must not leak\n" {
				t.Fatal(got)
			}
		})
	}
}

// Version-12 creation sender and additional creators have implicit power, even
// with no users entry. An ordinary member still cannot initialize that room.
func TestCardRoomInitV12Creators(t *testing.T) {
	for _, additional := range []bool{false, true} {
		name := "creator"
		if additional {
			name = "additional"
		}
		t.Run(name, func(t *testing.T) {
			e, id, _ := cardInitFixture(t)
			create := `{"room_version":"12"}`
			sender := "@alice:test.local"
			if additional {
				create = `{"room_version":"12","additional_creators":["@alice:test.local"]}`
				sender = "@other:test.local"
			}
			e.hs.mu.Lock()
			e.hs.rooms[id].state["m.room.create|"] = json.RawMessage(create)
			e.hs.rooms[id].stateSenders = map[string]string{"m.room.create|": sender}
			e.hs.rooms[id].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"state_default":50,"events":{"m.room.power_levels":100,"ai.relux.a2a.card":100}}`)
			e.hs.mu.Unlock()
			e.as("bob", "card", "room", "init").failsWith(t, ExitError, "card_room_init_forbidden")
			tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			e.as("alice", "card", "room", "init").ok(t)
			e.stdin = "bob own card after v12 init"
			e.as("bob", "card", "set", "--file", "-").ok(t)
			e.as("alice", "card", "get", "bob").ok(t)
		})
	}
}
