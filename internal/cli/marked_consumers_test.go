package cli

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func markedRow(marked bool) string {
	if marked {
		return "marked"
	}
	return "unmarked"
}

// markFixtureIdentity turns a fixture identity into one that has bound
// before: its State is marked and its lease and cursor move to the sidecar,
// by the migration a first bind runs. It creates no binding.
func markFixtureIdentity(t *testing.T, e *env, name string) {
	t.Helper()
	// The migration takes over the lease the fixture's commands hold, as a
	// first bind takes over its own session's.
	holder := ""
	if prior := e.state(name); prior.Lease != nil && prior.Lease.Expires.After(time.Now()) {
		holder = prior.Lease.Holder
	}
	st, err := config.EnsureBindPart(e.paths(), name, config.BindPartOpenOptions{Holder: holder, Now: time.Now()})
	if err != nil || !config.BindPartActive(st) {
		t.Fatalf("marking %s: active=%t err=%v", name, config.BindPartActive(st), err)
	}
}

// TestParkedConsumerLeavesOthersFree proves that a parked wait or watch of an
// identity does not keep its inbox and its hooks from running: both succeed
// while the consumer is parked in its long poll, an event that arrives then is
// printed by the parked consumer, and the inbox afterwards prints and receives
// exactly that event. The rows are an unmarked identity (the control for the
// expected result) and a marked one without a binding.
//
// Regression witness for the marked row: wait and watch reserved the sidecar's
// cursor before their poll and held it while parked. The inbox then exited
// with "bind-state operation is held by another owner", and the stop hook
// delivered nothing and printed the same line.
//
// It does not cover a consumer of another process: every command here runs in
// the test's process. The reservation names its owner by host, process and
// start token, and a second command of this process is another owner to it as
// a second process would be.
func TestParkedConsumerLeavesOthersFree(t *testing.T) {
	for _, consumer := range []string{"wait", "watch"} {
		for _, marked := range []bool{false, true} {
			t.Run(consumer+"/"+markedRow(marked), func(t *testing.T) {
				e, room := world(t)
				alice := "@alice:test.local"
				e.as("alice", "wait", "--timeout", "10ms").ok(t)
				if marked {
					markFixtureIdentity(t, e, "alice")
				}
				// In the all-marked lane the harness has marked the identity already.
				startedMarked := config.BindPartMarked(e.state("alice"))
				args := []string{"--as", "alice", "wait", "--timeout", "0"}
				if consumer == "watch" {
					args = []string{"--as", "alice", "watch"}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				calls := e.hs.SyncCount(alice)
				done := make(chan result, 1)
				go func() { done <- e.runCtx(ctx, args...) }()
				e.syncedParked(t, alice, calls+1, done, "before any mail")

				if r := e.as("alice", "inbox").ok(t); r.stdout != "no new peer events\n" {
					t.Fatalf("inbox beside the parked %s: %q", consumer, r.stdout)
				}
				if r := e.hook("alice", "stop", `{}`).ok(t); r.stdout != "" || r.stderr != "" {
					t.Fatalf("stop hook beside the parked %s: stdout=%q stderr=%q", consumer, r.stdout, r.stderr)
				}

				first := e.hs.inject(room, "@bob:test.local", ping(1))
				if consumer == "wait" {
					w := recv(t, done).ok(t)
					if !strings.Contains(w.stdout, "mention #proj "+first) {
						t.Fatalf("the parked wait did not print the event: %q", w.stdout)
					}
				} else {
					deadline := time.Now().Add(testDeadline)
					for {
						if en, ok := e.pendingEntry("alice", first); ok && en.Shown {
							break
						}
						select {
						case r := <-done:
							t.Fatalf("watch ended: code %d stderr=%q", r.code, r.stderr)
						default:
						}
						if time.Now().After(deadline) {
							t.Fatal("the parked watch never showed the event")
						}
						time.Sleep(2 * time.Millisecond)
					}
				}
				h := e.as("alice", "inbox").ok(t)
				if strings.Count(h.stdout, "--- event ") != 1 || !strings.Contains(h.stdout, "id="+first) {
					t.Fatalf("inbox after the event: %q", h.stdout)
				}
				if !e.received("alice", first) {
					t.Fatal("the event was not received")
				}
				if r := e.as("alice", "inbox").ok(t); r.stdout != "no new peer events\n" {
					t.Fatalf("the event was printed a second time: %q", r.stdout)
				}
				if consumer == "watch" {
					cancel()
					recv(t, done)
				}
				if startedMarked != config.BindPartMarked(e.state("alice")) {
					t.Fatalf("the identity's marker changed: marked=%t", !startedMarked)
				}
			})
		}
	}
}

// TestStatusPublicationDoesNotHoldDelivery proves that an own-status
// publication in flight does not keep the identity's mail from being
// delivered. While the status request of one command is held open, an event
// arrives; the inbox prints and receives it, a short wait finds nothing more,
// and the stop hook stays silent. When the request is released the status
// command succeeds, the status is stored, nothing is left pending, and a
// binding keeps its generation.
//
// Rows: unmarked (the control), marked without a terminal, marked with a
// stopped binding, and marked with an active binding. The last two publish
// through the sidecar's status operation. The bindings are State records made
// by the fixture; no consumer process runs for them.
//
// Regression witness: a pending status operation refused the lease and the
// cursor reservation of every other command, for as long as the request took,
// which is up to its ten-second budget.
func TestStatusPublicationDoesNotHoldDelivery(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		terminal, stopped, mark bool
	}{
		{"unmarked", false, false, false},
		{"marked-no-terminal", false, false, true},
		{"marked-stopped", true, true, true},
		{"marked-active", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "status", "init").ok(t)
			e.as("alice", "wait", "--timeout", "10ms").ok(t)
			if tc.terminal {
				if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Terminal = &config.TerminalBinding{Generation: "g1", SessionID: "s1", Host: machineLabel(), Stopped: tc.stopped, Interval: 2 * time.Second, RetryMax: 30 * time.Second}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.mark {
				markFixtureIdentity(t, e, "alice")
			}
			e.as("alice", statusArgs("working")...).ok(t)
			// In the all-marked lane the harness has marked the identity already.
			startedMarked := config.BindPartMarked(e.state("alice"))

			held, release := make(chan struct{}), make(chan struct{})
			var armed atomic.Bool
			armed.Store(true)
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/") && armed.CompareAndSwap(true, false) {
						close(held)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
			}
			defer func() { e.configure = nil }()
			status := make(chan result, 1)
			go func() { status <- e.as("alice", statusArgs("idle")...) }()
			select {
			case <-held:
			case r := <-status:
				t.Fatalf("the status command ended before its request was held: code %d stderr=%q", r.code, r.stderr)
			case <-time.After(testDeadline):
				t.Fatal("the status request was never sent")
			}
			released := false
			defer func() {
				if !released {
					close(release)
					<-status
				}
			}()

			id := e.hs.inject(room, "@bob:test.local", ping(1))
			h := e.as("alice", "inbox").ok(t)
			if strings.Count(h.stdout, "--- event ") != 1 || !strings.Contains(h.stdout, "id="+id) {
				t.Fatalf("inbox while the status request is in flight: %q", h.stdout)
			}
			if !e.received("alice", id) {
				t.Fatal("the event was not received")
			}
			if r := e.as("alice", "wait", "--timeout", "10ms").ok(t); r.stdout != "timeout\n" {
				t.Fatalf("wait while the status request is in flight: %q", r.stdout)
			}
			if !tc.terminal {
				if r := e.hook("alice", "stop", `{}`).ok(t); r.stdout != "" || r.stderr != "" {
					t.Fatalf("stop hook while the status request is in flight: stdout=%q stderr=%q", r.stdout, r.stderr)
				}
			}

			released = true
			close(release)
			recv(t, status).ok(t)
			st := e.state("alice")
			if st.Liveness == nil || st.Liveness.State != "idle" {
				t.Fatalf("stored status %+v, want idle", st.Liveness)
			}
			if st.OwnStatusWrite != nil || st.NoticeWriteUncertain {
				t.Fatalf("a publication is left pending: write=%v uncertain=%t", st.OwnStatusWrite, st.NoticeWriteUncertain)
			}
			if tc.terminal && (st.Terminal == nil || st.Terminal.Generation != "g1" || st.Terminal.Stopped != tc.stopped) {
				t.Fatalf("the binding changed: %+v", st.Terminal)
			}
			if config.BindPartMarked(st) != startedMarked {
				t.Fatalf("marked=%t, want %t", config.BindPartMarked(st), startedMarked)
			}
			if r := e.as("alice", "inbox").ok(t); r.stdout != "no new peer events\n" {
				t.Fatalf("the event was printed a second time: %q", r.stdout)
			}
		})
	}
}

// TestBoundConsumerUnderContention runs the sync step of a bound consumer in
// a loop while the session's own commands run beside it: an inbox after every
// injected event, and a status write on every second one. It proves, for the
// identity with an active binding:
//   - no event is lost and none is received twice: every injected event is
//     received, and the inbox printed each exactly once;
//   - contention is no failure of the consumer: its sync step answers either
//     success or that the cursor was taken elsewhere, never another error;
//   - the binding is the same afterwards: same generation, not stopped, no
//     operation left pending in the sidecar;
//   - every command of the session succeeded.
//
// The consumer here is the sync step called directly, as the binding tests
// call it. No terminal is attached and no process is started, so this does
// not prove the wake path or a consumer in another process.
func TestBoundConsumerUnderContention(t *testing.T) {
	e, a, s, generation, room := markedBindDeliveryFixture(t, false)
	e.as("alice", "status", "init").ok(t)

	stop := make(chan struct{})
	consumerDone := make(chan []error, 1)
	var cycles atomic.Int64
	go func() {
		var unexpected []error
		for {
			select {
			case <-stop:
				consumerDone <- unexpected
				return
			default:
			}
			if _, err := s.syncIdleBindNowOrSyncNow(context.Background(), a); err != nil && !cursorTakenElsewhere(err) {
				unexpected = append(unexpected, err)
			}
			cycles.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	stopped := false
	stopConsumer := func() []error {
		if stopped {
			return nil
		}
		stopped = true
		close(stop)
		return <-consumerDone
	}
	defer stopConsumer()

	const events = 24
	var ids []string
	var printed strings.Builder
	for i := 1; i <= events; i++ {
		ids = append(ids, e.hs.inject(room, "@bob:"+testServer, msg("@alice:"+testServer+" contention "+strings.Repeat("x", i), nil)))
		printed.WriteString(e.as("alice", "inbox").ok(t).stdout)
		if i%2 == 0 {
			state := "working"
			if i%4 == 0 {
				state = "idle"
			}
			e.as("alice", statusArgs(state)...).ok(t)
		}
	}
	if errs := stopConsumer(); len(errs) != 0 {
		t.Fatalf("the consumer's sync step failed %d times under contention; first: %v", len(errs), errs[0])
	}
	if cycles.Load() < events {
		t.Fatalf("the consumer ran only %d cycles beside %d events: the contention was not exercised", cycles.Load(), events)
	}
	printed.WriteString(e.as("alice", "inbox").ok(t).stdout)
	for _, id := range ids {
		if !e.received("alice", id) {
			t.Fatalf("event %s was not received", id)
		}
		if n := strings.Count(printed.String(), "id="+id); n != 1 {
			t.Fatalf("event %s was printed %d times by the inbox", id, n)
		}
	}
	st := e.state("alice")
	if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
		t.Fatalf("the binding changed under contention: %+v", st.Terminal)
	}
	view, err := config.ReadBindPartView(e.paths(), "alice", st)
	if err != nil || view.Generation != generation {
		t.Fatalf("sidecar after the contention: %+v err=%v", view, err)
	}
	if st.OwnStatusWrite != nil {
		t.Fatalf("a status publication is left pending: %+v", st.OwnStatusWrite)
	}
	// Nothing is left reserved: a fresh reservation of the current cursor is admitted.
	if _, err := s.syncIdleBindNowOrSyncNow(context.Background(), a); err != nil && !cursorTakenElsewhere(err) {
		t.Fatalf("a sync step after the contention: %v", err)
	}
}

// syncIdleBindNowOrSyncNow runs one sync step of the bound consumer as its
// cycle chooses it.
func (s *session) syncIdleBindNowOrSyncNow(ctx context.Context, a *App) (config.State, error) {
	start, subs, err := s.startBindDelivery(ctx, a.Now())
	if err != nil {
		return config.State{}, err
	}
	deadline := time.Now().Add(5 * time.Second)
	if start.cached {
		_, err = s.syncIdleBindNow(ctx, start.view, subs.Subs, deadline)
		return config.State{}, err
	}
	return s.syncNow(ctx, start.state, subs.Subs, deadline)
}
