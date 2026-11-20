//go:build linux || darwin

package cli

import (
	"strings"
	"testing"
)

// The station invokes the real delegated inbox command through the isolated
// in-process daemon, so a daemon adapter cannot bypass the shared audience view.
// This uses existing synthetic fixture accounts and a private fixture socket.
func TestAudienceDaemonDelegation(t *testing.T) {
	f := newDaemonFixture(t)
	e, room := f.envs["alice"], f.rooms["alice"]
	sender := "@bob:" + e.hs.server
	for _, own := range []bool{false, true} {
		target := "carol"
		if own {
			target = "alice"
		}
		id := e.hs.inject(room, sender, msg("fixture body", addressHeader(target)))
		r := f.run("alice", "", "--as", "alice", "inbox").ok(t)
		if strings.Contains(r.stdout, id) != own || f.received("alice", id) != own {
			t.Fatal("daemon delegation changed recipient eligibility or receipts")
		}
	}
}
