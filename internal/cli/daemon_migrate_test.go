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

type daemonCLISecretFixture struct{ present bool }

func (s *daemonCLISecretFixture) Read(_ context.Context, c config.Credentials) (config.Credentials, error) {
	if !s.present {
		return c, errors.New("fixture missing")
	}
	c.AccessToken = "inert-materialized-fixture"
	return c, nil
}
func (s *daemonCLISecretFixture) Delete(context.Context, config.Credentials) error {
	s.present = false
	return nil
}
func (s *daemonCLISecretFixture) Restore(_ context.Context, c config.Credentials) error {
	if c.AccessToken != "inert-materialized-fixture" {
		return errors.New("fixture mismatch")
	}
	s.present = true
	return nil
}
func TestDaemonNamedResolverRoundTrip(t *testing.T) {
	for _, backend := range []string{"file", "keychain"} {
		t.Run(backend, func(t *testing.T) {
			a, root, to := daemonCLIFixture(t)
			connection := config.Connection{ID: "fixture-id", URL: "https://example.invalid", ServerName: "example.invalid", Store: config.CredentialStore{Backend: backend}}
			if backend == "keychain" {
				connection.Store.AccessTokenService = "fixture.token"
				connection.Store.PasswordService = "fixture.password"
				a.daemonSecrets = &daemonCLISecretFixture{present: true}
				a.keychain = func(config.Paths) *keychainStore { t.Fatal("real Keychain adapter used"); return nil }
			}
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
