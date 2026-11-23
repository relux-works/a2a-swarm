package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const keychainUnsupportedText = "keychain requires a macOS cgo build"

// Proves that the legacy selector A2A_SECRET_STORE=keychain is refused at store
// selection, with the fixed text, in a build without the native backend, and is
// selected in a build that has it. Each build checks its own half: Linux and a
// darwin CGO_ENABLED=0 build run the refusal, a darwin cgo build runs the
// selection. It does not prove anything about a real Keychain: selection never
// opens one.
func TestKeychainSelectionNeedsNativeBuild(t *testing.T) {
	dir := t.TempDir()
	env := map[string]string{config.EnvConfigDir: dir, envSecretStore: "keychain", "HOME": dir}
	a := &App{Getenv: func(k string) string { return env[k] }}
	store, err := a.selectedStore()
	if keychainBuildSupported {
		if err != nil || store != "keychain" {
			t.Fatalf("selectedStore = %q, %v; want keychain in a build with the native backend", store, err)
		}
		return
	}
	if !errors.Is(err, errKeychainUnsupported) || store != "" || err.Error() != keychainUnsupportedText {
		t.Fatalf("selectedStore = %q, %v; want the fixed refusal", store, err)
	}
}

// Proves that file mode is selected in every build, by the default and by the
// explicit selector: the admission control for the refusal above.
func TestFileSelectionInEveryBuild(t *testing.T) {
	for _, selector := range []string{"", "file"} {
		dir := t.TempDir()
		env := map[string]string{config.EnvConfigDir: dir, envSecretStore: selector, "HOME": dir}
		a := &App{Getenv: func(k string) string { return env[k] }}
		if store, err := a.selectedStore(); err != nil || store != "file" {
			t.Fatalf("selector %q: selectedStore = %q, %v; want file", selector, store, err)
		}
	}
}

// Proves that a build without the native backend refuses a legacy Keychain
// invocation before any effect: no homeserver request at all, so register
// cannot create an account it could then not store, and no credential file is
// reserved. The same environment in file mode registers, which shows that the
// refusal comes from the selector and not from the fixture. A build with the
// native backend skips: there the selection would go on to the real Keychain,
// which no test may touch; its half is TestKeychainSelectionNeedsNativeBuild
// and the injected-backend registration in
// TestKeychainHumanLegacyRegistrationRefused/agent.
func TestKeychainUnsupportedBuildRefusesLegacyBeforeEffects(t *testing.T) {
	if keychainBuildSupported {
		t.Skip("this build has the native backend; the darwin CGO_ENABLED=0 and Linux lanes run the refusal")
	}
	for _, tc := range []struct {
		name string
		args func(e *env) []string
	}{
		{"register", func(*env) []string { return []string{"register", "newbie"} }},
		{"whoami", func(*env) []string { return []string{"--as", "alice", "whoami"} }},
		{"identity-ensure", func(e *env) []string {
			return []string{"identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--dir", e.projDir}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			e.swarm() // host slug and lobby, prepared in file mode
			e.extra[envSecretStore] = "keychain"
			e.hs.Reset()
			e.run(tc.args(e)...).failsWith(t, ExitError, keychainUnsupportedText)
			if calls := e.hs.Calls(); len(calls) != 0 {
				t.Fatalf("refused invocation reached the homeserver: %v", calls)
			}
			if _, err := os.Lstat(e.paths().AgentFile("newbie")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("refused registration reserved a credential file", err)
			}
		})
	}
	e := newEnv(t)
	e.extra[envSecretStore] = "file"
	e.run("register", "newbie").ok(t)
	if e.hs.CallCount("POST /_matrix/client/v3/register") == 0 {
		t.Fatal("file-mode control did not register")
	}
}

// Proves the same for a named Keychain profile: every command that needs a
// credential backend is refused with the fixed text and zero secret-store or
// HTTP effects, while profile management, which needs none, still runs.
func TestKeychainUnsupportedBuildRefusesNamedBeforeEffects(t *testing.T) {
	if keychainBuildSupported {
		t.Skip("this build has the native backend; the darwin CGO_ENABLED=0 and Linux lanes run the refusal")
	}
	for _, args := range [][]string{
		{"whoami"},
		{"register", "newbie"},
		{"identity", "ensure", "--role", "developer", "--runtime", "none"},
		{"rooms"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.connectionSecretStore = nil
			if code := a.Run(context.Background(), args); code == 0 {
				t.Fatal("Keychain profile admitted in a build without the native backend")
			}
			if !strings.Contains(f.err.String(), keychainUnsupportedText) {
				t.Fatalf("stderr = %q; want the fixed refusal", f.err.String())
			}
			if f.effects() != 0 {
				t.Fatalf("refusal produced %d effects", f.effects())
			}
		})
	}
	f := newNamedFixture(t, true)
	a := f.app()
	a.connectionSecretStore = nil
	if code := a.Run(context.Background(), []string{"connection", "list"}); code != 0 {
		t.Fatalf("profile management refused: %s", f.err.String())
	}
}
