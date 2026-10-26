package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func notificationApp(t *testing.T, e *env) (*App, *session) {
	t.Helper()
	a := NewApp()
	a.as = "alice"
	a.ctx = context.Background()
	a.Stdout, a.Stderr = io.Discard, io.Discard
	a.HTTP = e.hs.srv.Client()
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}
	if e.now != nil {
		a.Now = e.now
	}
	if e.configure != nil {
		e.configure(a)
	}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	return a, s
}
func notificationState(t *testing.T, e *env, fn func(*config.State)) {
	t.Helper()
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { fn(st); return nil }); err != nil {
		t.Fatal(err)
	}
}

// The fixture holds the same existing lock as a watch; it starts no process.
func notificationConsumerLock(t *testing.T, e *env) func() {
	t.Helper()
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- config.WithRuntimeLock(e.paths(), "alice", "consumer", time.Now().Add(time.Second), func() error { close(ready); <-release; return nil })
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(release)
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// A hook reservation crash emits nothing and credits nothing. It spaces native
// until exactly30s without becoming a global uncertainty fence or a receipt.
func TestNotificationHookCrashOtherPath(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	clock := e.clock()
	id := runtimeMail(e, room, "private body")
	prior := e.configure
	e.configure = func(a *App) {
		prior(a)
		a.afterNotificationReserve = func() error { return errors.New("synthetic interruption") }
	}
	r := e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t)
	if r.stdout != "" || !strings.Contains(r.stderr, "synthetic interruption") {
		t.Fatalf("wrong crash: %+v", r)
	}
	st := e.state("alice")
	if st.Notification.Reservation == nil || len(st.HookSession(hookSessionKey("owned-thread")).Rung) != 0 || e.received("alice", id) {
		t.Fatal("reservation crash lost uncertainty/mail")
	}
	due := st.Notification.NotBefore
	e.configure = prior
	clock.add(29999 * time.Millisecond)
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 0 || !e.state("alice").Notification.NotBefore.Equal(due) {
		t.Fatal("early native output or sliding deadline")
	}
	clock.add(time.Millisecond)
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 1 || e.received("alice", id) || e.state("alice").Notification.Reservation != nil {
		t.Fatal("trailing other path starved/receipted")
	}
}

// Only a live, same-generation, same-session watch can justify suppressing Stop.
// Missing lock, absent/dead/stale/wrong-scope metadata all retain fallback Stop.
func TestNotificationStopConsumerScope(t *testing.T) {
	for _, mode := range []string{"live", "no-lock", "registration-only", "dead", "stale-generation", "wrong-session"} {
		t.Run(mode, func(t *testing.T) {
			e, room, _, g := runtimeWorld(t)
			clock := e.clock()
			runtimeMail(e, room, "A")
			bellContext(t, e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t).stdout)
			a, s := notificationApp(t, e)
			if mode != "registration-only" {
				if err := a.nativeConsumer(context.Background(), s, g, true); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "no-lock" {
				notificationConsumerLock(t, e)
			}
			notificationState(t, e, func(st *config.State) {
				switch mode {
				case "dead":
					st.Runtime.ConsumerPID = 2147483647
				case "stale-generation":
					st.Runtime.ConsumerGeneration = "old"
				case "wrong-session":
					st.Runtime.SessionID = "other"
				}
			})
			clock.add(5 * time.Second)
			id := runtimeMail(e, room, "B")
			r := e.hook("alice", "stop", `{"session_id":"owned-thread"}`).ok(t)
			if mode == "live" {
				if r.stdout != "" || !e.state("alice").Notification.NotBefore.Equal(clock.now().Add(25*time.Second)) {
					t.Fatal("live-owned Stop bypassed spacing")
				}
			} else {
				stopReason(t, r.stdout)
				if !e.state("alice").Notification.NotBefore.Equal(clock.now().Add(30 * time.Second)) {
					t.Fatal("fallback Stop failed to anchor completion")
				}
			}
			if e.received("alice", id) {
				t.Fatal("Stop receipted mail")
			}
		})
	}
}

// Prompt A@0, fallback Stop B@5, C@6, then an admitted watch: the next effect
// is due35, not30. No new event at the deadline is required, and inbox still owns receipts.
func TestNotificationFallbackStopReanchorsTrailing(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	clock := e.clock()
	runtimeMail(e, room, "A")
	bellContext(t, e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t).stdout)
	clock.add(5 * time.Second)
	runtimeMail(e, room, "B")
	stopReason(t, e.hook("alice", "stop", `{"session_id":"owned-thread"}`).ok(t).stdout)
	clock.add(time.Second)
	id := runtimeMail(e, room, "C")
	a, s := notificationApp(t, e)
	notificationConsumerLock(t, e)
	if err := a.nativeConsumer(context.Background(), s, g, true); err != nil {
		t.Fatal(err)
	}
	defer a.nativeConsumer(context.Background(), s, g, false)
	clock.add(24 * time.Second)
	if err := a.dispatchNative(context.Background(), s, g); err != nil {
		t.Fatal(err)
	}
	if len(n.wakes) != 0 || a.notificationWait(e.state("alice"), time.Minute) != 5*time.Second {
		t.Fatal("trailing ignored newer Stop completion")
	}
	clock.add(4999 * time.Millisecond)
	if err := a.dispatchNative(context.Background(), s, g); err != nil {
		t.Fatal(err)
	}
	if len(n.wakes) != 0 {
		t.Fatal("trailing rang before35")
	}
	clock.add(time.Millisecond)
	if err := a.dispatchNative(context.Background(), s, g); err != nil {
		t.Fatal(err)
	}
	if len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("due work lost without another arrival")
	}
	if a.notificationWait(e.state("alice"), time.Minute) != 30*time.Second {
		t.Fatal("completion did not start next window")
	}
}

// The common control lock serializes competing paths: only one saved token can
// authorize an effect at a boundary. A stale completion cannot alter its successor.
func TestNotificationConcurrentReservationsAndStaleCompletion(t *testing.T) {
	e, _ := world(t)
	e.extra[EnvSessionID] = "own"
	clock := e.clock()
	a, s := notificationApp(t, e)
	var wg sync.WaitGroup
	tokens := make(chan string, 2)
	errs := make(chan error, 2)
	for _, path := range []string{"hook", "native"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(time.Second), func() error {
				token, err := a.reserveNotification(context.Background(), s, time.Now().Add(time.Second), path, "own", "g", false, func(*config.State, *config.Mailbox, string) (bool, error) { return true, nil })
				tokens <- token
				return err
			})
			errs <- err
		}(path)
	}
	wg.Wait()
	close(tokens)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var first string
	count := 0
	for token := range tokens {
		if token != "" {
			first = token
			count++
		}
	}
	if count != 1 {
		t.Fatalf("concurrent reservation count=%d", count)
	}
	clock.add(30 * time.Second)
	token, err := a.reserveNotification(context.Background(), s, time.Now().Add(time.Second), "terminal", "own", "g", false, func(*config.State, *config.Mailbox, string) (bool, error) { return true, nil })
	if err != nil || token == "" {
		t.Fatal("other path held beyond30s", err)
	}
	before, _ := json.Marshal(e.state("alice").Notification)
	notificationState(t, e, func(st *config.State) {
		if err := a.completeNotification(s, st, first); err != nil {
			t.Fatal(err)
		}
	})
	after, _ := json.Marshal(e.state("alice").Notification)
	if string(before) != string(after) {
		t.Fatal("stale completion changed successor")
	}
}

// Diagnostics expose only a closed state/deadline, never persisted opaque
// scope/token/path values; reads do not migrate, clamp or clear the ledger.
func TestNotificationDiagnosticReadOnly(t *testing.T) {
	e, _ := world(t)
	clock := e.clock()
	notificationState(t, e, func(st *config.State) {
		st.Notification = config.Notification{Version: 1, NotBefore: clock.now().Add(30 * time.Second), Reservation: &config.NotificationReservation{Token: strings.Repeat("a", 32), Path: "native", Session: "PRIVATE_SESSION", Generation: "PRIVATE_GENERATION", At: clock.now(), Previous: clock.now()}}
	})
	before, _ := json.Marshal(e.state("alice"))
	r := e.as("alice", "bind", "status", "--json").ok(t)
	if !strings.Contains(r.stdout, `"state":"uncertain cooldown"`) {
		t.Fatal(r.stdout)
	}
	for _, secret := range []string{"PRIVATE_SESSION", "PRIVATE_GENERATION", strings.Repeat("a", 32), `"path"`, `"reservation"`} {
		if strings.Contains(r.stdout, secret) {
			t.Fatal("private ledger leaked", r.stdout)
		}
	}
	after, _ := json.Marshal(e.state("alice"))
	if string(before) != string(after) {
		t.Fatal("diagnostic mutated ledger")
	}
	clock.add(30 * time.Second)
	if got := notificationDiagnostic(e.state("alice").Notification, clock.now()).State; got != "uncertain ready" {
		t.Fatal(got)
	}
	notificationState(t, e, func(st *config.State) { st.Notification.Reservation.Path = "PRIVATE_BAD_PATH" })
	r = e.as("alice", "bind", "status", "--json").ok(t)
	if !strings.Contains(r.stdout, `"state":"invalid"`) || strings.Contains(r.stdout, "PRIVATE_BAD_PATH") {
		t.Fatal(r.stdout)
	}
}

// A failed post-output state write never credits delivery or receipts. The
// pre-output saved reservation remains uncertain until another path is eligible.
func TestNotificationHookCompletionPersistenceFailure(t *testing.T) {
	e, room, _, _ := runtimeWorld(t)
	e.clock()
	id := runtimeMail(e, room, "pending")
	prior := e.configure
	e.configure = func(a *App) {
		prior(a)
		a.afterPrint = func() error { return os.Chmod(e.paths().StateFile("alice"), 0644) }
	}
	r := e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t)
	if err := os.Chmod(e.paths().StateFile("alice"), 0600); err != nil {
		t.Fatal(err)
	}
	if r.stdout == "" || r.stderr == "" {
		t.Fatalf("missing output or intended state-write failure: %+v", r)
	}
	st := e.state("alice")
	if st.Notification.Reservation == nil || len(st.HookSession(hookSessionKey("owned-thread")).Rung) != 0 || e.received("alice", id) {
		t.Fatal("post-output state failure lost fence/mail")
	}
}

// A late terminal result cannot credit a replacement attempt/session, and a
// matching own attempt can complete without clearing a newer common token.
func TestNotificationTerminalCompletionScope(t *testing.T) {
	for _, mode := range []string{"valid", "session", "ids", "token", "newer-common"} {
		t.Run(mode, func(t *testing.T) {
			e, _ := world(t)
			clock := e.clock()
			a, s := notificationApp(t, e)
			var expected config.TerminalBinding
			notificationState(t, e, func(st *config.State) {
				token, err := st.Notification.Reserve("terminal", "own", "g", clock.now(), false)
				if err != nil {
					t.Fatal(err)
				}
				expected = config.TerminalBinding{Generation: "g", SessionID: "own", NotificationToken: token, AttemptAt: clock.now(), AttemptIDs: []string{"mail"}}
				copy := expected
				copy.AttemptIDs = []string{"mail"}
				st.Terminal = &copy
				switch mode {
				case "session":
					st.Terminal.SessionID = "other"
				case "ids":
					st.Terminal.AttemptIDs = []string{"other"}
				case "token":
					st.Terminal.NotificationToken = "newer"
				case "newer-common":
					if _, err := st.Notification.Reserve("hook", "own", "", clock.now().Add(30*time.Second), false); err != nil {
						t.Fatal(err)
					}
				}
			})
			before, _ := json.Marshal(e.state("alice"))
			err := a.finishTerminal(s, "g", tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, nil, false, expected)
			if mode == "session" || mode == "ids" || mode == "token" {
				if !errors.Is(err, config.ErrNotificationStale) {
					t.Fatal("stale terminal completion accepted", err)
				}
				after, _ := json.Marshal(e.state("alice"))
				if string(before) != string(after) {
					t.Fatal("stale completion mutated state")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			st := e.state("alice")
			if len(st.Terminal.Rung) != 1 || len(st.Terminal.AttemptIDs) != 0 {
				t.Fatal("matching channel completion lost")
			}
			if (st.Notification.Reservation != nil) != (mode == "newer-common") {
				t.Fatal("completion cleared wrong common reservation")
			}
		})
	}
}

// Terminal spacing suppresses only new effects, retaining both first-observation
// and hourly/uncertain-input gates; another admitted hook can ring after30s.
func TestNotificationTerminalSpacingAndUncertainty(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	id := f.mail()
	f.update(func(st *config.State) {
		if err := st.Notification.Prepare(f.now, time.Time{}); err != nil {
			t.Fatal(err)
		}
		token, err := st.Notification.Reserve("hook", "owned-thread", "", f.now, false)
		if err != nil {
			t.Fatal(err)
		}
		if err = st.Notification.Complete(token, f.now); err != nil {
			t.Fatal(err)
		}
	})
	f.dispatch()
	f.step()
	if b := f.binding(); len(b.Reservations) != 0 || len(b.AttemptIDs) != 0 || b.Outcome != "suppressed: notification cooldown" {
		t.Fatalf("terminal spent early effect: %+v", b)
	}
	f.now = f.now.Add(28*time.Second + 999*time.Millisecond)
	f.dispatch()
	if len(f.binding().Reservations) != 0 || f.e.received("alice", id) {
		t.Fatal("terminal early output or receipt")
	}
	f.now = f.now.Add(time.Millisecond)
	f.dispatch()
	f.control(map[string]any{})
	if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 1 {
		t.Fatal("terminal due starvation", s)
	}
	// A real post-type deferral remains manual on this path after shared expiry.
	f.now = f.now.Add(30 * time.Second)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	if !f.binding().Typed {
		t.Fatal("missing uncertain terminal attempt")
	}
	f.now = f.now.Add(30 * time.Second)
	f.e.now = func() time.Time { return f.now }
	newID := f.mail()
	stopReason(t, f.e.hook("alice", "stop", `{"session_id":"owned-thread"}`).ok(t).stdout)
	f.dispatch()
	f.control(map[string]any{})
	if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 1 || s.Erased != 0 || s.Text == "" {
		t.Fatal("shared expiry replayed/erased terminal uncertainty", s)
	}
	if f.e.received("alice", newID) {
		t.Fatal("other path receipted uncertainty")
	}
}

// The actual existing native consumer wakes at a positive fixed deadline with
// no new event, advertises live ownership only while running, and cleans it up.
func TestNotificationNativeConsumerTrailingNoNewEvent(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	var offset atomic.Int64
	e.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	id := runtimeMail(e, room, "pending")
	bellContext(t, e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t).stdout)
	offset.Store(int64(29900 * time.Millisecond))
	a, s := notificationApp(t, e)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	owned := false
	n.onWake = func() {
		st := e.state("alice")
		owned = st.Runtime.ConsumerPID == os.Getpid() && st.Runtime.ConsumerGeneration == g
		cancel()
	}
	err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(time.Second), func() error { return a.runNative(ctx, s, g) })
	if err != nil {
		t.Fatal(err)
	}
	st := e.state("alice")
	if !owned || len(n.wakes) != 1 || st.Runtime.ConsumerPID != 0 || st.Runtime.ConsumerGeneration != "" || e.received("alice", id) {
		t.Fatalf("trailing/ownership/receipt invariant: owned=%t wakes=%d binding=%+v outcome=%s now=%v due=%v", owned, len(n.wakes), st.Runtime, st.Wake.Outcome, a.Now(), st.Notification.NotBefore)
	}
}

type notificationTransportFunc func(*http.Request) (*http.Response, error)

func (f notificationTransportFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

// If the fixed deadline passes during heartbeat publication after a cooldown
// refusal, the consumer must recheck admission before starting a normal long
// poll. A trapped long poll is the intended violation, not a test timeout.
func TestNotificationNativeDeadlineDuringMaintenance(t *testing.T) {
	for _, busy := range []bool{false, true} {
		t.Run(strconv.FormatBool(busy), func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			clock := e.clock()
			id := runtimeMail(e, room, "pending")
			bellContext(t, e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t).stdout)
			clock.add(29900 * time.Millisecond)
			a, s := notificationApp(t, e)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			crossed, wrongPoll, normalBusyPoll := false, false, false
			n.onWake = cancel
			s.client.HTTP = &http.Client{Transport: notificationTransportFunc(func(r *http.Request) (*http.Response, error) {
				if !crossed && r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/"+StatusEventType+"/") {
					clock.add(time.Second) // deterministic elapsed time during ordinary maintenance
					crossed = true
					if busy {
						n.thread.Status.Type = "active"
					}
				}
				if crossed && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sync") && len(n.wakes) == 0 {
					// A newly busy target must return to its normal positive poll. Due
					// time alone must neither send a wake nor create an immediate loop.
					normalBusyPoll = busy && e.state("alice").Wake.Outcome == "wake suppressed: busy" && r.URL.Query().Get("timeout") == "60000"
					wrongPoll = !normalBusyPoll
					cancel()
					return nil, errors.New("observed poll after fixed deadline")
				}
				return e.hs.srv.Client().Transport.RoundTrip(r)
			})}
			err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(time.Second), func() error { return a.runNative(ctx, s, g) })
			if err != nil {
				t.Fatal(err)
			}
			wantWakes := 1
			if busy {
				wantWakes = 0
			}
			if !crossed || wrongPoll || normalBusyPoll != busy || len(n.wakes) != wantWakes || e.received("alice", id) {
				t.Fatalf("busy=%t deadline crossed=%t wrong-poll=%t normal-busy-poll=%t wakes=%d", busy, crossed, wrongPoll, normalBusyPoll, len(n.wakes))
			}
		})
	}
}

// Proven zero-output cancellation restores the preceding deadline; partial
// output remains uncertain and cannot authorize an immediate retry.
func TestNotificationHookOutputFailures(t *testing.T) {
	for _, mode := range []string{"zero", "partial", "flush"} {
		t.Run(mode, func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			e.clock()
			id := runtimeMail(e, room, "pending")
			prior := e.configure
			e.configure = func(a *App) { prior(a); a.Stdout = notificationFaultWriter{mode: mode} }
			e.hook("alice", "prompt", `{"session_id":"owned-thread"}`).ok(t)
			st := e.state("alice")
			if (st.Notification.Reservation != nil) != (mode != "zero") || len(st.HookSession(hookSessionKey("owned-thread")).Rung) != 0 || e.received("alice", id) {
				t.Fatal("incorrect output failure accounting", mode)
			}
			e.configure = prior
			e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			want := 0
			if mode == "zero" {
				want = 1
			}
			if len(n.wakes) != want {
				t.Fatal("output uncertainty did not space subsequent path", mode, len(n.wakes))
			}
		})
	}
}

type notificationFaultWriter struct{ mode string }

func (w notificationFaultWriter) Write(p []byte) (int, error) {
	if w.mode == "zero" {
		return 0, errors.New("synthetic zero write")
	}
	if w.mode == "partial" {
		return 1, io.ErrShortWrite
	}
	return len(p), nil
}
func (w notificationFaultWriter) Flush() error { return errors.New("synthetic flush") }

// A selected-connection invalidation after the saved native reservation
// prevents the outward effect. An unchanged control wakes; neither receipts.
func TestNotificationReservedConnectionGuard(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(strconv.FormatBool(changed), func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			e.clock()
			id := runtimeMail(e, room, "pending")
			a, s := notificationApp(t, e)
			if changed {
				a.afterNotificationReserve = func() error {
					s.checkBinding = func() error { return errors.New("synthetic selected connection changed") }
					return nil
				}
			}
			err := a.dispatchNative(context.Background(), s, g)
			st := e.state("alice")
			if changed {
				if err == nil || !strings.Contains(err.Error(), "synthetic selected connection changed") || len(n.wakes) != 0 || st.Notification.Reservation == nil || st.Wake.Attempt == nil || len(st.Wake.Rung) != 0 {
					t.Fatal("invalidated connection produced/credited output", err)
				}
			} else {
				if err != nil || len(n.wakes) != 1 || st.Notification.Reservation != nil {
					t.Fatal("valid selected connection refused", err)
				}
			}
			if e.received("alice", id) {
				t.Fatal("connection control receipted mail")
			}
		})
	}
}

// Scheduling can shorten a normal positive poll only. Already-due work never
// busy-loops; this helper is not used on the terminal error-backoff branch.
func TestNotificationWaitBoundaries(t *testing.T) {
	a := NewApp()
	now := time.Now()
	a.Now = func() time.Time { return now }
	for _, tc := range []struct{ left, normal, want time.Duration }{{time.Second, time.Minute, time.Second}, {time.Minute, time.Second, time.Second}, {0, time.Second, time.Second}, {-time.Second, time.Second, time.Second}} {
		st := config.State{Notification: config.Notification{Version: 1, NotBefore: now.Add(tc.left)}}
		if got := a.notificationWait(st, tc.normal); got != tc.want {
			t.Fatalf("wait left=%s normal=%s got=%s", tc.left, tc.normal, got)
		}
	}
}
