package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const sendRoute = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"

// projWorld is world() plus a projector identity "proj" in #proj and a
// projection room #system where alice watches.
func projWorld(t *testing.T) (*env, string, string) {
	t.Helper()
	e, room := world(t)
	projUID := e.agent("proj")
	e.hs.mu.Lock()
	e.hs.rooms[room].members[projUID] = "join"
	e.hs.mu.Unlock()
	system := e.hs.addRoom("system", projUID, "@alice:test.local")
	return e, room, system
}

func (e *env) project(extra ...string) result {
	e.t.Helper()
	return e.as("proj", append([]string{"project", "--once", "--poll", "50ms"}, extra...)...)
}

// projections returns the projector's messages in the projection room.
func (e *env) projections(system string) []matrix.Event {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var out []matrix.Event
	for _, ev := range e.hs.events {
		if ev.RoomID == system && ev.Sender == "@proj:test.local" {
			out = append(out, ev)
		}
	}
	return out
}

func projectionOf(t *testing.T, ev matrix.Event) Projection {
	t.Helper()
	var c struct {
		P Projection `json:"ai.relux.a2a"`
	}
	if err := json.Unmarshal(ev.Content, &c); err != nil {
		t.Fatal(err)
	}
	return c.P
}

// Proves the projector mirrors exactly the system and owner-needed messages of
// other agents into #system, each with the origin room, event, kind, sender
// and a matrix.to link, and skips other kinds, header-less text, its own
// messages, messages already in #system, and projections.
func TestProjectMirrorsTypedMessages(t *testing.T) {
	e, room, system := projWorld(t)
	tuitestkit.ContainsStr(t, e.project().ok(t).stdout, "position taken")
	sys := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "Landing: PR #3 on main"))
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "notice", "just a notice"))
	e.hs.inject(room, "@bob:test.local", msg("no header system", nil))
	own := e.hs.inject(room, "@proj:test.local", kindMsg("proj", "system", "my own"))
	e.hs.inject(system, "@alice:test.local", kindMsg("alice", "system", "already in system"))
	e.hs.inject(room, "@bob:test.local", msg("fake", map[string]any{ContentKey: map[string]any{"v": 1, "type": "projection", "kind": "system"}}))
	on := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "owner-needed", "tailscale is logged out"))

	out := e.project().ok(t).stdout
	got := e.projections(system)
	if len(got) != 2 {
		t.Fatalf("projections = %d, want 2; stdout:\n%s", len(got), out)
	}
	for i, want := range []struct{ origin, kind, text string }{{sys, "system", "Landing: PR #3 on main"}, {on, "owner-needed", "tailscale is logged out"}} {
		p := projectionOf(t, got[i])
		if p.Type != "projection" || p.Kind != want.kind || p.From != "bob" || p.OriginRoom != room || p.OriginEvent != want.origin || p.OriginAlias != "#proj" || p.Slug != "alpha" {
			t.Fatalf("projection %d = %+v", i, p)
		}
		body := got[i].Message().Body
		tuitestkit.ContainsStr(t, body, "["+want.kind+"] #proj from bob (topic alpha):")
		tuitestkit.ContainsStr(t, body, want.text)
		tuitestkit.ContainsStr(t, body, "https://matrix.to/#/"+room+"/"+want.origin)
		tuitestkit.NotContainsStr(t, body, "- from:")
	}
	tuitestkit.NotContainsStr(t, out, own)
}

// Proves a restarted projector that re-reads an already projected batch
// (position rolled back, as after a crash before the save) does not duplicate
// the projection: the deterministic txn id is deduplicated by the server.
func TestProjectIdempotentAfterRestart(t *testing.T) {
	e, room, system := projWorld(t)
	e.project().ok(t)
	st, _ := config.LoadState(e.paths(), "proj")
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "once"))
	e.project().ok(t)
	config.UpdateState(e.paths(), "proj", func(cur *config.State) error { cur.NextBatch = st.NextBatch; return nil })
	e.project().ok(t)
	if n := len(e.projections(system)); n != 1 {
		t.Fatalf("projections after replay = %d, want 1", n)
	}
	if projectionTxn(room, "$a") == projectionTxn(room, "$b") || projectionTxn("!x", "$a") == projectionTxn("!y", "$a") {
		t.Fatal("txn ids collide across events or rooms")
	}
}

// Proves a failed send stops the projector without advancing its position, so
// the next run projects the message exactly once.
func TestProjectSendFailureKeepsPosition(t *testing.T) {
	e, room, system := projWorld(t)
	e.project().ok(t)
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "must not be lost"))
	e.hs.fail(sendRoute, 500, "M_UNKNOWN", "boom")
	e.project().failsWith(t, ExitError, "boom")
	if n := len(e.projections(system)); n != 0 {
		t.Fatalf("projected despite failure: %d", n)
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, sendRoute)
	e.hs.mu.Unlock()
	e.project().ok(t)
	if n := len(e.projections(system)); n != 1 {
		t.Fatalf("projections after recovery = %d, want 1", n)
	}
}

// Proves the projector joins rooms it is invited to and then projects from them.
func TestProjectAcceptsInvites(t *testing.T) {
	e, _, system := projWorld(t)
	e.project().ok(t)
	side := e.hs.addRoom("side", "@bob:test.local")
	e.as("bob", "invite", "side", "proj").ok(t)
	tuitestkit.ContainsStr(t, e.project().ok(t).stdout, "joined "+side)
	e.hs.inject(side, "@bob:test.local", kindMsg("bob", "owner-needed", "from side"))
	e.project().ok(t)
	if got := e.projections(system); len(got) != 1 || projectionOf(t, got[0]).OriginRoom != side {
		t.Fatalf("side projection = %+v", got)
	}
}

// Proves a projection never rings its original addressees or kind
// subscribers a second time; only `wait --all` on the projection room sees it.
func TestProjectionDoesNotRingTwice(t *testing.T) {
	e, room, system := projWorld(t)
	e.project().ok(t)
	e.as("alice", "subscribe", "--kind", "system").ok(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("- from: bob\n\n@alice:test.local look", map[string]any{
		ContentKey: Header{V: 1, Type: "message", From: "bob", To: []string{"alice"}, Kind: "system", Slug: "alpha"}}))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "to #proj "+id)
	e.project().ok(t)
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("projection rang again: %q", r.stdout)
	}
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "second"))
	e.project().ok(t)
	r = e.as("alice", "wait", "--room", "system", "--all", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "all #system ")
	if strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("want exactly the one projection: %q", r.stdout)
	}
	_ = system
}

// Proves bad projector input is refused before any network call.
func TestProjectNegative(t *testing.T) {
	e, _, _ := projWorld(t)
	e.as("proj", "project", "--kind", "gossip").failsWith(t, ExitUsage, "invalid --kind")
	e.as("proj", "project", "--kind", ",").failsWith(t, ExitUsage, "at least one kind")
	e.as("proj", "project", "--poll", "0s").failsWith(t, ExitUsage, "--poll")
	e.as("proj", "project", "extra").failsWith(t, ExitUsage, "usage")
	if n := e.hs.CallCount("GET /_matrix/client/v3/sync"); n != 0 {
		t.Fatalf("usage errors reached the server: %d syncs", n)
	}
	e.as("proj", "project", "--system", "nosuch", "--once").failsWith(t, ExitError, "")
}
