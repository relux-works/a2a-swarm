//go:build darwin || linux

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/admission"
	"github.com/relux-works/a2a-swarm/internal/admission/testdata/policy"
	"github.com/relux-works/a2a-swarm/internal/config"
)

// Proves Courier constructs/closes the checker under its identity base; station sees only counts and no admission files.
func TestAdmissionCourierIdentity(t *testing.T) {
	f := newCourierFixture(t)
	p := f.paths["alice"]
	setAdmissionPolicy(t, p, "alice", policy.Name)
	dir := filepath.Join(p.Base, "admission", "alice")
	checker := policy.Get(dir)
	checker.In = admission.Decision{Verdict: admission.Refused, Retryable: true, Reason: "dependency_unavailable"}
	e := f.envs["alice"]
	// Fixture cursor is s0: inject through its ordinary server event stream.
	e.as("bob", "post", f.rooms["alice"], "--to", "alice", policy.Leak).ok(t)
	r := f.run("alice", "", "inbox", "--json").ok(t)
	if strings.Contains(r.stdout+r.stderr, policy.Leak) {
		t.Fatal("Courier exposed held payload")
	}
	r = f.run("alice", "", "status", "admission", "--json").ok(t)
	var projection struct {
		Counts map[string]uint64 `json:"counts"`
	}
	if json.Unmarshal([]byte(r.stdout), &projection) != nil {
		t.Fatal("invalid count status")
	}
	checker.Mu.Lock()
	cfg, closed, calls := checker.Config, checker.Closes, checker.InCalls
	checker.Mu.Unlock()
	if cfg.StateDir != dir || cfg.Identity != "alice" || cfg.Nostr || closed < 1 || calls < 1 {
		t.Fatal("checker not owned by Courier identity session")
	}
	mb, err := config.OpenMailbox(p, "alice")
	if err != nil || len(mb.Pending()) != 0 {
		t.Fatal("held mail in Courier mailbox")
	}
	if _, err := os.Stat(filepath.Join(f.stations["alice"], "admission")); !os.IsNotExist(err) {
		t.Fatal("station created admission state")
	}
}
