package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func principalDMTestRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func principalFixture(t *testing.T) (*env, ownerDMFiles, string) {
	t.Helper()
	e, _ := world(t)
	var err error
	e.dir, err = filepath.EvalSymlinks(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	f := principalFiles(e.paths(), "bob")
	carol := "@carol:" + testServer
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{carol}})
	ownerPut(t, ownerFiles(e.paths(), "bob").path("pin"), ownerDMPin{Mode: "host", Owner: "@alice:" + testServer})
	e.configure = func(a *App) {
		a.ownerConfirm = func(s string) error {
			if s != carol {
				return errors.New("type-back mismatch")
			}
			return nil
		}
	}
	return e, f, carol
}
func principalEntry(t *testing.T, f ownerDMFiles, who string) principalDM {
	t.Helper()
	var st principalDMState
	err := json.Unmarshal([]byte(readFile(t, f.path("json"))), &st)
	if err != nil {
		t.Fatal(err)
	}
	d, ok := st.DMs[who]
	if !ok {
		t.Fatalf("no entry for %s", who)
	}
	return d
}
func principalJoin(e *env, room, who string) {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	r := e.hs.rooms[room]
	r.members[who] = "join"
	if r.stateSenders == nil {
		r.stateSenders = map[string]string{}
	}
	r.stateSenders["m.room.member|"+who] = who
	r.state["m.room.member|"+who] = json.RawMessage(`{"membership":"join"}`)
}

// Proves the full S1 lifecycle: terminal ensure creates one pending private
// room for the pinned principal, say refuses until the principal joins, then
// sends exactly the given text; re-ensure keeps the room; unset retires it.
func TestPrincipalDMLifecycle(t *testing.T) {
	e, f, carol := principalFixture(t)
	e.as("bob", "principal-dm", "ensure", carol).ok(t)
	d := principalEntry(t, f, carol)
	if !d.Pending || d.Active || d.Generation != 1 || d.SubID == "" {
		t.Fatalf("ensure state %+v", d)
	}
	if excluded, err := ownerFiles(e.paths(), "bob").exclusions(); err != nil || !excluded[d.Room] {
		t.Fatal("room not excluded from archive", err)
	}
	if subs := e.as("bob", "subs", "--json").ok(t); !strings.Contains(subs.stdout, d.Room) {
		t.Fatal("no all-events subscription for the room")
	}
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "pending claim")
	e.as("bob", "principal-dm", "say", carol, "hello").failsWith(t, ExitError, "pending until the principal joins")
	if b := ownerBodies(e, d.Room); len(b) != 0 {
		t.Fatal("sent before join", b)
	}
	principalJoin(e, d.Room, carol)
	e.as("bob", "principal-dm", "status", carol).ok(t)
	if principalEntry(t, f, carol).Active {
		t.Fatal("status mutated state")
	}
	e.as("bob", "principal-dm", "say", carol, "hello", "there").ok(t)
	if b := ownerBodies(e, d.Room); len(b) != 1 || b[0] != "hello there" {
		t.Fatal("say body", b)
	}
	if got := principalEntry(t, f, carol); !got.Active || got.Pending {
		t.Fatal("say did not activate", got)
	}
	out := e.as("bob", "principal-dm", "ensure", carol).ok(t)
	if !strings.Contains(out.stdout, "already active") || principalEntry(t, f, carol).Room != d.Room {
		t.Fatal("re-ensure replaced an active room")
	}
	status := e.as("bob", "principal-dm", "status").ok(t)
	if !strings.Contains(status.stdout, "active generation 1") || !strings.Contains(status.stdout, "audience: valid") {
		t.Fatal(status.stdout)
	}
	e.as("bob", "principal-dm", "unset", carol).ok(t)
	if got := principalEntry(t, f, carol); got.Active || got.Pending || got.SubID != "" || got.Generation != 2 {
		t.Fatal("unset state", got)
	}
	if subs := e.as("bob", "subs", "--json").ok(t); strings.Contains(subs.stdout, d.Room) {
		t.Fatal("unset kept the subscription")
	}
	e.as("bob", "principal-dm", "say", carol, "again").failsWith(t, ExitError, "no active room")
	e.as("bob", "principal-dm", "ensure", carol).ok(t)
	if got := principalEntry(t, f, carol); got.Room == d.Room || got.Generation != 3 || !got.Pending {
		t.Fatal("ensure after unset did not create a fresh room", got)
	}
}

// Proves admission refusals leave no config and create no room: unpinned
// principal, the pinned owner, this identity itself, an invalid pin, a
// noninteractive or mismatched terminal, and malformed arguments.
func TestPrincipalDMAdmissionRefusals(t *testing.T) {
	e, f, carol := principalFixture(t)
	rooms := len(e.hs.rooms)
	e.as("bob", "principal-dm", "ensure", "@dave:"+testServer).failsWith(t, ExitError, "not in the host pin")
	e.as("bob", "principal-dm", "say", "@dave:"+testServer, "x").failsWith(t, ExitError, "not in the host pin")
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{carol, "@alice:" + testServer, "@bob:" + testServer}})
	e.as("bob", "principal-dm", "ensure", "@alice:"+testServer).failsWith(t, ExitError, "use owner-dm")
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	e.as("bob", "principal-dm", "ensure", "@bob:"+testServer).failsWith(t, ExitError, "is this identity")
	ownerPut(t, f.path("pin"), map[string]any{"principals": []string{carol, carol}})
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "invalid principal-dm pin")
	ownerPut(t, f.path("pin"), map[string]any{"principals": []string{carol}, "extra": true})
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "invalid principal-dm pin")
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{carol}})
	e.configure = nil
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "interactive")
	e.as("bob", "principal-dm", "unset", carol).failsWith(t, ExitError, "interactive")
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return errors.New("type-back mismatch") } }
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "type-back mismatch")
	e.as("bob", "principal-dm", "ensure", "carol").failsWith(t, ExitUsage, "expected @user:server")
	e.as("bob", "principal-dm", "say", carol).failsWith(t, ExitUsage, "exactly one body")
	e.as("bob", "principal-dm", "say", carol, "-", "extra").failsWith(t, ExitUsage, "exactly one body")
	e.as("bob", "principal-dm", "bind", carol).failsWith(t, ExitUsage, "unknown principal-dm command")
	if _, st, _ := ownerRead(f.path("json")); st != nil || len(e.hs.rooms) != rooms {
		t.Fatal("a refusal wrote config or created a room")
	}
	// Positive control on the same fixture: the pinned principal is admitted.
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	e.as("bob", "principal-dm", "ensure", carol).ok(t)
}

// Proves say refuses a room whose audience drifted (extra member, encryption,
// public join rule) without sending, and that an expired pending claim is
// neither usable nor blocking: ensure replaces it with a fresh room.
func TestPrincipalDMAudienceAndExpiry(t *testing.T) {
	e, f, carol := principalFixture(t)
	e.as("bob", "principal-dm", "ensure", carol).ok(t)
	d := principalEntry(t, f, carol)
	principalJoin(e, d.Room, carol)
	e.as("bob", "principal-dm", "say", carol, "first").ok(t)
	if !principalEntry(t, f, carol).Active {
		t.Fatal("not active before drift checks")
	}
	for _, drift := range []string{"member", "encryption", "public"} {
		e.hs.mu.Lock()
		r := e.hs.rooms[d.Room]
		switch drift {
		case "member":
			r.members["@alice:"+testServer] = "invite"
		case "encryption":
			r.state["m.room.encryption|"] = json.RawMessage(`{"algorithm":"m.megolm.v1.aes-sha2"}`)
		case "public":
			r.state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"public"}`)
		}
		e.hs.mu.Unlock()
		before := principalSnapshot(t, e, f)
		e.as("bob", "principal-dm", "say", carol, "leak").failsWith(t, ExitError, map[string]string{"member": "expected only joined identity", "encryption": "encrypted", "public": "join_rule must be invite"}[drift])
		if principalSnapshot(t, e, f) != before {
			t.Fatal("say repaired or mutated state")
		}
		if b := ownerBodies(e, d.Room); len(b) != 1 {
			t.Fatal(drift, "sent despite drift", b)
		}
		e.hs.mu.Lock()
		delete(r.members, "@alice:"+testServer)
		delete(r.state, "m.room.encryption|")
		r.state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"invite"}`)
		e.hs.mu.Unlock()
	}
	e.as("bob", "principal-dm", "say", carol, "ok").ok(t)

	e2, f2, _ := principalFixture(t)
	e2.as("bob", "principal-dm", "ensure", carol).ok(t)
	first := principalEntry(t, f2, carol)
	e2.now = func() time.Time { return time.UnixMilli(first.CreatedAt).Add(principalDMPendingTTL) }
	e2.as("bob", "principal-dm", "say", carol, "late").failsWith(t, ExitError, "no active room")
	if st := e2.as("bob", "principal-dm", "status", carol).ok(t); !strings.Contains(st.stdout, "expired") {
		t.Fatal(st.stdout)
	}
	e2.as("bob", "principal-dm", "ensure", carol).ok(t)
	if second := principalEntry(t, f2, carol); second.Room == first.Room || second.Generation != 2 || !second.Pending {
		t.Fatal("expired claim not replaced", second)
	}
}

// Proves entries are independent: two principals get separate rooms and
// subscriptions; unset of one, a never-configured unset, re-ensure activation
// after a join, and a pin that drops one principal leave the other untouched.
func TestPrincipalDMTwoPrincipals(t *testing.T) {
	e, f, carol := principalFixture(t)
	e.agent("dave")
	dave := "@dave:" + testServer
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{carol, dave}})
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	e.as("bob", "principal-dm", "ensure", carol).ok(t)
	e.as("bob", "principal-dm", "ensure", dave).ok(t)
	c, d := principalEntry(t, f, carol), principalEntry(t, f, dave)
	if c.Room == d.Room || c.SubID == d.SubID {
		t.Fatal("principals share a room or subscription")
	}
	before := readFile(t, f.path("json"))
	e.as("bob", "principal-dm", "unset", "@erin:"+testServer).ok(t)
	if readFile(t, f.path("json")) != before {
		t.Fatal("never-configured unset wrote config")
	}
	e.as("bob", "principal-dm", "unset", carol).ok(t)
	if got := principalEntry(t, f, dave); got != d {
		t.Fatal("unset of one principal changed the other", got)
	}
	subs := e.as("bob", "subs", "--json").ok(t).stdout
	if strings.Contains(subs, c.Room) || !strings.Contains(subs, d.Room) {
		t.Fatal("wrong subscription removed")
	}
	principalJoin(e, d.Room, dave)
	out := e.as("bob", "principal-dm", "ensure", dave).ok(t)
	if got := principalEntry(t, f, dave); !strings.Contains(out.stdout, "activated") || !got.Active || got.Room != d.Room {
		t.Fatal("re-ensure after join did not activate in place", got)
	}
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{dave}})
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "not in the host pin")
	e.as("bob", "principal-dm", "unset", carol).ok(t)
	e.as("bob", "principal-dm", "say", dave, "still here").ok(t)
}

// Proves a missing pin pins nobody, an invalid principal config or a present
// but invalid owner pin refuses mutations, refusals leave subscriptions and
// archive exclusions untouched, and world-readable history blocks say.
func TestPrincipalDMMissingPinInvalidConfigAndHistory(t *testing.T) {
	e, f, carol := principalFixture(t)
	subsBefore := e.as("bob", "subs", "--json").ok(t).stdout
	if err := os.Remove(f.path("pin")); err != nil {
		t.Fatal(err)
	}
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "not in the host pin")
	e.as("bob", "principal-dm", "say", carol, "x").failsWith(t, ExitError, "not in the host pin")
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{carol}})
	ownerPin := ownerFiles(e.paths(), "bob").path("pin")
	if err := os.Chmod(ownerPin, 0644); err != nil {
		t.Fatal(err)
	}
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "owner-dm pin is present but invalid")
	e.as("bob", "principal-dm", "unset", carol).failsWith(t, ExitError, "owner-dm pin is present but invalid")
	if err := os.Chmod(ownerPin, 0600); err != nil {
		t.Fatal(err)
	}
	ownerPut(t, ownerPin, map[string]any{"mode": "bogus", "owner": "@alice:" + testServer})
	e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "owner-dm pin is present but invalid")
	e.as("bob", "principal-dm", "unset", carol).failsWith(t, ExitError, "owner-dm pin is present but invalid")
	ownerPut(t, ownerPin, ownerDMPin{Mode: "host", Owner: "@alice:" + testServer})
	for _, bad := range []string{
		`{"v":1,"dms":{"@carol:test.local":{"generation":0,"active":false,"pending_join":true,"room":"!r","sub_id":"s","created_at":1}}}`,
		`{"v":1,"dms":{"@carol:test.local":{"generation":1,"active":true,"pending_join":true,"room":"!r","sub_id":"s","created_at":1}}}`,
		`{"v":1,"dms":{"@carol:test.local":{"generation":1,"active":true,"pending_join":false,"room":"!r","sub_id":"","created_at":1}}}`,
		`{"v":1,"dms":{"@carol:test.local":{"generation":1,"active":false,"pending_join":true,"room":"!r","sub_id":"s","created_at":null}}}`,
		`{"v":1,"dms":{"@carol:test.local":{"generation":1,"active":false,"pending_join":true,"room":"!r","sub_id":"s","created_at":1,"x":1}}}`,
		`{"v":1,"dms":null}`, `{"v":2,"dms":{}}`, `{"v":1,"dms":{},"x":1}`, `[]`,
	} {
		ownerPut(t, f.path("json"), json.RawMessage(bad))
		e.as("bob", "principal-dm", "ensure", carol).failsWith(t, ExitError, "invalid principal-dm config")
		e.as("bob", "principal-dm", "say", carol, "x").failsWith(t, ExitError, "invalid principal-dm config")
		if st := e.as("bob", "principal-dm", "status").ok(t); !strings.Contains(st.stdout, "invalid") {
			t.Fatal("status did not report invalid config", bad)
		}
	}
	if e.as("bob", "subs", "--json").ok(t).stdout != subsBefore {
		t.Fatal("a refusal changed subscriptions")
	}
	if ex, err := ownerFiles(e.paths(), "bob").exclusions(); err != nil || len(ex) != 0 {
		t.Fatal("a refusal changed archive exclusions", ex, err)
	}
	if err := os.Remove(f.path("json")); err != nil {
		t.Fatal(err)
	}
	e.as("bob", "principal-dm", "ensure", carol).ok(t)
	d := principalEntry(t, f, carol)
	principalJoin(e, d.Room, carol)
	e.hs.mu.Lock()
	e.hs.rooms[d.Room].state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"world_readable"}`)
	e.hs.mu.Unlock()
	e.as("bob", "principal-dm", "say", carol, "leak").failsWith(t, ExitError, "history_visibility")
	if b := ownerBodies(e, d.Room); len(b) != 0 {
		t.Fatal("sent into world-readable room", b)
	}
}
