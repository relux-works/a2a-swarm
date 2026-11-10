//go:build unix

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Human sessions and direct credential loaders must observe an agent fence in
// both phases even when humans/<id> appears afterward. A cached human session
// must refuse effects too. The rollback neighbour restores ordinary human use.
func TestDaemonHumanFenceAfterFileAppears(t *testing.T) {
	for _, phase := range []string{"in_progress", "done"} {
		t.Run(phase, func(t *testing.T) {
			a, p, to := daemonCLIFixture(t)
			raw, err := os.ReadFile(p.AgentFile("fixture"))
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(filepath.Dir(p.HumanFile("fixture")), 0700); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(p.HumanFile("fixture"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			a.human = true
			cached, err := a.sessionAsMode("fixture", false)
			if err != nil {
				t.Fatal("unfenced human neighbour", err)
			}
			if err = os.Remove(p.HumanFile("fixture")); err != nil {
				t.Fatal(err)
			}
			opts := config.DaemonMigrationOptions{To: to, Now: time.Now()}
			if phase == "in_progress" {
				opts.Secrets = &daemonCLISecretFixture{present: true}
				opts.Keychain = true
				var metadata config.Credentials
				if err = json.Unmarshal(raw, &metadata); err != nil {
					t.Fatal(err)
				}
				metadata.AccessToken = ""
				clean, _ := json.Marshal(metadata)
				if err = os.WriteFile(p.AgentFile("fixture"), clean, 0600); err != nil {
					t.Fatal(err)
				}
				// A failed fixture Read persists the fence before cutover.
				opts.Secrets = &daemonCLISecretFixture{}
			}
			err = config.MigrateDaemon(t.Context(), p, "fixture", opts)
			if phase == "done" && err != nil || phase == "in_progress" && err == nil {
				t.Fatal("fixture migration", err)
			}
			if err = os.WriteFile(p.HumanFile("fixture"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			for _, metadataOnly := range []bool{false, true} {
				if _, err = a.sessionAsMode("fixture", metadataOnly); !errors.Is(err, config.ErrIdentityMigrated) {
					t.Fatal("human session bypass", err)
				}
			}
			if _, err = config.LoadHumanCredentials(p, "fixture"); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("direct human load bypass", err)
			}
			if err = cached.checkConnection(); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("cached human bypass", err)
			}
			opts.Rollback = true
			opts.Secrets = &daemonCLISecretFixture{present: true}
			if err = config.MigrateDaemon(t.Context(), p, "fixture", opts); err != nil {
				t.Fatal(err)
			}
			if _, err = a.sessionAsMode("fixture", false); err != nil {
				t.Fatal("restored human neighbour", err)
			}
		})
	}
}

// Named humans have a distinct runtime namespace. The existing resolver must
// supply their agent counterpart explicitly; observing only human State would
// bypass the fence. Unfenced named human metadata remains readable.
func TestDaemonNamedHumanCounterpartFence(t *testing.T) {
	for _, phase := range []string{"in_progress", "done"} {
		t.Run(phase, func(t *testing.T) {
			a, root, to := daemonCLIFixture(t)
			c := config.Connection{ID: "fixture-named", URL: "https://example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: "file"}}
			registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "fixture", Profiles: map[string]config.Connection{"fixture": c}}
			data, _ := json.Marshal(registry)
			if err := os.WriteFile(root.ConnectionsFile(), data, 0600); err != nil {
				t.Fatal(err)
			}
			agent, err := c.Paths(root, "fixture", false)
			if err != nil {
				t.Fatal(err)
			}
			human, err := c.Paths(root, "fixture", true)
			if err != nil {
				t.Fatal(err)
			}
			b, err := c.Binding("fixture", true)
			if err != nil {
				t.Fatal(err)
			}
			creds := config.Credentials{UserID: b.UserID, Homeserver: c.URL, AccessToken: "DUMMY-human", ConnectionBinding: &b}
			if err = config.PublishBoundCredentials(human, "fixture", true, creds); err != nil {
				t.Fatal(err)
			}
			a.human = true
			cached, err := a.sessionAsMode("fixture", false)
			if err != nil {
				t.Fatal("unfenced named human", err)
			}
			agentBinding, e := c.Binding("fixture", false)
			if e != nil {
				t.Fatal(e)
			}
			agentCreds := creds
			agentCreds.ConnectionBinding = &agentBinding
			if err = config.PublishBoundCredentials(agent, "fixture", false, agentCreds); err != nil {
				t.Fatal(err)
			}
			if err = config.SaveState(agent, "fixture", config.State{}); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "in_progress" {
				cancel()
			}
			err = config.MigrateDaemon(ctx, agent, "fixture", config.DaemonMigrationOptions{Binding: &agentBinding, To: to, Now: time.Now()})
			if phase == "done" && err != nil || phase == "in_progress" && err == nil {
				t.Fatal("fixture migration", err)
			}
			if _, err = a.sessionAsMode("fixture", false); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("named human bypass", err)
			}
			if err = cached.checkConnection(); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("cached named human bypass", err)
			}
			if _, err = config.LoadHumanCredentials(human, "fixture"); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("config named human bypass", err)
			}
			if err = config.RewriteCredentialMetadata(human, "fixture", true, func(*config.Credentials) error { t.Error("callback ran"); return nil }); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("named human rewrite bypass", err)
			}
		})
	}
}

// Legacy creds migrate and register refuse before any fake/real provider access
// when their source is fenced. The existing migration recoverability tests are
// the valid provider neighbour; move-human's actual CLI neighbour runs here.
func TestDaemonCredentialCommandFence(t *testing.T) {
	for _, phase := range []string{"in_progress", "done"} {
		t.Run(phase, func(t *testing.T) {
			a, p, to := daemonCLIFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "in_progress" {
				cancel()
			}
			err := config.MigrateDaemon(ctx, p, "fixture", config.DaemonMigrationOptions{To: to, Now: time.Now()})
			if phase == "done" && err != nil || phase == "in_progress" && err == nil {
				t.Fatal("fixture migration", err)
			}
			a.as = "fixture"
			originalGetenv := a.Getenv
			a.Getenv = func(key string) string {
				if key == envSecretStore {
					return "keychain"
				}
				return originalGetenv(key)
			}
			a.keychain = func(config.Paths) *keychainStore { t.Fatal("provider reached through fence"); return nil }
			for _, human := range []bool{false, true} {
				a.human = human
				if err := a.migrateCredentials(t.Context(), nil); !errors.Is(err, config.ErrIdentityMigrated) {
					t.Fatal("creds migrate bypass", err)
				}
			}
			a.human = false
			if err := cmdCreds(t.Context(), a, []string{"move-human", "fixture"}); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("move CLI bypass", err)
			}
			if _, _, err := a.registerAgent(t.Context(), p, config.Matrix{URL: "https://example.invalid"}, "fixture", nil); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("registration bypass", err)
			}
			good, q, _ := daemonCLIFixture(t)
			if err := cmdCreds(t.Context(), good, []string{"move-human", "fixture"}); err != nil {
				t.Fatal("move CLI neighbour", err)
			}
			if _, err := config.LoadHumanCredentials(q, "fixture"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Low-level credential provider entry helpers take the credential lock too;
// neither direct writes nor named creates reach a provider through a fence.
// Existing fake-provider write and named routing tests exercise the neighbour.
func TestDaemonCredentialProviderHelpersFenced(t *testing.T) {
	for _, phase := range []string{"in_progress", "done"} {
		t.Run(phase, func(t *testing.T) {
			a, p, to := daemonCLIFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if phase == "in_progress" {
				cancel()
			}
			err := config.MigrateDaemon(ctx, p, "fixture", config.DaemonMigrationOptions{To: to, Now: time.Now()})
			if phase == "done" && err != nil || phase == "in_progress" && err == nil {
				t.Fatal("fixture migration", err)
			}
			k := &keychainStore{paths: p}
			if err = k.write(t.Context(), "@fixture:example.invalid", "DUMMY-never-stored"); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("provider write bypass", err)
			}
			original := a.Getenv
			a.Getenv = func(key string) string {
				if key == envSecretStore {
					return "keychain"
				}
				return original(key)
			}
			a.keychain = func(config.Paths) *keychainStore { t.Fatal("provider reached"); return nil }
			if err = a.createConnectionSecret(t.Context(), p, config.SecretReference{Service: "fixture.token", Account: "@fixture:example.invalid"}, "DUMMY-never-stored"); !errors.Is(err, config.ErrIdentityMigrated) {
				t.Fatal("named create bypass", err)
			}
		})
	}
}

// The actual durable-post caller holds its attempt lock after reservation.
// Migration refuses before fencing or provider effects; once posting completes,
// the ACK journal can migrate. No real Matrix or Keychain access is performed.
func TestDaemonDurablePostReservationExcludesMigration(t *testing.T) {
	w := operationWorldNew(t)
	a, _, diag := w.app()
	_, _, to := daemonCLIFixture(t)
	binding, err := w.connection.Binding("alice", false)
	if err != nil {
		t.Fatal(err)
	}
	opts := config.DaemonMigrationOptions{Binding: &binding, To: to, Now: time.Now(), Keychain: true, Secrets: &daemonCLISecretFixture{present: true}}
	reached := false
	a.postOperationFault = func(stage string) error {
		if stage == "after_reservation" {
			reached = true
			if err := config.MigrateDaemon(t.Context(), w.paths, "alice", opts); !errors.Is(err, config.ErrDaemonPostPending) {
				t.Error("live reserved post admitted", err)
			}
			if err := config.CheckIdentityMigration(w.paths, "alice"); err != nil {
				t.Error("reservation refusal fenced source", err)
			}
		}
		return nil
	}
	if code := a.Run(t.Context(), operationArgs()); code != 0 || !reached {
		t.Fatal("post fixture", code, diag)
	}
	if err := config.MigrateDaemon(t.Context(), w.paths, "alice", opts); err != nil {
		t.Fatal("finished ACK neighbour", err)
	}
}
