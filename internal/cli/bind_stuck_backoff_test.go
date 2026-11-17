//go:build linux || darwin

package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// TestTerminalStuckUnavailablePredicate proves a suppressed-input binding
// with nothing else pending is stuck, an armed binding is not, and an
// attempt, notice, reservation or cooldown (or any other outstanding
// deadline state) is never stuck.
func TestTerminalStuckUnavailablePredicate(t *testing.T) {
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	pid := os.Getpid()
	base := func() config.State {
		return config.State{
			Lease:    &config.Lease{Holder: "owned-thread", Host: machineLabel(), Expires: now.Add(time.Minute)},
			Terminal: &config.TerminalBinding{Generation: "g", SessionID: "owned-thread", Host: machineLabel(), DaemonPID: pid, DaemonBirth: "b", Outcome: "suppressed: busy, draft or unknown input", Interval: time.Second, RetryMax: time.Second},
		}
	}
	if !terminalStuckUnavailable(base(), "g", pid, now) {
		t.Fatal("positive control: suppressed with nothing pending must be stuck")
	}
	armed := base()
	armed.Terminal.Outcome = "armed: no new mail"
	if terminalStuckUnavailable(armed, "g", pid, now) || !terminalQuiescent(armed, "g", pid, now) {
		t.Fatal("armed control must be quiescent, not stuck")
	}
	if terminalQuiescent(base(), "g", pid, now) {
		t.Fatal("suppressed control must not be quiescent")
	}
	yes := true
	for name, mutate := range map[string]func(*config.State){
		"attempt pending":   func(st *config.State) { st.Terminal.AttemptIDs = []string{"$e"} },
		"notice pending":    func(st *config.State) { st.Terminal.Notice = "a2a: 1" },
		"typed pending":     func(st *config.State) { st.Terminal.Typed = true },
		"typing started":    func(st *config.State) { st.Terminal.TypingStarted = true },
		"reservation":       func(st *config.State) { st.Notification.Reservation = &config.NotificationReservation{} },
		"cooldown":          func(st *config.State) { st.Notification.NotBefore = now.Add(time.Second) },
		"idle sample":       func(st *config.State) { st.Terminal.Idle = &config.TerminalIdle{At: now} },
		"notice origin":     func(st *config.State) { st.Terminal.NoticeOrigin = &config.TerminalIdle{At: now} },
		"notice report":     func(st *config.State) { st.Terminal.NoticeReport = &config.NoticeReport{} },
		"consumer episode":  func(st *config.State) { st.Terminal.ConsumerReport = &config.NoticeReport{} },
		"possibly orphaned": func(st *config.State) { st.Terminal.OwnNotice.PossiblyOrphaned = &yes },
		"publication retry": func(st *config.State) { st.NoticeRetry = &config.NoticeRetry{} },
		"expired lease":     func(st *config.State) { st.Lease.Expires = now },
		"stopped":           func(st *config.State) { st.Terminal.Stopped = true },
		"manual gate":       func(st *config.State) { st.Terminal.Outcome = "suppressed: manual gate" },
		"hooks paused":      func(st *config.State) { st.HooksPaused = true },
		"no binding":        func(st *config.State) { st.Terminal = nil },
	} {
		st := base()
		mutate(&st)
		if terminalStuckUnavailable(st, "g", pid, now) {
			t.Errorf("%s: must not be stuck", name)
		}
	}
	if terminalStuckUnavailable(base(), "other", pid, now) {
		t.Error("other generation: must not be stuck")
	}
	if terminalStuckUnavailable(base(), "g", pid+1, now) {
		t.Error("other consumer: must not be stuck")
	}
}

// TestTerminalCycleWorkStuckSchedule proves the stuck wait schedule doubles
// from Interval to the 60 s cap, runs at most 15 full cycles in a simulated
// 10 minutes, and returns to Interval after a reset.
func TestTerminalCycleWorkStuckSchedule(t *testing.T) {
	var b stuckBackoff
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		if got := b.next(2 * time.Second); got != w*time.Second {
			t.Fatalf("wait %d = %v, want %v", i, got, w*time.Second)
		}
	}
	b.reset()
	if got := b.next(2 * time.Second); got != 2*time.Second {
		t.Fatalf("after reset wait = %v, want Interval", got)
	}
	var sim stuckBackoff
	cycles := 1 // the cycle at t=0
	for elapsed := time.Duration(0); ; {
		elapsed += sim.next(2 * time.Second)
		if elapsed > 10*time.Minute {
			break
		}
		cycles++
	}
	if cycles > 15 {
		t.Fatalf("simulated cycles in 10 minutes = %d, want at most 15", cycles)
	}
	t.Logf("simulated cycles in 10 minutes: %d", cycles)
	var one stuckBackoff
	if got := one.next(time.Second); got != time.Second {
		t.Fatalf("first wait at 1 s Interval = %v", got)
	}
}

// TestTerminalStuckBackoffWakeOnMail proves the stuck wait ends at once when
// the mailbox file changes during a long backed-off wait, beside a control
// wait with no change that runs to its timer.
func TestTerminalStuckBackoffWakeOnMail(t *testing.T) {
	p := config.Paths{Base: t.TempDir()}
	mailbox := p.MailboxFile("fixture")
	if err := os.MkdirAll(filepath.Dir(mailbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mailbox, []byte("{\"op\":\"seenby\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	a := &App{Stdout: io.Discard, Stderr: io.Discard}
	s := &session{app: a, paths: p, name: "fixture"}
	policy := config.TerminalBinding{OwnerPID: os.Getpid(), Interval: 10 * time.Millisecond}
	owner := &ownerIdentity{}
	ctx := context.Background()
	start := time.Now()
	if got := a.waitStuck(ctx, s, 300*time.Millisecond, policy, owner, nil); got != "" {
		t.Fatalf("unchanged control woke early: %q", got)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("unchanged control returned after %v, before its timer", elapsed)
	}
	done := make(chan string, 1)
	go func() { done <- a.waitStuck(ctx, s, 30*time.Second, policy, owner, nil) }()
	time.Sleep(100 * time.Millisecond)
	f, err := os.OpenFile(mailbox, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"op\":\"seenby\"}\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != "wake" {
			t.Fatalf("mail change wake = %q, want wake", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mail change during a backed-off wait did not wake at once")
	}
}

// stuckProbeTerminal is a terminal fake whose readiness flips atomically:
// nonempty input (never eligible) until eligible is set, then empty input
// with a valid key. It replaces terminal I/O only; the loop, dispatch and
// reservation paths under it are production code.
type stuckProbeTerminal struct {
	b            config.TerminalBinding
	eligible     atomic.Bool
	observations atomic.Int64
	types        atomic.Int64
	enters       atomic.Int64
}

func (f *stuckProbeTerminal) assessment() tmuxwake.ReadinessAssessment {
	state := tmuxwake.InputNonempty
	if f.eligible.Load() {
		state = tmuxwake.InputEmpty
	}
	return tmuxwake.ReadinessAssessment{
		Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeReady, Reason: tmuxwake.RuntimeNone},
		Input:   tmuxwake.InputAssessment{State: state, Reason: tmuxwake.InputReasonNone},
		Key:     tmuxwake.ReadinessKey{Route: tmuxwake.LegacyCodex, Target: f.b.Target, Generation: f.b.Generation, SessionID: f.b.SessionID, PaneWidth: 120},
	}
}

func (f *stuckProbeTerminal) ObserveReadiness(context.Context, string) tmuxwake.ReadinessAssessment {
	f.observations.Add(1)
	return f.assessment()
}

func (f *stuckProbeTerminal) AssessOwnedNotice(context.Context, string) tmuxwake.ReadinessAssessment {
	return f.assessment()
}

func (f *stuckProbeTerminal) Send(ctx context.Context, _ int, _ tmuxwake.ReadinessKey, before, after func()) (tmuxwake.SendResult, error) {
	if before != nil {
		before()
	}
	if ctx.Err() != nil {
		return tmuxwake.SendResult{}, ctx.Err()
	}
	f.types.Add(1)
	if after != nil {
		after()
	}
	f.enters.Add(1)
	return tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, nil
}

func (f *stuckProbeTerminal) ResumeNotice(_ context.Context, _ int, _ string, _ tmuxwake.ReadinessKey, before ...func() error) (tmuxwake.SendResult, error) {
	for _, fence := range before {
		if err := fence(); err != nil {
			return tmuxwake.SendResult{}, err
		}
	}
	f.enters.Add(1)
	return tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, nil
}

// stuckBackoffCapForTest shortens the stuck cap seam for one test.
func stuckBackoffCapForTest(t *testing.T, d time.Duration) {
	t.Helper()
	old := terminalStuckBackoffMax
	terminalStuckBackoffMax = d
	t.Cleanup(func() { terminalStuckBackoffMax = old })
}

func appendPendingStuckMail(t *testing.T, f *terminalWorkFixture, id string) {
	t.Helper()
	event, _ := json.Marshal(map[string]any{"type": "m.room.message", "content": map[string]string{"body": "x"}})
	line, _ := json.Marshal(map[string]any{"op": "add", "entry": config.MailEntry{ID: id, Kind: config.MailEvent, Room: "!fixture:fixture.invalid", Event: event}})
	file, err := os.OpenFile(f.s.paths.MailboxFile(f.s.name), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

// startStuckLoop runs the real consumer loop over the work fixture with one
// pending un-rung entry and a pane that stays ineligible until flipped.
func startStuckLoop(t *testing.T, interval time.Duration) (*terminalWorkFixture, *stuckProbeTerminal, *consumerRun) {
	t.Helper()
	f := newTerminalWorkFixture(t)
	appendPendingStuckMail(t, f, "$stuck00001")
	fake := &stuckProbeTerminal{}
	f.a.terminalFixture = func(b config.TerminalBinding) terminalEffects { fake.b = b; return fake }
	f.a.Now = time.Now
	initial := f.initial
	initial.Interval = interval
	initial.RetryMax = max(interval, time.Second)
	if _, err := config.UpdateState(f.s.paths, f.s.name, func(st *config.State) error {
		st.Terminal.Interval = initial.Interval
		st.Terminal.RetryMax = initial.RetryMax
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	run := &consumerRun{trace: &terminalTraceLog{}, done: make(chan struct{})}
	f.a.terminalTrace = run.trace.add
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	run.cancel = cancel
	go func() {
		defer close(run.done)
		run.outcome, run.err = f.a.runTerminalLoop(ctx, f.s, "fixture", initial, nil, nil)
	}()
	t.Cleanup(func() {
		cancel()
		<-run.done
	})
	return f, fake, run
}

func awaitEnters(t *testing.T, fake *stuckProbeTerminal, want int64, within time.Duration) {
	t.Helper()
	until := time.Now().Add(within)
	for time.Now().Before(until) {
		if fake.enters.Load() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("ring not observed within %v (enters=%d)", within, fake.enters.Load())
}

// TestTerminalStuckProbeWakesWithinInterval proves that once the stuck
// back-off has reached its cap, readiness becoming eligible — with no State,
// mailbox or local-file change and mail already pending — runs the next
// full cycle within one Interval of probing (plus probe execution slack)
// and the pending mail is then rung; beside it, the no-change control
// inside the same capped wait runs no full cycle and rings nothing while
// probes keep running every Interval.
func TestTerminalStuckProbeWakesWithinInterval(t *testing.T) {
	stuckBackoffCapForTest(t, 4*time.Second)
	interval := 250 * time.Millisecond
	_, fake, run := startStuckLoop(t, interval)
	// Waits double 250ms, 500ms, 1s, 2s, then the 4s cap: the fifth stuck
	// wait is the first capped one.
	cappedAt := run.trace.nth(t, "wait:stuck", 5, 30*time.Second)
	time.Sleep(1200 * time.Millisecond)
	if n := run.trace.count("cycle", cappedAt, time.Now()); n != 0 {
		t.Fatalf("control: %d full cycles ran inside the capped wait with no readiness change", n)
	}
	if fake.enters.Load() != 0 {
		t.Fatal("control: mail was rung while readiness stayed ineligible")
	}
	if n := run.trace.count("probe:stuck", cappedAt, time.Now()); n < 2 {
		t.Fatalf("control: %d readiness probes during 1.2s of capped wait, want probes every Interval", n)
	}
	flip := time.Now()
	fake.eligible.Store(true)
	woke := run.trace.await(t, "cycle", flip, 1500*time.Millisecond)
	if latency := woke.Sub(flip); latency > interval+time.Second {
		t.Fatalf("turn-end latency %v after readiness became eligible, want within one Interval (%v) plus probe slack; the capped wait had %v left", latency, interval, 4*time.Second-flip.Sub(cappedAt))
	} else {
		t.Logf("turn-end latency after eligibility flip: %v (Interval %v, capped wait 4s)", latency, interval)
	}
	awaitEnters(t, fake, 1, 5*time.Second)
}

// TestTerminalStuckProbeNoStateOrMailboxDecode proves the stuck wait's
// readiness probes perform no State load, State decode, seal digest or
// mailbox decode/parse (global counting counters stay at zero while probes
// run), beside the eligible control in which a probe ends the wait at once
// with the same zero counts.
func TestTerminalStuckProbeNoStateOrMailboxDecode(t *testing.T) {
	p := config.Paths{Base: t.TempDir()}
	mailbox := p.MailboxFile("fixture")
	if err := os.MkdirAll(filepath.Dir(mailbox), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mailbox, []byte("{\"op\":\"seenby\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &stuckProbeTerminal{}
	a := &App{Stdout: io.Discard, Stderr: io.Discard}
	a.terminalFixture = func(b config.TerminalBinding) terminalEffects { fake.b = b; return fake }
	s := &session{app: a, paths: p, name: "fixture"}
	policy := config.TerminalBinding{Generation: "g", SessionID: "s", OwnerPID: os.Getpid(), Interval: 20 * time.Millisecond, Target: tmuxwake.Target{Runtime: "codex", OwnerPID: os.Getpid(), OwnerBirth: "b", Pane: "%0", SocketID: "sock"}}
	owner := &ownerIdentity{}
	zero := func(before, after config.StateDecodeStats) {
		t.Helper()
		if d := after.LoadStateCalls - before.LoadStateCalls; d != 0 {
			t.Fatalf("probe loaded State %d times", d)
		}
		if d := after.StateReadAttempts - before.StateReadAttempts; d != 0 {
			t.Fatalf("probe read State %d times", d)
		}
		if d := after.UnmarshalStatePasses - before.UnmarshalStatePasses; d != 0 {
			t.Fatalf("probe decoded State %d times", d)
		}
		if d := after.StateDigests - before.StateDigests; d != 0 {
			t.Fatalf("probe computed %d seal digests", d)
		}
		if d := after.WriterSealChecks - before.WriterSealChecks; d != 0 {
			t.Fatalf("probe checked %d writer seals", d)
		}
		if d := after.MailboxDecodes - before.MailboxDecodes; d != 0 {
			t.Fatalf("probe fully decoded the mailbox %d times", d)
		}
		if d := after.MailboxParseBytes - before.MailboxParseBytes; d != 0 {
			t.Fatalf("probe parsed %d mailbox bytes", d)
		}
		if d := after.MailboxIncrementalReads - before.MailboxIncrementalReads; d != 0 {
			t.Fatalf("probe opened the mailbox incrementally %d times", d)
		}
	}
	before := config.ReadStateDecodeStats()
	if got := a.waitStuck(context.Background(), s, 220*time.Millisecond, policy, owner, nil); got != "" {
		t.Fatalf("ineligible probes woke the wait: %q", got)
	}
	zero(before, config.ReadStateDecodeStats())
	if n := fake.observations.Load(); n < 3 {
		t.Fatalf("only %d readiness probes in a 220ms stuck wait at 20ms Interval", n)
	}
	fake.eligible.Store(true)
	before = config.ReadStateDecodeStats()
	start := time.Now()
	if got := a.waitStuck(context.Background(), s, 30*time.Second, policy, owner, nil); got != "wake" {
		t.Fatalf("eligible control wake = %q, want wake", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("eligible control took %v to wake", elapsed)
	}
	zero(before, config.ReadStateDecodeStats())
}

// TestTerminalStuckLoopFullCyclesBounded proves through the real loop's
// cycle traces that a consumer whose readiness stays unavailable runs at
// most 15 full cycles per simulated 10 minutes: Interval 50ms stands for
// 2s and the 1.5s cap for 60s (the same ratio), so 15s of real waits stand
// for 600s; readiness probes keep running every Interval meanwhile and
// nothing is rung.
func TestTerminalStuckLoopFullCyclesBounded(t *testing.T) {
	stuckBackoffCapForTest(t, 1500*time.Millisecond)
	_, fake, run := startStuckLoop(t, 50*time.Millisecond)
	from := run.trace.nth(t, "cycle", 1, 20*time.Second)
	time.Sleep(15 * time.Second)
	to := time.Now()
	cycles := run.trace.count("cycle", from, to)
	probes := run.trace.count("probe:stuck", from, to)
	t.Logf("simulated 10 minutes stuck: %d full cycles, %d readiness probes in %v", cycles, probes, to.Sub(from))
	if cycles > 15 {
		t.Fatalf("%d full cycles in simulated 10 minutes stuck, want at most 15", cycles)
	}
	if cycles < 5 {
		t.Fatalf("only %d full cycles; the loop did not keep cycling while stuck", cycles)
	}
	if probes < 100 {
		t.Fatalf("only %d readiness probes while stuck; probes must run every Interval", probes)
	}
	if fake.enters.Load() != 0 {
		t.Fatal("mail was rung while readiness stayed ineligible")
	}
}

// TestTerminalStuckWakeResetsBackoff proves through the real loop that new
// mail during a capped stuck wait wakes at once (production decode-scope
// branch) and resets the schedule: measuring the second wait after the
// wake (the first can re-wake on the same foreign-write evidence the wake
// cycle recorded), its cycle-to-cycle gap is back near Interval, not the
// 3s cap the schedule had reached; once readiness becomes eligible the
// appended mail is rung with the mail that was already pending.
func TestTerminalStuckWakeResetsBackoff(t *testing.T) {
	stuckBackoffCapForTest(t, 3*time.Second)
	f, fake, run := startStuckLoop(t, 100*time.Millisecond)
	// Waits double 100ms, 200ms, 400ms, 800ms, 1.6s, then the 3s cap: the
	// sixth stuck wait is the first capped one.
	run.trace.nth(t, "wait:stuck", 6, 30*time.Second)
	sent := time.Now()
	appendPendingStuckMail(t, f, "$stuck00002")
	wakeCycle := run.trace.await(t, "cycle", sent, 2*time.Second)
	secondCycle := run.trace.await(t, "cycle", wakeCycle.Add(time.Millisecond), 5*time.Second)
	thirdCycle := run.trace.await(t, "cycle", secondCycle.Add(time.Millisecond), 6*time.Second)
	t.Logf("wake traces in the second post-wake wait: %d", run.trace.count("wake:stuck", secondCycle, thirdCycle))
	if gap := thirdCycle.Sub(secondCycle); gap > 1500*time.Millisecond {
		t.Fatalf("second wait after the mail wake was %v (cycle-to-cycle), want the back-off reset to Interval (cap is 3s)", gap)
	} else {
		t.Logf("cycle-to-cycle gap of the second post-wake wait: %v (Interval 100ms, cap 3s)", gap)
	}
	fake.eligible.Store(true)
	awaitEnters(t, fake, 1, 5*time.Second)
}

// BenchmarkTerminalStuckProbe measures one production stuck readiness probe
// against the fixture tmux target (Inspect and capture only; an
// unrecognized screen is never eligible), with no State or mailbox work.
func BenchmarkTerminalStuckProbe(b *testing.B) {
	target, _, _ := usabilityTarget(b, "codex")
	policy := config.TerminalBinding{Generation: "fixture", SessionID: "fixture", Target: target, Interval: time.Second}
	a := &App{Stdout: io.Discard, Stderr: io.Discard}
	ctx := context.Background()
	if a.probeStuckReadiness(ctx, policy) {
		b.Fatal("unrecognized fixture screen must not probe eligible")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = a.probeStuckReadiness(ctx, policy)
	}
}
