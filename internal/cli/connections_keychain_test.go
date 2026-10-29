//go:build unix

package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// routingBackend records every operation and answers every lookup with one
// synthetic token.
type routingBackend struct{ calls *[]opRecord }

func (r routingBackend) find(_ context.Context, keychain, service, account string) ([]byte, error) {
	*r.calls = append(*r.calls, opRecord{"find", keychain, service, account})
	return []byte("isolated-alpha"), nil
}

func (r routingBackend) add(_ context.Context, keychain, service, account string, _ []byte) error {
	*r.calls = append(*r.calls, opRecord{"add", keychain, service, account})
	return nil
}

func (r routingBackend) update(_ context.Context, keychain, service, account string, _ []byte) error {
	*r.calls = append(*r.calls, opRecord{"update", keychain, service, account})
	return nil
}

// Proves the production Keychain adapter routes a named lookup through the
// selected service, full MXID, explicit keychain file and isolated bookkeeping,
// without touching a real Keychain. The existing item is never updated by Create.
func TestNamedKeychainAdapterRouting(t *testing.T) {
	f := newNamedFixture(t, true)
	a := f.app()
	a.connectionSecretStore = nil
	var calls []opRecord
	fixture := filepath.Join(f.root, "synthetic.keychain-db")
	a.keychain = func(p config.Paths) *keychainStore {
		return &keychainStore{
			paths: p, fixture: fixture, hostID: "isolated-host", timeout: time.Second, probeCommand: unlockedProbe,
			backend: routingBackend{&calls},
		}
	}
	if a.Run(context.Background(), []string{"--profile", "alpha", "whoami"}) != 0 {
		t.Fatal(f.err.String())
	}
	if len(calls) != 1 || calls[0] != (opRecord{"find", fixture, "test.alpha.token", "@alice:alpha.invalid"}) {
		t.Fatal("Keychain lookup did not use selected reference")
	}
	p := f.paths("alpha", false)
	if e := a.createConnectionSecret(context.Background(), p, config.SecretReference{Service: "test.alpha.token", Account: "@alice:alpha.invalid"}, "replacement"); e != errKeychainDuplicate {
		t.Fatalf("create admitted replacement: %v", e)
	}
	if len(calls) != 2 || calls[1].Op != "find" {
		t.Fatal("exclusive create wrote an existing item")
	}
	legacy := &keychainStore{paths: p}
	named := &keychainStore{paths: p, service: "test.alpha.token"}
	if legacy.identityPath("@alice:alpha.invalid") == named.identityPath("@alice:alpha.invalid") {
		t.Fatal("selected service shares credential bookkeeping")
	}
}
