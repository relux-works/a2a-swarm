package cli

import (
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func kindMsg(from, kind, body string) map[string]any {
	return msg("- from: "+from+"\n\n"+body, map[string]any{ContentKey: Header{V: 1, Type: "message", From: from, Kind: kind, Slug: "alpha"}})
}

// Proves subscribe/unsubscribe/subscriptions persist kind subscriptions per
// room (by alias or all) and print them, with a no-op note for unknown ones.
func TestSubscribeCommands(t *testing.T) {
	e, _ := world(t)
	tuitestkit.ContainsStr(t, e.as("alice", "subscribe", "--kind", "system,owner-needed", "--room", "proj").ok(t).stdout, "#proj\towner-needed,system")
	e.as("alice", "subscribe", "--kind", "decision").ok(t)
	out := e.as("alice", "subscriptions").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "#proj\towner-needed,system")
	tuitestkit.ContainsStr(t, out, "all\tdecision")
	tuitestkit.ContainsStr(t, e.as("alice", "subscriptions", "--json").ok(t).stdout, `{"#proj":["owner-needed","system"],"all":["decision"]}`)
	tuitestkit.ContainsStr(t, e.as("alice", "unsubscribe", "--kind", "system", "--room", "proj").ok(t).stdout, "#proj\towner-needed")
	tuitestkit.ContainsStr(t, e.as("alice", "unsubscribe", "--kind", "notice", "--room", "proj").ok(t).stdout, "no matching subscription")
	e.as("alice", "unsubscribe", "--room", "all").ok(t)
	if out := e.as("alice", "subscriptions").ok(t).stdout; strings.Contains(out, "all") {
		t.Fatalf("all-rooms subscription survived: %q", out)
	}
	// Subscriptions are per agent.
	if out := e.as("bob", "subscriptions").ok(t).stdout; out != "" {
		t.Fatalf("bob sees %q", out)
	}
}

// Proves bad subscribe input is refused before anything is stored.
func TestSubscribeNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "subscribe", "--room", "proj").failsWith(t, ExitUsage, "--kind is required")
	e.as("alice", "subscribe", "--kind", "system,gossip").failsWith(t, ExitUsage, "invalid --kind")
	e.as("alice", "subscribe", "--kind", "system", "extra").failsWith(t, ExitUsage, "usage")
	e.as("alice", "subscribe", "--kind", "system", "--room", "nosuch").failsWith(t, ExitError, "")
	if out := e.as("alice", "subscriptions").ok(t).stdout; out != "" {
		t.Fatalf("refused input was stored: %q", out)
	}
}

// Proves wait wakes on an unaddressed message whose kind I subscribed to in
// that room (type "sub"), and not on other kinds, other rooms, or my own
// messages of that kind.
func TestWaitWakesOnSubscribedKind(t *testing.T) {
	e, room := world(t)
	lobby := e.hs.aliases["#lobby:"+testServer]
	e.as("alice", "subscribe", "--kind", "system", "--room", "proj").ok(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t) // take a position
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "notice", "plain notice"))
	e.hs.inject(lobby, "@bob:test.local", kindMsg("bob", "system", "system elsewhere"))
	e.hs.inject(room, "@alice:test.local", kindMsg("alice", "system", "my own system"))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("woke without a match: %q", r.stdout)
	}
	id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "landing on main"))
	r := e.as("alice", "wait", "--timeout", "2s", "--json").ok(t)
	recs := decodeRecords(t, r.stdout)
	if len(recs) != 1 || recs[0].Type != "sub" || recs[0].EventID != id || recs[0].Kind != "system" {
		t.Fatalf("records = %+v", recs)
	}
}

// Proves wait --all wakes on any other agent's message in the watched room,
// never on my own, never in rooms outside --room, and needs a --room.
func TestWaitAll(t *testing.T) {
	e, room := world(t)
	lobby := e.hs.aliases["#lobby:"+testServer]
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.hs.inject(room, "@alice:test.local", msg("mine", nil))
	e.hs.inject(lobby, "@bob:test.local", msg("lobby chatter", nil))
	if r := e.as("alice", "wait", "--room", "proj", "--all", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("woke on own or foreign-room message: %q", r.stdout)
	}
	id := e.hs.inject(room, "@bob:test.local", msg("anything at all", nil))
	r := e.as("alice", "wait", "--room", "proj", "--all", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "all #proj "+id)
	e.as("alice", "wait", "--all").failsWith(t, ExitUsage, "--all needs at least one --room")
}
