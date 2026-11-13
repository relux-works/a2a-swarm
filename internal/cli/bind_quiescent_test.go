//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// quiescentTiming shortens the package-private quiescence seams for one test
// and restores the production values afterwards.
func quiescentTiming(t *testing.T, quietAfter, reconcile, poll, tick time.Duration) {
	t.Helper()
	old := []time.Duration{terminalQuietAfter, terminalReconcileEvery, terminalLongPoll, terminalQuietTickMax}
	terminalQuietAfter, terminalReconcileEvery, terminalLongPoll, terminalQuietTickMax = quietAfter, reconcile, poll, tick
	t.Cleanup(func() {
		terminalQuietAfter, terminalReconcileEvery, terminalLongPoll, terminalQuietTickMax = old[0], old[1], old[2], old[3]
	})
}

type terminalTraceLog struct {
	mu     sync.Mutex
	events []string
	at     []time.Time
}

func (l *terminalTraceLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
	l.at = append(l.at, time.Now())
}

// count returns how many events with the prefix happened in [from, to).
func (l *terminalTraceLog) count(prefix string, from, to time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for i, e := range l.events {
		if strings.HasPrefix(e, prefix) && !l.at[i].Before(from) && l.at[i].Before(to) {
			n++
		}
	}
	return n
}

func (l *terminalTraceLog) await(t *testing.T, prefix string, after time.Time, within time.Duration) time.Time {
	t.Helper()
	until := time.Now().Add(within)
	for time.Now().Before(until) {
		l.mu.Lock()
		for i, e := range l.events {
			if strings.HasPrefix(e, prefix) && !l.at[i].Before(after) {
				at := l.at[i]
				l.mu.Unlock()
				return at
			}
		}
		l.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	t.Fatalf("trace %q not observed; events=%v", prefix, l.events)
	return time.Time{}
}

type consumerRun struct {
	trace   *terminalTraceLog
	cancel  context.CancelFunc
	done    chan struct{}
	outcome string
	err     error
}

// startQuiescentConsumer runs the real consumer loop of the orphan fixture as
// this process's own consumer (its PID/birth own the binding), on real time.
func startQuiescentConsumer(t *testing.T, f *orphanDispatchFixture, interval time.Duration, view terminalRefreshView) *consumerRun {
	t.Helper()
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	f.a.Now = time.Now
	f.update(func(st *config.State) {
		st.Terminal.DaemonPID, st.Terminal.DaemonBirth = os.Getpid(), birth
		st.Terminal.Interval, st.Terminal.RetryMax = interval, max(interval, time.Second)
	})
	run := &consumerRun{trace: &terminalTraceLog{}, done: make(chan struct{})}
	f.a.terminalTrace = run.trace.add
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	run.cancel = cancel
	initial := *f.binding()
	go func() {
		defer close(run.done)
		run.outcome, run.err = f.a.runTerminalLoop(ctx, f.s, "orphan-gen", initial, nil, view)
	}()
	t.Cleanup(func() {
		cancel()
		<-run.done
	})
	return run
}

func (r *consumerRun) wait(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-r.done:
	case <-time.After(within):
		t.Fatalf("consumer did not return within %s", within)
	}
}

// quietWindowError is the bounded-work assertion for a quiescent window
// shorter than the reconciliation deadline, split by work source:
//   - extra full terminal cycles (State/connection guards, ps/tmux probes,
//     lease and observation writes, dispatch): none;
//   - owner re-registration (two ps Birth checks): none, as ticks only run
//     the fork-free start-time check and stamp comparisons;
//   - self-write wakes: none;
//   - long-poll request starts: at least one, at most one per completed
//     response plus the one in flight, and never more often than polls can
//     complete (window/longPoll + 2);
//   - authoritative ingestion: exactly one per completed response (its own
//     guards, State/mailbox reads and cursor or privacy writes are expected).
func quietWindowError(trace *terminalTraceLog, from, to time.Time, longPoll time.Duration) error {
	if n := trace.count("cycle", from, to); n != 0 {
		return fmt.Errorf("quiescent window ran %d extra full cycles", n)
	}
	if n := trace.count("register", from, to); n != 0 {
		return fmt.Errorf("quiescent window re-registered the owner %d times", n)
	}
	if n := trace.count("wake:state", from, to); n != 0 {
		return fmt.Errorf("own writes woke the consumer %d times", n)
	}
	polls, ingests := trace.count("poll", from, to), trace.count("ingest", from, to)
	if polls == 0 {
		return fmt.Errorf("no long-poll during the quiescent window")
	}
	if limit := int(to.Sub(from)/longPoll) + 2; polls > limit {
		return fmt.Errorf("%d long-poll starts exceed %d", polls, limit)
	}
	if ingests > polls {
		return fmt.Errorf("%d ingestions for %d long-polls", ingests, polls)
	}
	return nil
}

// T1/T11: a quiescent consumer does bounded work per source (see
// quietWindowError) through the production long-poll and ingestion path, its
// own writes do not wake it, and the reconciliation deadline then forces
// exactly one full cycle and one fresh registration. Limits: fixture timing
// with shortened seams; this is not the real-binary CPU target.
func TestQuiescentConsumerBoundedWork(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, 4*time.Second, time.Second, 100*time.Millisecond)
	f := newOrphanDispatchFixture(t)
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	from := run.trace.await(t, "poll", time.Time{}, 20*time.Second)
	time.Sleep(2500 * time.Millisecond)
	to := from.Add(2500 * time.Millisecond)
	if err := quietWindowError(run.trace, from, to, time.Second); err != nil {
		t.Fatal(err)
	}
	if run.trace.count("ingest", from, to) == 0 {
		t.Fatal("no completed long-poll was ingested through the production path")
	}
	run.trace.await(t, "wake:reconcile", from, 5*time.Second)
	reconciled := run.trace.await(t, "cycle", from, 5*time.Second)
	if n := run.trace.count("cycle", from, reconciled.Add(time.Millisecond)); n != 1 {
		t.Fatalf("reconciliation ran %d full cycles, want 1", n)
	}
	run.trace.await(t, "register", reconciled, 5*time.Second)
}

// Calibration for T1: with quiescence disabled the per-Interval loop returns,
// and the same checker must fail on its intended counter (extra full cycles).
func TestQuiescentConsumerBoundedWorkCalibration(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, 4*time.Second, time.Second, 100*time.Millisecond)
	terminalQuiescence = false
	t.Cleanup(func() { terminalQuiescence = true })
	f := newOrphanDispatchFixture(t)
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	run.trace.await(t, "cycle", time.Time{}, 20*time.Second)
	from := time.Now()
	time.Sleep(2500 * time.Millisecond)
	err := quietWindowError(run.trace, from, from.Add(2500*time.Millisecond), time.Second)
	if err == nil || !strings.Contains(err.Error(), "extra full cycles") {
		t.Fatalf("checker did not detect per-Interval full cycles: %v", err)
	}
}

// Checker controls with known-violation traces: per-tick re-registration,
// self-write wakes, per-tick request restarts and ingestion without a
// completed response are each rejected at their own counter; a valid trace
// passes.
func TestQuietWindowErrorControls(t *testing.T) {
	from := time.Now()
	build := func(events ...string) *terminalTraceLog {
		l := &terminalTraceLog{}
		for i, e := range events {
			l.events = append(l.events, e)
			l.at = append(l.at, from.Add(time.Duration(i+1)*10*time.Millisecond))
		}
		return l
	}
	to := from.Add(2500 * time.Millisecond)
	if err := quietWindowError(build("poll", "ingest", "poll", "ingest", "poll"), from, to, time.Second); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	for want, trace := range map[string]*terminalTraceLog{
		"extra full cycles": build("poll", "cycle"),
		"re-registered":     build("poll", "register", "register"),
		"own writes woke":   build("poll", "wake:state"),
		"exceed":            build("poll", "poll", "poll", "poll", "poll", "poll"),
		"ingestions":        build("poll", "ingest", "ingest"),
		"no long-poll":      build("ingest"),
	} {
		if err := quietWindowError(trace, from, to, time.Second); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: checker accepted a violating trace: %v", want, err)
		}
	}
}

// T2: mail arriving at the server during a long-poll is ingested by the
// authoritative path, wakes the consumer for the mail reason and is notified
// exactly once after the ordinary two-sample idle qualification.
func TestQuiescentConsumerDeliversNewMail(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, time.Minute, 20*time.Second, 100*time.Millisecond)
	f := newOrphanDispatchFixture(t)
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	polled := run.trace.await(t, "poll", time.Time{}, 20*time.Second)
	id := f.e.hs.inject(f.room, "@bob:test.local", msg("@alice:test.local SYNTHETIC_QUIET_MAIL", nil))
	run.trace.await(t, "wake:mail", polled, 10*time.Second)
	screen := f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
	if screen.Submitted[0] != tmuxwake.Notice(1) || f.e.received("alice", id) {
		t.Fatalf("quiescent wake did not deliver exactly one notice: %+v", screen.Submitted)
	}
}

// T11: a change to the State file by another writer (here a hooks pause)
// wakes a quiescent consumer within its tick and runs one full cycle.
func TestQuiescentConsumerWakesOnExternalStateWrite(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, time.Minute, 20*time.Second, 100*time.Millisecond)
	f := newOrphanDispatchFixture(t)
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	polled := run.trace.await(t, "poll", time.Time{}, 20*time.Second)
	time.Sleep(300 * time.Millisecond)
	if n := run.trace.count("cycle", polled, time.Now()); n != 0 {
		t.Fatalf("setup: consumer not quiescent (%d cycles)", n)
	}
	written := time.Now()
	f.update(func(st *config.State) { st.HooksPaused = true })
	woke := run.trace.await(t, "wake:state", written, 3*time.Second)
	run.trace.await(t, "cycle", woke, 3*time.Second)
	awaitBinding(t, f.e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "suppressed: manual gate" })
}

// T7: the owner's exit is noticed by the fork-free start-time check while
// quiescent, and the consumer stops within the 3s prior-consumer budget of a
// SessionStart rebind, including a lane whose Interval is 30s.
func TestQuiescentConsumerStopsOnOwnerExit(t *testing.T) {
	isolateBindTest(t)
	for _, tc := range []struct {
		interval   time.Duration
		lockedHeld bool
	}{{300 * time.Millisecond, false}, {30 * time.Second, false}, {300 * time.Millisecond, true}} {
		t.Run(fmt.Sprintf("%s-locked-%t", tc.interval, tc.lockedHeld), func(t *testing.T) {
			interval := tc.interval
			quiescentTiming(t, 0, time.Minute, 20*time.Second, time.Second)
			f := newOrphanDispatchFixture(t)
			run := startQuiescentConsumer(t, f, interval, nil)
			run.trace.await(t, "poll", time.Time{}, 40*time.Second)
			owner := f.binding().OwnerPID
			if tc.lockedHeld {
				// Another writer holds the State lock for the whole stop path.
				held, release := make(chan struct{}), make(chan struct{})
				go config.UpdateState(f.e.paths(), "alice", func(*config.State) error {
					close(held)
					<-release
					return errors.New("released")
				})
				<-held
				t.Cleanup(func() { close(release) })
			}
			killed := time.Now()
			if err := syscall.Kill(owner, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			run.wait(t, 3*time.Second)
			if run.err != nil || run.outcome != "owner gone or replaced" {
				t.Fatalf("outcome=%q err=%v after %s", run.outcome, run.err, time.Since(killed))
			}
		})
	}
}

// T13: cancelling the consumer during a long-poll returns promptly, joins the
// request and leaves the cursor and mailbox exactly as they were.
func TestQuiescentConsumerCancelDuringLongPoll(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, time.Minute, 20*time.Second, 100*time.Millisecond)
	f := newOrphanDispatchFixture(t)
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	run.trace.await(t, "poll", time.Time{}, 20*time.Second)
	time.Sleep(200 * time.Millisecond)
	cursor := f.e.state("alice").NextBatch
	mailbox := readFile(t, f.s.paths.MailboxFile("alice"))
	run.cancel()
	run.wait(t, 2*time.Second)
	if run.outcome != "cancelled" || run.err != nil {
		t.Fatalf("outcome=%q err=%v", run.outcome, run.err)
	}
	if f.e.state("alice").NextBatch != cursor || readFile(t, f.s.paths.MailboxFile("alice")) != mailbox {
		t.Fatal("cancelled long-poll changed the cursor or mailbox")
	}
}

// Resource lifetime: repeated quiescent ticks keep one registration and one
// in-flight request, and a cancelled consumer leaves no goroutine or file
// descriptor behind (idle HTTP connections closed; small tolerance for the
// runtime and the fixture's helpers).
func TestQuiescentConsumerResourceLifetime(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, time.Minute, 20*time.Second, 50*time.Millisecond)
	f := newOrphanDispatchFixture(t)
	goroutines, fds := runtime.NumGoroutine(), openFDs(t)
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	from := run.trace.await(t, "poll", time.Time{}, 20*time.Second)
	time.Sleep(time.Second) // about 20 ticks
	if n := run.trace.count("register", from, time.Now()); n != 0 {
		t.Fatalf("ticks re-registered the owner %d times", n)
	}
	if n := run.trace.count("poll", from, time.Now()); n != 1 {
		t.Fatalf("ticks restarted the long-poll: %d request starts", n)
	}
	run.cancel()
	run.wait(t, 2*time.Second)
	f.a.HTTP.CloseIdleConnections()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) && (runtime.NumGoroutine() > goroutines+2 || openFDs(t) > fds+2) {
		time.Sleep(20 * time.Millisecond)
	}
	if runtime.NumGoroutine() > goroutines+2 || openFDs(t) > fds+2 {
		t.Fatalf("consumer leaked: goroutines %d->%d fds %d->%d", goroutines, runtime.NumGoroutine(), fds, openFDs(t))
	}
}

func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		t.Skip("fd inspection unavailable")
	}
	return len(entries)
}

type stubRefreshView struct {
	mu      sync.Mutex
	stamps  [2]config.FileStamp
	waiting bool
}

func (v *stubRefreshView) pending() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.waiting
}

func (v *stubRefreshView) aliasStamps() [2]config.FileStamp {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.stamps
}

// T9 wiring: a launch-alias change wakes a quiescent consumer for a local
// reason (the full cycle then runs the ordinary refresh poll), and a pending
// refresh candidate keeps the active loop with no long-poll at all.
func TestQuiescentConsumerAliasAndPendingRefresh(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, time.Minute, 20*time.Second, 100*time.Millisecond)
	known := config.FileStamp{Known: true}
	t.Run("alias change", func(t *testing.T) {
		view := &stubRefreshView{stamps: [2]config.FileStamp{known, known}}
		f := newOrphanDispatchFixture(t)
		run := startQuiescentConsumer(t, f, 300*time.Millisecond, view)
		polled := run.trace.await(t, "poll", time.Time{}, 20*time.Second)
		view.mu.Lock()
		view.stamps[0] = config.FileStamp{Known: true, Present: true, Ino: 7}
		view.mu.Unlock()
		run.trace.await(t, "wake:local", polled, 3*time.Second)
	})
	t.Run("pending candidate", func(t *testing.T) {
		view := &stubRefreshView{stamps: [2]config.FileStamp{known, known}, waiting: true}
		f := newOrphanDispatchFixture(t)
		run := startQuiescentConsumer(t, f, 300*time.Millisecond, view)
		first := run.trace.await(t, "cycle", time.Time{}, 20*time.Second)
		time.Sleep(1500 * time.Millisecond)
		if n := run.trace.count("poll", first, time.Now()); n != 0 {
			t.Fatalf("pending candidate entered quiescence (%d polls)", n)
		}
		if n := run.trace.count("cycle", first, time.Now()); n < 3 {
			t.Fatalf("pending candidate lost its active schedule (%d cycles)", n)
		}
	})
}

// The quiescence predicate: a settled binding is quiescent; every state that
// carries its own deadline or effect is not. Diagnostic history is ignored.
func TestTerminalQuiescentPredicate(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	pid := os.Getpid()
	base := func() config.State {
		return config.State{
			Lease:    &config.Lease{Holder: "owned-thread", Host: machineLabel(), Expires: now.Add(time.Minute)},
			Terminal: &config.TerminalBinding{Generation: "g", SessionID: "owned-thread", Host: machineLabel(), DaemonPID: pid, DaemonBirth: "b", Outcome: "armed: no new mail", Interval: time.Second, RetryMax: time.Second},
		}
	}
	if !terminalQuiescent(base(), "g", pid, now) {
		t.Fatal("positive control: a settled binding must be quiescent")
	}
	yes := true
	no := false
	diagnostic := base()
	diagnostic.Terminal.Recovery = &config.TerminalRecovery{Reason: "x", At: now}
	diagnostic.Terminal.SendRefusal = &config.TerminalSendRefusal{At: now}
	diagnostic.Terminal.OwnNotice.PossiblyOrphaned = &no
	if !terminalQuiescent(diagnostic, "g", pid, now) {
		t.Fatal("diagnostic history must not prevent quiescence")
	}
	for name, mutate := range map[string]func(*config.State){
		"other generation":      func(st *config.State) {},
		"stopped":               func(st *config.State) { st.Terminal.Stopped = true },
		"other consumer":        func(st *config.State) { st.Terminal.DaemonPID = pid + 1 },
		"expired lease":         func(st *config.State) { st.Lease.Expires = now },
		"foreign lease":         func(st *config.State) { st.Lease.Holder = "other" },
		"attempt":               func(st *config.State) { st.Terminal.AttemptIDs = []string{"$e"} },
		"typed":                 func(st *config.State) { st.Terminal.Typed = true },
		"notice":                func(st *config.State) { st.Terminal.Notice = "a2a: 1" },
		"reservation":           func(st *config.State) { st.Notification.Reservation = &config.NotificationReservation{} },
		"cooldown":              func(st *config.State) { st.Notification.NotBefore = now.Add(time.Second) },
		"idle sample":           func(st *config.State) { st.Terminal.Idle = &config.TerminalIdle{At: now} },
		"notice origin":         func(st *config.State) { st.Terminal.NoticeOrigin = &config.TerminalIdle{At: now} },
		"typing started":        func(st *config.State) { st.Terminal.TypingStarted = true },
		"busy outcome":          func(st *config.State) { st.Terminal.Outcome = "suppressed: busy, draft or unknown input" },
		"notice report":         func(st *config.State) { st.Terminal.NoticeReport = &config.NoticeReport{} },
		"consumer episode":      func(st *config.State) { st.Terminal.ConsumerReport = &config.NoticeReport{} },
		"possibly orphaned":     func(st *config.State) { st.Terminal.OwnNotice.PossiblyOrphaned = &yes },
		"publication retry":     func(st *config.State) { st.NoticeRetry = &config.NoticeRetry{} },
		"uncertain publication": func(st *config.State) { st.NoticeWriteUncertain = true },
		"no binding":            func(st *config.State) { st.Terminal = nil },
		"hooks paused":          func(st *config.State) { st.HooksPaused = true },
		"gated outcome":         func(st *config.State) { st.Terminal.Outcome = "suppressed: manual gate" },
	} {
		st := base()
		mutate(&st)
		generation := "g"
		if name == "other generation" {
			generation = "h"
		}
		if terminalQuiescent(st, generation, pid, now) {
			t.Errorf("%s: must not be quiescent", name)
		}
	}
}

// The fork-free start time identifies the running owner: it is stable for a
// live process and refuses a PID that has exited or never existed.
func TestProcessStart(t *testing.T) {
	first, err := processStart(os.Getpid())
	if err != nil || first == "" {
		t.Fatalf("own start: %q %v", first, err)
	}
	if again, err := processStart(os.Getpid()); err != nil || again != first {
		t.Fatalf("own start changed: %q %v", again, err)
	}
	cmd := exec.Command("sleep", "0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if got, err := processStart(cmd.Process.Pid); err == nil {
		t.Fatalf("exited child still has a start time %q", got)
	}
	if _, err := processStart(1 << 30); err == nil {
		t.Fatal("nonexistent pid has a start time")
	}
	if _, err := processStart(1); err == nil {
		t.Fatal("pid 1 must be refused")
	}
}
