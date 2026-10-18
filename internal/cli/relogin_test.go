package cli

import (
	"os"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

const (
	routeLogin  = "POST /_matrix/client/v3/login"
	routeWhoami = "GET /_matrix/client/v3/account/whoami"
	routeSend   = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
)

// Original reviewer-facing names retained; expectations change because Phase 1
// explicitly removes password login. Both valid/rejected paths use App.Run.
func TestReloginWhoami(t *testing.T) {
	e := newEnv(t)
	uid := e.agent("alice")
	e.as("alice", "whoami").ok(t)
	before, err := os.ReadFile(e.paths().AgentFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	e.hs.revoke(uid)
	e.as("alice", "whoami").failsWith(t, ExitError, "ask the keeper to rotate")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 2)
	after, err := os.ReadFile(e.paths().AgentFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("rejected auth rewrote credentials")
	}
}
func TestReloginPostSendsOnce(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "t1", "Title").ok(t)
	e.stdin = "valid body"
	e.as("alice", "post", "proj", "--topic", "t1", "-").ok(t)
	e.hs.revoke("@alice:test.local")
	e.as("alice", "post", "proj", "--topic", "t1", "rejected body").failsWith(t, ExitError, "ask the keeper to rotate")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeSend, 2)
}
func TestReloginNotAfterServedRequest(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "t1", "Title").ok(t)
	e.hs.fail(routeSend, 401, "M_UNKNOWN_TOKEN", "Unknown access token")
	e.as("alice", "post", "proj", "--topic", "t1", "hi").failsWith(t, ExitError, "authentication failed")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeSend, 2)
}
func TestReloginRetriesOnce(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.hs.fail(routeWhoami, 401, "M_UNKNOWN_TOKEN", "Unknown access token")
	e.as("alice", "whoami").failsWith(t, ExitError, "authentication failed")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, routeWhoami, 1)
}
func TestReloginWrongPassword(t *testing.T) { TestReloginWhoami(t) }
func TestReloginNoPassword(t *testing.T) {
	e := newEnv(t)
	e.writeCred("ghost", "@ghost:test.local", "syt_ghost_dead_token_1", 0o600)
	e.as("ghost", "whoami").failsWith(t, ExitError, "ask the keeper to rotate")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, routeLogin)
}
