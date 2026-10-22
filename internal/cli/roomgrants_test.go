package cli

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// Drive all three public sinks with retired accounts absent and registered.
// The fake models requests and membership, not real account lifecycle rules.
func TestRoomAutomaticIdentitiesEmpty(t *testing.T) {
	for _, registered := range []bool{false, true} {
		state := "absent"
		if registered {
			state = "registered"
		}
		for _, sink := range []string{"room", "product", "identity"} {
			for _, property := range []string{"power", "invites"} {
				t.Run(sink+"/"+state+"/"+property, func(t *testing.T) {
					e := newEnv(t)
					e.agent("alice")
					if registered {
						e.hs.addUser("local-models-e9")
						e.hs.addUser("a2a-projector")
					}
					local, wantCreates := "team", 1
					switch sink {
					case "room":
						e.as("alice", "room", "new", local).ok(t)
					case "product":
						local, wantCreates = "alpha-lobby", 4
						e.as("alice", "product", "new", "alpha").ok(t)
					case "identity":
						local = "proj"
						e.agent("mini_proj_orch")
						e.ensure("--role", "orch", "--project", local, "--runtime", "none").ok(t)
					}
					created := e.hs.createdRooms()
					if len(created) != wantCreates {
						t.Fatalf("create control: got %d rooms, want %d", len(created), wantCreates)
					}
					found := false
					for _, req := range created {
						if property == "power" {
							users, ok := req.PowerLevelContentOverride["users"].(map[string]any)
							if !ok || len(users) != 0 {
								t.Fatalf("automatic power entries in %s: %#v; want empty users", req.RoomAliasName, req.PowerLevelContentOverride["users"])
							}
						}
						if req.RoomAliasName == local {
							found = true
							if req.Preset != "public_chat" || req.Visibility != "private" || req.PowerLevelContentOverride["redact"] != float64(100) {
								t.Fatalf("room policy changed: %+v", req)
							}
						}
						if property == "invites" && len(req.Invite) != 0 {
							t.Fatalf("automatic createRoom invites: %v", req.Invite)
						}
					}
					if !found {
						t.Fatalf("missing create control for %s", local)
					}
					if property == "invites" && e.hs.CallCount(inviteRoute) != 0 {
						t.Fatalf("automatic invite requests: %d; want zero even for absent accounts", e.hs.CallCount(inviteRoute))
					}
				})
			}
		}
	}
}

// Legitimate caller invites work in either retired-account world, with no
// power entry, and duplicate/self invitees do not create extra requests.
func TestRoomExplicitInvitesWithoutPower(t *testing.T) {
	for _, registered := range []bool{false, true} {
		name := "absent"
		if registered {
			name = "registered"
		}
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.agent("alice")
			bob := e.agent("bob")
			if registered {
				e.hs.addUser("local-models-e9")
				e.hs.addUser("a2a-projector")
			}
			e.as("alice", "room", "new", "team", "--invite", "bob,@bob:test.local,alice,@ALICE:test.local").ok(t)
			created := e.hs.createdRooms()
			if len(created) != 1 {
				t.Fatalf("create control: %d", len(created))
			}
			users, ok := created[0].PowerLevelContentOverride["users"].(map[string]any)
			if !ok || len(users) != 0 {
				t.Fatalf("explicit invite granted power (creator %s must also be omitted): %v", alice, users)
			}
			if e.hs.CallCount(inviteRoute) != 1 {
				t.Fatalf("invite attempts = %d, want exactly bob", e.hs.CallCount(inviteRoute))
			}
			e.hs.mu.Lock()
			defer e.hs.mu.Unlock()
			if len(e.hs.invites) != 1 || e.hs.invites[0].user != bob || e.hs.invites[0].room != "!teamRoomID" {
				t.Fatalf("explicit invites: %+v", e.hs.invites)
			}
		})
	}
}

// Missing explicit accounts remain nonfatal; other invite errors remain
// actionable failures after creation. No default invite can mask the error.
func TestRoomExplicitInvitationErrors(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		e := newEnv(t)
		e.agent("alice")
		e.agent("bob")
		out := e.as("alice", "room", "new", "team", "--invite", "missing,bob").ok(t).stdout
		if !strings.Contains(out, "not invited @missing:test.local to #team: no such account yet") || !strings.Contains(out, "invited @bob:test.local to #team") || e.hs.CallCount(inviteRoute) != 2 {
			t.Fatalf("explicit absent invite handling: %s", out)
		}
	})
	t.Run("forbidden", func(t *testing.T) {
		e := newEnv(t)
		e.agent("alice")
		e.agent("bob")
		e.hs.fail(inviteRoute, http.StatusForbidden, "M_FORBIDDEN", "invite denied")
		e.as("alice", "room", "new", "team", "--invite", "bob").failsWith(t, ExitError, "#team was created, but inviting @bob:test.local failed:")
		if len(e.hs.createdRooms()) != 1 || e.hs.CallCount(inviteRoute) != 1 {
			t.Fatal("invite failure lost the created room or attempted implicit invites")
		}
	})
}

// A concurrent winner between resolve and create is returned without invites
// or a new creator subscription; a failed resolve is never treated as absence.
func TestRoomAliasRaceAndResolveFailure(t *testing.T) {
	t.Run("concurrent winner", func(t *testing.T) {
		e := newEnv(t)
		alice := e.agent("alice")
		e.agent("bob")
		e.configure = func(a *App) {
			base := a.HTTP.Transport
			a.HTTP = &http.Client{Transport: roomPolicyRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPost && r.URL.Path == "/_matrix/client/v3/createRoom" {
					e.hs.addRoom("team", alice)
				}
				return base.RoundTrip(r)
			})}
		}
		out := e.as("alice", "room", "new", "team", "--invite", "bob").ok(t).stdout
		if out != "exists #team !teamRoomID\n" || len(e.hs.createdRooms()) != 0 || e.hs.CallCount(createRoute) != 1 || e.hs.CallCount(inviteRoute) != 0 || len(e.subsOf("alice")) != 0 {
			t.Fatalf("race changed room or subscription: %s", out)
		}
	})
	t.Run("resolve failure", func(t *testing.T) {
		e := newEnv(t)
		e.agent("alice")
		e.hs.fail("GET /_matrix/client/v3/directory/room/{alias}", http.StatusForbidden, "M_FORBIDDEN", "alias unreadable")
		e.as("alice", "room", "new", "team").failsWith(t, ExitError, "M_FORBIDDEN")
		if e.hs.CallCount(createRoute) != 0 || e.hs.CallCount(inviteRoute) != 0 {
			t.Fatal("resolve failure treated as absent alias")
		}
	})
}

type roomPolicyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f roomPolicyRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// Preset onboarding refuses a missing required project room, then succeeds
// when it exists; neither path falls through into plain onboarding creation.
func TestIdentityPresetNoAutomaticRoomCreation(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.hs.makePublic(e.hs.addRoom("broadcast", alice))
	e.as("alice", "product", "new", "alpha").ok(t)
	e.as("alice", "host", "init", "mini").ok(t)
	e.agent("mini_proj_orch")
	before := e.hs.createdRooms()
	invites := e.hs.CallCount(inviteRoute)
	args := []string{"identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "orch"}
	e.run(args...).failsWith(t, ExitError, "required room #alpha-proj")
	if !reflect.DeepEqual(before, e.hs.createdRooms()) || e.hs.CallCount(inviteRoute) != invites {
		t.Fatal("missing preset room triggered creation or invitations")
	}
	e.hs.makePublic(e.hs.addRoom("alpha-proj", alice))
	out := e.run(args...).ok(t).stdout
	if !reflect.DeepEqual(before, e.hs.createdRooms()) || e.hs.CallCount(inviteRoute) != invites || !strings.Contains(out, "joined #alpha-lobby") {
		t.Fatalf("preset created rooms or invited users: %s", out)
	}
}
