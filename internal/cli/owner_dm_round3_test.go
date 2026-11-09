package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Keeper P2 and ubS6Ltbau: bind/ensure use the same effective state. Only a
// never/inactive effective state creates/invites; pending refuses without writes;
// active keeps room/generation while changing flags. Terminal never advances order.
func TestOwnerDMEnsureBindStateParity(t *testing.T) {
	type outcome struct {
		failed, active, pending, kept bool
		rooms, invites                int
		generation                    uint64
		tag, mirror                   bool
		render                        string
	}
	for _, state := range []string{"never-configured", "pending-unjoined", "pending-joined", "pending-expired", "active", "inactive"} {
		t.Run(state, func(t *testing.T) {
			outcomes := []outcome{}
			for _, action := range []string{"bind", "ensure"} {
				t.Run(action, func(t *testing.T) {
					e, shared, f := ownerFixture(t)
					var c *ownerDMConfig
					target := shared
					if state != "never-configured" {
						c = ownerBind(t, e, shared, f)
						switch state {
						case "pending-joined":
							ownerJoin(e, c, c.Owner)
							target = c.Room
						case "active":
							ownerActivate(t, e, f, c)
							c = ownerLoad(t, f)
							target = c.Room
						case "pending-expired":
							old := time.Now().Add(-25 * time.Hour)
							if err := os.Chtimes(f.path("json"), old, old); err != nil {
								t.Fatal(err)
							}
						case "inactive":
							e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
							e.as("bob", "owner-dm", "unset").ok(t)
							c = ownerLoad(t, f)
						}
					}
					e.configure = func(a *App) {
						a.ownerConfirm = func(want string) error {
							if want != "@alice:"+testServer {
								t.Fatal(want)
							}
							return nil
						}
					}
					before := ownerSnapshot(t, f)
					beforeState := ownerRound3State(t, e)
					beforeBodies := 0
					if c != nil {
						beforeBodies = len(ownerBodies(e, c.Room))
					}
					rooms, invites := ownerRoomInviteCounts(e)
					args := []string{"owner-dm", "ensure", "--tag", "--mirror", "--render", "outside-frame"}
					if action == "bind" {
						id := ownerRequest(e, target, "bind", "tag mirror render=outside-frame", time.Now())
						args = []string{"owner-dm", "bind", "--event", target, id}
					}
					r := e.as("bob", args...)
					failed := state == "pending-unjoined"
					if failed {
						r.failsWith(t, ExitError, "pending claim")
						ownerUnchanged(t, f, before)
						if ownerRound3State(t, e) != beforeState || len(ownerBodies(e, c.Room)) != beforeBodies {
							t.Fatal("pending refusal changed subscription or sent confirmation")
						}
					} else {
						r.ok(t)
					}
					afterRooms, afterInvites := ownerRoomInviteCounts(e)
					got := ownerLoad(t, f)
					if action == "ensure" && ownerSnapshot(t, f)["bindings"] != before["bindings"] {
						t.Fatal("terminal changed watermark/store")
					}
					create := state == "never-configured" || state == "pending-expired" || state == "inactive"
					delta := 0
					if create {
						delta = 1
					}
					oldGen := uint64(0)
					if c != nil {
						oldGen = c.Generation
					}
					wantGen := oldGen
					if create {
						wantGen++
					}
					if afterRooms-rooms != delta || afterInvites-invites != delta || got.Generation != wantGen {
						t.Fatalf("room/invite/generation effects: %d/%d, gen%d want %d/%d, gen%d", afterRooms-rooms, afterInvites-invites, got.Generation, delta, delta, wantGen)
					}
					if create {
						e.hs.mu.Lock()
						inv := e.hs.invites[invites]
						request := e.hs.created[rooms]
						e.hs.mu.Unlock()
						if inv.user != "@alice:"+testServer || inv.room != got.Room || len(request.Invite) != 1 || request.Invite[0] != inv.user {
							t.Fatal("creation did not invite only pinned owner", inv, request.Invite)
						}
					}
					kept := c != nil && c.Room == got.Room
					if c != nil && kept == create {
						t.Fatal("wrong room reuse", state, got.Room, c.Room)
					}
					if !failed && (!got.Tag || !got.Mirror || got.Render != "outside-frame") {
						t.Fatal("flags not applied", got)
					}
					if action == "ensure" && !create && !failed && (got.BoundBy != c.BoundBy || got.SubID != c.SubID) {
						t.Fatal("ensure changed more than flags/lifecycle", got, c)
					}
					outcomes = append(outcomes, outcome{failed, got.Active, got.Pending, kept, afterRooms - rooms, afterInvites - invites, got.Generation, got.Tag, got.Mirror, got.Render})
					if action == "ensure" && state == "pending-joined" {
						bodies := ownerBodies(e, got.Room)
						if !got.Active || got.Pending || len(bodies) != beforeBodies+1 || !strings.Contains(bodies[len(bodies)-1], ownerFingerprint(got)) || !strings.Contains(bodies[len(bodies)-1], "tag=true (forced off in S1), mirror=true, render=outside-frame") {
							t.Fatal("activation confirmation missing new flags", got, bodies)
						}
						journal := ownerSnapshot(t, f)["journal"]
						e.as("bob", args...).ok(t)
						repeated := ownerLoad(t, f)
						repeatedRooms, repeatedInvites := ownerRoomInviteCounts(e)
						if *repeated != *got || len(ownerBodies(e, got.Room)) != len(bodies) || ownerSnapshot(t, f)["journal"] != journal || repeatedRooms != afterRooms || repeatedInvites != afterInvites || ownerRound3State(t, e) != beforeState {
							t.Fatal("repeated ensure changed binding/subscription or repeated confirmation")
						}
					}
				})
			}
			if len(outcomes) != 2 || outcomes[0] != outcomes[1] {
				t.Fatal("bind/ensure diverged", outcomes)
			}
		})
	}
}

func ownerRoomInviteCounts(e *env) (int, int) {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	return len(e.hs.created), len(e.hs.invites)
}

func ownerRound3State(t *testing.T, e *env) string {
	t.Helper()
	b, err := os.ReadFile(e.paths().StateFile("bob"))
	if os.IsNotExist(err) {
		return "<absent>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Confirmation-adapter controls: a mismatch or external pin replacement before
// the locked recheck causes no CLI writes/messages/provisioning. Restoring the
// pin and confirming permits a flags-only change; real TTY checks remain separate.
func TestOwnerDMEnsureConfirmationFence(t *testing.T) {
	for _, failure := range []string{"type-back", "pin-change"} {
		t.Run(failure, func(t *testing.T) {
			e, c, f := ownerActiveFixture(t)
			before := ownerSnapshot(t, f)
			state := ownerRound3State(t, e)
			bodies := len(ownerBodies(e, c.Room))
			rooms, invites := ownerRoomInviteCounts(e)
			e.configure = func(a *App) {
				a.ownerConfirm = func(want string) error {
					if want != c.Owner {
						t.Fatal("wrong confirmation target", want)
					}
					if failure == "type-back" {
						return errors.New("type-back mismatch")
					}
					// Simulate an external host edit, then assert no subsequent CLI writes.
					ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@carol:" + testServer})
					before = ownerSnapshot(t, f)
					return nil
				}
			}
			message := "type-back mismatch"
			if failure == "pin-change" {
				message = "pin changed after confirmation"
			}
			e.as("bob", "owner-dm", "ensure", "--tag").failsWith(t, ExitError, message)
			ownerUnchanged(t, f, before)
			afterRooms, afterInvites := ownerRoomInviteCounts(e)
			if ownerRound3State(t, e) != state || len(ownerBodies(e, c.Room)) != bodies || afterRooms != rooms || afterInvites != invites {
				t.Fatal("confirmation refusal had effects")
			}
			ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: c.Owner})
			e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
			e.as("bob", "owner-dm", "ensure", "--tag").ok(t)
			got := ownerLoad(t, f)
			afterRooms, afterInvites = ownerRoomInviteCounts(e)
			if !got.Tag || !got.Active || got.Room != c.Room || got.Generation != c.Generation || afterRooms != rooms || afterInvites != invites || len(ownerBodies(e, c.Room)) != bodies {
				t.Fatal("confirmed flags-only control", got)
			}
		})
	}
}

// A malformed event store rejects verified bind, demonstrating the read would
// fail. Terminal ensure succeeds with that same store untouched; repaired-store
// bind is the valid event control. Neither terminal reads nor writes the watermark.
func TestOwnerDMEnsureIgnoresWatermark(t *testing.T) {
	e, c, f := ownerActiveFixture(t)
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	saved := readFile(t, f.path("bindings"))
	if err := os.WriteFile(f.path("bindings"), []byte(`{"not-valid"`), 0600); err != nil {
		t.Fatal(err)
	}
	before := ownerSnapshot(t, f)
	rooms, invites := ownerRoomInviteCounts(e)
	id := ownerRequest(e, c.Room, "bind", "tag", time.Now())
	e.as("bob", "owner-dm", "bind", "--event", c.Room, id).failsWith(t, ExitError, "invalid owner-dm binding store")
	ownerUnchanged(t, f, before)
	e.as("bob", "owner-dm", "ensure", "--tag").ok(t)
	got := ownerLoad(t, f)
	afterRooms, afterInvites := ownerRoomInviteCounts(e)
	if !got.Tag || got.Room != c.Room || got.Generation != c.Generation || afterRooms != rooms || afterInvites != invites || ownerSnapshot(t, f)["bindings"] != before["bindings"] {
		t.Fatal("terminal consulted/changed event store or provisioned", got)
	}
	if err := os.WriteFile(f.path("bindings"), []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	e.as("bob", "owner-dm", "bind", "--event", c.Room, id).ok(t)
}

// Active ensure still refuses a bad audience without room/invite/config/store
// effects; repair is the valid flags-only control at the same generation.
func TestOwnerDMEnsureAudienceRefusal(t *testing.T) {
	e, c, f := ownerActiveFixture(t)
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	e.hs.mu.Lock()
	e.hs.rooms[c.Room].state["m.room.encryption|"] = json.RawMessage(`{"algorithm":"m.megolm.v1.aes-sha2"}`)
	e.hs.mu.Unlock()
	before := ownerSnapshot(t, f)
	rooms, invites := ownerRoomInviteCounts(e)
	e.as("bob", "owner-dm", "ensure", "--tag").failsWith(t, ExitError, "encrypted")
	ownerUnchanged(t, f, before)
	gotRooms, gotInvites := ownerRoomInviteCounts(e)
	if rooms != gotRooms || invites != gotInvites || ownerLoad(t, f).Generation != c.Generation {
		t.Fatal("refusal provisioned")
	}
	e.hs.mu.Lock()
	delete(e.hs.rooms[c.Room].state, "m.room.encryption|")
	e.hs.mu.Unlock()
	e.as("bob", "owner-dm", "ensure", "--tag").ok(t)
	got := ownerLoad(t, f)
	gotRooms, gotInvites = ownerRoomInviteCounts(e)
	if !got.Tag || got.Mirror || got.Render != "in-frame" || got.Room != c.Room || got.Generation != c.Generation || rooms != gotRooms || invites != gotInvites {
		t.Fatal("flags-only control", got)
	}
}

// P3.2: a first verified unbind consumes/advances only its owner-event store,
// keeping config absent. Old bind/replay refuse; fresh bind and configured unbind work.
func TestOwnerDMNeverConfiguredUnbind(t *testing.T) {
	e, room, f := ownerFixture(t)
	now := time.Now().Truncate(time.Millisecond)
	e.now = func() time.Time { return now.Add(10 * time.Second) }
	old := ownerRequest(e, room, "bind", "mirror", now)
	unbind := ownerRequest(e, room, "unbind", "", now.Add(time.Second))
	before := ownerSnapshot(t, f)
	e.as("bob", "owner-dm", "unbind", "--event", room, unbind).ok(t)
	got := ownerSnapshot(t, f)
	for _, key := range []string{"json", "pin", "journal", "archive-exclude"} {
		if got[key] != before[key] {
			t.Fatal("never-configured unbind wrote", key)
		}
	}
	store, err := f.bindings()
	if err != nil || len(store.Records) != 1 || store.Watermarks["@alice:"+testServer].ID != unbind {
		t.Fatal("missing accepted watermark", store, err)
	}
	rooms, invites := ownerRoomInviteCounts(e)
	if rooms != 0 || invites != 0 {
		t.Fatal("unbind provisioned")
	}
	if status := e.as("bob", "owner-dm", "status").ok(t); status.stdout != "never-configured\n" {
		t.Fatal(status)
	}
	before = ownerSnapshot(t, f)
	e.as("bob", "owner-dm", "unbind", "--event", room, unbind).failsWith(t, ExitError, "already used")
	e.as("bob", "owner-dm", "bind", "--event", room, old).failsWith(t, ExitError, "watermark")
	ownerUnchanged(t, f, before)
	fresh := ownerRequest(e, room, "bind", "mirror", now.Add(2*time.Second))
	e.as("bob", "owner-dm", "bind", "--event", room, fresh).ok(t)
	c := ownerLoad(t, f)
	configured := ownerRequest(e, room, "unbind", "", now.Add(3*time.Second))
	e.as("bob", "owner-dm", "unbind", "--event", room, configured).ok(t)
	after := ownerLoad(t, f)
	rooms, invites = ownerRoomInviteCounts(e)
	if after.Active || after.Pending || after.Generation != c.Generation+1 || after.Room != c.Room || rooms != 1 || invites != 1 {
		t.Fatal("configured unbind control", after, rooms, invites)
	}
}

// P3.1: evaluate generated plain-command rules using the documented '*'
// whole-text glob and exact argv-prefix contracts, not a live runtime. Terminal
// verbs are denied; bind/unbind aliases or IDs containing those words are not.
func TestOwnerDMDenySubcommandBoundary(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e := newEnv(t)
			e.exePath("/usr/local/bin/a2a")
			path := t.TempDir() + "/hooks.json"
			e.run("hook", "install", "--agent", "bob", "--runtime", runtime, "--settings", path).ok(t)
			matches := func(command string) bool {
				if runtime == "claude" {
					var root struct {
						Permissions struct {
							Deny []string `json:"deny"`
						} `json:"permissions"`
					}
					if err := json.Unmarshal([]byte(readFile(t, path)), &root); err != nil {
						t.Fatal(err)
					}
					for _, rule := range root.Permissions.Deny {
						pattern := strings.TrimSuffix(strings.TrimPrefix(rule, "Bash("), ")")
						re := "^" + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*") + "$"
						if regexp.MustCompile(re).MatchString(command) {
							return true
						}
					}
					return false
				}
				rules := readFile(t, filepath.Dir(path)+"/rules/a2a-owner-dm.rules")
				words := strings.Fields(command)
				for _, line := range strings.Split(rules, "\n") {
					if !strings.HasPrefix(line, "prefix_rule(pattern = ") {
						continue
					}
					body := strings.SplitN(strings.TrimPrefix(line, "prefix_rule(pattern = "), ", decision =", 2)[0]
					var tokens []json.RawMessage
					if err := json.Unmarshal([]byte(body), &tokens); err != nil {
						t.Fatal(err)
					}
					if len(words) < len(tokens) {
						continue
					}
					matched := true
					for i, raw := range tokens {
						var literal string
						var union []string
						if json.Unmarshal(raw, &literal) == nil {
							matched = matched && words[i] == literal
						} else if json.Unmarshal(raw, &union) == nil {
							matched = matched && contains(union, words[i])
						} else {
							t.Fatal("bad exact-prefix token", string(raw))
						}
					}
					if matched {
						return true
					}
				}
				return false
			}
			for _, prefix := range []string{"a2a", "bin/a2a", "./bin/a2a", "/usr/local/bin/a2a"} {
				for _, verb := range []string{"ensure", "set", "unset"} {
					for _, suffix := range []string{"", " --room !room"} {
						command := prefix + " owner-dm " + verb + suffix
						if !matches(command) {
							t.Fatal("terminal verb not denied", command)
						}
					}
				}
				for _, verb := range []string{"ensure", "unset"} {
					command := prefix + " principal-dm " + verb + " @carol:test.local"
					if !matches(command) {
						t.Fatal("principal terminal verb not denied", command)
					}
				}
				for _, verb := range []string{"status", "say"} {
					command := prefix + " principal-dm " + verb + " @ensure-unset:test.local"
					if matches(command) {
						t.Fatal("principal agent verb denied", command)
					}
				}
				for _, verb := range []string{"bind", "unbind"} {
					for _, ref := range []string{"#asset-room $event", "#ensure-room $set", "#room $ensure-unset-event"} {
						command := prefix + " owner-dm " + verb + " --event " + ref
						if matches(command) {
							t.Fatal("legitimate event command denied", command)
						}
					}
				}
			}
		})
	}
}

// Reinstall replaces our old broad globs, preserving unrelated user denies;
// uninstall removes both managed generations without deleting the user rule.
func TestOwnerDMDenyMigration(t *testing.T) {
	e := newEnv(t)
	e.exePath("/usr/local/bin/a2a")
	path := principalDMTestRoot(t) + "/settings.json"
	old := `{"permissions":{"deny":["Bash(rm *)","Bash(*owner-dm*ensure*)","Bash(*owner-dm*set*)","Bash(*owner-dm*unset*)"]}}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	e.run("hook", "install", "--agent", "bob", "--runtime", "claude", "--settings", path).ok(t)
	var got struct {
		Permissions struct {
			Deny []string `json:"deny"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions.Deny) != 11 || !contains(got.Permissions.Deny, "Bash(rm *)") {
		t.Fatal("lost user/managed rules", got)
	}
	for _, legacy := range []string{"Bash(*owner-dm*ensure*)", "Bash(*owner-dm*set*)", "Bash(*owner-dm*unset*)"} {
		if contains(got.Permissions.Deny, legacy) {
			t.Fatal("legacy deny still shadows bind", legacy)
		}
	}
	e.run("hook", "uninstall", "--runtime", "claude", "--settings", path).ok(t)
	if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Permissions.Deny) != 1 || got.Permissions.Deny[0] != "Bash(rm *)" {
		t.Fatal("uninstall removed user rule", got)
	}
}
