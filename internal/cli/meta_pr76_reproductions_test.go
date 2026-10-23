package cli

import (
	"bytes"
	"context"
	"github.com/relux-works/a2a-swarm/internal/config"
	"os"
	"testing"
	"time"
)

// Public plain onboarding must not add the baseline to exempt room types;
// an ordinary project nearby remains a positive control.
func TestMetaReviewPlainOnboardingExemptions(t *testing.T) {
	for _, project := range []string{"alpha", "system", "broadcast", "ordinary"} {
		t.Run(project, func(t *testing.T) {
			e := newEnv(t)
			e.swarm()
			alice := e.agent("alice")
			if project == "alpha" {
				e.as("alice", "product", "new", "alpha").ok(t)
			} else {
				e.hs.makePublic(e.hs.addRoom(project, alice))
			}
			e.ensure("--role", "orch", "--project", project, "--runtime", "none").ok(t)
			found := false
			for _, sub := range e.serverSubs("mini_" + project + "_orch").Subs {
				if sub.Room == "!"+project+"RoomID" {
					found = true
				}
			}
			if project == "ordinary" && !found {
				t.Error("ordinary project lost its required baseline")
			}
			if project != "ordinary" && found {
				t.Errorf("plain onboarding created forbidden baseline in exempt %s", project)
			}
		})
	}
}

// Re-entering plain --keeper onboarding must not undo a deliberately narrowed
// global-system role selector; trim is an explicit room/kind policy.
func TestMetaReviewPlainKeeperAllEventsTrim(t *testing.T) {
	e := newEnv(t)
	e.swarm()
	alice := e.agent("alice")
	sys := e.hs.addRoom("system", alice)
	e.hs.makePublic(sys)
	e.ensure("--role", "keeper", "--project", "proj", "--runtime", "none", "--keeper").ok(t)
	name := "mini_proj_keeper"
	e.as(name, "unsubscribe", "--all").ok(t)
	e.as(name, "subscribe", "--room", "system", "--kind", "owner-needed").ok(t)
	e.as(name, "subs", "trim", "--room", "system", "--drop", "system,decision", "--reason", "Keep owner-needed only").ok(t)
	e.ensure("--role", "keeper", "--project", "proj", "--runtime", "none", "--keeper").ok(t)
	for _, sub := range e.serverSubs(name).Subs {
		if sub.Room == sys && (sub.AllEvents || contains(sub.Kinds, "system") || contains(sub.Kinds, "decision")) {
			t.Errorf("plain keeper reintroduced dropped kinds via selector: %+v", sub.SubSelector)
		}
	}
	// Clearing the deliberate exception restores the ordinary keeper role;
	// skipping every catch-all would fail this neighboring positive control.
	e.as(name, "subs", "trim", "--room", "system", "--clear").ok(t)
	e.ensure("--role", "keeper", "--project", "proj", "--runtime", "none", "--keeper").ok(t)
	found := false
	for _, sub := range e.serverSubs(name).Subs {
		found = found || sub.Room == sys && sub.AllEvents
	}
	if !found {
		t.Fatal("cleared trim did not restore the keeper all-events role")
	}
}

// Preset re-entry removes a stale catch-all owned by that preset while
// retaining the manual owner-needed selector and honoring the recorded trim.
// Current presets generate kind-filtered role rows, not catch-all rows.
func TestPresetReentryWithAllEventsRoleAndTrim(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.as("alice", "broadcast-room", "init").ok(t)
	room := e.hs.addRoom("alpha-proj", alice)
	e.hs.makePublic(room)
	e.as("alice", "product", "new", "alpha").ok(t)
	e.as("alice", "host", "init", "mini").ok(t)
	name := "mini_proj_orch"
	args := []string{"identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "orch"}
	e.as(name, args...).ok(t)
	e.as(name, "unsubscribe", "--all").ok(t)
	id, _ := e.subscribe(name, "--room", "alpha-proj", "--kind", "owner-needed")
	e.as(name, "subs", "trim", "--room", "alpha-proj", "--drop", "system,decision", "--reason", "Keep owner-needed only").ok(t)
	list := e.serverSubs(name)
	list.Subs = append(list.Subs, Sub{ID: "sub-stale-all", Preset: "orch:alpha", SubSelector: SubSelector{Room: room, AllEvents: true}})
	e.hs.setAccountData("@"+name+":"+testServer, SubsType, list)
	e.as(name, args...).ok(t)
	e.as(name, "subs", "check").ok(t)
	manual := false
	for _, sub := range e.serverSubs(name).Subs {
		manual = manual || sub.ID == id
		if sub.Room == room && (sub.AllEvents || contains(sub.Kinds, "system") || contains(sub.Kinds, "decision")) {
			t.Fatal("preset re-entry retained a stale catch-all or reintroduced dropped kinds")
		}
	}
	if !manual {
		t.Fatal("preset re-entry removed the manual retained selector")
	}
}

// Malformed and unsupported private policy reads must fail without any remote
// subscription mutation or rewriting the corrupt private bytes.
func TestMetaReviewMalformedPolicyAtomicRead(t *testing.T) {
	for _, bad := range []string{`{`, `{"v":1,"trims":{"!projRoomID":{"dropped":["system"],"reason":"x","recorded_at":"invalid"}}}`, `{"v":1,"trims":{"!projRoomID":{"dropped":["notice"],"reason":"x","recorded_at":"2026-01-01T00:00:00Z"}}}`} {
		t.Run(bad, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "subs", "trim", "--room", "proj", "--drop", "system", "--reason", "control").ok(t)
			path := e.paths().SubscriptionPolicyFile("alice")
			if err := os.WriteFile(path, []byte(bad), 0600); err != nil {
				t.Fatal(err)
			}
			puts := e.hs.CallCount(accountDataPut)
			for _, args := range [][]string{{"subs", "check", "--json"}, {"subs", "sync"}, {"subs", "trim", "--room", "proj", "--clear"}} {
				r := e.as("alice", args...)
				if r.code != ExitError {
					t.Errorf("invalid policy accepted: args=%v result=%+v", args, r)
				}
				if len(args) > 1 && args[1] == "check" && r.stdout != "" {
					t.Error("invalid-policy check emitted success output")
				}
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, []byte(bad)) || e.hs.CallCount(accountDataPut) != puts {
				t.Fatal("failed policy read changed bytes/account data")
			}
		})
	}
}

// A public repair started while policy is locked must use the new policy
// committed before unlock. A cleared-policy control must still restore kinds.
func TestMetaReviewPolicySerialization(t *testing.T) {
	e, room := world(t)
	e.as("alice", "subscribe", "--room", "proj", "--kind", "owner-needed").ok(t)
	done := make(chan result, 1)
	var early *result
	err := config.WithRuntimeLock(e.paths(), "alice", "subscription-policy", time.Now().Add(3*time.Second), func() error {
		go func() { done <- e.as("alice", "subs", "sync") }()
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		select {
		case r := <-done:
			early = &r
		case <-timer.C:
		}
		policy := config.SubscriptionPolicy{V: 1, Trims: map[string]config.SubscriptionTrim{room: {Dropped: []string{"system", "decision"}, Reason: "Committed concurrent trim", RecordedAt: time.Now().UTC().Format(time.RFC3339Nano)}}}
		return config.SaveSubscriptionPolicy(e.paths(), "alice", policy)
	})
	if err != nil {
		t.Fatal(err)
	}
	var r result
	if early != nil {
		r = *early
		t.Error("repair completed before policy lock released")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		select {
		case r = <-done:
		case <-ctx.Done():
			t.Fatal("repair did not finish after unlock")
		}
	}
	r.ok(t)
	for _, sub := range e.serverSubs("alice").Subs {
		if sub.Room == room && (contains(sub.Kinds, "system") || contains(sub.Kinds, "decision")) {
			t.Error("repair used stale policy and broadened concurrent trim")
		}
	}
	e.as("alice", "subs", "trim", "--room", "proj", "--clear").ok(t)
	e.as("alice", "subs", "sync").ok(t)
	e.as("alice", "subs", "check").ok(t)
}
