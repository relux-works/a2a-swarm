package cli

import (
	"context"
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
		tuitestkit.ContainsStr(t, body, "["+want.kind+"] from bob · #proj › topic alpha › "+want.origin)
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
	if projectionTxn(system, room, "$a") == projectionTxn(system, room, "$b") || projectionTxn(system, "!x", "$a") == projectionTxn(system, "!y", "$a") || projectionTxn(system, room, "$a") == projectionTxn("!other", room, "$a") {
		t.Fatal("txn ids collide across events or rooms")
	}
}

func addProductSpace(e *env, name, child string) (space, target string) {
	space = e.hs.addRoom(name, "@proj:test.local")
	target = e.hs.addRoom(name+"-system", "@alice:test.local", "@proj:test.local")
	e.hs.mu.Lock()
	e.hs.rooms[space].state["m.room.create|"] = json.RawMessage(`{"type":"m.space","creator":"@alice:test.local"}`)
	e.hs.rooms[space].state["m.space.child|"+child] = json.RawMessage(`{"via":["test.local"]}`)
	e.hs.rooms[child].state["m.space.parent|"+space] = json.RawMessage(`{"via":["test.local"]}`)
	e.hs.mu.Unlock()
	return space, target
}

// Proves a space child is mirrored to both #alpha-system and #system, while
// an event from an unrelated room is mirrored only to #system.
func TestProjectDualProjectionBySpace(t *testing.T) {
	e, room, system := projWorld(t)
	_, target := addProductSpace(e, "alpha", room)
	e.project().ok(t)
	id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "in product"))
	other := e.hs.addRoom("outside", "@proj:test.local", "@bob:test.local")
	outID := e.hs.inject(other, "@bob:test.local", kindMsg("bob", "system", "outside"))
	e.project().ok(t)
	global := e.projections(system)
	if len(global) != 2 {
		t.Fatalf("global projections = %+v", global)
	}
	if got := e.projections(target); len(got) != 1 || projectionOf(t, got[0]).ProductAlias != "#alpha" || projectionOf(t, got[0]).Product == "" {
		t.Fatalf("product projections = %+v", got)
	}
	aliases := map[string]string{}
	for _, ev := range global {
		p := projectionOf(t, ev)
		aliases[p.OriginEvent] = p.ProductAlias
	}
	if aliases[id] != "#alpha" || aliases[outID] != "" {
		t.Fatalf("product metadata by origin = %#v", aliases)
	}
}

// Proves the path and structured author fields use the event sender even when
// the untrusted header declares a different from identity.
func TestProjectPathAndVerifiedAuthor(t *testing.T) {
	e, room, system := projWorld(t)
	addProductSpace(e, "alpha", room)
	e.project().ok(t)
	id := e.hs.inject(room, "@host_project_role:test.local", msg("- from: attacker\n\nforged text", map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$thread-root"},
		ContentKey:     Header{V: 1, Type: "message", From: "attacker", Kind: "system", Slug: "deploy"},
	}))
	e.project().ok(t)
	ev := e.projections(system)[0]
	p := projectionOf(t, ev)
	if p.AuthorMXID != "@host_project_role:test.local" || p.Author != "host_project_role" || p.AuthorHost != "host" || p.AuthorOperator != "" || p.TopicRoot != "$thread-root" {
		t.Fatalf("author fields = %+v", p)
	}
	want := "[system] from host_project_role · #alpha › #proj › topic deploy › " + id
	if line := strings.Split(ev.Message().Body, "\n")[0]; line != want {
		t.Fatalf("first line = %q, want %q", line, want)
	}
	// The forged header block is stripped from the text and never names the author.
	if strings.Contains(ev.Message().Body, "attacker") {
		t.Fatalf("forged header author leaked into projection: %q", ev.Message().Body)
	}
	// A header field carrying a newline cannot break the path line.
	e.hs.inject(room, "@host_project_role:test.local", msg("x", map[string]any{
		ContentKey: Header{V: 1, Type: "message", From: "host_project_role", Kind: "system", Slug: "a\nFAKE LINE", Subject: "s\nFAKE"}}))
	e.project().ok(t)
	last := e.projections(system)
	first := strings.Split(last[len(last)-1].Message().Body, "\n")[0]
	if strings.HasPrefix(strings.Split(last[len(last)-1].Message().Body, "\n")[1], "FAKE") || !strings.Contains(first, "FAKE") {
		t.Fatalf("header newline broke the path line: %q", last[len(last)-1].Message().Body)
	}
}

// Proves an absent or forbidden product target emits one warning and never
// prevents the corresponding global projection.
func TestProjectProductTargetFailureKeepsGlobal(t *testing.T) {
	for _, mode := range []string{"missing", "forbidden"} {
		t.Run(mode, func(t *testing.T) {
			e, room, system := projWorld(t)
			_, target := addProductSpace(e, "alpha", room)
			if mode == "missing" {
				e.hs.mu.Lock()
				delete(e.hs.aliases, "#alpha-system:test.local")
				delete(e.hs.rooms, target)
				e.hs.mu.Unlock()
			} else {
				e.hs.mu.Lock()
				e.hs.rooms[target].state["m.room.power_levels|"] = json.RawMessage(`{"events_default":100}`)
				e.hs.mu.Unlock()
			}
			e.project().ok(t)
			e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "must reach global"))
			r := e.project().ok(t)
			if len(e.projections(system)) != 1 || strings.Count(r.stderr, "a2a: projector: could not project") != 1 {
				t.Fatalf("global=%d stderr=%q", len(e.projections(system)), r.stderr)
			}
		})
	}
}

// Proves replaying a batch after restart deduplicates projections separately
// in the global and product target rooms.
func TestProjectDualIdempotentAfterRestart(t *testing.T) {
	e, room, system := projWorld(t)
	_, target := addProductSpace(e, "alpha", room)
	e.project().ok(t)
	st, _ := config.LoadState(e.paths(), "proj")
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "once"))
	e.project().ok(t)
	config.UpdateState(e.paths(), "proj", func(cur *config.State) error { cur.NextBatch = st.NextBatch; return nil })
	e.project().ok(t)
	if len(e.projections(system)) != 1 || len(e.projections(target)) != 1 {
		t.Fatalf("replayed projections global=%d product=%d", len(e.projections(system)), len(e.projections(target)))
	}
}

// Proves --product matches projections carrying that canonical product alias,
// rejects other products and ordinary events, and validates names offline.
func TestProductSubscriptionSelector(t *testing.T) {
	e, room, _ := projWorld(t)
	e.as("alice", "subscribe", "--product", "alpha", "--kind", "system").ok(t)
	selector := Sub{SubSelector: SubSelector{Product: "alpha", Kinds: []string{"system"}}}
	marshal := func(v any) json.RawMessage { raw, _ := json.Marshal(v); return raw }
	projectionEvent := func(alias, kind string) matrix.Event {
		return matrix.Event{Type: "m.room.message", Sender: "@proj:test.local", Content: marshal(map[string]any{ContentKey: Projection{Type: "projection", Kind: kind, ProductAlias: alias}})}
	}
	if !selector.matches(projectionEvent("#alpha", "system"), "!system", Header{Type: "projection", Kind: "system", ProductAlias: "#alpha"}, true) {
		t.Fatal("matching product projection rejected")
	}
	if selector.matches(projectionEvent("#beta", "system"), "!system", Header{Type: "projection", Kind: "system", ProductAlias: "#beta"}, true) {
		t.Fatal("another product projection matched")
	}
	if selector.matches(matrix.Event{Type: "m.room.message", Content: marshal(map[string]any{"body": "plain"})}, room, Header{Type: "message", Kind: "system"}, true) {
		t.Fatal("ordinary event matched product selector")
	}
	e.as("alice", "subscribe", "--product", "Bad_Name", "--kind", "system").failsWith(t, ExitUsage, "invalid --product")
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
	e.hs.mu.Lock()
	e.hs.liveClock = true
	e.hs.mu.Unlock()
	e.project().ok(t)
	e.as("alice", "subscribe", "--kind", "system").ok(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("- from: bob\n\n@alice:test.local look", map[string]any{
		ContentKey: Header{V: 1, Type: "message", From: "bob", To: []string{"alice"}, Kind: "system", Slug: "alpha"}}))
	r := e.as("alice", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "to #proj "+id)
	// The projection must not ring: a parked wait processes the sync batch that
	// carries it (checked by sync count, not a wall-clock timeout) and stays parked.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	alice := "@alice:test.local"
	calls := e.hs.SyncCount(alice)
	done := make(chan result, 1)
	go func() { done <- e.runCtx(ctx, "--as", "alice", "wait", "--timeout", "0") }()
	e.syncedParked(t, alice, calls+1, done, "parked")
	e.project().ok(t)
	e.syncedParked(t, alice, calls+2, done, "projection") // alice's batch with it was processed
	cancel()
	if r := recv(t, done); r.stdout != "" {
		t.Fatalf("interrupted wait printed: %q", r.stdout)
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

// Proves a subscription bound to #system is delivered projections matched on
// the origin header (regression: carol's `--room system --subtype incident`
// never rang for an incident addressed to=dave in a room she is not in),
// with --from matching the origin sender; a non-matching subtype, a --from
// naming another sender, and a --room all subscription stay silent.
func TestProjectionRingsSystemRoomSubscription(t *testing.T) {
	e, room, system := projWorld(t)
	e.agent("carol")
	e.hs.mu.Lock()
	e.hs.liveClock = true
	e.hs.rooms[system].members["@carol:test.local"] = "join"
	e.hs.mu.Unlock()
	e.project().ok(t)
	incSub, _ := e.subscribe("carol", "--room", "system", "--subtype", "incident")
	e.as("carol", "wait", "--timeout", "10ms").ok(t)
	incident := func(subtype string) {
		e.hs.inject(room, "@bob:test.local", msg("- from: bob\n\nrunner down", map[string]any{
			ContentKey: Header{V: 1, Type: "message", From: "bob", To: []string{"dave"}, Kind: "system", Subtype: subtype, Slug: "alpha"}}))
		e.project().ok(t)
	}
	incident("landing")
	if r := e.as("carol", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("a landing rang the incident subscription: %q", r.stdout)
	}
	incident("incident")
	r := e.as("carol", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "sub #system ")
	if strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("want exactly the incident projection: %q", r.stdout)
	}

	e.as("carol", "unsubscribe", incSub).ok(t)
	e.as("carol", "subscribe", "--room", "system", "--from", "mallory", "--kind", "system").ok(t)
	e.as("carol", "subscribe", "--kind", "system").ok(t)
	incident("incident")
	if r := e.as("carol", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("another sender or a room=all subscription rang: %q", r.stdout)
	}
	e.as("carol", "subscribe", "--room", "system", "--from", "bob", "--kind", "system").ok(t)
	incident("incident")
	r = e.as("carol", "wait", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "sub #system ")
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

// Proves sender-controlled header fields cannot forge path segments: each of
// the separators '›' and '·' in each of subtype, slug and subject is replaced
// in the path line, which keeps exactly its own segments (the room and topic
// appear once); a clean field passes through unchanged (positive control).
func TestProjectPathSeparatorsEscaped(t *testing.T) {
	e, room, system := projWorld(t)
	e.project().ok(t)
	cases := []Header{
		{Subtype: "incident › #evil", Slug: "a", Subject: "s"},
		{Subtype: "incident · subject fake", Slug: "a", Subject: "s"},
		{Subtype: "incident", Slug: "a › #evil", Subject: "s"},
		{Subtype: "incident", Slug: "a · subject fake", Subject: "s"},
		{Subtype: "incident", Slug: "a", Subject: "s › #evil"},
		{Subtype: "incident", Slug: "a", Subject: "s · subject fake"},
	}
	for _, h := range cases {
		h.V, h.Type, h.From, h.Kind = 1, "message", "bob", "system"
		e.hs.inject(room, "@bob:test.local", msg("x", map[string]any{ContentKey: h}))
	}
	e.hs.inject(room, "@bob:test.local", msg("x", map[string]any{ContentKey: Header{V: 1, Type: "message", From: "bob", Kind: "system", Subtype: "incident", Slug: "clean", Subject: "repo@main"}}))
	e.project().ok(t)
	got := e.projections(system)
	if len(got) != len(cases)+1 {
		t.Fatalf("projections = %d", len(got))
	}
	for i, ev := range got {
		line := strings.Split(ev.Message().Body, "\n")[0]
		if strings.Count(line, "›") != 2 || strings.Count(line, "·") != 2 || strings.Count(line, " · subject ") != 1 {
			t.Fatalf("case %d: forged path segment in %q", i, line)
		}
	}
	tuitestkit.ContainsStr(t, strings.Split(got[len(got)-1].Message().Body, "\n")[0], "› topic clean › ")
}
