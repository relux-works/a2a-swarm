//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
)

// Default and finite waits retain runtime session sharing only after the raw
// consumer fence admits them. Start without a lease so a PID fallback cannot
// pass by borrowing an existing owner's lease, including after a clean stop.
func TestCourierDefaultWaitOwner(t *testing.T) {
	for _, native := range []string{"CODEX_THREAD_ID", "CLAUDE_CODE_SESSION_ID"} {
		for _, tc := range []struct {
			name string
			args []string
		}{
			{"default", []string{"wait"}}, {"finite", []string{"wait", "--timeout", "2s"}},
		} {
			t.Run(native+"/"+tc.name, func(t *testing.T) {
				const owner = "wait-owner"
				seen := make(chan string, 1)
				f := newCourierFixtureHandler(t, func(next courier.Handler) courier.Handler {
					return func(ctx context.Context, inv courier.Invocation) int {
						if inv.Request.Argv[0] == "wait" {
							select {
							case seen <- inv.Request.Session:
							case <-ctx.Done():
								return 1
							}
						}
						return next(ctx, inv)
					}
				})
				run := func(args ...string) result {
					var out, diag bytes.Buffer
					a := f.app("alice", "", &out, &diag)
					base := a.Getenv
					a.Getenv = func(k string) string {
						if k == native {
							return owner
						}
						return base(k)
					}
					ctx, cancel := context.WithTimeout(context.Background(), courier.LockReleaseBound)
					defer cancel()
					code := a.Run(ctx, args)
					return result{code: code, stdout: out.String(), stderr: diag.String()}
				}
				claim := func(t *testing.T) {
					t.Helper()
					select {
					case got := <-seen:
						if got != owner {
							t.Error("station wait lost native owner session")
						}
					case <-f.done:
						t.Fatal("daemon stopped")
					case <-time.After(courier.LockReleaseBound):
						t.Fatal("wait request not observed")
					}
				}
				consumerFree := func(t *testing.T) {
					t.Helper()
					release, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", time.Now())
					if err != nil {
						t.Fatal("wait retained the consumer lock after exit", err)
					}
					release()
				}
				waitDelivery := func(t *testing.T, id string) {
					t.Helper()
					before, err := config.LoadState(f.paths["alice"], "alice")
					if err != nil || before.Lease != nil {
						t.Fatal("wait witness must start without a lease", err)
					}
					r := run(tc.args...)
					claim(t)
					if r.code != 0 || strings.Count(r.stdout, id) != 1 || r.stderr != shownHint+"\n" || courierReceiptCount(t, f, "alice", id) != 0 {
						t.Fatal("native owner wait did not mark shown without receipt", r)
					}
					after, err := config.LoadState(f.paths["alice"], "alice")
					if err != nil || after.Lease == nil || after.Lease.Holder != owner {
						t.Error("wait did not persist the native runtime session lease")
					}
					consumerFree(t)
					r = run("inbox")
					if r.code != 0 || r.stderr != "" || strings.Count(r.stdout, id) != 1 || courierReceiptCount(t, f, "alice", id) != 1 {
						t.Fatal("inbox receipt neighbor", r)
					}
				}
				t.Run("no-monitor", func(t *testing.T) {
					st, err := config.LoadState(f.paths["alice"], "alice")
					if err != nil || st.Monitor != nil {
						t.Fatal("no-monitor prerequisite", err)
					}
					id := f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(1))
					waitDelivery(t, id)
				})
				var pending string
				// Keep generation setup outside the phase subtest so selecting
				// only stopped-monitor still exercises a real clean stop.
				courierOwnerMonitor(t, f, "alice", owner, func() {
					pending = f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(1))
					t.Run("live-monitor", func(t *testing.T) {
						before, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
						if err != nil {
							t.Fatal(err)
						}
						unchanged := func() {
							t.Helper()
							after, err := os.ReadFile(f.paths["alice"].StateFile("alice"))
							if err != nil || !bytes.Equal(before, after) || f.received("alice", pending) {
								t.Fatal("refused wait changed State/lease or receipted mail", err)
							}
						}
						r := run(tc.args...)
						claim(t)
						if r.code != ExitError || r.stdout != "" || r.stderr != "a2a: courier: courier_lease_conflict\n" {
							t.Fatal("active monitor station refusal", r)
						}
						unchanged()
						ctx, cancel := context.WithTimeout(context.Background(), courier.LockReleaseBound)
						defer cancel()
						var out, diag bytes.Buffer
						engine := &App{as: "alice", courierEngine: true, ctx: ctx, Stdout: &out, Stderr: &diag, HTTP: f.daemon.HTTP, Now: time.Now, connectionSecretStore: f.store, Getenv: func(k string) string {
							switch k {
							case config.EnvConfigDir:
								return filepath.Join(f.home, "identities", "alice")
							case EnvSessionID:
								return owner
							}
							return ""
						}}
						if err := cmdWait(ctx, engine, tc.args[1:]); err != config.ErrMonitorConflict || out.Len() != 0 || diag.Len() != 0 {
							t.Fatal("active monitor engine refusal must be ErrMonitorConflict", err)
						}
						unchanged()
					})
				})
				// The legitimate monitor held this lock during both refusals.
				// Once it closes, no refused waiter may retain another hold.
				consumerFree(t)
				t.Run("stopped-monitor", func(t *testing.T) {
					st, err := config.UpdateState(f.paths["alice"], "alice", func(st *config.State) error {
						st.Lease = nil
						return nil
					})
					if err != nil || st.Monitor == nil || st.Monitor.Phase != "stopped" || pending == "" {
						t.Fatal("clean-stopped monitor prerequisite", err)
					}
					waitDelivery(t, pending)
				})
			})
		}
	}
}

func TestCourierLeadingHumanHook(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix []string
	}{
		{"first", []string{"--human"}},
		{"after-selector", []string{"--as=identity", "--human"}},
		{"before-selectors", []string{"--human", "--profile=p", "--as", "identity"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newC5Station(t, func(_ context.Context, inv courier.Invocation) int {
				if string(inv.Request.Stdin) == "ok" {
					return 0
				}
				return 1
			})
			r := s.run(context.Background(), "bad", append(append([]string{}, tc.prefix...), "hook", "prompt")...)
			data, err := os.ReadFile(filepath.Join(s.home, "state", hookFailureLog))
			if r.code != 0 || r.stdout != "" || err != nil || bytes.Count(data, []byte("\n")) != 1 || !bytes.HasSuffix(data, []byte(" UserPromptSubmit not_allowed\n")) {
				t.Fatal("leading human hook did not fail open with one exact log", r, err)
			}
			r = s.run(context.Background(), "ok", "hook", "prompt")
			if r.code != 0 || r.stdout != "" || !strings.HasPrefix(r.stderr, "courier: 1 hook deliveries failed open since ") || strings.Count(r.stderr, "\n") != 1 {
				t.Fatal("leading human hook valid neighbor", r)
			}
			if _, err := courier.Authorize([]string{"--human", "hook", "prompt"}, "identity"); err == nil || err.Error() != "courier: refused" {
				t.Fatal("leading human authorization", err)
			}
			if _, err := courier.Authorize([]string{"hook", "--", "--human", "prompt"}, "identity"); err == nil || err.Error() != "courier: refused" {
				t.Fatal("literal human operand manufactured a hook", err)
			}
		})
	}
}

func TestCourierSessionEndSuccess(t *testing.T) {
	f := newCourierFixture(t)
	if _, err := config.UpdateState(f.paths["alice"], "alice", func(s *config.State) error { s.HooksPaused = true; return nil }); err != nil {
		t.Fatal(err)
	}
	start := `{"session_id":"ending-owner","hook_event_name":"SessionStart"}`
	end := `{"session_id":"ending-owner","hook_event_name":"SessionEnd"}`
	for _, tc := range []struct{ arg, input string }{{"session-start", start}, {"session-end", end}} {
		r := f.run("alice", tc.input, "--as", "alice", "hook", tc.arg, "--runtime", "muse")
		if r.code != 0 || r.stdout != "" || r.stderr != "" {
			t.Fatal("owner lifecycle hook refused", tc.arg, r)
		}
	}
	st, err := config.LoadState(f.paths["alice"], "alice")
	if err != nil || st.MonitorOwner != nil {
		t.Fatal("session end did not clear the owner", err)
	}
	log := filepath.Join(f.stations["alice"], "state", hookFailureLog)
	data, err := os.ReadFile(log)
	if !errors.Is(err, os.ErrNotExist) && (err != nil || len(data) != 0) {
		t.Fatal("successful session end recorded a failure/gap", err)
	}
	r := f.run("alice", end, "--as", "alice", "hook", "session-finish", "--runtime", "muse")
	data, err = os.ReadFile(log)
	if r.code != 0 || r.stdout != "" || err != nil || bytes.Count(data, []byte("\n")) != 1 || !bytes.HasSuffix(data, []byte(" Hook not_allowed\n")) {
		t.Fatal("unknown lifecycle event refusal", r, err)
	}
}
