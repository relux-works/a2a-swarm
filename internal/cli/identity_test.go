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
	e.configure = func(a *App) { a.stdinTTY = func() bool { return true } } // a human at a terminal
	e.run("--human", "whoami").failsWith(t, ExitUsage, "--human needs --as")
	tuitestkit.ContainsStr(t, e.run("--human", "--as", "ivan", "whoami").ok(t).stdout, "@ivan:")
	e.configure = nil
	if _, err := os.Stat(filepath.Join(e.dir, "agents", "ivan.json")); !os.IsNotExist(err) {
		t.Fatalf("agents/ivan.json still present: %v", err)
	}
	e.run("creds", "move-human", "nobody").failsWith(t, ExitError, "")
	e.run("creds", "move-human", "../x").failsWith(t, ExitUsage, "")
}

// Proves --human is refused (usage error, exit 2, before any credential is
// read or the homeserver is contacted) when stdin is not an interactive
// terminal, as in every agent tool call, even with the right --as; the same
// command with a terminal on stdin works (positive control).
func TestHumanNeedsTerminal(t *testing.T) {
	e, _ := world(t)
	e.agent("ivan")
	e.run("creds", "move-human", "ivan").ok(t)
	before := len(e.hs.Calls())
	e.configure = func(a *App) { a.stdinTTY = func() bool { return false } }
	e.run("--human", "--as", "ivan", "whoami").failsWith(t, ExitUsage, "--human needs an interactive terminal")
	e.run("--as", "ivan", "--human", "post", "proj", "--topic", "x", "hi").failsWith(t, ExitUsage, "--human needs an interactive terminal")
	if n := len(e.hs.Calls()); n != before {
		t.Fatalf("a refused --human reached the homeserver: %d requests", n-before)
	}
	e.configure = nil // the real check, when go test's stdin is no terminal
	if !stdinIsTerminal(os.Stdin) {
		e.run("--human", "--as", "ivan", "whoami").failsWith(t, ExitUsage, "--human needs an interactive terminal")
	}
	e.configure = func(a *App) { a.stdinTTY = func() bool { return true } }
	tuitestkit.ContainsStr(t, e.run("--human", "--as", "ivan", "whoami").ok(t).stdout, "@ivan:")
}

// Proves the terminal check itself: a pipe and the null device (a character
// device, which a bare ModeCharDevice test would accept) are not terminals,
// while another character device is accepted (positive control of the mode test).
func TestStdinIsTerminal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	if stdinIsTerminal(r) {
		t.Fatal("a pipe counted as a terminal")
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	if stdinIsTerminal(null) {
		t.Fatal("the null device counted as a terminal")
	}
	zero, err := os.Open("/dev/zero")
	if err != nil {
		t.Skip("no /dev/zero:", err)
	}
	defer zero.Close()
	if !stdinIsTerminal(zero) {
		t.Fatal("a character device other than the null device was refused")
	}
}
