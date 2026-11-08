package cli

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Long consumers ignore native IDs, with adjacent short-command sharing controls;
// explicit hook/A2A ownership wins in both scopes and unknown birth stays conservative.
func TestLongConsumerLeaseHolder(t *testing.T) {
	for _, native := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		for _, tc := range []struct {
			name, hook, explicit, want string
			long, unread               bool
		}{
			{name: "detached", long: true},
			{name: "short control", want: "native-session"},
			{name: "explicit detached", long: true, explicit: "explicit-session", want: "explicit-session"},
			{name: "explicit short control", explicit: "explicit-session", want: "explicit-session"},
			{name: "hook detached", long: true, hook: "hook-session", explicit: "explicit-session", want: "hook-session"},
			{name: "hook short control", hook: "hook-session", explicit: "explicit-session", want: "hook-session"},
			{name: "unknown birth", long: true, unread: true},
			{name: "known birth control", long: true},
		} {
			t.Run(native+"/"+tc.name, func(t *testing.T) {
				a := leaseApp(func(pid int) (int64, error) {
					if pid != os.Getpid() {
						t.Fatalf("birth PID = %d", pid)
					}
					if tc.unread {
						return 0, errors.New("synthetic unavailable birth")
					}
					return 42, nil
				})
				a.Getenv = func(k string) string {
					if k == native {
						return "native-session"
					}
					if k == EnvSessionID {
						return tc.explicit
					}
					return ""
				}
				want := tc.want
				if want == "" {
					want = "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel()
					if !tc.unread {
						want += "#42"
					}
				}
				if got := a.leaseHolderFor(config.Paths{}, tc.hook, time.Time{}, tc.long); got != want {
					t.Fatalf("holder = %q, want %q", got, want)
				}
			})
		}
	}
}

// Real command dispatch and every retry/renewal retain process scope for watch
// and unbounded wait. Finite wait/inbox and explicit ownership are valid neighbors.
func TestLongConsumerLeaseCommands(t *testing.T) {
	for _, native := range []string{"CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID"} {
		for _, tc := range []struct {
			name     string
			args     []string
			process  bool
			explicit string
		}{
			{"watch", []string{"watch"}, true, ""},
			{"inbox control", []string{"inbox"}, false, ""},
			{"unbounded wait", []string{"wait", "--timeout", "0"}, true, ""},
			{"finite wait control", []string{"wait", "--timeout", "1m"}, false, ""},
			{"explicit watch control", []string{"watch"}, false, "explicit-session"},
			{"explicit unbounded control", []string{"wait", "--timeout", "0"}, false, "explicit-session"},
		} {
			t.Run(native+"/"+tc.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					var p config.Paths
					calls := 0
					want := "native-session"
					if tc.explicit != "" {
						want = tc.explicit
					} else if tc.process {
						want = "pid:" + strconv.Itoa(os.Getpid()) + "@" + machineLabel() + "#42"
					}
					a, paths, _, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
						calls++
						st, err := config.LoadState(p, "alice")
						if err != nil || st.Lease == nil || st.Lease.Holder != want {
							t.Fatalf("poll %d lease = %+v, err %v; want %q", calls, st.Lease, err, want)
						}
						if tc.args[0] != "inbox" && calls == 1 {
							return nil, waitRefused() // Virtual backoff must renew with the same scope.
						}
						if tc.args[0] == "watch" && calls == 3 {
							cancel()
							return nil, ctx.Err()
						}
						return waitResponse(r, 200, waitValidEvent)
					})
					p = paths
					base := a.Getenv
					a.Getenv = func(k string) string {
						if k == native {
							return "native-session"
						}
						if k == EnvSessionID {
							return tc.explicit
						}
						return base(k)
					}
					a.leaseBirth = ctxAware(func(int) (int64, error) { return 42, nil })
					args := append([]string{"--as", "alice"}, tc.args...)
					if code := a.Run(ctx, args); code != ExitOK {
						t.Fatalf("command code = %d: %s", code, errb)
					}
					wantCalls := 2
					if tc.args[0] == "inbox" {
						wantCalls = 1
					} else if tc.args[0] == "watch" {
						wantCalls = 3
					}
					if calls != wantCalls {
						t.Fatalf("sync calls = %d, want %d", calls, wantCalls)
					}
				})
			})
		}
	}
}

// A new same-host Claude session can acquire from a detached process lease;
// subsequent watcher renewal borrows it without prolonging that session's TTL.
// Explicit watcher ownership is the adjacent refusal control and is not bypassed.
func TestLongConsumerLeaseSessionHandoff(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(strconv.FormatBool(explicit), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var p config.Paths
				var a *App
				calls := 0
				var after config.Lease
				app, paths, _, errb := waitRetryApp(t, "seed", func(r *http.Request) (*http.Response, error) {
					calls++
					if calls == 1 {
						// Independent command environment, same synthetic identity storage.
						next := NewApp()
						next.Stdout, next.Stderr = a.Stdout, a.Stderr
						next.Getenv = func(k string) string {
							if k == "CLAUDE_CODE_SESSION_ID" {
								return "new-session"
							}
							if k == config.EnvConfigDir {
								return p.Base
							}
							return ""
						}
						next.leaseBirth = a.leaseBirth
						s, err := next.sessionAs("alice")
						if err != nil {
							t.Fatal(err)
						}
						err = next.acquireLease(s, "", time.Time{})
						var held *config.LeaseHeldError
						if explicit && !errors.As(err, &held) || !explicit && err != nil {
							t.Fatalf("explicit=%v new session acquisition = %v", explicit, err)
						}
						st, err := config.LoadState(p, "alice")
						if err != nil || st.Lease == nil {
							t.Fatalf("state = %+v, %v", st, err)
						}
						after = *st.Lease
						want := "new-session"
						if explicit {
							want = "old-session"
						}
						if after.Holder != want {
							t.Fatalf("holder = %q, want %q", after.Holder, want)
						}
						return nil, waitRefused() // Advances virtual time across a renewal.
					}
					st, err := config.LoadState(p, "alice")
					if err != nil || st.Lease == nil || st.Lease.Holder != after.Holder {
						t.Fatalf("renewal changed holder: %+v, %v", st.Lease, err)
					}
					if !explicit && *st.Lease != after {
						t.Fatalf("detached watcher renewed the native session lease: %+v -> %+v", after, st.Lease)
					}
					if explicit && !st.Lease.Expires.After(after.Expires) {
						t.Fatal("explicit owner failed to renew its lease")
					}
					cancel()
					return nil, ctx.Err()
				})
				a, p = app, paths
				base := a.Getenv
				a.Getenv = func(k string) string {
					if k == "CLAUDE_CODE_SESSION_ID" || explicit && k == EnvSessionID {
						return "old-session"
					}
					return base(k)
				}
				a.leaseBirth = ctxAware(func(int) (int64, error) { return 42, nil })
				if code := a.Run(ctx, []string{"--as", "alice", "watch"}); code != ExitOK || calls != 2 {
					t.Fatalf("watch code=%d polls=%d: %s", code, calls, errb)
				}
			})
		})
	}
}

// Synthetic local process lifetime distinguishes a live matching PID from exit
// or confirmed reuse. Unknown/legacy/foreign holders retain conservative refusal;
// every refusal has an adjacent successful recovery or same-host control.
func TestLongConsumerLeasePIDLifetime(t *testing.T) {
	const pid = 4242
	const born = int64(100)
	now := time.Unix(1000, 0)
	host := machineLabel()
	for _, tc := range []struct {
		name, host, suffix string
		gone, unread, ok   bool
		birth              int64
	}{
		{name: "live hostless refuses", suffix: "#100", birth: born},
		{name: "exited hostless control", suffix: "#100", gone: true, ok: true},
		{name: "unknown birth refuses", suffix: "#100", unread: true},
		{name: "reused birth control", suffix: "#100", birth: born + 1, ok: true},
		{name: "legacy live refuses", birth: born + 1},
		{name: "legacy exit control", gone: true, ok: true},
		{name: "live same host control", host: host, suffix: "#100", birth: born, ok: true},
		{name: "foreign host refuses", host: "foreign-host", suffix: "#100", gone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := leaseApp(func(got int) (int64, error) {
				if got != pid {
					t.Fatalf("birth PID = %d", got)
				}
				if tc.unread {
					return 0, errors.New("synthetic unreadable birth")
				}
				return tc.birth, nil
			})
			a.leaseProcessGone = func(got int) bool {
				if got != pid {
					t.Fatalf("liveness PID = %d", got)
				}
				return tc.gone
			}
			holderHost := host
			if tc.host == "foreign-host" {
				holderHost = tc.host
			}
			st := config.State{Lease: &config.Lease{Holder: "pid:4242@" + holderHost + tc.suffix, Host: tc.host, PID: pid, Expires: now.Add(time.Minute)}}
			before := *st.Lease
			err := st.Acquire("alice", "new-session", host, 7, now, config.LeaseTTL, a.leaseDead(config.Paths{}))
			if tc.ok {
				if err != nil || st.Lease.Holder != "new-session" {
					t.Fatalf("recovery = %v, lease %+v", err, st.Lease)
				}
			} else {
				var held *config.LeaseHeldError
				if !errors.As(err, &held) || *st.Lease != before {
					t.Fatalf("live exclusion weakened: %v, lease %+v", err, st.Lease)
				}
			}
		})
	}
}
