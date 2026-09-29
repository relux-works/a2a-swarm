package cli

import (
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves mute stops replies in a thread I posted in from ringing, while a
// reply addressed to me in that thread still rings; follow restores thread
// rings; repeating either reports "already".
func TestMuteFollow(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	tuitestkit.ContainsStr(t, e.as("alice", "mute", "proj", "--topic", "alpha").ok(t).stdout, "alpha\t"+root+"\tmuted")
	tuitestkit.ContainsStr(t, e.as("alice", "mute", "proj", "--topic", "alpha").ok(t).stdout, "already muted")
	threadReply := map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}
	e.hs.inject(room, "@bob:test.local", msg("plain reply", threadReply))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("muted thread rang: %q", r.stdout)
	}
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local addressed in muted thread", threadReply))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "mention #proj "+id)

	tuitestkit.ContainsStr(t, e.as("alice", "follow", "proj", "--topic", root).ok(t).stdout, "followed")
	id = e.hs.inject(room, "@bob:test.local", msg("plain again", threadReply))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "thread #proj "+id)
	tuitestkit.ContainsStr(t, e.as("alice", "follow", "proj", "--topic", "alpha").ok(t).stdout, "already followed")
}

// Proves mute/follow refuse missing arguments and unknown topics.
func TestMuteNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "mute", "proj").failsWith(t, ExitUsage, "usage")
	e.as("alice", "mute", "--topic", "alpha").failsWith(t, ExitUsage, "usage")
	e.as("alice", "mute", "proj", "--topic", "nosuch").failsWith(t, ExitError, "not found")
	e.as("carol", "follow", "proj", "--topic", "alpha").failsWith(t, ExitError, "")
}

// Proves the mute is sticky (regression for skill-project-management-e7's
// report): after muting, the agent's own post in the thread does not re-follow
// it, so a peer's plain reply stays silent; follow lifts the mute and the next
// plain reply rings again (positive control).
func TestMuteStickyAcrossOwnPosts(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.as("alice", "mute", "proj", "--topic", "alpha").ok(t)
	e.as("alice", "post", "proj", "--topic", "alpha", "my own reply after muting").ok(t)
	threadReply := map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}
	e.hs.inject(room, "@bob:test.local", msg("plain reply", threadReply))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("muted thread rang after own post: %q", r.stdout)
	}
	e.as("alice", "follow", "proj", "--topic", "alpha").ok(t)
	id := e.hs.inject(room, "@bob:test.local", msg("plain again", threadReply))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "thread #proj "+id)
}
