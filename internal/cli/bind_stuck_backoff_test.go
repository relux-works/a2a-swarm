//go:build linux || darwin

package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
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
