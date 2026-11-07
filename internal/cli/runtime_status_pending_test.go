package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func runtimeStatusBindArgs(n *nativeFixture) []string {
	return []string{"--codex-thread", n.thread.ID, "--endpoint", n.endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "2m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "installed", "--product", "bind-product"}
}

// A seeded fence cleared after refusal allows exactly one bind-profile PUT,
// with no control/state lock blocking its clearer. The no-fence control also
// publishes once. Both cover absent liveness and preservation of manual state.
func TestRuntimeBindStatusPendingRetry(t *testing.T) {
	for _, prior := range []string{"absent", "manual"} {
		for _, mode := range []string{"clear", "none"} {
			t.Run(prior+"/"+mode, func(t *testing.T) {
				e, _, n, generation := runtimeWorld(t)
				if prior == "manual" {
					e.as("alice", append(statusArgs("blocked"), "keep manual detail")...).ok(t)
				} else {
					_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
						st.Runtime, st.Liveness, st.LivenessSource = nil, nil, ""
						return nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				if mode == "clear" {
					setOwnStatusFence(t, e, liveOwnStatusFence())
				}
				before := e.state("alice")
				puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
				refusals := 0
				e.configure = func(a *App) {
					a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
					a.afterOwnStatusPending = func() {
						refusals++
						if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
							t.Fatal("contended bind sent a PUT before its fence cleared")
						}
						err := config.WithRuntimeLock(e.paths(), "alice", "control", time.Now().Add(time.Second), func() error {
							setOwnStatusFence(t, e, nil)
							return nil
						})
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				r := e.as("alice", append([]string{"runtime", "bind"}, runtimeStatusBindArgs(n)...)...).ok(t)
				wantRefusals := 0
				if mode == "clear" {
					wantRefusals = 1
				}
				st := e.state("alice")
				if refusals != wantRefusals || st.RuntimeProfilePending != nil || st.OwnStatusWrite != nil || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts+1 {
					t.Fatalf("bind did not publish once after contention: refusals=%d state=%+v", refusals, st)
				}
				if st.Runtime == nil || st.Runtime.Generation == "" || st.Runtime.HeartbeatEvery != 2*time.Minute || !reflect.DeepEqual(st.Wake, before.Wake) {
					t.Fatal("bind lost binding/journal or heartbeat setting")
				}
				if before.Runtime != nil && st.Runtime.Generation != generation {
					t.Fatal("idempotent bind changed generation")
				}
				if st.Liveness == nil || st.Liveness.Runtime != "codex" || st.Liveness.Hooks != "installed" || st.Liveness.HeartbeatEvery != "2m0s" || st.Liveness.Product != "bind-product" {
					t.Fatalf("bind profile was not acknowledged: %+v", st.Liveness)
				}
				if prior == "manual" {
					if st.Liveness.State != before.Liveness.State || st.Liveness.Detail != before.Liveness.Detail || st.Liveness.Doorbell != before.Liveness.Doorbell || st.LivenessSource != "manual" {
						t.Fatal("bind publication lost manual state/detail/doorbell/source")
					}
				} else if st.Liveness.State != "idle" || st.LivenessSource != "runtime" {
					t.Fatal("initial bind did not create runtime liveness")
				}
				if r.stderr != "" || !strings.Contains(r.stdout, " bound generation="+st.Runtime.Generation) {
					t.Fatalf("successful bind output changed: %+v", r)
				}
				s, err := eApp(e).sessionAs("alice")
				if err != nil {
					t.Fatal(err)
				}
				room, _, err := s.resolveRoom(context.Background(), "status")
				if err != nil {
					t.Fatal(err)
				}
				var published config.AgentStatus
				if err := s.client.GetState(context.Background(), room, StatusEventType, "@alice:test.local", &published); err != nil || !reflect.DeepEqual(published, *st.Liveness) {
					t.Fatalf("remote bind profile differs from acknowledged record: %+v err=%v", published, err)
				}
			})
		}
	}
}

// A fence beyond the bind publication deadline is success with a durable full
// profile and no fabricated ACK. A later consumer (direct or via rebind --watch)
// and immediate --watch both arm with no liveness record. The first unfenced
// heartbeat publishes the full profile, clearing recovery only after the ACK.
func TestRuntimeBindStatusPendingDeadline(t *testing.T) {
	for _, start := range []string{"watch", "later-consumer", "later-bind-watch"} {
		for _, clear := range []string{"before-heartbeat", "after-first-refusal"} {
			t.Run(start+"/"+clear, func(t *testing.T) {
				e, _, n := runtimeUnboundWorld(t)
				e.extra[EnvSessionID] = n.thread.ID
				var err error
				fence := liveOwnStatusFence()
				setOwnStatusFence(t, e, fence)
				puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
				a := eApp(e)
				a.as = "alice"
				a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
				a.runtimeBindStatusBudget = time.Second
				var out, errout bytes.Buffer
				a.Stdout, a.Stderr = &out, &errout
				refusals, heartbeatRefusals := 0, 0
				a.afterOwnStatusPending = func() {
					refusals++
					st := e.state("alice")
					if st.Runtime.ConsumerPID != 0 {
						heartbeatRefusals++
						if st.Liveness != nil || st.RuntimeProfilePending == nil {
							t.Fatal("contended heartbeat fabricated ACK or lost recovery")
						}
						setOwnStatusFence(t, e, nil)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
				defer cancel()
				armed, acknowledged := false, false
				a.Stdout = runtimeStatusWriter(func(p []byte) (int, error) {
					if strings.Contains(string(p), "native consumer armed generation=") {
						armed = true
						st := e.state("alice")
						if st.Runtime.ConsumerPID != os.Getpid() || st.Runtime.ConsumerGeneration != st.Runtime.Generation ||
							st.Liveness != nil || st.RuntimeProfilePending == nil || !reflect.DeepEqual(st.OwnStatusWrite, fence) ||
							e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
							t.Fatal("consumer did not arm with retained fence/profile and no ACK/PUT")
						}
						assertRuntimePendingProfile(t, st)
						if clear == "before-heartbeat" {
							setOwnStatusFence(t, e, nil)
						}
					}
					return out.Write(p)
				})
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut && armed && !acknowledged {
						st := e.state("alice")
						if st.Liveness != nil || st.RuntimeProfilePending == nil {
							t.Fatal("marker cleared or ACK fabricated before publication")
						}
						resp, err := http.DefaultTransport.RoundTrip(r)
						if e.state("alice").RuntimeProfilePending == nil {
							t.Fatal("marker cleared before the response was decoded and committed")
						}
						return resp, err
					}
					if strings.HasSuffix(r.URL.Path, "/sync") && armed {
						st := e.state("alice")
						if st.RuntimeProfilePending == nil {
							if st.Liveness == nil || st.Liveness.Runtime != "codex" || st.Liveness.Hooks != "installed" ||
								st.Liveness.HeartbeatEvery != "2m0s" || st.Liveness.Product != "bind-product" || st.Liveness.Doorbell != "armed" ||
								e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts+1 {
								t.Fatalf("first unfenced heartbeat did not ACK the full bind profile exactly once: %+v", st.Liveness)
							}
							acknowledged = true
							cancel()
							return nil, ctx.Err()
						}
						// A refused heartbeat skips to the next cycle without a sleep.
						r = r.Clone(r.Context())
						u := *r.URL
						r.URL = &u
						q := u.Query()
						q.Set("timeout", "0")
						u.RawQuery = q.Encode()
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
				args := runtimeStatusBindArgs(n)
				if start == "watch" {
					args = append(args, "--watch")
				}
				if err := cmdRuntimeBind(ctx, a, args); err != nil {
					t.Fatalf("pending bind failed: %v", err)
				}
				if start != "watch" {
					st := e.state("alice")
					assertRuntimePendingProfile(t, st)
					if armed || st.Runtime.ConsumerPID != 0 || st.Liveness != nil ||
						!reflect.DeepEqual(st.OwnStatusWrite, fence) || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts {
						t.Fatal("non-watch bind started a consumer, changed fence or fabricated ACK")
					}
					generation := st.Runtime.Generation
					if start == "later-bind-watch" {
						err = cmdRuntimeBind(ctx, a, append(args, "--watch"))
					} else {
						s, sessionErr := a.session()
						if sessionErr != nil {
							t.Fatal(sessionErr)
						}
						err = config.WithRuntimeLock(e.paths(), "alice", "consumer", time.Now().Add(time.Second), func() error {
							return a.runNative(ctx, s, generation)
						})
					}
					if err != nil || e.state("alice").Runtime.Generation != generation {
						t.Fatalf("later consumer failed or changed generation: %v", err)
					}
				}
				warnings := 1
				if start == "later-bind-watch" {
					warnings++
				}
				if !armed || !acknowledged || refusals == 0 || errout.String() != strings.Repeat("liveness not yet published; the first heartbeat republishes it\n", warnings) ||
					!strings.Contains(out.String(), " bound generation=") || (clear == "after-first-refusal" && heartbeatRefusals != 1) {
					t.Fatalf("missing consumer/recovery/closed warning: armed=%t ACK=%t refusals=%d heartbeat=%d stderr=%q", armed, acknowledged, refusals, heartbeatRefusals, errout.String())
				}
				if e.state("alice").RuntimeProfilePending != nil {
					t.Fatal("ACK did not durably clear marker")
				}
				if err := config.WithRuntimeLock(e.paths(), "alice", "consumer", time.Now().Add(time.Second), func() error { return nil }); err != nil {
					t.Fatalf("consumer exit retained lock: %v", err)
				}
			})
		}
	}
}

type runtimeStatusWriter func([]byte) (int, error)

func (f runtimeStatusWriter) Write(p []byte) (int, error) { return f(p) }

func assertRuntimePendingProfile(t *testing.T, st config.State) {
	t.Helper()
	p := st.RuntimeProfilePending
	if p == nil || st.Runtime == nil || p.Generation != st.Runtime.Generation || p.Profile.Runtime != "codex" ||
		p.Profile.Hooks != "installed" || p.Profile.HeartbeatEvery != "2m0s" || p.Profile.Product != "bind-product" {
		t.Fatalf("missing durable generation/full bind profile: %+v", p)
	}
}

// Bind-specific publication failures remain errors (also with --watch): no
// success line, no armed consumer, no fabricated ACK or cleared pending profile.
func TestRuntimeBindStatusPublicationError(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(strconv.FormatBool(watch), func(t *testing.T) {
			e, _, n, generation := runtimeWorld(t)
			before := e.state("alice")
			puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			e.hs.fail("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}", http.StatusForbidden, "M_FORBIDDEN", "bind publication refused")
			a := eApp(e)
			a.as = "alice"
			a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
			var out, errout bytes.Buffer
			a.Stdout, a.Stderr = &out, &errout
			args := runtimeStatusBindArgs(n)
			if watch {
				args = append(args, "--watch")
			}
			err := cmdRuntimeBind(context.Background(), a, args)
			if err == nil || errors.Is(err, ErrOwnStatusPending) || !strings.Contains(err.Error(), "M_FORBIDDEN") || a.fail(err) != ExitError {
				t.Fatalf("real publication failure masked: %v", err)
			}
			st := e.state("alice")
			assertRuntimePendingProfile(t, st)
			if out.Len() != 0 || strings.Contains(errout.String(), "first heartbeat") || st.Runtime.Generation != generation || st.Runtime.ConsumerPID != 0 ||
				!reflect.DeepEqual(st.Liveness, before.Liveness) || !reflect.DeepEqual(st.Wake, before.Wake) || st.OwnStatusWrite != nil ||
				e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts+1 {
				t.Fatal("publication error reported success, armed consumer, changed ACK/journal or lost binding")
			}
		})
	}
}

// Refusal, missing ACK and a different acknowledged profile cannot clear the
// marker. ACKs racing a new generation/profile cannot clear its marker either.
// Each case has a nearby full-profile acknowledged recovery control.
func TestRuntimePendingProfileAcknowledgement(t *testing.T) {
	for _, mode := range []string{"pending", "forbidden", "missing-ack", "different-profile", "new-generation", "new-profile"} {
		t.Run(mode, func(t *testing.T) {
			e, _, _, generation := runtimeWorld(t)
			profile := config.AgentStatus{State: "idle", Runtime: "codex", Hooks: "installed", HeartbeatEvery: "2m0s", Product: "bind-product", Doorbell: "armed"}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Liveness = nil
				st.RuntimeProfilePending = &config.RuntimeProfilePending{Generation: generation, Profile: profile}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a := eApp(e)
			s, err := a.sessionAs("alice")
			if err != nil {
				t.Fatal(err)
			}
			fault := true
			s.client.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
				resp, err := http.DefaultTransport.RoundTrip(r)
				if err != nil || r.Method != http.MethodPut || !fault {
					return resp, err
				}
				switch mode {
				case "forbidden", "missing-ack":
					resp.Body.Close()
					body := "{}"
					if mode == "forbidden" {
						resp.StatusCode = http.StatusForbidden
						body = `{"errcode":"M_FORBIDDEN","error":"refused"}`
					}
					resp.Body = io.NopCloser(strings.NewReader(body))
				case "new-generation", "new-profile":
					_, updateErr := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
						if mode == "new-generation" {
							st.Runtime.Generation = "replacement-generation"
							st.RuntimeProfilePending.Generation = st.Runtime.Generation
						} else {
							st.RuntimeProfilePending.Profile.Product = "replacement-product"
						}
						return nil
					})
					if updateErr != nil {
						t.Fatal(updateErr)
					}
				}
				return resp, nil
			})}
			if mode == "pending" {
				setOwnStatusFence(t, e, liveOwnStatusFence())
			}
			attempt := profile
			if mode == "different-profile" {
				attempt.Product = "old-product"
			}
			err = s.writeLivenessSource(context.Background(), attempt, "runtime")
			switch mode {
			case "pending":
				if !errors.Is(err, ErrOwnStatusPending) {
					t.Fatalf("missing pending refusal: %v", err)
				}
			case "forbidden":
				if err == nil || !strings.Contains(err.Error(), "M_FORBIDDEN") {
					t.Fatalf("missing explicit publication rejection: %v", err)
				}
			case "missing-ack":
				if err == nil || !strings.Contains(err.Error(), "missing required control field") {
					t.Fatalf("missing ACK did not produce its specific rejection: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			st := e.state("alice")
			if st.RuntimeProfilePending == nil {
				t.Fatal("marker cleared without ACK for current full profile/generation")
			}
			if (mode == "pending" || mode == "forbidden" || mode == "missing-ack") && st.Liveness != nil {
				t.Fatal("failure fabricated acknowledged liveness")
			}
			fault = false
			setOwnStatusFence(t, e, nil)
			want := st.RuntimeProfilePending.Profile
			if err := s.writeLivenessSource(context.Background(), want, "runtime"); err != nil {
				t.Fatal(err)
			}
			st = e.state("alice")
			if st.RuntimeProfilePending != nil || st.Liveness == nil || st.Liveness.Runtime != want.Runtime || st.Liveness.Hooks != want.Hooks ||
				st.Liveness.HeartbeatEvery != want.HeartbeatEvery || st.Liveness.Product != want.Product {
				t.Fatal("full-profile ACK failed to clear recovery")
			}
		})
	}
}
