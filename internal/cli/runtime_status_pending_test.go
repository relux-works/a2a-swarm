package cli

import (
	"bytes"
	"context"
	"errors"
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
				if refusals != wantRefusals || st.OwnStatusWrite != nil || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts+1 {
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

// A fence beyond the caller deadline returns the pending sentinel and a closed
// nonzero diagnostic, sends no PUT and preserves status/fence/journal. Both
// bind forms keep the successful binding; --watch exits before consumer start
// and releases its consumer lock so the launcher can retry the same generation.
func TestRuntimeBindStatusPendingDeadline(t *testing.T) {
	for _, watch := range []bool{false, true} {
		t.Run(strconv.FormatBool(watch), func(t *testing.T) {
			e, _, n, generation := runtimeWorld(t)
			if !watch {
				_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Runtime, st.Liveness, st.LivenessSource = nil, nil, ""
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			setOwnStatusFence(t, e, liveOwnStatusFence())
			before := e.state("alice")
			puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			a := eApp(e)
			a.as = "alice"
			a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
			var out, errout bytes.Buffer
			a.Stdout, a.Stderr = &out, &errout
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			refusals := 0
			a.afterOwnStatusPending = func() { refusals++ }
			args := runtimeStatusBindArgs(n)
			if watch {
				args = append(args, "--watch")
			}
			err := cmdRuntimeBind(ctx, a, args)
			if !errors.Is(err, ErrOwnStatusPending) || a.fail(err) != ExitError || refusals == 0 {
				t.Fatalf("deadline lost pending refusal/exit code: refusals=%d err=%v", refusals, err)
			}
			if out.Len() != 0 || errout.String() != "a2a: runtime binding kept; liveness not published: own status publication already pending\n" {
				t.Fatalf("deadline claimed success or emitted an open diagnostic: stdout=%q stderr=%q", out.String(), errout.String())
			}
			if deadline, _ := ctx.Deadline(); time.Now().Before(deadline) {
				t.Fatal("pending refusal returned before caller deadline")
			}
			st := e.state("alice")
			if st.Runtime == nil || st.Runtime.Generation == "" || st.Runtime.HeartbeatEvery != 2*time.Minute || !reflect.DeepEqual(st.Wake, before.Wake) {
				t.Fatal("publication refusal undid binding or changed generation/journal")
			}
			if before.Runtime != nil && st.Runtime.Generation != generation {
				t.Fatal("publication refusal changed existing generation")
			}
			if !reflect.DeepEqual(st.Liveness, before.Liveness) || st.LivenessSource != before.LivenessSource || !reflect.DeepEqual(st.OwnStatusWrite, before.OwnStatusWrite) || !reflect.DeepEqual(st.NoticeRetry, before.NoticeRetry) || st.NoticeWriteUncertain != before.NoticeWriteUncertain || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != puts || len(n.wakes) != 0 {
				t.Fatal("deadline changed status/fence/retry state, published or woke the runtime")
			}
			if err := config.WithRuntimeLock(e.paths(), "alice", "consumer", time.Now().Add(time.Second), func() error { return nil }); err != nil {
				t.Fatalf("deadline retained consumer lock: %v", err)
			}
		})
	}
}
