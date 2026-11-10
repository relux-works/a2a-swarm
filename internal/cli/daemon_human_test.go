//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/daemon"
)

func TestDaemonHumanPostRefusal(t *testing.T) {
	f := newDaemonFixture(t)
	f.run("alice", "", "topic", "new", f.rooms["alice"], "human-control", "Human control").ok(t)
	before, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	calls := len(f.envs["alice"].hs.Calls())
	r := f.run("alice", "", "--human", "post", f.rooms["alice"], "--topic", "human-control", "human-post-control")
	after, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
	if err != nil || r.code != ExitError || r.stdout != "" || r.stderr != "a2a: daemon: not_allowed\n" || !bytes.Equal(before, after) || calls != len(f.envs["alice"].hs.Calls()) {
		t.Fatal("leading human post executed or changed State instead of closed refusal")
	}
	// Independently exercise the engine gate, bypassing the station parser.
	r, rawErr := f.rawRun("alice", "--human", "post", f.rooms["alice"], "--topic", "human-control", "human-post-control")
	if r.code != ExitError || rawErr != daemon.NotAllowed || r.stderr != "" || calls != len(f.envs["alice"].hs.Calls()) {
		t.Fatal("engine admitted human post")
	}
	f.run("alice", "", "post", f.rooms["alice"], "--topic", "human-control", "plain-post-neighbour").ok(t)
}

// These are preserved contracts, deliberately executable on the unmodified base.
func TestDaemonHumanHookFailOpenControl(t *testing.T) {
	s := newC5Station(t, func(context.Context, daemon.Invocation) int { return 1 })
	r := s.run(context.Background(), "{}", "--human", "hook", "prompt")
	log, err := os.ReadFile(filepath.Join(s.home, "state", hookFailureLog))
	if err != nil || r.code != 0 || r.stdout != "" || r.stderr != "" || bytes.Count(log, []byte{'\n'}) != 1 {
		t.Fatal("M3 fail-open contract changed")
	}
	// The classification is tested separately: base executes and fails, whereas
	// the candidate refuses before execution. Both leave one counts-only line.
}

func TestDaemonPlainPostControl(t *testing.T) {
	f := newDaemonFixture(t)
	f.run("alice", "", "topic", "new", f.rooms["alice"], "human-control", "Human control").ok(t)
	f.run("alice", "", "post", f.rooms["alice"], "--topic", "human-control", "plain-post-neighbour").ok(t)
}
