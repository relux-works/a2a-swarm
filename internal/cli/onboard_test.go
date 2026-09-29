package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const (
	createRoute = "POST /_matrix/client/v3/createRoom"
	joinRoute   = "POST /_matrix/client/v3/join/{room}"
	inviteRoute = "POST /_matrix/client/v3/rooms/{room}/invite"
	putADRoute  = "PUT /_matrix/client/v3/user/{user}/account_data/{type}"
)

// swarm sets up what onboarding needs once: the host slug, a public #lobby,
// the meta-orchestrator and projector accounts, a fixed binary path, and a
// temporary project directory for the hooks file.
func (e *env) swarm() {
	e.t.Helper()
	if e.projDir != "" {
		return
	}
	e.projDir = e.t.TempDir()
	e.run("host", "init", "mini").ok(e.t)
	e.hs.makePublic(e.hs.addRoom("lobby"))
	e.hs.addUser(MetaOrchestrator)
	e.hs.addUser(ProjectorAgent)
	e.exePath("/usr/local/bin/a2a")
}

// ensure runs identity ensure in the swarm world, adding --runtime claude and
// --dir <projDir> unless the arguments name them.
func (e *env) ensure(args ...string) result {
	e.t.Helper()
	e.swarm()
	joined := " " + strings.Join(args, " ") + " "
	if !strings.Contains(joined, " --runtime ") {
		args = append(args, "--runtime", "claude")
	}
	if !strings.Contains(joined, " --dir ") {
		args = append(args, "--dir", e.projDir)
	}
	return e.run(append([]string{"identity", "ensure"}, args...)...)
}

// subsOf returns the stored subscriptions of name.
func (e *env) subsOf(name string) []Sub {
	e.t.Helper()
	raw := e.hs.accountDataOf("@"+name+":"+testServer, SubsType)
	if raw == nil {
		return nil
	}
	var list SubList
	if err := json.Unmarshal(raw, &list); err != nil {
		e.t.Fatal(err)
	}
	return list.Subs
}

// Proves the first ensure onboards completely (joins #lobby, notes the missing
// #broadcast, creates and joins #proj with the standard invites, one project
// subscription for system/owner-needed/decision, the Claude hooks file, the
// wait note and the summary), and a second ensure changes nothing: no new
// room, join, invite or account-data write, the same subscription id, and a
// byte-identical hooks file, with "already"/"unchanged" in the output.
func TestEnsureOnboardsIdempotently(t *testing.T) {
	e := newEnv(t)
	e.extra["USER"] = "ops"
	out := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	for _, want := range []string{
		"joined #lobby !lobbyRoomID\n",
		"#broadcast does not exist yet; skipped\n",
		"created #proj !projRoomID (preset public_chat, visibility private",
		"invited @local-models-e9:test.local to #proj\n",
		"invited @a2a-projector:test.local to #proj\n",
		"room=#proj kinds=system,owner-needed,decision",
		"hooks (claude, --runtime):\ninstalled " + filepath.Join(e.projDir, ".claude", "settings.local.json"),
		"Also keep one background `a2a wait --timeout 0` for idle wake, and read with `a2a inbox`.\n",
		"identity:  mini_proj_orch\n",
		"mxid:      @mini_proj_orch:test.local\n",
		"operator:  ops   host: mini   project: proj   role: orch\n",
		"hooks:     " + filepath.Join(e.projDir, ".claude", "settings.local.json") + " (Claude Code)\n",
		"write agents/mini_proj_orch.md",
		"a2a card set --file agents/mini_proj_orch.md",
		"a2a topic new lobby join-mini-proj-orch",
	} {
		tuitestkit.ContainsStr(t, out, want)
	}
	subs := e.subsOf("mini_proj_orch")
	// The project room and #lobby each get the room subscription (every joined room does).
	if len(subs) != 2 || subs[0].Room != "!projRoomID" || subs[1].Room != "!lobbyRoomID" ||
		kindSet(subs[0].Kinds) != kindSet(onboardKinds) || kindSet(subs[1].Kinds) != kindSet(onboardKinds) {
		t.Fatalf("subscriptions after the first run: %+v", subs)
	}
	tuitestkit.ContainsStr(t, out, "created "+subs[0].ID+"\t")
	hooks := filepath.Join(e.projDir, ".claude", "settings.local.json")
	first := readFile(t, hooks)
	tuitestkit.ContainsStr(t, first, `"A2A_AGENT": "mini_proj_orch"`)
	tuitestkit.ContainsStr(t, first, "/usr/local/bin/a2a --as mini_proj_orch hook stop --runtime claude")

	counts := func() [4]int {
		return [4]int{len(e.hs.CallsFor(createRoute)), len(e.hs.CallsFor(joinRoute)), len(e.hs.CallsFor(inviteRoute)), len(e.hs.CallsFor(putADRoute))}
	}
	before := counts()
	again := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestkit.ContainsStr(t, again, "reusing mini_proj_orch")
	tuitestkit.ContainsStr(t, again, "already in #lobby !lobbyRoomID\n")
	tuitestkit.ContainsStr(t, again, "already in #proj !projRoomID\n")
	tuitestkit.ContainsStr(t, again, "already "+subs[0].ID+"\t")
	tuitestkit.ContainsStr(t, again, "unchanged "+hooks)
	tuitestkit.NotContainsStr(t, again, "created ")
	if after := counts(); after != before {
		t.Fatalf("second run changed the server: create/join/invite/account-data %v -> %v", before, after)
	}
	if n := len(e.subsOf("mini_proj_orch")); n != 2 {
		t.Fatalf("%d subscriptions after the second run", n)
	}
	if got := readFile(t, hooks); got != first {
		t.Fatalf("hooks file changed on the second run:\n%s", got)
	}
}

// Proves ensure without a #lobby fails (exit 1) naming the problem, before
// any join, room creation, subscription or hooks file; a re-run after the
// lobby exists completes (positive control).
func TestEnsureNeedsLobby(t *testing.T) {
	e := newEnv(t)
	e.projDir = t.TempDir() // no swarm(): no #lobby
	e.run("host", "init", "mini").ok(t)
	e.exePath("/usr/local/bin/a2a")
	e.agent("mini_proj_orch")
	e.ensure("--role", "orch", "--project", "proj").failsWith(t, ExitError, "#lobby does not exist on test.local")
	if n := len(e.hs.CallsFor(joinRoute)) + len(e.hs.CallsFor(createRoute)) + len(e.hs.CallsFor(putADRoute)); n != 0 {
		t.Fatalf("%d changing requests without a lobby", n)
	}
	if _, err := os.Stat(filepath.Join(e.projDir, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("hooks written without a lobby: %v", err)
	}
	e.hs.makePublic(e.hs.addRoom("lobby"))
	tuitestkit.ContainsStr(t, e.ensure("--role", "orch", "--project", "proj").ok(t).stdout, "joined #lobby")
}

// Proves #broadcast is joined when it exists and skipped with the note (and
// no join attempt for it) when it does not.
func TestEnsureBroadcast(t *testing.T) {
	e := newEnv(t)
	e.agent("mini_proj_orch")
	out := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "#broadcast does not exist yet; skipped")
	tuitestkit.NotContainsStr(t, out, "joined #broadcast")

	e2 := newEnv(t)
	e2.agent("mini_proj_orch")
	e2.hs.makePublic(e2.hs.addRoom("broadcast"))
	out = e2.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "joined #broadcast !broadcastRoomID")
	tuitestkit.NotContainsStr(t, out, "does not exist yet")
}

// Proves the runtime choice: CLAUDECODE=1 alone is detected as claude;
// CLAUDECODE=1 with a Codex marker, CLAUDECODE with another value, a Codex
// marker alone, and no marker are usage errors asking for --runtime, before
// any traffic and without a hooks file; --runtime codex wins over CLAUDECODE=1
// (Codex hooks file, trust step, no Claude file).
func TestEnsureRuntime(t *testing.T) {
	e := newEnv(t)
	e.swarm()
	e.agent("mini_proj_orch")
	claudeFile := filepath.Join(e.projDir, ".claude", "settings.local.json")
	codexFile := filepath.Join(e.projDir, ".codex", "hooks.json")
	run := func(extra ...string) result {
		return e.run(append([]string{"identity", "ensure", "--role", "orch", "--project", "proj", "--dir", e.projDir}, extra...)...)
	}
	calls := len(e.hs.Calls())
	for _, c := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, "cannot detect the runtime (no CLAUDECODE=1 in the environment): pass --runtime claude|codex"},
		{map[string]string{"CLAUDECODE": "1", "CODEX_THREAD_ID": "t1"}, "the runtime is ambiguous (CLAUDECODE=1, CODEX_THREAD_ID)"},
		{map[string]string{"CLAUDECODE": "0"}, "the runtime is ambiguous (CLAUDECODE=0)"},
		{map[string]string{"CODEX_SANDBOX": "seatbelt"}, "the runtime is ambiguous (CODEX_SANDBOX)"},
	} {
		e.extra = c.env
		run().failsWith(t, ExitUsage, c.want)
	}
	if n := len(e.hs.Calls()); n != calls {
		t.Fatalf("a runtime usage error reached the homeserver: %d requests", n-calls)
	}
	if _, err := os.Stat(claudeFile); !os.IsNotExist(err) {
		t.Fatalf("hooks file written on a usage error: %v", err)
	}
	e.extra = map[string]string{"CLAUDECODE": "1"}
	tuitestkit.ContainsStr(t, run().ok(t).stdout, "hooks (claude, detected: CLAUDECODE=1):")
	tuitestkit.ContainsStr(t, readFile(t, claudeFile), "hook stop --runtime claude")
	if err := os.RemoveAll(filepath.Dir(claudeFile)); err != nil {
		t.Fatal(err)
	}
	out := run("--runtime", "codex").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "hooks (codex, --runtime):")
	tuitestkit.ContainsStr(t, out, `at "Hooks need review" choose "Trust all and continue"`)
	tuitestkit.ContainsStr(t, readFile(t, codexFile), "hook stop --runtime codex")
	if _, err := os.Stat(claudeFile); !os.IsNotExist(err) {
		t.Fatalf("--runtime codex also wrote the Claude file: %v", err)
	}
	run("--runtime", "gemini").failsWith(t, ExitUsage, `unknown --runtime "gemini"`)
}

// Proves --keeper joins #system and adds exactly one all-events subscription
// for it (idempotently), without --keeper no #system subscription exists, and
// --keeper without a #system room fails (exit 1) before any change.
func TestEnsureKeeper(t *testing.T) {
	e := newEnv(t)
	e.agent("mini_proj_orch")
	e.ensure("--role", "orch", "--project", "proj", "--keeper").failsWith(t, ExitError, "#system does not exist on test.local: --keeper subscribes to it")
	if n := len(e.hs.CallsFor(joinRoute)) + len(e.hs.CallsFor(createRoute)); n != 0 || e.subsOf("mini_proj_orch") != nil {
		t.Fatalf("a failed keeper ensure changed the server: %d joins/creates, subs %+v", n, e.subsOf("mini_proj_orch"))
	}
	sys := e.hs.addRoom("system")
	e.hs.makePublic(sys)
	out := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestkit.NotContainsStr(t, out, "#system")
	for _, s := range e.subsOf("mini_proj_orch") {
		if s.AllEvents || s.Room == sys {
			t.Fatalf("a #system subscription without --keeper: %+v", s)
		}
	}
	out = e.ensure("--role", "orch", "--project", "proj", "--keeper").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "joined #system !systemRoomID")
	tuitestkit.ContainsStr(t, out, "room=#system all-events")
	e.ensure("--role", "orch", "--project", "proj", "--keeper").ok(t)
	n := 0
	for _, s := range e.subsOf("mini_proj_orch") {
		if s.Room == sys && s.AllEvents {
			n++
		}
	}
	if n != 1 || len(e.subsOf("mini_proj_orch")) != 3 {
		t.Fatalf("keeper subscriptions: %+v", e.subsOf("mini_proj_orch"))
	}
}

// Proves room new applies the standard policy: preset public_chat, visibility
// private, redact 100, local-models-e9 at 100 and the creator never listed in
// users; it invites local-models-e9, a2a-projector and --invite; an existing
// alias prints "exists" and changes nothing.
func TestRoomNew(t *testing.T) {
	e, _ := world(t)
	e.hs.addUser(MetaOrchestrator)
	e.hs.addUser(ProjectorAgent)
	out := e.as("alice", "room", "new", "#team", "--name", "Team", "--topic", "work", "--invite", "bob,carol").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "created #team !teamRoomID (preset public_chat, visibility private: not in the room directory; power: redact 100, @local-models-e9:test.local 100, creator @alice:test.local implicit)")
	for _, u := range []string{"local-models-e9", "a2a-projector", "bob", "carol"} {
		tuitestkit.ContainsStr(t, out, "invited @"+u+":test.local to #team\n")
	}
	created := e.hs.createdRooms()
	if len(created) != 1 {
		t.Fatalf("createRoom calls: %d", len(created))
	}
	c := created[0]
	users, _ := c.PowerLevelContentOverride["users"].(map[string]any)
	if c.Preset != "public_chat" || c.Visibility != "private" || c.RoomAliasName != "team" || c.Name != "Team" || c.Topic != "work" ||
		c.PowerLevelContentOverride["redact"] != float64(100) || users["@local-models-e9:test.local"] != float64(100) || len(users) != 1 {
		t.Fatalf("createRoom body: %+v", c)
	}
	invites := len(e.hs.CallsFor(inviteRoute))
	again := e.as("alice", "room", "new", "team", "--invite", "bob").ok(t).stdout
	if again != "exists #team !teamRoomID\n" {
		t.Fatalf("existing alias: %q", again)
	}
	if len(e.hs.createdRooms()) != 1 || len(e.hs.CallsFor(inviteRoute)) != invites {
		t.Fatal("room new on an existing alias changed the server")
	}
}

// Proves the meta-orchestrator creating a room is not listed in users (the
// fake refuses that like tuwunel) and does not invite itself; an invitee
// without an account is noted, not fatal.
func TestRoomNewByMetaOrchestrator(t *testing.T) {
	e := newEnv(t)
	e.agent(MetaOrchestrator)
	out := e.as(MetaOrchestrator, "room", "new", "ops").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "power: redact 100, creator @local-models-e9:test.local implicit")
	tuitestkit.NotContainsStr(t, out, "invited @local-models-e9")
	tuitestkit.ContainsStr(t, out, "not invited @a2a-projector:test.local to #ops: no such account yet")
	users, _ := e.hs.createdRooms()[0].PowerLevelContentOverride["users"].(map[string]any)
	if len(users) != 0 {
		t.Fatalf("users = %v", users)
	}
}

// Proves room new usage errors (no alias, a foreign server, a room id, a bad
// alias or --invite, a missing subcommand) exit 2 before any traffic.
func TestRoomNewUsage(t *testing.T) {
	e, _ := world(t)
	for _, args := range [][]string{
		{"room"}, {"room", "create", "x"}, {"room", "new"}, {"room", "new", "a", "b"}, {"room", "new", "#x:other"},
		{"room", "new", "!abc"}, {"room", "new", "bad room"}, {"room", "new", "x", "--invite", "b c"},
	} {
		e.as("alice", args...).failsWith(t, ExitUsage, "")
	}
	if n := len(e.hs.Calls()); n != 0 {
		t.Fatalf("%d requests from usage errors", n)
	}
}

// Proves the fake homeserver refuses a createRoom that lists the creator in
// power_levels.users with M_INVALID_PARAM, as tuwunel does for room version
// 12, and accepts the same request without it (positive control): the room
// tests rely on this refusal.
func TestFakeRefusesCreatorInUsers(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	c := matrix.New(e.hs.srv.URL, e.tokens["alice"])
	c.HTTP = e.hs.srv.Client()
	req := matrix.CreateRoomRequest{Preset: "public_chat", RoomAliasName: "x", PowerLevelContentOverride: map[string]any{"users": map[string]int{uid: 100}}}
	if _, err := c.CreateRoom(context.Background(), req); !matrix.IsCode(err, "M_INVALID_PARAM") {
		t.Fatalf("err = %v", err)
	}
	req.PowerLevelContentOverride = map[string]any{"users": map[string]int{}}
	if id, err := c.CreateRoom(context.Background(), req); err != nil || id != "!xRoomID" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}

// Proves --runtime none onboards a service account (rooms, subscriptions)
// without writing any hooks file, even inside a Claude Code session
// (CLAUDECODE=1 would otherwise select claude); the summary says so.
func TestEnsureRuntimeNone(t *testing.T) {
	e := newEnv(t)
	e.extra[envClaudeCode] = "1"
	out := e.ensure("--role", "projector", "--project", "proj", "--runtime", "none").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "hooks: none (--runtime none")
	tuitestkit.ContainsStr(t, out, "identity:  mini_proj_projector\n")
	tuitestkit.NotContainsStr(t, out, "background `a2a wait")
	for _, f := range []string{filepath.Join(e.projDir, ".claude", "settings.local.json"), filepath.Join(e.projDir, ".codex", "hooks.json")} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("hooks file written for a service account: %s", f)
		}
	}
	e.ensure("--role", "orch2", "--project", "proj", "--runtime", "bogus").failsWith(t, ExitUsage, "unknown --runtime")
}
