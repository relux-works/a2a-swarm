package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves card set stores an ai.relux.a2a.card state event in #lobby keyed by my
// user id, and card get returns its markdown for another agent.
func TestCard(t *testing.T) {
	e, _ := world(t)
	f := filepath.Join(t.TempDir(), "card.md")
	os.WriteFile(f, []byte("# alice\n\nrole: reviewer\n"), 0o644)
	r := e.as("alice", "card", "set", "--file", f).ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "$state")
	e.hs.mu.Lock()
	raw := e.hs.rooms["!lobbyRoomID"].state[CardEventType+"|@alice:test.local"]
	e.hs.mu.Unlock()
	var c Card
	if err := json.Unmarshal(raw, &c); err != nil || c.Name != "alice" || c.Format != "text/markdown" || c.UpdatedAt == "" {
		t.Fatalf("card = %+v, %v", c, err)
	}
	r = e.as("bob", "card", "get", "alice").ok(t)
	if r.stdout != "# alice\n\nrole: reviewer\n" {
		t.Fatalf("card get = %q", r.stdout)
	}
}

// Proves card errors: a forbidden state event explains the power-level fix, a
// missing card is reported, empty cards and bad usage are refused before any send.
func TestCardNegative(t *testing.T) {
	e, _ := world(t)
	f := filepath.Join(t.TempDir(), "card.md")
	os.WriteFile(f, []byte("hello"), 0o644)
	e.hs.mu.Lock()
	e.hs.forbidden[CardEventType] = true
	e.hs.mu.Unlock()
	e.as("alice", "card", "set", "--file", f).failsWith(t, ExitError, `power_levels.events["ai.relux.a2a.card"] = 0`)
	e.as("bob", "card", "get", "alice").failsWith(t, ExitError, "no card for @alice:test.local in #lobby")
	empty := filepath.Join(t.TempDir(), "empty.md")
	os.WriteFile(empty, []byte("\n\n"), 0o644)
	e.as("alice", "card", "set", "--file", empty).failsWith(t, ExitUsage, "empty message body refused")
	e.as("alice", "card", "set").failsWith(t, ExitUsage, "usage")
	e.as("alice", "card", "get").failsWith(t, ExitUsage, "usage")
	e.as("alice", "card", "burn").failsWith(t, ExitUsage, "usage")
	e.as("alice", "card").failsWith(t, ExitUsage, "usage")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}", 1)
}

// Proves status set maps active/idle/away to Matrix presence with the a2a state
// kept in status_msg. Presence alone cannot establish session liveness.
func TestStatus(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "set", "idle", "waiting", "for", "review").ok(t)
	e.hs.mu.Lock()
	p := e.hs.presence["@alice:test.local"]
	e.hs.mu.Unlock()
	if p.Presence != "unavailable" || p.StatusMsg != "idle: waiting for review" {
		t.Fatalf("presence = %+v", p)
	}
	r := e.as("bob", "status", "get", "alice").ok(t)
	if r.stdout != "alice\tunknown\tunknown\tage=-\town notice unknown\n" {
		t.Fatalf("status get = %q", r.stdout)
	}
	e.as("alice", "status", "set", "active").ok(t)
	r = e.as("alice", "status", "get").ok(t)
	if r.stdout != "alice\tunknown\tunknown\tage=-\town notice unknown\n" {
		t.Fatalf("status get self = %q", r.stdout)
	}
	r = e.as("alice", "status", "get", "carol").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "carol\tunknown\tunknown\tage=-")
}

// Proves invalid status values and usage are refused without touching presence.
func TestStatusNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "set", "busy").failsWith(t, ExitUsage, "invalid status")
	e.as("alice", "status", "set").failsWith(t, ExitUsage, "usage")
	e.as("alice", "status", "set", "idle", strings.Repeat("x", 501)).failsWith(t, ExitUsage, "too long")
	e.as("alice", "status").failsWith(t, ExitUsage, "usage")
	e.as("alice", "status", "get", "a", "b").failsWith(t, ExitUsage, "usage")
	e.as("alice", "status", "get", "--stale-after", "0s").failsWith(t, ExitUsage, "usage")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/presence/{user}/status")
}
