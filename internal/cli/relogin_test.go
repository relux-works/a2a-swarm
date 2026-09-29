package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const (
	routeLogin  = "POST /_matrix/client/v3/login"
	routeWhoami = "GET /_matrix/client/v3/account/whoami"
	routeSend   = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
)

// Proves a revoked token is repaired transparently: whoami succeeds after one
// password login that keeps the stored device id, and the credential file is
// rewritten in place (mode 0600, new token, old token gone, no temp files left).
func TestReloginWhoami(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	old := e.tokens["alice"]
	path := e.paths().AgentFile("alice")
	e.hs.revoke(uid)

	r := e.as("alice", "whoami").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "user: @alice:test.local")
	tuitestkit.ContainsStr(t, r.stderr, "logged in again (device D) and updated "+path)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeLogin, 1)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 2)
	if got := e.hs.loginDevices(); len(got) != 1 || got[0] != "D" {
		t.Fatalf("login devices = %v, want [D] (device kept)", got)
	}

	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential file after relogin: %v %v", st, err)
	}
	raw, _ := os.ReadFile(path)
	tuitestkit.NotContainsStr(t, string(raw), old)
	c, err := config.LoadCredentials(e.paths(), "alice")
	if err != nil || c.AccessToken != "syt_alice_relogin_1xyz" || c.DeviceID != "D" || c.Password != "pw-alice-secret" || c.UserID != uid {
		t.Fatalf("credentials after relogin = %+v, %v", c, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".tmp") {
			t.Fatalf("temporary file left behind: %s", en.Name())
		}
	}

	// The stored token now works directly: no second login.
	e.as("alice", "whoami").ok(t)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeLogin, 1)
}

// Proves a post whose token was rejected is sent exactly once after relogin,
// and that a body read from stdin by the rejected attempt is replayed intact.
func TestReloginPostSendsOnce(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "t1", "Title").ok(t)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeSend, 1)
	e.hs.revoke("@alice:test.local")

	e.stdin = "hello from stdin"
	e.as("alice", "post", "proj", "--topic", "t1", "-").ok(t)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeSend, 2)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeLogin, 1)
	tuitestkit.ContainsStr(t, string(e.hs.lastEvent().Content), "hello from stdin")
}

// Proves no retry happens once a request may have been executed: a token
// rejection that arrives after successful requests (here on the send itself)
// fails with the authentication error, with no login and a single send attempt.
// Positive control: TestReloginPostSendsOnce.
func TestReloginNotAfterServedRequest(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "t1", "Title").ok(t)
	e.hs.fail(routeSend, 401, "M_UNKNOWN_TOKEN", "Unknown access token")
	e.as("alice", "post", "proj", "--topic", "t1", "hi").failsWith(t, ExitError, "authentication failed")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeSend, 2)
}

// Proves the command is retried at most once: when the fresh token is rejected
// too, the second failure is final (one login, two whoami attempts).
func TestReloginRetriesOnce(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.hs.fail(routeWhoami, 401, "M_UNKNOWN_TOKEN", "Unknown access token")
	e.as("alice", "whoami").failsWith(t, ExitError, "authentication failed")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeLogin, 1)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 2)
}

// Proves a rejected stored password exits 1 with a message naming the file and
// the way out, prints no secret, and leaves the credential file byte-for-byte unchanged.
func TestReloginWrongPassword(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	path := e.paths().AgentFile("alice")
	e.hs.revoke(uid)
	e.hs.mu.Lock()
	e.hs.passwords[uid] = "changed-elsewhere"
	e.hs.mu.Unlock()
	before, _ := os.ReadFile(path)

	r := e.as("alice", "whoami").failsWith(t, ExitError, "re-registration or an admin password reset is needed")
	tuitestkit.ContainsStr(t, r.stderr, path)
	tuitestkit.ContainsStr(t, r.stderr, "M_UNKNOWN_TOKEN")
	tuitestkit.ContainsStr(t, r.stderr, "M_FORBIDDEN")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeLogin, 1)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 1)
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("credential file changed after a failed relogin")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", st.Mode().Perm())
	}
}

// Proves a credential file without a stored password is not repaired: exit 1
// with the re-registration hint, no login request, file unchanged.
func TestReloginNoPassword(t *testing.T) {
	e := newEnv(t)
	path := e.paths().AgentFile("ghost")
	os.MkdirAll(filepath.Dir(path), 0o700)
	data := `{"user_id":"@ghost:test.local","access_token":"syt_ghost_dead_token_1","device_id":"D","homeserver":"` + e.hs.srv.URL + `"}`
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	e.tokens["ghost"] = "syt_ghost_dead_token_1"
	e.as("ghost", "whoami").failsWith(t, ExitError, "no stored password")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
	if after, _ := os.ReadFile(path); string(after) != data {
		t.Fatal("credential file changed")
	}
}
