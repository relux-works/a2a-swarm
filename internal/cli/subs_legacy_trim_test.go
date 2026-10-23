package cli

import (
	"bytes"
	"os"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Each public repair path must retain a room-ID trim recorded via its alias.
// A restored legacy State must not add an absent dropped kind to account data.
func TestReviewR7TrimMigration(t *testing.T) {
	for _, command := range []string{"join", "sync", "plain", "preset", "reconcile"} {
		for _, legacy := range []bool{false, true} {
			label := "clean"
			if legacy {
				label = "legacy"
			}
			t.Run(command+"/"+label, func(t *testing.T) {
				e := newEnv(t)
				e.swarm()
				name := "mini_proj_orch"
				uid := e.agent(name)
				id := e.hs.addRoom("proj", uid)
				e.hs.makePublic(id)
				e.as(name, "join", "lobby").ok(t)
				e.as(name, "subs", "trim", "--room", "#proj:test.local", "--drop", "system", "--reason", "explicit omission").ok(t)
				if legacy {
					if err := config.SaveState(e.paths(), name, config.State{LegacySubscriptions: map[string][]string{id: {"system"}}}); err != nil {
						t.Fatal(err)
					}
				}
				switch command {
				case "join":
					e.as(name, "join", id).ok(t)
				case "sync":
					e.as(name, "subs", "sync").ok(t)
				case "plain":
					e.as(name, "identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none").ok(t)
				case "preset":
					e.as(name, "identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--preset", "arbiter", "--resource", id).ok(t)
				case "reconcile":
					e.as(name, "subs", "sync", "--preset", "arbiter", "--resource", id).ok(t)
				}
				decision := false
				for _, s := range e.serverSubs(name).Subs {
					if s.Room == id {
						if contains(s.Kinds, "system") {
							t.Errorf("repair re-added recorded dropped kind system: %+v", s)
						}
						decision = decision || contains(s.Kinds, "decision")
					}
				}
				if !decision {
					t.Error("untrimmed decision positive control missing")
				}
			})
		}
	}
}

// Automatic migration retains allowed legacy kinds. A failed server write
// preserves local legacy intent and policy as well as remote subscription bytes.
func TestRepairLegacyMigrationKeepsAllowedKindsAndRetries(t *testing.T) {
	for _, failWrite := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failed-write-retry"}[failWrite], func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "subs", "trim", "--room", room, "--drop", "system", "--reason", "Retain owner-needed and decision").ok(t)
			if err := config.SaveState(e.paths(), "alice", config.State{LegacySubscriptions: map[string][]string{room: {"system", "decision", "notice"}, "!lobbyRoomID": {"system", "notice"}}}); err != nil {
				t.Fatal(err)
			}
			state, _ := os.ReadFile(e.paths().StateFile("alice"))
			policy, _ := os.ReadFile(e.paths().SubscriptionPolicyFile("alice"))
			remote := e.hs.accountDataOf("@alice:"+testServer, SubsType)
			if failWrite {
				e.hs.fail(accountDataPut, 503, "M_UNAVAILABLE", "migration unavailable")
				e.as("alice", "subs", "sync").failsWith(t, ExitError, "save subscriptions")
				after, _ := os.ReadFile(e.paths().StateFile("alice"))
				if !bytes.Equal(state, after) || !bytes.Equal(remote, e.hs.accountDataOf("@alice:"+testServer, SubsType)) {
					t.Fatal("failed write consumed legacy state or changed server rows")
				}
				e.hs.mu.Lock()
				delete(e.hs.failures, accountDataPut)
				e.hs.mu.Unlock()
			}
			e.as("alice", "subs", "sync").ok(t)
			found := map[string]bool{}
			unrelated := false
			for _, sub := range e.serverSubs("alice").Subs {
				unrelated = unrelated || sub.Room == "!lobbyRoomID" && sub.Source == "legacy" && contains(sub.Kinds, "system") && contains(sub.Kinds, "notice")
				if sub.Room == room && sub.Source == "legacy" {
					for _, kind := range sub.Kinds {
						found[kind] = true
					}
				}
			}
			if found["system"] || !found["decision"] || !found["notice"] || !unrelated {
				t.Fatalf("legacy migration changed allowed kinds or restored trim: %v", found)
			}
			st, err := config.LoadState(e.paths(), "alice")
			if err != nil || len(st.LegacySubscriptions) != 0 {
				t.Fatal("successful migration retained legacy map", err)
			}
			after, _ := os.ReadFile(e.paths().SubscriptionPolicyFile("alice"))
			if !bytes.Equal(policy, after) {
				t.Fatal("migration changed policy bytes")
			}
		})
	}
}

// A legacy global selector cannot represent an exception for one room.
// Conflicting repair refuses before writes; disjoint kinds still migrate.
// Explicit unsubscribe-all resolves the conflict atomically without publishing
// the forbidden intermediate global selector, then ordinary repair succeeds.
func TestRepairLegacyGlobalTrimConflict(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "disjoint-control", true: "conflicting-global"}[conflict], func(t *testing.T) {
			e, room := world(t)
			drop := "decision"
			if conflict {
				drop = "system"
			}
			e.as("alice", "subs", "trim", "--room", room, "--drop", drop, "--reason", "Explicit room exception").ok(t)
			if err := config.SaveState(e.paths(), "alice", config.State{LegacySubscriptions: map[string][]string{config.LegacyAllRooms: {"system", "notice"}}}); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(e.paths().StateFile("alice"))
			policy, _ := os.ReadFile(e.paths().SubscriptionPolicyFile("alice"))
			remote := e.hs.accountDataOf("@alice:"+testServer, SubsType)
			puts := e.hs.CallCount(accountDataPut)
			if conflict {
				e.as("alice", "subs", "sync").failsWith(t, ExitError, "legacy global subscription conflicts with a recorded room trim")
				after, _ := os.ReadFile(e.paths().StateFile("alice"))
				if !bytes.Equal(before, after) || !bytes.Equal(remote, e.hs.accountDataOf("@alice:"+testServer, SubsType)) || e.hs.CallCount(accountDataPut) != puts {
					t.Fatal("conflicting global migration changed state or subscriptions")
				}
				e.as("alice", "unsubscribe", "--all").ok(t)
				if len(e.serverSubs("alice").Subs) != 0 {
					t.Fatal("explicit resolution published a migrated global selector")
				}
			}
			e.as("alice", "subs", "sync").ok(t)
			global := false
			for _, sub := range e.serverSubs("alice").Subs {
				global = global || sub.Room == "" && sub.Source == "legacy" && contains(sub.Kinds, "system") && contains(sub.Kinds, "notice")
				if conflict && (sub.Room == room || sub.Room == "") && contains(sub.Kinds, drop) {
					t.Fatal("repair restored the dropped kind after explicit resolution")
				}
			}
			if !conflict && !global {
				t.Fatal("disjoint trim prevented allowed global migration")
			}
			after, _ := os.ReadFile(e.paths().SubscriptionPolicyFile("alice"))
			if !bytes.Equal(policy, after) {
				t.Fatal("global migration changed policy")
			}
		})
	}
}
