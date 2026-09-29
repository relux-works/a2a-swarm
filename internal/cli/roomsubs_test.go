package cli

import (
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves joining a room creates its room subscription in the same step
// (printed, idempotent on a second join), leaving removes it, and 'subs sync'
// backfills a subscription for every joined room except #system and
// #broadcast (skipped with the reason printed); a second sync creates nothing.
func TestRoomSubscriptions(t *testing.T) {
	e, _ := world(t)
	e.hs.makePublic(e.hs.addRoom("side"))
	out := e.as("alice", "join", "side").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "joined #side")
	tuitestkit.ContainsStr(t, out, "subscribed sub-")
	tuitestkit.ContainsStr(t, out, "kinds=system,owner-needed,decision")
	e.as("alice", "join", "side").ok(t)
	count := func() int { return len(e.subsOf("alice")) }
	if count() != 1 {
		t.Fatalf("subscriptions after joining twice: %+v", e.subsOf("alice"))
	}
	left := e.as("alice", "leave", "side").ok(t).stdout
	tuitestkit.ContainsStr(t, left, "unsubscribed sub-")
	if count() != 0 {
		t.Fatalf("subscription survived leave: %+v", e.subsOf("alice"))
	}

	alice := "@alice:test.local"
	e.hs.addRoom("system", alice)
	e.hs.addRoom("broadcast", alice)
	sync := e.as("alice", "subs", "sync").ok(t).stdout
	tuitestkit.ContainsStr(t, sync, "no room subscription for #system: a projection room")
	tuitestkit.ContainsStr(t, sync, "no room subscription for #broadcast: broadcasts ring every member")
	for _, room := range []string{"#proj", "#lobby"} {
		tuitestkit.ContainsStr(t, sync, room)
	}
	if n := strings.Count(sync, "\nsubscribed ") + strings.Count("\n"+sync, "\nsubscribed "); n == 0 || count() != 2 {
		t.Fatalf("sync did not subscribe #proj and #lobby: %q %+v", sync, e.subsOf("alice"))
	}
	again := e.as("alice", "subs", "sync").ok(t).stdout
	tuitestkit.NotContainsStr(t, "\n"+again, "\nsubscribed ")
	if count() != 2 {
		t.Fatalf("second sync duplicated: %+v", e.subsOf("alice"))
	}
	e.as("alice", "subs", "sync", "extra").failsWith(t, ExitUsage, "usage: a2a subs sync")
}
