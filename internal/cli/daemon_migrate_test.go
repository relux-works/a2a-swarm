//go:build unix

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type daemonTransport func(r *http.Request) (*http.Response, error)

func (f daemonTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func daemonCLIFixture(t *testing.T) (*App, config.Paths, string) {
	t.Helper()
	sourceRoot, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(sourceRoot, "private")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	p := config.Paths{Base: root}
	file, e := config.CreateCredentialFile(p, "fixture")
	if e != nil {
		t.Fatal(e)
	}
	if e = config.WriteCredentials(file, config.Credentials{UserID: "@fixture:example.invalid", AccessToken: "inert-fixture-only", Homeserver: "https://example.invalid"}); e != nil {
		t.Fatal(e)
	}
	if e = config.SaveState(p, "fixture", config.State{NextBatch: "fixture-cursor"}); e != nil {
		t.Fatal(e)
	}
	targetRoot, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	to := filepath.Join(targetRoot, "private")
	if e = os.Mkdir(to, 0700); e != nil {
		t.Fatal(e)
	}
	a := NewApp()
	a.Stdout = io.Discard
	a.Stderr = io.Discard
	a.Stdin = strings.NewReader(`{"session_id":"fixture-session"}`)
	a.Getenv = func(k string) string {
		switch k {
		case config.EnvConfigDir:
			return p.Base
		case config.EnvAgent:
			return "fixture"
		}
		return ""
	}
	a.HTTP = &http.Client{Transport: daemonTransport(func(r *http.Request) (*http.Response, error) {
		if e := r.Context().Err(); e != nil {
			return nil, e
		}
		t.Error("unexpected provider access")
		return nil, errors.New("fixture refuses network")
	})}
	return a, p, to
}
func daemonCLIMigrate(t *testing.T, p config.Paths, to string, rollback bool) {
	t.Helper()
	if e := config.MigrateDaemon(context.Background(), p, "fixture", config.DaemonMigrationOptions{To: to, Rollback: rollback, Now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}); e != nil {
		t.Fatal(e)
	}
}

// This is the actual cached-session causal counterexample from the earlier
// probes: credentials load before migration, then lease acquisition after both
// migration locks have been released. No request reaches a provider.
func TestDaemonPreloadedSessionFence(t *testing.T) {
	a, p, to := daemonCLIFixture(t)
	cached, e := a.sessionAsMode("fixture", false)
	if e != nil {
		t.Fatal(e)
	}
	if e = cached.checkConnection(); e != nil {
		t.Fatal("unfenced guard neighbor", e)
	}
	if e = a.acquireLease(cached, "fixture-session", time.Now().Add(time.Second)); e != nil {
		t.Fatal("unfenced lease neighbor", e)
	}
	if _, e = config.UpdateState(p, "fixture", func(s *config.State) error { s.Lease = nil; return nil }); e != nil {
		t.Fatal(e)
	}
	daemonCLIMigrate(t, p, to, false)
	before, e := os.ReadFile(p.StateFile("fixture"))
	if e != nil {
		t.Fatal(e)
	}
	if e = cached.checkConnection(); !errors.Is(e, config.ErrIdentityMigrated) {
		t.Fatal("cached session bypassed fence", e)
	}
	if e = cached.client.BeforeRequest(); !errors.Is(e, config.ErrIdentityMigrated) {
		t.Fatal("request boundary bypassed fence", e)
	}
	for _, holder := range []string{"fixture-session", "fixture-renew"} {
		if e = a.acquireLease(cached, holder, time.Now().Add(time.Second)); !errors.Is(e, config.ErrIdentityMigrated) {
			t.Fatal("cached acquire/renew bypassed fence", e)
		}
	}
	after, e := os.ReadFile(p.StateFile("fixture"))
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("cached session rewrote source state")
	}
	daemonCLIMigrate(t, p, to, true)
	if e = cached.checkConnection(); e != nil {
		t.Fatal("restored guard neighbor", e)
	}
	if e = a.acquireLease(cached, "fixture-session", time.Now().Add(time.Second)); e != nil {
		t.Fatal("restored lease neighbor", e)
	}
}

func TestDaemonSessionAndStartFences(t *testing.T) {
	for _, entry := range []string{"session", "metadata", "credentials", "connection", "hook", "wait", "watch", "bind", "bound-wait", "bound-consumer"} {
		t.Run(entry, func(t *testing.T) {
			a, p, to := daemonCLIFixture(t)
			cached, e := a.sessionAsMode("fixture", false)
			if e != nil {
				t.Fatal(e)
			}
			if entry == "hook" {
				if _, e = config.UpdateState(p, "fixture", func(s *config.State) error { s.HooksPaused = true; return nil }); e != nil {
					t.Fatal(e)
				}
			}
			daemonCLIMigrate(t, p, to, false)
			invoke := func() error {
				switch entry {
				case "session":
					_, e := a.sessionAsMode("fixture", false)
					return e
				case "metadata":
					_, e := a.sessionAsMode("fixture", true)
					return e
				case "credentials":
					_, e := a.loadCredentials(context.Background(), p, "fixture")
					return e
				case "connection":
					return cached.checkConnection()
				case "hook":
					blocked := false
					return a.hookDeliver(context.Background(), hookRuntimes[0], hookEvents[0], hookInput{SessionID: "fixture-session"}, &blocked)
				case "wait":
					return cmdWait(context.Background(), a, []string{"--timeout", "1ns", "--json"})
				case "watch":
					ctx, cancel := context.WithCancel(context.Background())
					cancel()
					return cmdWatch(ctx, a, nil)
				case "bind":
					return cmdBind(context.Background(), a, nil)
				case "bound-wait":
					return refuseBoundWait(context.Background(), cached)
				default:
					return refuseHostConsumer(cached)
				}
			}
			if e = invoke(); !errors.Is(e, config.ErrIdentityMigrated) {
				t.Fatal("entry bypassed closed fence", e)
			}
			daemonCLIMigrate(t, p, to, true)
			if entry == "wait" {
				a.HTTP = &http.Client{Transport: daemonTransport(func(r *http.Request) (*http.Response, error) {
					body := `{}`
					if strings.HasSuffix(r.URL.Path, "/joined_rooms") {
						body = `{"joined_rooms":[]}`
					}
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
				})}
			}
			// Same invocation with identical identity/files, only the verified fence
			// removed. Bind's neighbour stops at usage validation before any OS effect;
			// watch's neighbour accepts its selected session then exits on cancellation.
			e = invoke()
			switch entry {
			case "bind":
				var usage usageError
				if !errors.As(e, &usage) {
					t.Fatal("unfenced bind did not reach flag validation", e)
				}
			case "watch":
				if errors.Is(e, config.ErrIdentityMigrated) {
					t.Fatal("unfenced watch still fenced")
				}
			case "hook": // Pause is a successful hook start with no provider interaction.
			default:
				if e != nil {
					t.Fatal("unfenced entry neighbor", e)
				}
			}
		})
	}
}

// TestDaemonMigrationCLIOnly proves migration refuses implicit resume and supports
// explicit rollback; neighbouring commands still require their own arguments.
func TestDaemonMigrationCLIOnly(t *testing.T) {
	a, p, to := daemonCLIFixture(t)
	if e := cmdDaemon(context.Background(), a, []string{"enroll"}); e == nil {
		t.Fatal("enroll accepted missing arguments")
	}
	if e := cmdDaemon(context.Background(), a, []string{"serve"}); e == nil {
		t.Fatal("serve accepted missing arguments")
	}
	if e := cmdDaemon(context.Background(), a, []string{"migrate", "fixture", "--to", to}); e != nil {
		t.Fatal(e)
	}
	if e := cmdDaemon(context.Background(), a, []string{"migrate", "fixture", "--to", to}); e == nil {
		t.Fatal("silent resume")
	}
	if e := cmdDaemon(context.Background(), a, []string{"migrate", "fixture", "--to", to, "--rollback"}); e != nil {
		t.Fatal(e)
	}
	if _, e := config.LoadCredentials(p, "fixture"); e != nil {
		t.Fatal(e)
	}
}

func TestDaemonHookStartBeforeExport(t *testing.T) {
	for _, runtime := range []string{"codex", "claude", "muse"} {
		t.Run(runtime, func(t *testing.T) {
			a, p, to := daemonCLIFixture(t)
			a.as = "fixture"
			var stderr bytes.Buffer
			a.Stderr = &stderr
			envFile := filepath.Join(t.TempDir(), "fixture-env")
			original := a.Getenv
			a.Getenv = func(k string) string {
				if k == "CLAUDE_ENV_FILE" {
					return envFile
				}
				return original(k)
			}
			if _, e := config.UpdateState(p, "fixture", func(s *config.State) error { s.HooksPaused = true; return nil }); e != nil {
				t.Fatal(e)
			}
			daemonCLIMigrate(t, p, to, false)
			if e := a.exportSessionID("fixture-session"); !errors.Is(e, config.ErrIdentityMigrated) {
				t.Fatal("session export crossed fence", e)
			}
			if e := cmdHook(context.Background(), a, []string{"session-start", "--runtime", runtime}); e != nil {
				t.Fatal(e)
			}
			if !strings.Contains(stderr.String(), "identity_migrated") {
				t.Fatal("hook startup lacked closed refusal")
			}
			if _, e := os.Stat(envFile); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("hook wrote environment through fence")
			}
			daemonCLIMigrate(t, p, to, true)
			if e := a.exportSessionID("fixture-session"); e != nil {
				t.Fatal("unfenced export neighbor", e)
			}
			data, e := os.ReadFile(envFile)
			if e != nil || !strings.Contains(string(data), "fixture-session") {
				t.Fatal("unfenced export absent")
			}
		})
	}
}

// Migration preflight preserves #389's malformed-owner/config delivery_error,
// while other unreadable State still reports state_unavailable. Neither path
// emits mail or repairs State; a restored valid owner hook must still refresh.
// This exercises local Muse hooks with paused delivery, not a live runtime.
func TestDaemonMuseHookStateRefusals(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, state := range []struct{ name, raw, diagnostic string }{
		{"owner", `{"monitor_owner":{}}`, "delivery_error"},
		{"config", `{"monitor_config":{}}`, "delivery_error"},
		{"json", `{`, "state_unavailable"},
	} {
		for _, ev := range []hookEvent{{"session-start", "SessionStart"}, {"prompt", "UserPromptSubmit"}} {
			t.Run(state.name+"/"+ev.arg, func(t *testing.T) {
				a, p, _ := daemonCLIFixture(t)
				a.as = "fixture"
				var stdout, stderr bytes.Buffer
				a.Stdout, a.Stderr = &stdout, &stderr
				admittedAt := time.Now().Add(-time.Minute)
				if err := config.RecordMonitorOwnerHook(p, "fixture", time.Now().Add(time.Second), "muse", "fixture-session", "SessionStart", false, admittedAt); err != nil {
					t.Fatal(err)
				}
				if _, err := config.UpdateState(p, "fixture", func(s *config.State) error { s.HooksPaused = true; return nil }); err != nil {
					t.Fatal(err)
				}
				path := p.StateFile("fixture")
				valid, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(state.raw), 0600); err != nil {
					t.Fatal(err)
				}
				invoke := func() {
					a.Stdin = strings.NewReader(`{"session_id":"fixture-session","hook_event_name":"` + ev.name + `"}`)
					if err := cmdHook(t.Context(), a, []string{ev.arg, "--runtime", "muse"}); err != nil {
						t.Fatal(err)
					}
				}
				invoke()
				if !strings.Contains(stderr.String(), ": "+state.diagnostic+";") || stdout.Len() != 0 {
					t.Fatal("malformed State did not retain closed diagnostic", stderr.String())
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(after, []byte(state.raw)) {
					t.Fatal("refused hook changed malformed State", err)
				}
				if err := os.WriteFile(path, valid, 0600); err != nil {
					t.Fatal(err)
				}
				stderr.Reset()
				invoke()
				s, err := config.LoadState(p, "fixture")
				if err != nil || s.MonitorOwner == nil || !s.MonitorOwner.LastHookAt.After(admittedAt) || stderr.Len() != 0 || stdout.Len() != 0 {
					t.Fatal("valid paused owner-hook neighbour was not admitted", err, stderr.String())
				}
			})
		}
	}
}

type daemonCLISecretFixture struct {
	present                  bool
	failDelete               bool
	reads, deletes, restores int
}

func (s *daemonCLISecretFixture) Read(_ context.Context, c config.Credentials) (config.Credentials, error) {
	s.reads++
	if !s.present {
		return c, errors.New("fixture missing")
	}
	c.AccessToken = "inert-materialized-fixture"
	return c, nil
}
func (s *daemonCLISecretFixture) Delete(context.Context, config.Credentials) error {
	s.deletes++
	if s.failDelete {
		return errors.New("synthetic deletion interrupted")
	}
	s.present = false
	return nil
}
func (s *daemonCLISecretFixture) Restore(_ context.Context, c config.Credentials) error {
	s.restores++
	if c.AccessToken != "inert-materialized-fixture" {
		return errors.New("fixture mismatch")
	}
	s.present = true
	return nil
}

// The named FILE CLI still migrates and restores the selected roots. The old
// injected Keychain-success row is deliberately replaced by the six-route
// preflight refusals in TestDaemonKeychainMigrationDeferred; it is not support
// for native CLI migration through a fake provider.
func TestDaemonNamedResolverRoundTrip(t *testing.T) {
	for _, backend := range []string{"file"} {
		t.Run(backend, func(t *testing.T) {
			a, root, to := daemonCLIFixture(t)
			connection := config.Connection{ID: "fixture-id", URL: "https://example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: backend}}

			registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "fixture", Profiles: map[string]config.Connection{"fixture": connection}}
			encoded, e := json.Marshal(registry)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(root.ConnectionsFile(), encoded, 0600); e != nil {
				t.Fatal(e)
			}
			p, e := connection.Paths(root, "fixture", false)
			if e != nil {
				t.Fatal(e)
			}
			binding, e := connection.Binding("fixture", false)
			if e != nil {
				t.Fatal(e)
			}
			c := config.Credentials{UserID: binding.UserID, Homeserver: connection.URL, ConnectionBinding: &binding}
			if backend == "file" {
				c.AccessToken = "inert-named-fixture"
			}
			if e = config.PublishBoundCredentials(p, "fixture", false, c); e != nil {
				t.Fatal(e)
			}
			if e = config.SaveState(p, "fixture", config.State{NextBatch: "named-fixture"}); e != nil {
				t.Fatal(e)
			}
			if e = cmdDaemon(context.Background(), a, []string{"migrate", "fixture", "--to", to}); e != nil {
				t.Fatal(e)
			}
			if e = config.CheckIdentityMigration(p, "fixture"); !errors.Is(e, config.ErrIdentityMigrated) {
				t.Fatal("resolved runtime root not fenced", e)
			}
			if e = config.CheckIdentityMigration(root, "fixture"); e != nil {
				t.Fatal("unselected legacy identity was fenced", e)
			}
			if e = cmdDaemon(context.Background(), a, []string{"migrate", "fixture", "--to", to, "--rollback"}); e != nil {
				t.Fatal(e)
			}
			if _, e = a.namedMetadata(p, "fixture"); e != nil {
				t.Fatal("named metadata not restored", e)
			}
		})
	}
}

func TestDaemonReadOnlyRequestFence(t *testing.T) {
	a, p, to := daemonCLIFixture(t)
	calls := 0
	a.HTTP = &http.Client{Transport: daemonTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 200, ContentLength: -1, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"chunk":[{"type":"m.room.member","room_id":"!room:fixture","state_key":"@fixture:example.invalid","content":{"membership":"join"}}]}`)), Request: r}, nil
	})}
	daemonCLIMigrate(t, p, to, false)
	if e := cmdRoomMembers(context.Background(), a, []string{"!room:fixture", "--json"}); !errors.Is(e, config.ErrIdentityMigrated) || calls != 0 {
		t.Fatal("read-only request crossed fence", e)
	}
	daemonCLIMigrate(t, p, to, true)
	if e := cmdRoomMembers(context.Background(), a, []string{"!room:fixture", "--json"}); e != nil || calls != 1 {
		t.Fatal("unfenced read-only neighbor", e)
	}
	if e := os.WriteFile(p.StateFile("fixture"), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := cmdRoomMembers(context.Background(), a, []string{"!room:fixture", "--json"}); !errors.Is(e, config.ErrMigrationStateUnavailable) || calls != 1 {
		t.Fatal("malformed State allowed a request", e)
	}
}

// TestDaemonDispatch proves App.Run routes migrate and enroll to their real local
// handlers, preserves Daemon help as a valid neighbour, and refuses an unknown
// subcommand. It uses private fixtures, not a live daemon or secret store.
func TestDaemonDispatch(t *testing.T) {
	a, p, to := daemonCLIFixture(t)
	var stdout, stderr bytes.Buffer
	a.Stdout, a.Stderr = &stdout, &stderr
	ctx := context.Background()
	if code := a.Run(ctx, []string{"daemon", "migrate", "fixture", "--to", to}); code != ExitOK {
		t.Fatalf("migrate dispatch: code=%d stderr=%s", code, &stderr)
	}
	if stdout.String() != "daemon migration verified; source identity fenced\n" {
		t.Fatalf("migrate output: %q", &stdout)
	}
	if _, err := config.LoadCredentials(p, "fixture"); !errors.Is(err, config.ErrIdentityMigrated) {
		t.Fatalf("migration did not fence credentials: %v", err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := a.Run(ctx, []string{"daemon", "migrate", "fixture", "--to", to, "--rollback"}); code != ExitOK {
		t.Fatalf("rollback dispatch: code=%d stderr=%s", code, &stderr)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, ".a2a")
	payload, err := json.Marshal(map[string]string{
		"socket": "/run/a2a/daemon.sock", "identity": "enrolled", "station": "station-1",
		"secret_b64": base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{31}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Stdin = bytes.NewReader(append(payload, '\n'))
	stdout.Reset()
	stderr.Reset()
	if code := a.Run(ctx, []string{"daemon", "enroll", "--dir", dir, "--from", "-"}); code != ExitOK {
		t.Fatalf("enroll dispatch: code=%d stderr=%s", code, &stderr)
	}
	identity, err := os.ReadFile(filepath.Join(dir, "identity"))
	if err != nil || string(identity) != "enrolled\n" || stdout.String() != "Enrollment installed.\n" {
		t.Fatalf("enroll handler effect: identity=%q err=%v stdout=%q", identity, err, &stdout)
	}
	stdout.Reset()
	stderr.Reset()
	if code := a.Run(ctx, []string{"daemon", "help"}); code != ExitOK {
		t.Fatalf("valid help neighbour: code=%d stderr=%s", code, &stderr)
	}
	for _, sub := range []string{"enroll", "migrate", "serve", "register-station", "unregister-station"} {
		if !strings.Contains(stdout.String(), sub) {
			t.Fatalf("help omitted %s", sub)
		}
	}
	stdout.Reset()
	stderr.Reset()
	if code := a.Run(ctx, []string{"daemon", "unknown-subcommand"}); code != ExitUsage || stdout.Len() != 0 || stderr.Len() == 0 {
		t.Fatalf("unknown subcommand: code=%d stdout=%q stderr=%q", code, &stdout, &stderr)
	}
}

type daemonMigrationSnapshotEntry struct {
	mode os.FileMode
	data []byte
	info os.FileInfo
}

// Capture the entire owned fixture tree, including directories and lock files;
// no filtered baseline can hide a new staging/journal/credential namespace.
func daemonMigrationSnapshot(t *testing.T, root string) map[string]daemonMigrationSnapshotEntry {
	t.Helper()
	entries := map[string]daemonMigrationSnapshotEntry{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		var data []byte
		if !d.IsDir() {
			if !info.Mode().IsRegular() {
				return errors.New("unexpected fixture entry")
			}
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		rel, err := filepath.Rel(root, path)
		if err == nil {
			entries[rel] = daemonMigrationSnapshotEntry{info.Mode(), data, info}
		}
		return err
	}); err != nil {
		t.Fatal("fixture inventory unavailable")
	}
	return entries
}

func assertDaemonMigrationSnapshot(t *testing.T, root string, before map[string]daemonMigrationSnapshotEntry) {
	t.Helper()
	after := daemonMigrationSnapshot(t, root)
	if len(before) != len(after) {
		t.Fatal("deferred migration changed fixture inventory")
	}
	for name, old := range before {
		fresh, ok := after[name]
		if !ok || old.mode != fresh.mode || !bytes.Equal(old.data, fresh.data) || !os.SameFile(old.info, fresh.info) || !old.info.ModTime().Equal(fresh.info.ModTime()) {
			t.Fatal("deferred migration changed fixture bytes, modes or identity")
		}
	}
}

type daemonSelectedMigrationFixture struct {
	app           *App
	root, paths   config.Paths
	to            string
	binding       *config.ConnectionBinding
	named         bool
	store         *daemonCLISecretFixture
	constructors  int
	items, probes int
	clockCalls    int
	out, stderr   bytes.Buffer
}

// This fixture selects one backend independently of the registry's current
// profile. Every possible native/provider boundary is synthetic and counted;
// even the deliberate constructor-before-refusal plant cannot touch Keychain.
func daemonSelectedMigration(t *testing.T, routing, backend string) *daemonSelectedMigrationFixture {
	t.Helper()
	a, root, to := daemonCLIFixture(t)
	f := &daemonSelectedMigrationFixture{app: a, root: root, paths: root, to: to, named: routing == "named", store: &daemonCLISecretFixture{present: true}}
	if f.named {
		c := config.Connection{ID: "selected-id", URL: "https://example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: backend}}
		if backend == "keychain" {
			c.Store.AccessTokenService, c.Store.PasswordService = "fixture.token", "fixture.password"
		}
		other := config.Connection{ID: "other-id", URL: c.URL, ServerName: c.ServerName, Store: config.CredentialStore{Backend: "file"}}
		if backend == "file" {
			other.Store = config.CredentialStore{Backend: "keychain", AccessTokenService: "other.token", PasswordService: "other.password"}
		}
		registry := config.ConnectionRegistry{SchemaVersion: 1, Current: "other", Profiles: map[string]config.Connection{"selected": c, "other": other}}
		data, err := json.Marshal(registry)
		if err != nil || os.WriteFile(root.ConnectionsFile(), data, 0600) != nil {
			t.Fatal("selected fixture registry unavailable")
		}
		p, err := c.Paths(root, "fixture", false)
		if err != nil {
			t.Fatal("selected fixture paths unavailable")
		}
		b, err := c.Binding("fixture", false)
		if err != nil {
			t.Fatal("selected fixture binding unavailable")
		}
		creds := config.Credentials{UserID: b.UserID, Homeserver: c.URL, ConnectionBinding: &b}
		if backend == "file" {
			creds.AccessToken = "inert-named-fixture"
		}
		if config.PublishBoundCredentials(p, "fixture", false, creds) != nil || config.SaveState(p, "fixture", config.State{NextBatch: "named-fixture"}) != nil {
			t.Fatal("selected fixture publication failed")
		}
		f.paths, f.binding = p, &b
	} else if backend == "keychain" {
		data, err := json.Marshal(config.Credentials{UserID: "@fixture:example.invalid", Homeserver: "https://example.invalid"})
		if err != nil || os.WriteFile(root.AgentFile("fixture"), data, 0600) != nil {
			t.Fatal("legacy fixture metadata unavailable")
		}
	}
	getenv := a.Getenv
	a.Getenv = func(key string) string {
		if key == envSecretStore && !f.named {
			return backend
		}
		return getenv(key)
	}
	a.Stdout, a.Stderr = &f.out, &f.stderr
	a.Now = func() time.Time { f.clockCalls++; return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
	a.keychain = func(p config.Paths) *keychainStore {
		f.constructors++
		return isolatedKeychainStore(keychainStore{paths: p, backend: daemonMigrationCountingBackend{calls: &f.items}, probeCommand: func(ctx context.Context, args ...string) *exec.Cmd {
			f.probes++
			return unlockedProbe(ctx, args...)
		}})
	}
	return f
}

type daemonMigrationCountingBackend struct{ calls *int }

func (b daemonMigrationCountingBackend) find(context.Context, string, string, string) ([]byte, error) {
	*b.calls++
	return []byte("inert-materialized-fixture"), nil
}
func (b daemonMigrationCountingBackend) add(context.Context, string, string, string, []byte) error {
	*b.calls++
	return nil
}
func (b daemonMigrationCountingBackend) update(context.Context, string, string, string, []byte) error {
	*b.calls++
	return nil
}

func (f *daemonSelectedMigrationFixture) args(operation string) []string {
	args := []string{}
	if f.named {
		args = append(args, "--profile", "selected")
	}
	args = append(args, "daemon", "migrate", "fixture", "--to", f.to)
	if operation != "initial" {
		args = append(args, "--"+operation)
	}
	return args
}

// Prepare a genuine lower-level ABSTRACT transaction stopped at its synthetic
// Delete after private copies/journal verification. This is not CLI/native
// migration support. Its source item and pending recovery bytes must survive B.
func (f *daemonSelectedMigrationFixture) pending(t *testing.T) {
	t.Helper()
	f.store.failDelete = true
	err := config.MigrateDaemon(context.Background(), f.paths, "fixture", config.DaemonMigrationOptions{Binding: f.binding, To: f.to, Keychain: true, Secrets: f.store, Now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	state, stateErr := config.LoadState(f.paths, "fixture")
	if !errors.Is(err, config.ErrDaemonMigration) || stateErr != nil || state.Migration == nil || state.Migration.Phase != "in_progress" || !state.Migration.CutoverStarted || f.store.deletes != 1 || !f.store.present {
		t.Fatal("abstract pending fixture did not stop at synthetic Delete")
	}
	for _, name := range []string{"migration.json", "rollback/credential.json", "rollback/source-credential.json"} {
		info, err := os.Stat(filepath.Join(f.to, "identities", "fixture", name))
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() == 0 {
			t.Fatal("abstract pending recovery evidence unavailable")
		}
	}
	f.store.failDelete = false // This injected provider would otherwise succeed.
	f.store.reads, f.store.deletes, f.store.restores = 0, 0, 0
}

// Actual CLI dispatch refuses all six selected-backend routes before any native
// constructor/item/probe, provider, migration clock or filesystem effect. Both
// fresh and real abstract pending fixtures retain all bytes/modes/inodes/items;
// a successful injected store cannot reopen the CLI. No native call is real.
func TestDaemonKeychainMigrationDeferred(t *testing.T) {
	for _, routing := range []string{"legacy", "named"} {
		t.Run(routing, func(t *testing.T) {
			for _, operation := range []string{"initial", "resume", "rollback"} {
				t.Run(operation, func(t *testing.T) {
					for _, state := range []string{"fresh", "pending"} {
						t.Run(state, func(t *testing.T) {
							for _, seam := range []string{"no-secret-store", "injected"} {
								t.Run(seam, func(t *testing.T) {
									f := daemonSelectedMigration(t, routing, "keychain")
									if state == "pending" {
										f.pending(t)
									}
									if seam == "injected" {
										f.app.daemonSecrets = f.store
									}
									source := daemonMigrationSnapshot(t, f.root.Base)
									target := daemonMigrationSnapshot(t, filepath.Dir(f.to))
									code := f.app.Run(context.Background(), f.args(operation))
									if code != ExitError || f.out.Len() != 0 || f.stderr.String() != "a2a: "+errDaemonKeychainMigrationDeferred.Error()+"\n" {
										t.Fatal("selected Keychain migration did not give the exact preflight deferral")
									}
									if f.constructors != 0 {
										t.Fatal("deferred migration reached native constructor")
									}
									if f.items != 0 || f.probes != 0 || f.clockCalls != 0 || f.store.reads != 0 || f.store.deletes != 0 || f.store.restores != 0 || !f.store.present {
										t.Fatal("deferred migration performed native/provider/transaction work")
									}
									assertDaemonMigrationSnapshot(t, f.root.Base, source)
									assertDaemonMigrationSnapshot(t, filepath.Dir(f.to), target)
								})
							}
						})
					}
				})
			}
		})
	}
}

// File CLI initial/resume/rollback remains real on both selected paths, even
// when registry current points at Keychain. A cancelled initial call supplies
// genuine pending-journal recovery beside an ordinary successful initial call.
// These are owned directory fixtures, not native or cross-user qualification.
func TestDaemonFileMigrationRecoveryCLI(t *testing.T) {
	for _, routing := range []string{"legacy", "named"} {
		for _, start := range []string{"clean", "interrupted"} {
			t.Run(routing+"/"+start, func(t *testing.T) {
				f := daemonSelectedMigration(t, routing, "file")
				original, err := os.ReadFile(f.paths.AgentFile("fixture"))
				if err != nil {
					t.Fatal("file source credential unavailable")
				}
				ctx := context.Background()
				if start == "interrupted" {
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					ctx = cancelled
				}
				code := f.app.Run(ctx, f.args("initial"))
				state, err := config.LoadState(f.paths, "fixture")
				want := "done"
				if start == "interrupted" {
					want = "in_progress"
				}
				if err != nil || state.Migration == nil || state.Migration.Phase != want || (code == ExitOK) != (start == "clean") {
					t.Fatal("file initial control did not reach its intended phase")
				}
				if _, err := os.Stat(filepath.Join(f.to, "identities", "fixture", "migration.json")); err != nil {
					t.Fatal("file initial control lacks target journal")
				}
				if _, err := config.LoadCredentials(f.paths, "fixture"); !errors.Is(err, config.ErrIdentityMigrated) {
					t.Fatal("file source admission was not fenced")
				}
				locks := map[string]os.FileInfo{}
				for _, name := range []string{filepath.Join(f.paths.Base, "state", "fixture.runtime-consumer.lock"), filepath.Join(filepath.Dir(f.paths.AgentFile("fixture")), ".fixture.lock")} {
					info, err := os.Stat(name)
					if err != nil {
						t.Fatal("file migration lock unavailable")
					}
					locks[name] = info
				}
				f.out.Reset()
				f.stderr.Reset()
				if f.app.Run(context.Background(), f.args("resume")) != ExitOK {
					t.Fatal("file resume control refused")
				}
				state, err = config.LoadState(f.paths, "fixture")
				if err != nil || state.Migration == nil || state.Migration.Phase != "done" {
					t.Fatal("file resume lost the done fence")
				}
				if f.app.Run(context.Background(), f.args("rollback")) != ExitOK {
					t.Fatal("file rollback control refused")
				}
				restored, err := os.ReadFile(f.paths.AgentFile("fixture"))
				info, statErr := os.Stat(f.paths.AgentFile("fixture"))
				if err != nil || statErr != nil || !bytes.Equal(original, restored) || info.Mode().Perm() != 0600 || config.CheckIdentityMigration(f.paths, "fixture") != nil {
					t.Fatal("file rollback did not restore exact admitted source")
				}
				for name, old := range locks {
					info, err := os.Stat(name)
					if err != nil || !os.SameFile(old, info) {
						t.Fatal("file recovery changed lock identity")
					}
				}
				if f.constructors != 0 || f.items != 0 || f.probes != 0 || f.store.reads != 0 || f.store.deletes != 0 || f.store.restores != 0 {
					t.Fatal("file recovery touched a native provider")
				}
			})
		}
	}
}

// B does not admit malformed arguments, conflicting backend selection or a
// changed named binding. These ordinary guards still refuse without effects;
// the valid selected-Keychain deferral control is the six-route table above.
func TestDaemonMigrationDeferralValidation(t *testing.T) {
	for _, mode := range []string{"conflicting-flags", "named-binding", "unknown-backend", "human"} {
		t.Run(mode, func(t *testing.T) {
			routing := "legacy"
			if mode == "named-binding" {
				routing = "named"
			}
			f := daemonSelectedMigration(t, routing, "keychain")
			args := f.args("initial")
			want := ExitError
			switch mode {
			case "conflicting-flags":
				args = append(args, "--resume", "--rollback")
				want = ExitUsage
			case "named-binding":
				args = append(args, "--connection-fingerprint", strings.Repeat("0", 64))
			case "unknown-backend":
				getenv := f.app.Getenv
				f.app.Getenv = func(key string) string {
					if key == envSecretStore {
						return "unknown"
					}
					return getenv(key)
				}
			case "human":
				f.app.stdinTTY = func() bool { return true }
				args = append(args, "--human", "--as", "fixture")
				want = ExitUsage
			}
			source := daemonMigrationSnapshot(t, f.root.Base)
			target := daemonMigrationSnapshot(t, filepath.Dir(f.to))
			if code := f.app.Run(context.Background(), args); code != want || f.out.Len() != 0 || f.stderr.Len() == 0 || strings.Contains(f.stderr.String(), "keychain_daemon_migration_deferred") {
				t.Fatal("ordinary migration validation was bypassed or reclassified")
			}
			if f.constructors != 0 || f.items != 0 || f.probes != 0 || f.clockCalls != 0 {
				t.Fatal("invalid migration selection performed work")
			}
			assertDaemonMigrationSnapshot(t, f.root.Base, source)
			assertDaemonMigrationSnapshot(t, filepath.Dir(f.to), target)
		})
	}
}
