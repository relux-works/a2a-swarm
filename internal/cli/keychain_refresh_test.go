//go:build unix

package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Reconcile is selected-backend/identity work: file, foreign, human, changed
// binding and migration-fenced selections refuse before constructing Keychain.
// A valid named Keychain selection reaches its own adapter without HTTP/items
// when there is no outstanding fence. The migration case uses the supported
// abstract transaction authority, cancelled before any secret-store call; it
// does not claim CLI/native migration capability. No real Keychain is exercised.
func TestKeychainReconcileSelection(t *testing.T) {
	for _, mode := range []string{"keychain", "file", "human", "human-class", "foreign", "changed-binding", "backend-conflict", "unknown-profile", "migration"} {
		t.Run(mode, func(t *testing.T) {
			var f *namedFixture
			if mode == "file" {
				f = newNamedFileFixture(t)
			} else {
				f = newNamedFixture(t, true)
			}
			if mode == "migration" {
				// The migration API rejects symlink ancestors, including a platform
				// temporary-directory alias; use its actual private fixture root.
				root, err := filepath.EvalSymlinks(f.root)
				if err != nil {
					t.Fatal("fixture canonical root unavailable")
				}
				f.root = root
			}
			a := f.app()
			a.profile = "alpha"
			p := f.paths("alpha", false)
			want := ""
			migrationTarget := ""
			switch mode {
			case "file":
				want = "requires the selected Keychain backend"
			case "human":
				a.human = true
				want = errHumanKeychain.Error()
			case "human-class":
				f.publish("alpha", true)
				want = "human identity"
			case "foreign", "changed-binding":
				data, err := os.ReadFile(p.AgentFile("alice"))
				if err != nil {
					t.Fatal("fixture metadata unavailable")
				}
				if mode == "foreign" {
					data = bytes.ReplaceAll(data, []byte("@alice:alpha.invalid"), []byte("@foreign:alpha.invalid"))
				} else {
					data = bytes.ReplaceAll(data, []byte("alpha-id"), []byte("changed-id"))
				}
				if err := os.WriteFile(p.AgentFile("alice"), data, 0600); err != nil {
					t.Fatal("fixture metadata preparation failed")
				}
				want = "binding"
			case "backend-conflict":
				f.env[envSecretStore] = "file"
				want = "conflicts with selected profile"
			case "unknown-profile":
				a.profile = "absent"
				want = "unknown connection profile"
			case "migration":
				// Establish the ordinary state directory through its supported
				// writer before the migration authority takes ownership of the fence.
				if err := config.SaveState(p, "alice", config.State{}); err != nil {
					t.Fatal("fixture ordinary state unavailable")
				}
				original, err := os.ReadFile(p.AgentFile("alice"))
				if err != nil {
					t.Fatal("fixture source unavailable")
				}
				targetRoot, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal("fixture target unavailable")
				}
				migrationTarget = filepath.Join(targetRoot, "daemon")
				binding, err := f.r.Profiles["alpha"].Binding("alice", false)
				if err != nil {
					t.Fatal("fixture binding unavailable")
				}
				store := &daemonCLISecretFixture{present: true}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
				err = config.MigrateDaemon(ctx, p, "alice", config.DaemonMigrationOptions{Binding: &binding, To: migrationTarget, Keychain: true, Secrets: store, Now: at})
				state, stateErr := config.LoadState(p, "alice")
				if !errors.Is(err, config.ErrDaemonMigration) || stateErr != nil || state.Migration == nil || state.Migration.Phase != "in_progress" || state.Migration.CutoverStarted || state.Migration.DaemonBase != migrationTarget || !state.Migration.At.Equal(at) || !errors.Is(config.CheckIdentityMigration(p, "alice"), config.ErrIdentityMigrated) {
					t.Fatal("supported abstract migration did not establish source fence")
				}
				if data, err := os.ReadFile(p.AgentFile("alice")); err != nil || !bytes.Equal(data, original) || !store.present || store.reads != 0 || store.deletes != 0 || store.restores != 0 {
					t.Fatal("cancelled fixture changed credentials or reached abstract provider")
				}
				want = config.ErrIdentityMigrated.Error()
			}
			constructors := 0
			a.keychain = func(got config.Paths) *keychainStore {
				constructors++
				if got != p {
					t.Fatal("reconcile selected wrong bookkeeping root")
				}
				return isolatedKeychainStore(keychainStore{paths: got})
			}
			before := memberRootFiles(t, f.root)
			sourceSnapshot := daemonMigrationSnapshot(t, f.root)
			var targetSnapshot map[string]daemonMigrationSnapshotEntry
			if migrationTarget != "" {
				targetSnapshot = daemonMigrationSnapshot(t, migrationTarget)
			}
			err := a.reconcileCredentials(context.Background(), nil)
			if mode == "migration" {
				if constructors != 0 {
					t.Fatal("migration-fenced reconcile reached native constructor")
				}
				if !errors.Is(err, config.ErrIdentityMigrated) {
					t.Fatal("migration-fenced reconcile lost identity_migrated")
				}
				assertDaemonMigrationSnapshot(t, migrationTarget, targetSnapshot)
			}
			if mode != "keychain" {
				assertDaemonMigrationSnapshot(t, f.root, sourceSnapshot)
			}
			if mode == "keychain" {
				if err != nil || constructors != 1 || !strings.Contains(f.out.String(), "no unresolved keychain write") {
					t.Fatal("valid selected reconcile control refused")
				}
			} else if err == nil || !strings.Contains(err.Error(), want) || constructors != 0 || f.out.Len() != 0 || !reflect.DeepEqual(before, memberRootFiles(t, f.root)) {
				t.Fatal("reconcile crossed selected backend/identity guard")
			}
			if f.effects() != 0 {
				t.Fatal("reconcile performed HTTP or secret-provider effects")
			}
		})
	}
}

// The actual legacy reconcile path observes supported file-migration fences
// before its synthetic Keychain constructor, without changing source/target
// bytes, modes or lock inodes. An unfenced selected-Keychain control still
// reconciles normally; its ordinary provider-lock creation is permitted.
// This prepares only owned abstract/file fixtures, never CLI/native migration.
func TestKeychainReconcileLegacyMigrationFence(t *testing.T) {
	for _, phase := range []string{"unfenced", "in_progress", "done"} {
		t.Run(phase, func(t *testing.T) {
			a, p, to := daemonCLIFixture(t)
			if phase != "unfenced" {
				ctx, cancel := context.WithCancel(context.Background())
				if phase == "in_progress" {
					cancel()
				}
				err := config.MigrateDaemon(ctx, p, "fixture", config.DaemonMigrationOptions{To: to, Now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
				cancel()
				state, stateErr := config.LoadState(p, "fixture")
				if stateErr != nil || state.Migration == nil || state.Migration.Phase != phase || !errors.Is(config.CheckIdentityMigration(p, "fixture"), config.ErrIdentityMigrated) || phase == "done" && err != nil || phase == "in_progress" && !errors.Is(err, config.ErrDaemonMigration) {
					t.Fatal("supported legacy migration did not establish source fence")
				}
			}
			getenv := a.Getenv
			a.Getenv = func(key string) string {
				if key == envSecretStore {
					return "keychain"
				}
				return getenv(key)
			}
			var out bytes.Buffer
			a.Stdout = &out
			constructors := 0
			a.keychain = func(got config.Paths) *keychainStore {
				constructors++
				if got != p {
					t.Fatal("legacy reconcile selected wrong source")
				}
				return isolatedKeychainStore(keychainStore{paths: got})
			}
			sourceBefore := daemonMigrationSnapshot(t, p.Base)
			targetBefore := daemonMigrationSnapshot(t, to)
			err := a.reconcileCredentials(context.Background(), nil)
			if phase == "unfenced" {
				if err != nil || constructors != 1 || !strings.Contains(out.String(), "no unresolved keychain write") {
					t.Fatal("valid unfenced legacy reconcile control refused")
				}
			} else {
				if constructors != 0 {
					t.Fatal("migration-fenced legacy reconcile reached native constructor")
				}
				if !errors.Is(err, config.ErrIdentityMigrated) || out.Len() != 0 {
					t.Fatal("legacy reconcile lost migration fence refusal")
				}
				assertDaemonMigrationSnapshot(t, p.Base, sourceBefore)
			}
			assertDaemonMigrationSnapshot(t, to, targetBefore)
		})
	}
}

// The legacy file path also refuses before the native factory, preserving its
// metadata; legacy Keychain's successful reconcile control is in the fence suite.
func TestKeychainReconcileLegacyFile(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	before := memberRootFiles(t, e.dir)
	e.configure = func(a *App) {
		a.keychain = func(config.Paths) *keychainStore { t.Fatal("file reconcile constructed Keychain"); return nil }
	}
	e.as("alice", "creds", "reconcile").failsWith(t, ExitError, "requires the selected Keychain backend")
	if !reflect.DeepEqual(before, memberRootFiles(t, e.dir)) {
		t.Fatal("refused file reconcile changed source")
	}
}

// The current transaction calls the locked adapter directly. Its human guard
// must precede even an injected provider, beside an agent create control. The
// public helper's separate human guard remains covered by the named suite.
func TestNamedHumanLockedSecretCreateRefused(t *testing.T) {
	f := newNamedFixture(t, true)
	a := f.app()
	a.human = true
	ref := config.SecretReference{Service: "test.alpha.token", Account: "@carol:alpha.invalid"}
	before := memberRootFiles(t, f.root)
	if err := a.createConnectionSecretLocked(context.Background(), f.paths("alpha", true), ref, "isolated-new"); !errors.Is(err, errHumanKeychain) || f.effects() != 0 || !reflect.DeepEqual(before, memberRootFiles(t, f.root)) {
		t.Fatal("locked human create reached provider or changed source")
	}
	if err := f.app().createConnectionSecretLocked(context.Background(), f.paths("alpha", false), ref, "isolated-new"); err != nil || f.effects() != 1 {
		t.Fatal("locked agent create control refused")
	}
}

type memberSourceBackend struct {
	calls  []opRecord
	refuse bool
}

func (b *memberSourceBackend) find(_ context.Context, target, service, account string) ([]byte, error) {
	b.calls = append(b.calls, opRecord{"find", target, service, account})
	if b.refuse {
		return nil, errKeychainInteraction
	}
	if service != "test.access" || account != admin198User {
		return nil, errors.New("unexpected source credential reference")
	}
	return []byte("isolated-admin-session"), nil
}
func (b *memberSourceBackend) add(_ context.Context, target, service, account string, _ []byte) error {
	b.calls = append(b.calls, opRecord{"add", target, service, account})
	return errors.New("source mutation forbidden")
}
func (b *memberSourceBackend) update(_ context.Context, target, service, account string, _ []byte) error {
	b.calls = append(b.calls, opRecord{"update", target, service, account})
	return errors.New("source mutation forbidden")
}

// This exercises the current production named Keychain adapter with a synthetic
// backend, rather than bypassing it through connectionSecretStore: the source
// administrator authorizes registration, all member storage/use is file-only,
// and a refused source read cannot issue/register/reserve a destination. Native
// ACL/UI/resource behavior and a real server remain outside this fixture.
func TestMemberRootInProcessSourceSeparation(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorized", true: "source-refused"}[refused], func(t *testing.T) {
			f := newAdmin198Backend(t, "broad-subscription", "keychain")
			var out, stderr bytes.Buffer
			a, rt := memberRootApp(f, &out, &stderr)
			backend := &memberSourceBackend{}
			constructors := 0
			install := func(app *App) {
				app.connectionSecretStore = nil
				app.keychain = func(p config.Paths) *keychainStore {
					constructors++
					return isolatedKeychainStore(keychainStore{paths: p, backend: backend, probeCommand: unlockedProbe})
				}
			}
			install(a)
			// Normal session setup precedes the strict source snapshot; no directory
			// filter or post-registration baseline can hide child-side writes.
			if a.Run(context.Background(), []string{"whoami"}) != 0 {
				t.Fatal("source session setup failed")
			}
			before := memberRootFiles(t, f.root)
			subs := f.subscriptionSnapshot()
			backend.refuse = refused
			out.Reset()
			stderr.Reset()
			a, rt = memberRootApp(f, &out, &stderr)
			install(a)
			target := memberRootPath(t)
			code := a.Run(context.Background(), memberRootArgs(target))
			if constructors != 2 || len(backend.calls) != 2 {
				t.Fatal("source authorization was bypassed or repeated")
			}
			for _, op := range backend.calls {
				if op.Op != "find" || op.Service != "test.access" || op.Account != admin198User {
					t.Fatal("member transaction accessed child source credentials")
				}
			}
			if refused {
				if code == 0 || f.issues != 0 || f.registrations != 0 || f.auths != 0 {
					t.Fatal("refused source authorization performed registration")
				}
				if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("refused source authorization reserved destination")
				}
				f.assertSubscriptionsUnchanged(subs)
				return
			}
			if code != 0 || f.issues != 1 || f.auths != 1 || f.registrations != 2 || f.redactions != 2 || rt.adminWhoami != 1 {
				t.Fatal("authorized registration/cleanup control failed")
			}
			assertMemberSourceUntouched(t, f, before, subs)
			registry, err := config.LoadConnections(config.Paths{Base: target})
			if err != nil || registry == nil || len(registry.Profiles) != 1 || registry.Profiles[registry.Current].Store != (config.CredentialStore{Backend: "file"}) {
				t.Fatal("member destination retained native source backend")
			}
			moved := f.root + "-unavailable"
			if err := os.Rename(f.root, moved); err != nil {
				t.Fatal("source isolation setup failed")
			}
			t.Cleanup(func() { _ = os.Rename(moved, f.root) })
			member := NewApp()
			member.Stdout, member.Stderr, member.HTTP = &out, &stderr, &http.Client{Transport: rt}
			member.Getenv = func(key string) string {
				switch key {
				case config.EnvConfigDir:
					return target
				case config.EnvAgent:
					return "newbie"
				}
				return ""
			}
			forbidFileKeychain(member)
			if member.Run(context.Background(), []string{"whoami"}) != 0 || rt.memberWhoami != 1 || constructors != 2 {
				t.Fatal("member file use depended on native/source credentials")
			}
		})
	}
}

// A named fence uses the selected token service/full identity, clearing only an
// observed intended value. A different value keeps the same fence. This tests
// Go routing/reconcile behavior through the backend seam, not native liveness.
func TestNamedKeychainFenceReconcile(t *testing.T) {
	for _, observed := range []bool{true, false} {
		t.Run(map[bool]string{true: "observed", false: "different-value"}[observed], func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			a.profile = "alpha"
			a.connectionSecretStore = nil
			p := f.paths("alpha", false)
			var calls []opRecord
			k := isolatedKeychainStore(keychainStore{paths: p, backend: routingBackend{&calls}, probeCommand: unlockedProbe,
				liveness: func(context.Context, int, string) ownerState { return ownerGone },
				boot:     func() (string, error) { return sessionA, nil }})
			k.service = "test.alpha.token"
			a.keychain = func(config.Paths) *keychainStore { return k }
			intended := "isolated-alpha"
			if !observed {
				intended = "isolated-other"
			}
			uid := "@alice:alpha.invalid"
			plantFence(t, k, uid, deadFence(intended), 0600)
			err := a.reconcileCredentials(context.Background(), nil)
			if len(calls) != 1 || calls[0] != (opRecord{"find", k.fixture, "test.alpha.token", uid}) || f.effects() != 0 {
				t.Fatal("named reconcile used wrong service/identity or repeated lookup")
			}
			_, stateErr := os.Lstat(k.fencePath(uid))
			if observed {
				if err != nil || !errors.Is(stateErr, os.ErrNotExist) || !strings.Contains(f.out.String(), "observed completed") {
					t.Fatal("observed named completion did not clear fence")
				}
			} else if !errors.Is(err, errKeychainFenced) || stateErr != nil || f.out.Len() != 0 {
				t.Fatal("different named value cleared fence or reported success")
			}
		})
	}
}

// The low-level legacy register path also rejects an interactive human flag
// before remote creation, credential reservation or native construction. Its
// adjacent ordinary-agent control completes with the fake in-process backend.
func TestKeychainHumanLegacyRegistrationRefused(t *testing.T) {
	for _, human := range []bool{false, true} {
		t.Run(map[bool]string{false: "agent", true: "human"}[human], func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			dir := fakeKeychain(e, map[string]string{}, "")
			configure := e.configure
			constructors := 0
			e.configure = func(a *App) {
				configure(a)
				a.stdinTTY = func() bool { return true }
				factory := a.keychain
				a.keychain = func(p config.Paths) *keychainStore { constructors++; return factory(p) }
			}
			if human {
				e.run("--human", "--as", "alice", "register", "newbie").failsWith(t, ExitError, errHumanKeychain.Error())
				if constructors != 0 || len(records(t, dir)) != 0 {
					t.Fatal("human registration reached Keychain")
				}
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/register")
				if _, err := os.Lstat(e.paths().AgentFile("newbie")); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("human registration reserved credential file")
				}
				return
			}
			e.run("register", "newbie").ok(t)
			if constructors != 1 || readModel(t, dir).Writes != 1 {
				t.Fatal("agent registration control did not use in-process adapter")
			}
			tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/register", 2)
		})
	}
}
