package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves the two-step UIA registration works end to end: the credential file is
// created with mode 0600, holds the issued token, and output reveals no secret.
func TestRegister(t *testing.T) {
	e := newEnv(t)
	r := e.run("register", "newbie").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "registered @newbie:test.local")
	st, err := os.Stat(e.paths().AgentFile("newbie"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", st, err)
	}
	c, err := config.LoadCredentials(e.paths(), "newbie")
	if err != nil || c.AccessToken != "syt_newbie_newtoken_abcdef" || c.Password != "" || c.Homeserver != e.hs.srv.URL {
		t.Fatalf("registered credentials mismatch: %v", err)
	}
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/register", 2)
	// The new identity works.
	e.as("newbie", "whoami").ok(t)
}

// Proves register refuses to overwrite an existing credential file: it fails
// before any network call and leaves the existing file byte-for-byte intact.
func TestRegisterRefusesOverwrite(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	before, _ := os.ReadFile(e.paths().AgentFile("alice"))
	e.run("register", "alice").failsWith(t, ExitError, "refusing to overwrite")
	after, _ := os.ReadFile(e.paths().AgentFile("alice"))
	if string(before) != string(after) {
		t.Fatal("existing credential file was modified")
	}
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/register")
}

// Proves a rejected registration token gives a clear error without echoing the
// token, and that no credential file is left behind.
func TestRegisterBadToken(t *testing.T) {
	e := newEnv(t)
	e.extra[config.EnvRegistrationToken] = "wrong-registration-token"
	r := e.run("register", "newbie").failsWith(t, ExitError, "registration token rejected")
	tuitestkit.NotContainsStr(t, r.stderr, "wrong-registration-token")
	if _, err := os.Stat(e.paths().AgentFile("newbie")); !os.IsNotExist(err) {
		t.Fatalf("credential file left behind: %v", err)
	}
}

// Proves a taken user name is reported as such and leaves no credential file.
func TestRegisterUserInUse(t *testing.T) {
	e := newEnv(t)
	e.hs.addUser("taken")
	e.run("register", "taken").failsWith(t, ExitError, "already exists on the homeserver")
	if _, err := os.Stat(e.paths().AgentFile("taken")); !os.IsNotExist(err) {
		t.Fatal("credential file left behind")
	}
}

// Proves invalid names and missing arguments are usage errors (exit 2) with no traffic.
func TestRegisterUsage(t *testing.T) {
	e := newEnv(t)
	e.run("register").failsWith(t, ExitUsage, "usage")
	e.run("register", "../evil").failsWith(t, ExitUsage, "invalid agent name")
	e.run("register", "Upper").failsWith(t, ExitUsage, "invalid agent name")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/register")
}

// Proves whoami prints the identity resolved from --as and from A2A_AGENT, and
// that a missing identity is a usage error.
func TestWhoamiIdentity(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	r := e.as("alice", "whoami").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "user: @alice:test.local")
	e.extra[config.EnvAgent] = "alice"
	r = e.run("whoami").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "agent: alice")
	delete(e.extra, config.EnvAgent)
	e.run("whoami").failsWith(t, ExitUsage, "no identity")
}

// Proves a revoked token (401 M_UNKNOWN_TOKEN) surfaces as a clear
// authentication error, and that the token the server echoed back is redacted.
func TestUnknownTokenNoEcho(t *testing.T) {
	e := newEnv(t)
	e.writeCred("ghost", "@ghost:test.local", "syt_revoked_token_value_123", 0o600)
	e.tokens["ghost"] = "syt_revoked_token_value_123"
	r := e.as("ghost", "whoami").failsWith(t, ExitError, "authentication failed")
	tuitestkit.ContainsStr(t, r.stderr, "M_UNKNOWN_TOKEN")
	tuitestkit.NotContainsStr(t, r.stderr, "revoked_token")
}

// Proves a credential file with any mode other than 0600 is refused before any
// request is sent (the token never leaves the machine).
func TestCredentialModeRefused(t *testing.T) {
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		e := newEnv(t)
		uid, tok := e.hs.addUser("alice")
		e.writeCred("alice", uid, tok, mode)
		e.tokens["alice"] = tok
		e.as("alice", "whoami").failsWith(t, ExitError, "want 0600")
		if n := len(e.hs.Calls()); n != 0 {
			t.Fatalf("mode %o: %d requests sent, want 0", mode, n)
		}
	}
}

// Proves that a server error message that contains a secret is scrubbed before
// it is printed (secret redaction in error messages).
func TestErrorRedaction(t *testing.T) {
	e, _ := world(t)
	tok := e.tokens["alice"]
	e.hs.fail("GET /_matrix/client/v3/joined_rooms", 500, "M_UNKNOWN", "backend exploded while handling token "+tok+" and password=pw-alice-secret")
	r := e.as("alice", "rooms").failsWith(t, ExitError, "backend exploded")
	tuitestkit.ContainsStr(t, r.stderr, "<redacted>")
}

// Proves the access token never appears in any request URL (it travels only in
// the Authorization header).
func TestTokenNeverInURL(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "rooms").ok(t)
	e.as("alice", "topic", "new", "proj", "t1", "Title").ok(t)
	e.as("alice", "wait", "--timeout", "50ms").ok(t)
	for _, u := range e.hs.urls() {
		for _, tok := range e.tokens {
			if strings.Contains(u, tok) {
				t.Fatalf("token in URL %q", u)
			}
		}
	}
}

// Proves rooms lists alias, name, and id for joined rooms only.
func TestRooms(t *testing.T) {
	e, _ := world(t)
	r := e.as("alice", "rooms").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "#proj\tproj room\t!projRoomID")
	tuitestkit.ContainsStr(t, r.stdout, "#lobby\t")
	r = e.as("carol", "rooms").ok(t)
	if strings.TrimSpace(r.stdout) != "" {
		t.Fatalf("carol sees rooms: %q", r.stdout)
	}
}

// Proves join fails with an actionable message when not invited, and succeeds
// after an invite by a member; every room argument form resolves.
func TestJoinAndInvite(t *testing.T) {
	e, _ := world(t)
	e.as("carol", "join", "#proj").failsWith(t, ExitError, "invite-only")
	r := e.as("alice", "invite", "proj", "carol").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "invited @carol:test.local to #proj")
	r = e.as("carol", "join", "#proj:test.local").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "joined #proj !projRoomID")
	e.as("carol", "join", "!projRoomID").ok(t)
	e.as("carol", "join", "proj").ok(t)
}

// Proves invite rejects malformed users and reports unknown users and rooms.
func TestInviteNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "invite", "proj", "bad user").failsWith(t, ExitUsage, "invalid user")
	e.as("alice", "invite", "proj", "@nobody").failsWith(t, ExitError, "Unknown user")
	e.as("alice", "invite", "nowhere", "bob").failsWith(t, ExitError, "unknown room alias #nowhere:test.local")
	e.as("carol", "invite", "proj", "bob").failsWith(t, ExitError, "M_FORBIDDEN")
}

// Proves an unknown room alias is reported clearly by every room-taking command.
func TestUnknownRoomAlias(t *testing.T) {
	e, _ := world(t)
	for _, args := range [][]string{
		{"join", "#nope"},
		{"topics", "nope"},
		{"read", "nope"},
		{"topic", "new", "nope", "s", "Title"},
		{"post", "nope", "--topic", "s", "hello"},
		{"wait", "--room", "nope", "--timeout", "10ms"},
	} {
		e.as("alice", args...).failsWith(t, ExitError, "unknown room alias #nope:test.local")
	}
	e.as("alice", "read", "#bad alias!").failsWith(t, ExitUsage, "invalid room")
}

// Proves unknown commands and bare invocations are usage errors, and help succeeds.
func TestUsage(t *testing.T) {
	e := newEnv(t)
	e.run().failsWith(t, ExitUsage, "usage: a2a")
	e.run("frobnicate").failsWith(t, ExitUsage, "unknown command")
	r := e.run("help").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "wait [--room R ...]")
	e.run("--as").failsWith(t, ExitUsage, "--as needs a name")
	e.run("--as", "../x", "whoami").failsWith(t, ExitUsage, "invalid agent name")
}

// Proves leave removes the room from rooms and cuts read access (M_FORBIDDEN),
// while another member (positive control) still reads the room.
func TestLeave(t *testing.T) {
	e, _ := world(t)
	r := e.as("alice", "leave", "proj").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "left #proj !projRoomID")
	tuitestkit.NotContainsStr(t, e.as("alice", "rooms").ok(t).stdout, "#proj")
	e.as("alice", "read", "proj").failsWith(t, ExitError, "M_FORBIDDEN")
	e.as("bob", "read", "proj").ok(t)
	// A room id argument is labelled with its canonical alias.
	r = e.as("bob", "leave", "!projRoomID").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "left #proj !projRoomID")
}

// Proves leaving #lobby (by any room form) is a usage error without --force and
// never reaches the leave route; with --force it leaves.
func TestLeaveLobbyNeedsForce(t *testing.T) {
	e, _ := world(t)
	for _, arg := range []string{"lobby", "#lobby", "#lobby:test.local", "!lobbyRoomID"} {
		e.as("alice", "leave", arg).failsWith(t, ExitUsage, "pass --force")
	}
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/rooms/{room}/leave")
	tuitestkit.ContainsStr(t, e.as("alice", "rooms").ok(t).stdout, "#lobby")
	r := e.as("alice", "leave", "lobby", "--force").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "left #lobby !lobbyRoomID")
	tuitestkit.NotContainsStr(t, e.as("alice", "rooms").ok(t).stdout, "#lobby")
}

// Proves leave reports unknown rooms and rejects bad arguments before any leave request.
func TestLeaveNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "leave", "nowhere").failsWith(t, ExitError, "unknown room alias #nowhere:test.local")
	e.as("alice", "leave", "!missingRoomID").failsWith(t, ExitError, "M_NOT_FOUND")
	e.as("alice", "leave").failsWith(t, ExitUsage, "usage: a2a leave")
	e.as("alice", "leave", "a", "b").failsWith(t, ExitUsage, "usage: a2a leave")
	e.as("alice", "leave", "#bad alias!").failsWith(t, ExitUsage, "invalid room")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/rooms/{room}/leave", 1)
}

// Proves Deactivate completes the two-step m.login.password UIA against the
// fake homeserver: after it the account's token is rejected.
func TestDeactivate(t *testing.T) {
	e := newEnv(t)
	uid, tok := e.hs.addUser("gone")
	c := matrix.New(e.hs.srv.URL, tok)
	c.HTTP = e.hs.srv.Client()
	if err := c.Deactivate(context.Background(), uid, "pw-gone-secret"); err != nil {
		t.Fatal(err)
	}
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/account/deactivate", 2)
	if _, err := c.Whoami(context.Background()); !errors.Is(err, matrix.ErrAuth) {
		t.Fatalf("whoami after deactivate: %v, want ErrAuth", err)
	}
}

// Proves Deactivate with a wrong password is refused with a clear error that
// does not echo the password, and the account keeps working.
func TestDeactivateWrongPassword(t *testing.T) {
	e := newEnv(t)
	uid, tok := e.hs.addUser("kept")
	c := matrix.New(e.hs.srv.URL, tok)
	c.HTTP = e.hs.srv.Client()
	err := c.Deactivate(context.Background(), uid, "wrong-password-value")
	if err == nil || !strings.Contains(err.Error(), "password rejected") || strings.Contains(err.Error(), "wrong-password-value") {
		t.Fatalf("err = %v", err)
	}
	if w, err := c.Whoami(context.Background()); err != nil || w.UserID != uid {
		t.Fatalf("account broken after refused deactivation: %+v %v", w, err)
	}
}
