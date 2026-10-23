package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A planted missing subscription is reported as a coverage failure (not a
// command/setup failure). Repairing it makes the same checker pass. Neither
// check writes account data or delivery state, including legacy migration.
func TestSubsCheckMissingAndRepair(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "subs", "sync").ok(t)
	e.as("alice", "subs", "check").ok(t)
	list := e.serverSubs("alice")
	list.Subs = nil
	e.hs.setAccountData("@alice:"+testServer, SubsType, list)
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.LegacySubscriptions = map[string][]string{"*": {"system", "owner-needed", "decision"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	puts := e.hs.CallCount(accountDataPut)
	r := e.as("alice", "subs", "check", "--json").failsWith(t, ExitError, "subscription coverage gaps")
	var result struct {
		OK    bool `json:"ok"`
		Rooms []struct {
			Missing []string `json:"missing"`
		} `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &result); err != nil || result.OK || len(result.Rooms) != 2 {
		t.Fatalf("coverage result = %s (%v)", r.stdout, err)
	}
	for _, room := range result.Rooms {
		if strings.Join(room.Missing, ",") != "system,owner-needed,decision" {
			t.Fatalf("missing standard kinds = %v", room.Missing)
		}
	}
	after, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || e.hs.CallCount(accountDataPut) != puts {
		t.Fatal("read-only coverage check mutated state or subscriptions")
	}
	e.as("alice", "subs", "sync").ok(t)
	if out := e.as("alice", "subs", "check").ok(t).stdout; !strings.Contains(out, "coverage: ok") {
		t.Fatalf("repair did not satisfy checker: %s", out)
	}
}

// Coverage is the union of active, unscoped selectors. A restricted or
// expired row cannot hide a missing kind; all-events still obeys its filters.
func TestSubsCheckSelectorCoverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sel     SubSelector
		covered bool
	}{
		{"standard", SubSelector{Kinds: []string{"system", "owner-needed", "decision"}}, true},
		{"partial", SubSelector{Kinds: []string{"decision"}}, false},
		{"all-events", SubSelector{AllEvents: true}, true},
		{"all-events-kind-filter", SubSelector{AllEvents: true, Kinds: []string{"decision"}}, false},
		{"sender", SubSelector{Kinds: onboardKinds, From: "@bob:test.local"}, false},
		{"thread", SubSelector{Kinds: onboardKinds, Topic: "$thread"}, false},
		{"subject", SubSelector{Kinds: onboardKinds, Subject: "BUG-1"}, false},
		{"subtype", SubSelector{Kinds: onboardKinds, Subtypes: []string{"incident"}}, false},
		{"product-projection", SubSelector{Kinds: onboardKinds, Product: "alpha"}, false},
		{"expired", SubSelector{Kinds: onboardKinds, Until: "2000-01-01T00:00:00Z"}, false},
		{"invalid-expiry", SubSelector{Kinds: onboardKinds, Until: "invalid"}, false},
		{"one-shot", SubSelector{Kinds: onboardKinds, UntilKind: "system"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			sel := tc.sel
			sel.Room = room
			list := SubList{V: 1, Subs: []Sub{{ID: "sub-control", SubSelector: sel, CreatedAt: "2000-01-01T00:00:00Z"}, {ID: "sub-lobby", SubSelector: SubSelector{Room: "!lobbyRoomID", Kinds: []string{"system", "owner-needed", "decision"}}}}}
			e.hs.setAccountData("@alice:"+testServer, SubsType, list)
			if tc.covered {
				e.as("alice", "subs", "check").ok(t)
			} else {
				e.as("alice", "subs", "check").failsWith(t, ExitError, "subscription coverage gaps")
			}
		})
	}
	// Independent global selectors can collectively cover the three kinds.
	e, _ := world(t)
	list := SubList{V: 1}
	for _, kind := range []string{"system", "owner-needed", "decision"} {
		list.Subs = append(list.Subs, Sub{SubSelector: SubSelector{Kinds: []string{kind}}, CreatedAt: time.Now().Add(-time.Hour).Format(time.RFC3339)})
	}
	e.hs.setAccountData("@alice:"+testServer, SubsType, list)
	e.as("alice", "subs", "check").ok(t)
}

// Only the actual global system/broadcast rooms are exempt. Unknown reads
// fail without printing a success report; bad usage does not contact Matrix.
func TestSubsCheckExemptionsAndFailures(t *testing.T) {
	e, _ := world(t)
	e.hs.addRoom("system", "@alice:"+testServer)
	e.hs.addRoom("broadcast", "@alice:"+testServer)
	e.as("alice", "product", "new", "alpha").ok(t)
	e.as("alice", "subs", "sync").ok(t)
	out := e.as("alice", "subs", "check", "--json").ok(t).stdout
	if !strings.Contains(out, "projection room") || !strings.Contains(out, "broadcast membership") || !strings.Contains(out, `"skipped":"space"`) {
		t.Fatalf("missing explicit exemptions: %s", out)
	}
	for _, sub := range e.serverSubs("alice").Subs {
		if sub.Room == "!alphaRoomID" {
			t.Fatal("sync subscribed a space")
		}
	}
	puts := e.hs.CallCount(accountDataPut)
	e.as("alice", "subs", "check", "extra").failsWith(t, ExitUsage, "usage: a2a subs check")
	e.hs.fail("GET /_matrix/client/v3/user/{user}/account_data/{type}", 503, "M_UNAVAILABLE", "unavailable")
	r := e.as("alice", "subs", "check", "--json").failsWith(t, ExitError, "read subscriptions")
	if r.stdout != "" || e.hs.CallCount(accountDataPut) != puts {
		t.Fatal("failed read emitted coverage or wrote account data")
	}
}

// A previously narrowed project remains narrowed across preset onboarding
// and preset reconciliation; role-specific rows cannot reintroduce opt-outs.
func TestPresetRespectsRecordedTrim(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.as("alice", "broadcast-room", "init").ok(t)
	project := e.hs.addRoom("alpha-proj", alice)
	e.hs.makePublic(project)
	e.as("alice", "product", "new", "alpha").ok(t)
	e.as("alice", "host", "init", "mini").ok(t)
	name := "mini_proj_orch"
	e.agent(name)
	e.as(name, "join", "alpha-proj").ok(t)
	e.as(name, "unsubscribe", "--all").ok(t)
	id, _ := e.subscribe(name, "--room", "alpha-proj", "--kind", "owner-needed")
	e.as(name, "subs", "trim", "--room", "alpha-proj", "--drop", "system,decision", "--reason", "Deliberate own-room filter").ok(t)
	args := []string{"identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "orch"}
	e.as(name, args...).ok(t)
	e.as(name, "subs", "sync", "--preset", "orch", "--product", "alpha").ok(t)
	e.as(name, "subs", "sync").ok(t)
	e.as(name, "subs", "check").ok(t)
	manual := false
	for _, sub := range e.serverSubs(name).Subs {
		manual = manual || sub.ID == id
		if sub.Room == project && (contains(sub.Kinds, "system") || contains(sub.Kinds, "decision")) {
			t.Fatal("preset broadened recorded trim")
		}
	}
	if !manual {
		t.Fatal("preset removed manual row")
	}
}

// Plain onboarding, as well as product presets, preserves an explicit trim
// on re-entry and does not re-add the previously omitted standard kinds.
func TestPlainOnboardingRespectsRecordedTrim(t *testing.T) {
	e := newEnv(t)
	e.ensure("--role", "orch", "--project", "proj", "--runtime", "none").ok(t)
	name := "mini_proj_orch"
	e.as(name, "unsubscribe", "--all").ok(t)
	e.as(name, "subscribe", "--room", "proj", "--kind", "owner-needed").ok(t)
	e.as(name, "subs", "trim", "--room", "proj", "--drop", "system,decision", "--reason", "Deliberate own-room filter").ok(t)
	e.ensure("--role", "orch", "--project", "proj", "--runtime", "none").ok(t)
	e.as(name, "subs", "check").ok(t)
	for _, sub := range e.serverSubs(name).Subs {
		if sub.Room == "!projRoomID" && (contains(sub.Kinds, "system") || contains(sub.Kinds, "decision")) {
			t.Fatal("plain onboarding broadened recorded trim")
		}
	}
}

// A deliberate omission is quiet only after an explicit room/kind record.
// Sync/join preserve the trim, unrelated selectors and private policy bytes;
// an old delivery-state rewrite cannot erase it. Clearing makes gaps visible.
func TestSubsTrimLifecycle(t *testing.T) {
	e, room := world(t)
	e.as("alice", "subscribe", "--room", "proj", "--kind", "owner-needed").ok(t)
	e.as("alice", "subscribe", "--room", "lobby", "--kind", "system,owner-needed,decision").ok(t)
	e.as("alice", "subs", "check").failsWith(t, ExitError, "subscription coverage gaps")
	puts := e.hs.CallCount(accountDataPut)
	e.as("alice", "subs", "trim", "--room", "proj", "--drop", "system,decision", "--reason", "Deliberate duplicate-traffic filter").ok(t)
	if e.hs.CallCount(accountDataPut) != puts {
		t.Fatal("recording a trim changed server subscriptions")
	}
	policyPath := e.paths().SubscriptionPolicyFile("alice")
	policyBefore, err := os.ReadFile(policyPath)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(policyPath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("subscription policy is not private")
	}
	if err := config.SaveState(e.paths(), "alice", config.State{}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"subs", "check"}, {"subs", "sync"}, {"join", "proj"}} {
		e.as("alice", args...).ok(t)
	}
	// Leaving removes server rows, but must preserve the recorded intent for
	// a later invite/rejoin of this same room ID.
	e.as("alice", "leave", "proj").ok(t)
	e.as("bob", "invite", "proj", "alice").ok(t)
	e.as("alice", "join", "proj").ok(t)
	if out := e.as("alice", "subs", "check").ok(t).stdout; !strings.Contains(out, "trimmed: system,decision (recorded)") {
		t.Fatalf("trim not reported: %s", out)
	}
	for _, sub := range e.serverSubs("alice").Subs {
		if sub.Room == room && (contains(sub.Kinds, "system") || contains(sub.Kinds, "decision")) {
			t.Fatal("sync or join broadened the recorded trim")
		}
	}
	policyAfter, err := os.ReadFile(policyPath)
	if err != nil || !bytes.Equal(policyBefore, policyAfter) {
		t.Fatal("policy changed during checks/sync/state rewrite")
	}
	e.as("alice", "subs", "trim", "--room", "proj", "--clear").ok(t)
	e.as("alice", "subs", "check").failsWith(t, ExitError, "subscription coverage gaps")
	e.as("alice", "subs", "sync").ok(t)
	e.as("alice", "subs", "check").ok(t)
}

// An explicit opt-out of all three kinds creates no invalid empty selector;
// the same empty server list without those records was rejected above.
func TestAllKindsTrimHasNoDefault(t *testing.T) {
	e, _ := world(t)
	for _, room := range []string{"proj", "lobby"} {
		e.as("alice", "subs", "trim", "--room", room, "--drop", "system,owner-needed,decision", "--reason", "Explicit full baseline opt-out").ok(t)
	}
	e.as("alice", "subs", "sync").ok(t)
	e.as("alice", "subs", "check").ok(t)
	if len(e.serverSubs("alice").Subs) != 0 {
		t.Fatal("full trim created a subscription")
	}
}

// Inviting alone never joins/subscribes an identity. Acceptance via ordinary
// join creates one baseline; repeated acceptance does not duplicate it.
func TestInviteAcceptanceBaseline(t *testing.T) {
	e, room := world(t)
	e.as("carol", "join", "proj").failsWith(t, ExitError, "invite-only")
	e.as("alice", "invite", "proj", "carol").ok(t)
	if len(e.serverSubs("carol").Subs) != 0 {
		t.Fatal("invite auto-subscribed recipient")
	}
	e.as("carol", "join", "proj").ok(t)
	e.as("carol", "join", "proj").ok(t)
	list := e.serverSubs("carol")
	if len(list.Subs) != 1 || list.Subs[0].Room != room || strings.Join(list.Subs[0].Kinds, ",") != "system,owner-needed,decision" {
		t.Fatalf("invite acceptance subscriptions = %+v", list)
	}
	e.as("carol", "subs", "check").ok(t)
}

// Invalid policy/command input fails closed; neither server subscriptions nor
// a corrupt policy can be overwritten merely to make coverage pass.
func TestSubsTrimRefusals(t *testing.T) {
	e, _ := world(t)
	for _, args := range [][]string{
		{"--room", "proj", "--drop", "notice", "--reason", "bad"},
		{"--room", "proj", "--drop", "system"},
		{"--room", "proj", "--clear", "--drop", "system"},
	} {
		e.as("alice", append([]string{"subs", "trim"}, args...)...).failsWith(t, ExitUsage, "a2a:")
	}
	e.as("carol", "subs", "trim", "--room", "proj", "--drop", "system", "--reason", "not a member").failsWith(t, ExitError, "joined room")
	e.as("alice", "subs", "trim", "--room", "proj", "--drop", "system", "--reason", "control").ok(t)
	path := e.paths().SubscriptionPolicyFile("alice")
	bad := []byte(`{"v":999,"trims":{}}`)
	if err := os.WriteFile(path, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	puts := e.hs.CallCount(accountDataPut)
	e.as("alice", "subs", "check").failsWith(t, ExitError, "unsupported subscription policy")
	e.as("alice", "subs", "sync").failsWith(t, ExitError, "unsupported subscription policy")
	e.as("alice", "subs", "trim", "--room", "proj", "--clear").failsWith(t, ExitError, "unsupported subscription policy")
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, bad) || e.hs.CallCount(accountDataPut) != puts {
		t.Fatal("failed policy read was overwritten")
	}
}
