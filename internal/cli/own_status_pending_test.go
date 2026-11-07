package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func setOwnStatusFence(t *testing.T, e *env, fence *config.OwnStatusWrite) {
	t.Helper()
	_, err := config.UpdateStateBy(e.paths(), "alice", time.Now().Add(testDeadline), func(st *config.State) error {
		st.OwnStatusWrite = fence
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func liveOwnStatusFence() *config.OwnStatusWrite {
	return &config.OwnStatusWrite{Token: "seeded-live-writer", Until: time.Now().Add(2 * testDeadline)}
}

// A seeded live fence refuses before prepare, finish or HTTP, with errors.Is;
// clearing it permits the same writer without changing its requested record.
func TestOwnStatusPendingSentinel(t *testing.T) {
	e, _ := world(t)
	a := eApp(e)
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	fence := liveOwnStatusFence()
	setOwnStatusFence(t, e, fence)
	before := e.state("alice")
	requests := e.hs.CallCount("GET /_matrix/client/v3/directory/room/{alias}")
	prepared, finished := false, false
	write := func() error {
		return s.writeOwnStatus(context.Background(), func(*config.State) (config.AgentStatus, bool, error) {
			prepared = true
			return config.AgentStatus{}, false, nil
		}, func(*config.State, error) { finished = true })
	}
	if err = write(); !errors.Is(err, ErrOwnStatusPending) || err.Error() != "own status publication already pending" {
		t.Fatalf("local refusal lost sentinel/text: %v", err)
	}
	if prepared {
		t.Error("pending refusal called prepare")
	}
	if finished {
		t.Error("pending refusal called finish")
	}
	if after := e.state("alice"); !reflect.DeepEqual(before, after) {
		t.Errorf("pending refusal mutated state: before=%+v after=%+v", before, after)
	}
	if got := e.hs.CallCount("GET /_matrix/client/v3/directory/room/{alias}"); got != requests {
		t.Errorf("pending refusal resolved a room: requests=%d, want %d", got, requests)
	}
	setOwnStatusFence(t, e, nil)
	if err = write(); err != nil || !prepared || finished {
		t.Fatalf("unfenced no-op changed: prepared=%t finished=%t err=%v", prepared, finished, err)
	}
}

// Explicit status waits for a seeded fence without locking out its clearer;
// an expired crashed-writer fence and an absent fence both allow publication.
func TestStatusPendingRetry(t *testing.T) {
	for _, mode := range []string{"clear", "expired", "none"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "status", "init").ok(t)
			var refused atomic.Int32
			if mode != "none" {
				setOwnStatusFence(t, e, liveOwnStatusFence())
			}
			e.configure = func(a *App) {
				a.afterOwnStatusPending = func() {
					refused.Add(1)
					if mode == "expired" {
						setOwnStatusFence(t, e, &config.OwnStatusWrite{Token: "crashed", Until: time.Now().Add(-time.Second)})
					} else {
						setOwnStatusFence(t, e, nil)
					}
				}
			}
			e.as("alice", statusArgs("blocked")...).ok(t)
			st := e.state("alice")
			if st.Liveness == nil || st.Liveness.State != "blocked" || st.LivenessSource != "manual" || st.OwnStatusWrite != nil {
				t.Fatalf("explicit status did not publish: %+v", st)
			}
			want := int32(1)
			if mode == "none" {
				want = 0
			}
			if refused.Load() != want {
				t.Fatalf("pending attempts=%d, want %d", refused.Load(), want)
			}
		})
	}
}

// A fence beyond the explicit caller's deadline returns the original sentinel
// text, sends no PUT and preserves both acknowledged state and the live fence.
func TestStatusPendingDeadline(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	setOwnStatusFence(t, e, liveOwnStatusFence())
	before := e.state("alice")
	puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
	a := eApp(e)
	a.as = "alice"
	a.Stdout, a.Stderr = io.Discard, io.Discard
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var refused atomic.Int32
	a.afterOwnStatusPending = func() { refused.Add(1) }
	err := cmdStatusRecord(ctx, a, statusArgs("blocked")[2:])
	if !errors.Is(err, ErrOwnStatusPending) || err.Error() != "own status publication already pending" || a.fail(err) != ExitError {
		t.Fatalf("deadline lost sentinel identity/text/exit code: %v", err)
	}
	if deadline, _ := ctx.Deadline(); time.Now().Before(deadline) {
		t.Errorf("deadline refusal returned before caller deadline: %s", deadline)
	}
	if refused.Load() == 0 {
		t.Error("deadline caller observed no pending refusal")
	}
	if after := e.state("alice"); !reflect.DeepEqual(before, after) {
		t.Errorf("deadline refusal changed local state: before=%+v after=%+v", before, after)
	}
	if got := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}"); got != puts {
		t.Errorf("deadline refusal sent a PUT: count=%d, want %d", got, puts)
	}
}

// Model the interval after the deadline has elapsed but before the context's
// timer fires; Done and Err deliberately retain their parent's state.
type ownStatusDelayedDeadlineContext struct {
	context.Context
	deadline time.Time
}

func (c ownStatusDelayedDeadlineContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

// A lock or child-context deadline after a pending refusal retains the sentinel
// before the caller's timer fires, provided preparation never began.
func TestOwnStatusPendingEarlyDeadline(t *testing.T) {
	for _, failure := range []error{config.ErrLockTimeout, fmt.Errorf("write context: %w", context.DeadlineExceeded)} {
		t.Run(failure.Error(), func(t *testing.T) {
			ctx := ownStatusDelayedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(-time.Hour)}
			calls := 0
			err := retryOwnStatusAttempts(ctx, func(ctx context.Context) (bool, error) {
				calls++
				if ctx.Err() != nil {
					t.Fatalf("caller timer fired: %v", ctx.Err())
				}
				if calls == 1 {
					return false, ErrOwnStatusPending
				}
				return false, failure
			})
			if err != ErrOwnStatusPending || calls != 2 {
				t.Fatalf("early deadline lost pending sentinel: calls=%d err=%v", calls, err)
			}
		})
	}
}

// Prepared attempts retain their exact deadline error (and publication
// uncertainty remains owned by writeOwnStatus); other errors, unexpired or
// absent deadlines, and attempts with no prior refusal are never masked.
func TestOwnStatusPendingDeadlinePreservesErrors(t *testing.T) {
	expired := ownStatusDelayedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(-time.Hour)}
	future := ownStatusDelayedDeadlineContext{Context: context.Background(), deadline: time.Now().Add(time.Hour)}
	failure := errors.New("unrelated failure")
	publicationTimeout := fmt.Errorf("publication: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name     string
		ctx      context.Context
		pending  bool
		prepared bool
		failure  error
	}{
		{"prepared lock timeout", expired, true, true, config.ErrLockTimeout},
		{"prepared publication timeout", expired, true, true, publicationTimeout},
		{"no prior refusal", expired, false, false, config.ErrLockTimeout},
		{"unexpired caller deadline", future, true, false, config.ErrLockTimeout},
		{"no caller deadline", context.Background(), true, false, context.DeadlineExceeded},
		{"cancellation", expired, true, false, context.Canceled},
		{"unrelated failure", expired, true, false, failure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryOwnStatusAttempts(tc.ctx, func(context.Context) (bool, error) {
				calls++
				if tc.pending && calls == 1 {
					return false, ErrOwnStatusPending
				}
				return tc.prepared, tc.failure
			})
			wantCalls := 1
			if tc.pending {
				wantCalls++
			}
			if err != tc.failure || calls != wantCalls {
				t.Fatalf("error retried or masked: calls=%d want=%d err=%v want=%v", calls, wantCalls, err, tc.failure)
			}
		})
	}
}

// Cancellation while polling remains cancellation, rather than being reported
// as a contention deadline; unrelated errors are never retried or hidden.
func TestOwnStatusPendingRetryStops(t *testing.T) {
	e, _ := world(t)
	a := eApp(e)
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	setOwnStatusFence(t, e, liveOwnStatusFence())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.afterOwnStatusPending = cancel
	err = s.retryOwnStatus(ctx, func(*config.State) (config.AgentStatus, bool, error) {
		t.Fatal("live fence allowed preparation")
		return config.AgentStatus{}, false, nil
	}, nil)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrOwnStatusPending) {
		t.Fatalf("cancellation became a pending refusal: %v", err)
	}
	failure := errors.New("unrelated publication failure")
	calls := 0
	setOwnStatusFence(t, e, nil)
	err = s.retryOwnStatus(context.Background(), func(*config.State) (config.AgentStatus, bool, error) {
		calls++
		return config.AgentStatus{}, false, failure
	}, nil)
	if !errors.Is(err, failure) || calls != 1 {
		t.Fatalf("unrelated failure retried/hidden: calls=%d err=%v", calls, err)
	}
}

// Automatic notice/hook writers skip a seeded fence without HTTP, heartbeat or
// bounded retry changes; a due publication succeeds after it clears.
func TestAutomaticStatusPendingPreservesRetry(t *testing.T) {
	for _, writer := range []string{"notice", "hook", "heartbeat"} {
		t.Run(writer, func(t *testing.T) {
			isolateBindTest(t)
			f := newNoticeRetryFixture(t)
			f.update(t, func(st *config.State) {
				st.Terminal.OwnNotice = schemaNotice("false")
				st.NoticeRetry = &config.NoticeRetry{FailedAt: f.now.Add(-4 * time.Second), Next: f.now, Delay: 4 * time.Second}
				st.OwnStatusWrite = liveOwnStatusFence()
			})
			before, requests := f.e.state("alice"), f.requests.Load()
			write := func() error {
				switch writer {
				case "notice":
					return f.s.publishTerminalNotice(context.Background(), "deadline")
				case "hook":
					return f.a.hookLiveness(context.Background(), hookEvent{arg: "session-start"}, hookInput{SessionID: "synthetic"})
				default:
					return f.s.writeLivenessSource(context.Background(), *before.Liveness, "wait-heartbeat")
				}
			}
			for i := 0; i < 3; i++ {
				err := write()
				if writer == "heartbeat" {
					if !errors.Is(err, ErrOwnStatusPending) {
						t.Fatalf("heartbeat lost pending refusal: %v", err)
					}
				} else if err != nil {
					t.Fatalf("automatic writer failed on contention: %v", err)
				}
			}
			if after := f.e.state("alice"); !reflect.DeepEqual(before, after) {
				t.Errorf("skipped attempt changed local state: before=%+v after=%+v", before, after)
			}
			if got := f.requests.Load(); got != requests {
				t.Errorf("skipped attempt made HTTP: requests=%d, want %d", got, requests)
			}
			setOwnStatusFence(t, f.e, nil)
			if err := write(); err != nil {
				t.Fatal(err)
			}
			st := f.e.state("alice")
			if f.requests.Load() != requests+2 || st.NoticeRetry != nil || st.NoticeWriteUncertain {
				t.Fatalf("cleared fence did not reconcile at original eligibility: retry=%+v", st.NoticeRetry)
			}
		})
	}
}

// With frozen observation time and sync barriers, a running wait skips a live
// fence and publishes next cycle without waiting another heartbeat interval.
// Initial and periodic contention retain manual state; disarm retries a seeded
// fence after delivery, while the controlled mail remains unreceipted.
// Sync cursor advancement and lease renewal are independent of status refusal.
func TestWaitPendingHeartbeatAndDisarm(t *testing.T) {
	for _, initial := range []bool{false, true} {
		t.Run(map[bool]string{false: "periodic", true: "initial"}[initial], func(t *testing.T) {
			e, room := world(t)
			e.extra[EnvSessionID] = "pending-wait"
			e.as("alice", "status", "init").ok(t)
			e.as("alice", statusArgs("blocked")...).ok(t)
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
			base := time.Now().Add(time.Hour).UTC()
			var offset atomic.Int64
			e.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
			parked, proceed := make(chan struct{}), make(chan struct{})
			var pending atomic.Int32
			var disarmFence, disarming atomic.Bool
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						select {
						case parked <- struct{}{}:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						select {
						case <-proceed:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
						r = r.Clone(r.Context())
						u := *r.URL
						r.URL = &u
						q := u.Query()
						q.Set("timeout", "0")
						u.RawQuery = q.Encode()
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
				a.afterOwnStatusPending = func() {
					pending.Add(1)
					if disarming.Load() {
						setOwnStatusFence(t, e, nil)
					}
				}
				a.beforeDeliver = func() {
					if disarmFence.Swap(false) {
						setOwnStatusFence(t, e, liveOwnStatusFence())
					}
				}
				a.afterPrint = func() error {
					disarming.Store(true)
					return nil
				}
			}
			if initial {
				setOwnStatusFence(t, e, liveOwnStatusFence())
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan result, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				done <- e.runCtx(ctx, "--as", "alice", "wait", "--timeout", "0", "--status", "idle", "--heartbeat", "1h", "--runtime", "codex", "--hooks", "none")
			}()
			t.Cleanup(func() {
				cancel()
				setOwnStatusFence(t, e, nil)
				select {
				case <-finished:
				case <-time.After(testDeadline):
					t.Error("wait did not stop")
				}
			})
			park := func() {
				t.Helper()
				select {
				case <-parked:
				case r := <-done:
					t.Fatalf("wait exited on contention: %+v", r)
				case <-time.After(testDeadline):
					t.Fatal("wait never reached sync barrier")
				}
			}
			next := func() {
				t.Helper()
				select {
				case proceed <- struct{}{}:
				case <-time.After(testDeadline):
					t.Fatal("wait did not leave sync barrier")
				}
				park()
			}
			park()
			if !initial {
				setOwnStatusFence(t, e, liveOwnStatusFence())
				offset.Store(int64(time.Hour))
			}
			before := e.state("alice")
			if before.Liveness == nil {
				t.Fatal("missing acknowledged liveness before fenced heartbeat")
			}
			if !initial && e.now().Sub(before.Liveness.HeartbeatAt) < time.Hour {
				t.Fatal("periodic heartbeat is not due at the frozen observation time")
			}
			puts := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
			refusalsBefore := pending.Load()
			// The next sync barrier follows the loop's heartbeat check. Periodic
			// time is due above; initial refusal leaves lastHeartbeat zero. Require
			// a fresh refusal, rather than counting initial startup contention.
			next()
			if got := pending.Load(); got != refusalsBefore+1 {
				t.Errorf("fenced heartbeat refusals=%d, want %d after next sync barrier", got, refusalsBefore+1)
			}
			// waitRetry renews the lease after the periodic clock jump. ingest
			// also persists NextBatch, though the fake keeps it stable without
			// new stream events. Neither belongs to own-status preservation.
			st := e.state("alice")
			if !reflect.DeepEqual(before.Liveness, st.Liveness) {
				t.Errorf("live-fence skip changed liveness: before=%+v after=%+v", before.Liveness, st.Liveness)
			}
			if st.Liveness == nil {
				t.Fatal("live-fence skip removed acknowledged liveness")
			}
			if !st.Liveness.HeartbeatAt.Equal(before.Liveness.HeartbeatAt) {
				t.Errorf("live-fence skip changed heartbeat time: before=%s after=%s", before.Liveness.HeartbeatAt, st.Liveness.HeartbeatAt)
			}
			if st.LivenessSource != before.LivenessSource {
				t.Errorf("live-fence skip changed liveness source: before=%q after=%q", before.LivenessSource, st.LivenessSource)
			}
			if !reflect.DeepEqual(before.OwnStatusWrite, st.OwnStatusWrite) {
				t.Errorf("live-fence skip changed own-status fence: before=%+v after=%+v", before.OwnStatusWrite, st.OwnStatusWrite)
			}
			if !reflect.DeepEqual(before.NoticeRetry, st.NoticeRetry) {
				t.Errorf("live-fence skip changed notice retry: before=%+v after=%+v", before.NoticeRetry, st.NoticeRetry)
			}
			if st.NoticeWriteUncertain != before.NoticeWriteUncertain {
				t.Errorf("live-fence skip changed notice uncertainty: before=%t after=%t", before.NoticeWriteUncertain, st.NoticeWriteUncertain)
			}
			if got := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}"); got != puts {
				t.Errorf("live-fence skip sent a PUT: count=%d, want %d", got, puts)
			}
			setOwnStatusFence(t, e, nil)
			next() // Do not advance observation time after the skipped heartbeat.
			st = e.state("alice")
			if st.Liveness == nil || !st.Liveness.HeartbeatAt.After(before.Liveness.HeartbeatAt) || st.Liveness.State != "blocked" || st.LivenessSource != "manual" || st.Liveness.Doorbell != "armed" {
				t.Fatalf("skip advanced last-heartbeat or lost manual state: %+v", st.Liveness)
			}
			refusals := pending.Load()
			disarmFence.Store(true)
			id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local controlled delivery", nil))
			select {
			case proceed <- struct{}{}:
			case <-time.After(testDeadline):
				t.Fatal("wait did not resume delivery")
			}
			r := recv(t, done).ok(t)
			st = e.state("alice")
			if !strings.Contains(r.stdout, id) || strings.Contains(r.stderr, ErrOwnStatusPending.Error()) || pending.Load() != refusals+1 || st.OwnStatusWrite != nil || st.Liveness.Doorbell != "none" || st.Liveness.State != "blocked" || st.LivenessSource != "manual" || len(e.mailbox("alice").Pending()) != 1 {
				t.Fatalf("disarm retry/manual state/mail changed: result=%+v state=%+v", r, st.Liveness)
			}
		})
	}
}

// A local manual edit during unlocked HTTP remains authoritative; contention
// handling must not bypass the existing reconciliation-required merge guard.
func TestOwnStatusPendingPreservesRacedManualEdit(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	a := eApp(e)
	a.Stdout, a.Stderr = io.Discard, io.Discard
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	s.client.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			_, err := config.UpdateStateBy(e.paths(), "alice", time.Now().Add(testDeadline), func(st *config.State) error {
				manual := *st.Liveness
				manual.State, manual.Detail = "waiting-human", "local manual edit"
				st.Liveness, st.LivenessSource = &manual, "manual"
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}
	err = s.writeLivenessSource(context.Background(), *e.state("alice").Liveness, "wait-heartbeat")
	st := e.state("alice")
	if err == nil || err.Error() != "own status changed during publication" || st.Liveness.State != "waiting-human" || st.Liveness.Detail != "local manual edit" || st.LivenessSource != "manual" || !st.NoticeWriteUncertain || st.OwnStatusWrite != nil {
		t.Fatalf("manual edit overwritten: err=%v status=%+v", err, st.Liveness)
	}
}
