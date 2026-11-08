package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func watchRetryApp(t *testing.T, sync waitRoundTrip) (*App, config.Paths) {
	t.Helper()
	a, p, _, _ := waitRetryApp(t, "seed", sync)
	get := a.Getenv
	a.Getenv = func(k string) string {
		if k == EnvSessionID {
			return "watch-session"
		}
		return get(k)
	}
	return a, p
}

func watchEmpty(t *testing.T, p config.Paths, cursor string) {
	t.Helper()
	waitRetryState(t, p, cursor)
	mb, err := config.OpenMailbox(p, "alice")
	if err != nil || len(mb.Pending()) != 0 || mb.Received("$neighbor") {
		t.Fatalf("failed read changed mailbox/receipts: %v %v", mb, err)
	}
}

// The same invocation survives two safe Sync failures, preserving its cursor
// on every failed attempt, then flushes one shown/unreceived event. No listener,
// wall-clock delay, provider or real account is used; timers run in synctest.
func TestWatchRetryTransientSync(t *testing.T) {
	for _, transient := range []bool{false, true} {
		name := "valid"
		if transient {
			name = "transient"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var p config.Paths
				var at []time.Time
				a, paths := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
					if err := r.Context().Err(); err != nil {
						return nil, err
					}
					watchEmpty(t, p, "seed")
					at = append(at, time.Now())
					if r.URL.Query().Get("since") != "seed" {
						t.Fatal("failed cycle changed sync cursor")
					}
					if transient && len(at) == 1 {
						return nil, waitRefused()
					}
					if transient && len(at) == 2 {
						return waitResponse(r, 503, `{"errcode":"M_UNAVAILABLE"}`)
					}
					return waitResponse(r, 200, waitValidEvent)
				})
				p = paths
				printed := 0
				a.afterPrint = func() error { printed++; cancel(); return nil }
				if code := a.Run(ctx, []string{"--as", "alice", "watch", "--json"}); code != ExitOK {
					t.Fatalf("watch exited before recovery: code=%d stderr=%v", code, a.Stderr)
				}
				want := 1
				if transient {
					want = 3
				}
				if len(at) != want || printed != 1 {
					t.Fatalf("attempts=%d printed=%d want=%d/1", len(at), printed, want)
				}
				if transient && (at[1].Sub(at[0]) != 5*time.Second || at[2].Sub(at[1]) != 30*time.Second) {
					t.Fatalf("wrong retry delays: %v", at)
				}
				if strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 {
					t.Fatalf("delivery not exactly once: %v", a.Stdout)
				}
				waitRetryState(t, p, "delivered")
				mb, err := config.OpenMailbox(p, "alice")
				if err != nil || len(mb.Pending()) != 1 || !mb.Pending()[0].Shown || mb.Received("$neighbor") {
					t.Fatal("delivery lost/receipted event", err)
				}
			})
		})
	}
}

// Watch has no retry deadline: safe reads outlive the initial lease expiry,
// renew the same session, and retain wait's capped backoff before one delivery.
func TestWatchRetryNoDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var at []time.Time
		var leases []config.Lease
		var p config.Paths
		a, paths := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			watchEmpty(t, p, "seed")
			at = append(at, time.Now())
			st, err := config.LoadState(p, "alice")
			if err != nil || st.Lease == nil {
				t.Fatalf("missing retry lease: %v", err)
			}
			leases = append(leases, *st.Lease)
			if len(at) <= 30 {
				return nil, waitRefused()
			}
			return waitResponse(r, 200, waitValidEvent)
		})
		p = paths
		a.leaseTTL = 2 * time.Minute
		a.afterPrint = func() error { cancel(); return nil }
		if code := a.Run(ctx, []string{"--as", "alice", "watch"}); code != ExitOK || len(at) != 31 {
			t.Fatalf("watch acquired an unintended retry bound: code=%d attempts=%d stderr=%v", code, len(at), a.Stderr)
		}
		if at[len(at)-1].Sub(at[0]) <= 2*time.Hour {
			t.Fatal("retry span did not exceed two hours")
		}
		for i := range at {
			if leases[i].Holder != "watch-session" || leases[i].Expires.Sub(at[i]) != 2*time.Minute {
				t.Fatalf("attempt %d did not renew the same lease: %+v", i+1, leases[i])
			}
			if i == 0 {
				continue
			}
			delay := 5 * time.Minute
			if i <= 5 {
				delay = []time.Duration{5, 30, 60, 120, 240}[i-1] * time.Second
			}
			if at[i].Sub(at[i-1]) != delay || !leases[i].Expires.After(leases[i-1].Expires) {
				t.Fatalf("attempt %d delay=%s lease=%+v", i+1, at[i].Sub(at[i-1]), leases[i])
			}
		}
		waitRetryState(t, p, "delivered")
		if strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 {
			t.Fatal("unbounded recovery lost/duplicated delivery")
		}
	})
}

// Auth and permanent refusal stop at the first attempt (including auth inside
// a 503), without changing cursor/mail. The valid control runs the same path
// and proves it can deliver a well-formed response.
func TestWatchRetryPermanentRefusal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		err    error
	}{
		{name: "valid", status: 200, body: waitValidEvent},
		{name: "auth", status: 401, body: `{"errcode":"M_UNKNOWN_TOKEN"}`},
		{name: "auth-on-503", status: 503, body: `{"errcode":"M_UNKNOWN_TOKEN"}`},
		{name: "forbidden", status: 403, body: `{"errcode":"M_FORBIDDEN"}`},
		{name: "configuration", err: errors.New("permanent transport refusal")},
		{name: "malformed", status: 200, body: `{broken`},
		{name: "missing-cursor", status: 200, body: `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
					calls++
					if calls > 1 {
						t.Fatal("nontransient read retried")
					}
					if tc.err != nil {
						return nil, tc.err
					}
					return waitResponse(r, tc.status, tc.body)
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				a.afterPrint = func() error { cancel(); return nil }
				start := time.Now()
				code := a.Run(ctx, []string{"--as", "alice", "watch"})
				if tc.name == "valid" {
					if code != ExitOK || calls != 1 || time.Since(start) != 0 || strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 || a.Stderr.(*bytes.Buffer).Len() != 0 {
						t.Fatalf("valid control: code=%d calls=%d stdout=%v stderr=%v", code, calls, a.Stdout, a.Stderr)
					}
					waitRetryState(t, p, "delivered")
					return
				}
				if code != ExitError || calls != 1 || time.Since(start) != 0 || a.Stdout.(*bytes.Buffer).Len() != 0 || a.Stderr.(*bytes.Buffer).Len() == 0 {
					t.Fatalf("refusal: code=%d calls=%d time=%s stdout=%v stderr=%v", code, calls, time.Since(start), a.Stdout, a.Stderr)
				}
				watchEmpty(t, p, "seed")
			})
		})
	}
}

// A failed post-Sync subscription GET retries only that read, not the fetched
// batch or ingest. Cursor remains seed until the exact retained batch commits.
func TestWatchRetrySubscriptionRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		syncs, subs := 0, 0
		a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			syncs++
			return waitResponse(r, 200, waitValidEvent)
		})
		base := a.HTTP.Transport
		a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
				subs++
				if subs == 2 || subs == 3 {
					watchEmpty(t, p, "seed")
					return nil, waitRefused()
				}
			}
			return base.RoundTrip(r)
		})
		a.afterPrint = func() error { cancel(); return nil }
		start := time.Now()
		if code := a.Run(ctx, []string{"--as", "alice", "watch"}); code != ExitOK || syncs != 1 || subs != 4 || time.Since(start) != 35*time.Second {
			t.Fatalf("subscription retry: code=%d sync=%d subs=%d elapsed=%s stderr=%v", code, syncs, subs, time.Since(start), a.Stderr)
		}
		waitRetryState(t, p, "delivered")
		mb, err := config.OpenMailbox(p, "alice")
		if err != nil || len(mb.Pending()) != 1 || !mb.Pending()[0].Shown || mb.Received("$neighbor") {
			t.Fatal("retained batch not delivered once", err)
		}
	})
}

// A lease replaced during a failed read is refused before the next HTTP
// attempt; retry does not steal it. The own-lease recovery above remains valid.
func TestWatchRetryLeaseLoss(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		var p config.Paths
		var foreign config.Lease
		a, paths := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
			calls++
			foreign = config.Lease{Holder: "foreign-session", Host: machineLabel(), Expires: time.Now().Add(time.Hour).UTC()}
			_, err := config.UpdateState(p, "alice", func(st *config.State) error { st.Lease = &foreign; return nil })
			if err != nil {
				t.Fatal(err)
			}
			return nil, waitRefused()
		})
		p = paths
		start := time.Now()
		code := a.Run(context.Background(), []string{"--as", "alice", "watch"})
		if code != ExitError || calls != 1 || time.Since(start) != 5*time.Second || !strings.Contains(a.Stderr.(*bytes.Buffer).String(), "held by foreign-session") {
			t.Fatalf("lease loss: code=%d calls=%d elapsed=%s stderr=%v", code, calls, time.Since(start), a.Stderr)
		}
		watchEmpty(t, p, "seed")
		st, err := config.LoadState(p, "alice")
		if err != nil || st.Lease == nil || *st.Lease != foreign {
			t.Fatal("foreign lease changed", err)
		}
	})
}

// Cancellation joins the same watch while its virtual backoff timer is pending.
// No wall-clock scheduling assumption or timeout-based success criterion is used.
func TestWatchRetryCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		first := make(chan struct{})
		a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) { calls++; close(first); return nil, waitRefused() })
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		go func() { done <- a.Run(ctx, []string{"--as", "alice", "watch"}) }()
		<-first
		synctest.Wait()
		cancel()
		if code := <-done; code != ExitOK || calls != 1 || a.Stdout.(*bytes.Buffer).Len() != 0 || a.Stderr.(*bytes.Buffer).Len() != 0 {
			t.Fatalf("cancel: code=%d calls=%d stdout=%v stderr=%v", code, calls, a.Stdout, a.Stderr)
		}
		watchEmpty(t, p, "seed")
	})
}

// Transient-shaped subscription PUT or ingest callback failures are not reads:
// never replay the callback/write. Partial append stays unshown behind the
// unchanged cursor and is deduplicated by a later clean invocation.
func TestWatchRetryWriteFences(t *testing.T) {
	for _, phase := range []string{"subscription-put", "before-append", "after-append"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				syncs, gets, puts, callbacks := 0, 0, 0, 0
				a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
					if err := r.Context().Err(); err != nil {
						return nil, err
					}
					syncs++
					return waitResponse(r, 200, waitValidEvent)
				})
				base := a.HTTP.Transport
				a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
						if r.Method == "PUT" {
							puts++
							return waitResponse(r, 503, `{"errcode":"M_UNAVAILABLE"}`)
						}
						if r.Method != http.MethodGet {
							t.Fatalf("unexpected subscription method: %s", r.Method)
						}
						gets++
						if phase == "subscription-put" {
							return waitResponse(r, 200, `{"v":1,"subs":[{"id":"expired","room":"!proj:test.local","created_at":"1970-01-01T00:00:00.000Z","until":"1970-01-02T00:00:00Z"}]}`)
						}
					}
					return base.RoundTrip(r)
				})
				fault := func() error { callbacks++; return waitRefused() }
				if phase == "before-append" {
					a.beforeAppend = fault
				}
				if phase == "after-append" {
					a.afterAppend = fault
				}
				start := time.Now()
				code := a.Run(context.Background(), []string{"--as", "alice", "watch"})
				wantSync, wantGet, wantPut, wantCallback := 1, 2, 0, 1
				if phase == "subscription-put" {
					wantSync, wantGet, wantPut, wantCallback = 0, 1, 1, 0
				}
				if code != ExitError || syncs != wantSync || gets != wantGet || puts != wantPut || callbacks != wantCallback || time.Since(start) != 0 || a.Stdout.(*bytes.Buffer).Len() != 0 {
					t.Fatalf("write replay: phase=%s code=%d sync=%d get=%d put=%d callback=%d elapsed=%s", phase, code, syncs, gets, puts, callbacks, time.Since(start))
				}
				waitRetryState(t, p, "seed")
				mb, err := config.OpenMailbox(p, "alice")
				if err != nil {
					t.Fatal(err)
				}
				wantPending := 0
				if phase == "after-append" {
					wantPending = 1
				}
				if len(mb.Pending()) != wantPending || mb.Received("$neighbor") || wantPending == 1 && mb.Pending()[0].Shown {
					t.Fatal("failed commit crossed output/receipt fence")
				}
				if phase == "subscription-put" {
					phase = "repaired-subscriptions"
				}
				{
					a.beforeAppend = nil
					a.afterAppend = nil
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					a.afterPrint = func() error { cancel(); return nil }
					if code := a.Run(ctx, []string{"--as", "alice", "watch"}); code != ExitOK || strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 {
						t.Fatalf("clean neighbor failed/doubled: %d %v", code, a.Stdout)
					}
					mb, err = config.OpenMailbox(p, "alice")
					if err != nil || len(mb.Pending()) != 1 || !mb.Pending()[0].Shown || mb.Received("$neighbor") {
						t.Fatal("clean recovery lost/doubled mail", err)
					}
				}
			})
		})
	}
}

// Cold-position Sync is a safe read, but the successful response's recovery
// and commit run once. Resolver retries are independently safe and must retain
// the caller's filter; neither test starts a listener or waits on wall time.
func TestWatchRetrySetupReads(t *testing.T) {
	for _, phase := range []string{"cold-sync", "resolve-room"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				syncs, resolves, commits := 0, 0, 0
				a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
					if err := r.Context().Err(); err != nil {
						return nil, err
					}
					syncs++
					if phase == "cold-sync" && syncs == 1 {
						return nil, waitRefused()
					}
					if phase == "cold-sync" && syncs == 2 {
						if r.URL.Query().Get("since") != "" {
							t.Fatal("cold retry advanced cursor")
						}
						return waitResponse(r, 200, `{"next_batch":"positioned"}`)
					}
					body := waitValidEvent
					if phase == "resolve-room" {
						body = strings.Replace(body, `"join":{`, `"join":{"!outside:test.local":{"timeline":{"events":[{"event_id":"$outside","type":"m.room.message","sender":"@bob:test.local","content":{"msgtype":"m.text","body":"@alice:test.local outside"}}]}},`, 1)
					}
					return waitResponse(r, 200, body)
				})
				args := []string{"--as", "alice", "watch"}
				if phase == "cold-sync" {
					if err := config.SaveState(p, "alice", config.State{}); err != nil {
						t.Fatal(err)
					}
				} else {
					args = append(args, "--room", "proj")
					base := a.HTTP.Transport
					a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/directory/room/") {
							resolves++
							if resolves == 1 {
								return nil, waitRefused()
							}
							return waitResponse(r, 200, `{"room_id":"!proj:test.local"}`)
						}
						return base.RoundTrip(r)
					})
				}
				a.beforeDeliver = func() { commits++ }
				a.afterPrint = func() error { cancel(); return nil }
				if code := a.Run(ctx, args); code != ExitOK || strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 {
					t.Fatalf("setup read retry failed: code=%d stderr=%v", code, a.Stderr)
				}
				wantSync, wantResolve, wantCommit := 3, 0, 2
				if phase == "resolve-room" {
					wantSync, wantResolve, wantCommit = 1, 2, 1
				}
				if syncs != wantSync || resolves != wantResolve || commits != wantCommit {
					t.Fatalf("setup replay: sync=%d resolve=%d commit=%d", syncs, resolves, commits)
				}
				waitRetryState(t, p, "delivered")
				if phase == "resolve-room" {
					if strings.Contains(a.Stdout.(*bytes.Buffer).String(), "$outside") {
						t.Fatal("resolver retry widened the room filter")
					}
					mb, err := config.OpenMailbox(p, "alice")
					if err != nil || len(mb.Pending()) != 2 {
						t.Fatalf("room filter control did not retain both relevant events: %v %v", mb, err)
					}
					for _, entry := range mb.Pending() {
						if entry.Shown != (entry.ID == "$neighbor") || mb.Received(entry.ID) {
							t.Fatalf("room filter changed delivery/receipt state: %+v", entry)
						}
					}
				}
			})
		})
	}
}

// Ingest backfill is deliberately NOT retried as a mixed delivery operation,
// matching wait. A transient-looking failed backfill cannot run the commit
// callback or advance/receipt; a later clean invocation is its valid neighbor.
func TestWatchRetryBackfillFence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		syncs, reads, commits := 0, 0, 0
		fail := true
		a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
			if err := r.Context().Err(); err != nil {
				return nil, err
			}
			syncs++
			return waitResponse(r, 200, `{"next_batch":"delivered","rooms":{"join":{"!proj:test.local":{"timeline":{"limited":true,"prev_batch":"page","events":[{"event_id":"$neighbor","type":"m.room.message","sender":"@bob:test.local","content":{"msgtype":"m.text","body":"@alice:test.local recovered"}}]}}}}}`)
		})
		base := a.HTTP.Transport
		a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/messages") {
				reads++
				if fail {
					return nil, waitRefused()
				}
				return waitResponse(r, 200, `{"chunk":[],"end":""}`)
			}
			return base.RoundTrip(r)
		})
		a.beforeDeliver = func() { commits++ }
		code := a.Run(context.Background(), []string{"--as", "alice", "watch"})
		if code != ExitError || syncs != 1 || reads != 1 || commits != 0 || !strings.Contains(a.Stderr.(*bytes.Buffer).String(), "backfill") {
			t.Fatalf("backfill replay: code=%d sync=%d reads=%d commits=%d stderr=%v", code, syncs, reads, commits, a.Stderr)
		}
		watchEmpty(t, p, "seed")
		fail = false
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a.afterPrint = func() error { cancel(); return nil }
		if code := a.Run(ctx, []string{"--as", "alice", "watch"}); code != ExitOK || strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 || commits != 1 {
			t.Fatalf("backfill neighbor failed: code=%d commits=%d stdout=%v stderr=%v", code, commits, a.Stdout, a.Stderr)
		}
		waitRetryState(t, p, "delivered")
	})
}

// SIGINT/SIGTERM cancel watch's context, including while the first room
// resolution is waiting to retry. No sync or startup read may follow it.
func TestWatchRetryResolveCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		resolves, syncs, subs := 0, 0, 0
		first := make(chan struct{})
		a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
			syncs++
			return waitResponse(r, 200, waitValidEvent)
		})
		base := a.HTTP.Transport
		a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/directory/room/") {
				resolves++
				if resolves != 1 {
					t.Fatal("resolver ran after cancellation")
				}
				close(first)
				return nil, waitRefused()
			}
			if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
				subs++
			}
			return base.RoundTrip(r)
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan int, 1)
		start := time.Now()
		go func() { done <- a.Run(ctx, []string{"--as", "alice", "watch", "--room", "proj"}) }()
		<-first
		synctest.Wait()
		cancel()
		if code := <-done; code != ExitOK || resolves != 1 || syncs != 0 || subs != 0 || time.Since(start) != 0 || a.Stdout.(*bytes.Buffer).Len() != 0 || a.Stderr.(*bytes.Buffer).Len() != 0 {
			t.Fatalf("resolver cancel: code=%d resolves=%d sync=%d subs=%d elapsed=%s stdout=%v stderr=%v", code, resolves, syncs, subs, time.Since(start), a.Stdout, a.Stderr)
		}
		watchEmpty(t, p, "seed")
	})
}

// A successful cold Sync consumes 25s of its 30s request budget. Recovery's
// subscription GET gets only the remaining 5s, with no retry even for a known
// transient failure. The valid control proves that remaining time is usable.
func TestWatchRetryColdRecoveryBudget(t *testing.T) {
	for _, outcome := range []string{"valid", "deadline", "transient"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				syncs, gets, puts, commits := 0, 0, 0, 0
				var initialDeadline time.Time
				a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) {
					syncs++
					if syncs == 1 {
						var ok bool
						initialDeadline, ok = r.Context().Deadline()
						if !ok || time.Until(initialDeadline) != 30*time.Second || r.URL.Query().Get("since") != "" {
							t.Fatal("cold sync did not start with its 30s budget and empty cursor")
						}
						time.Sleep(25 * time.Second)
						return waitResponse(r, 200, `{"next_batch":"positioned"}`)
					}
					if syncs != 2 || r.URL.Query().Get("since") != "positioned" {
						t.Fatal("cold read or recovery replayed")
					}
					return waitResponse(r, 200, waitValidEvent)
				})
				if err := config.SaveState(p, "alice", config.State{}); err != nil {
					t.Fatal(err)
				}
				base := a.HTTP.Transport
				a.HTTP.Transport = waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
						if r.Method != http.MethodGet {
							puts++
							t.Fatal("cold recovery wrote subscriptions")
						}
						gets++
						if gets == 2 {
							deadline, ok := r.Context().Deadline()
							if !ok || !deadline.Equal(initialDeadline) || time.Until(deadline) != 5*time.Second {
								t.Fatalf("recovery restarted budget: deadline=%s initial=%s remaining=%s", deadline, initialDeadline, time.Until(deadline))
							}
							switch outcome {
							case "valid":
								time.Sleep(4 * time.Second)
							case "deadline":
								<-r.Context().Done()
								return nil, r.Context().Err()
							case "transient":
								time.Sleep(time.Second)
								return nil, waitRefused()
							}
						}
					}
					return base.RoundTrip(r)
				})
				a.beforeDeliver = func() { commits++ }
				a.afterPrint = func() error { cancel(); return nil }
				start := time.Now()
				code := a.Run(ctx, []string{"--as", "alice", "watch"})
				if outcome == "valid" {
					if code != ExitOK || syncs != 2 || gets != 3 || puts != 0 || commits != 2 || time.Since(start) != 29*time.Second || strings.Count(a.Stdout.(*bytes.Buffer).String(), "$neighbor") != 1 || a.Stderr.(*bytes.Buffer).Len() != 0 {
						t.Fatalf("valid cold recovery: code=%d sync=%d get=%d put=%d commit=%d elapsed=%s stdout=%v stderr=%v", code, syncs, gets, puts, commits, time.Since(start), a.Stdout, a.Stderr)
					}
					waitRetryState(t, p, "delivered")
					return
				}
				wantElapsed := 30 * time.Second
				if outcome == "transient" {
					wantElapsed = 26 * time.Second
				}
				if code != ExitError || syncs != 1 || gets != 2 || puts != 0 || commits != 0 || time.Since(start) != wantElapsed || a.Stdout.(*bytes.Buffer).Len() != 0 || a.Stderr.(*bytes.Buffer).Len() == 0 {
					t.Fatalf("cold recovery replay: code=%d sync=%d get=%d put=%d commit=%d elapsed=%s stdout=%v stderr=%v", code, syncs, gets, puts, commits, time.Since(start), a.Stdout, a.Stderr)
				}
				watchEmpty(t, p, "")
			})
		})
	}
}
