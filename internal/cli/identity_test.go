package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Proves a session bound to an identity (A2A_AGENT, set by hook install)
// refuses --as with another name, and still works with its own name or none.
func TestIdentityGuard(t *testing.T) {
	e, _ := world(t)
	e.extra[config.EnvAgent] = "alice"
	e.run("--as", "bob", "whoami").failsWith(t, ExitUsage, "never act as another identity")
	tuitestkit.ContainsStr(t, e.run("--as", "alice", "whoami").ok(t).stdout, "@alice:")
	tuitestkit.ContainsStr(t, e.run("whoami").ok(t).stdout, "@alice:")
	delete(e.extra, config.EnvAgent)
	tuitestkit.ContainsStr(t, e.run("--as", "bob", "whoami").ok(t).stdout, "@bob:") // unbound operator tooling
}

// Proves a human's account moved to humans/ cannot be used as an agent
// identity (not with --as, not via A2A_AGENT), cannot be re-registered as an
// agent name, and is usable only with --human --as by that human.
func TestHumanIdentityNotUsableByAgents(t *testing.T) {
	e, _ := world(t)
	e.agent("ivan")
	tuitestkit.ContainsStr(t, e.run("creds", "move-human", "ivan").ok(t).stdout, "moved ivan")
	e.run("--as", "ivan", "whoami").failsWith(t, ExitError, "human's identity")
	e.extra[config.EnvAgent] = "ivan"
	e.run("whoami").failsWith(t, ExitError, "human's identity")
	delete(e.extra, config.EnvAgent)
	e.run("register", "ivan").failsWith(t, ExitError, "human's identity")
	e.run("--human", "whoami").failsWith(t, ExitUsage, "--human needs --as")
	tuitestkit.ContainsStr(t, e.run("--human", "--as", "ivan", "whoami").ok(t).stdout, "@ivan:")
	if _, err := os.Stat(filepath.Join(e.dir, "agents", "ivan.json")); !os.IsNotExist(err) {
		t.Fatalf("agents/ivan.json still present: %v", err)
	}
	e.run("creds", "move-human", "nobody").failsWith(t, ExitError, "")
	e.run("creds", "move-human", "../x").failsWith(t, ExitUsage, "")
}
