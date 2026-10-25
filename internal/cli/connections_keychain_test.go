//go:build unix

package cli

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Proves the production Keychain adapter routes a named lookup through the
// selected service/full MXID and isolated bookkeeping, without invoking security
// or touching a real Keychain. The existing item is never updated by Create.
func TestNamedKeychainAdapterRouting(t *testing.T) {
	f := newNamedFixture(t, true)
	a := f.app()
	a.connectionSecretStore = nil
	var calls [][]string
	a.keychain = func(p config.Paths) *keychainStore {
		return &keychainStore{
			paths: p, fixture: filepath.Join(f.root, "synthetic.keychain-db"), hostID: "isolated-host", timeout: time.Second, probeCommand: unlockedProbe,
			command: func(ctx context.Context, args ...string) *exec.Cmd {
				calls = append(calls, append([]string(nil), args...))
				return exec.CommandContext(ctx, "/usr/bin/printf", "%s\n", "isolated-alpha")
			},
		}
	}
	if a.Run(context.Background(), []string{"--profile", "alpha", "whoami"}) != 0 {
		t.Fatal(f.err.String())
	}
	if len(calls) != 1 || argValue(calls[0], "-s") != "test.alpha.token" || argValue(calls[0], "-a") != "@alice:alpha.invalid" {
		t.Fatal("Keychain lookup did not use selected reference")
	}
	p := f.paths("alpha", false)
	if e := a.createConnectionSecret(context.Background(), p, config.SecretReference{Service: "test.alpha.token", Account: "@alice:alpha.invalid"}, "replacement"); e != errKeychainDuplicate {
		t.Fatalf("create admitted replacement: %v", e)
	}
	if len(calls) != 2 || calls[1][0] != "find-generic-password" {
		t.Fatal("exclusive create wrote an existing item")
	}
	legacy := &keychainStore{paths: p}
	named := &keychainStore{paths: p, service: "test.alpha.token"}
	if legacy.identityPath("@alice:alpha.invalid") == named.identityPath("@alice:alpha.invalid") {
		t.Fatal("selected service shares credential bookkeeping")
	}
}
