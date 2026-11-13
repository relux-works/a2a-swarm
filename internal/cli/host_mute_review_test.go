//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
	"golang.org/x/sys/unix"
)

// A bounded regular scope input remains usable beside FIFO refusal.
func TestReviewScopeRegularBudget(t *testing.T) {
	root := t.TempDir()
	fixture.Enroll(t, fixture.Scope(root))
	var stdout, stderr bytes.Buffer
	app := NewApp()
	app.Stdout, app.Stderr = &stdout, &stderr
	if code := app.Run(context.Background(), []string{"host", "mute", "status", "--scope", scopeFile(t, root), "--budget", "1s"}); code != 0 {
		t.Fatalf("regular-file positive rejected: %s", stderr.String())
	}
}

// A FIFO with no writer refuses promptly; a fixture peer releases only a broken implementation.
func TestReviewScopeFIFOBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scope-fifo")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	app := NewApp()
	app.Stdout, app.Stderr = &stdout, &stderr
	done := make(chan int, 1)
	go func() {
		done <- app.Run(context.Background(), []string{"host", "mute", "status", "--scope", path, "--budget", "10ms"})
	}()
	select {
	case code := <-done:
		if code == 0 || !strings.Contains(stderr.String(), "scope must be a bounded regular JSON file") {
			t.Fatalf("FIFO scope did not receive the intended regular-file refusal: exit=%d error=%q", code, stderr.String())
		}
	case <-time.After(300 * time.Millisecond):
		writer, err := os.OpenFile(path, os.O_RDWR|unix.O_NONBLOCK, 0)
		if err != nil {
			t.Fatal(err)
		}
		writer.Close()
		code := <-done
		t.Fatalf("scope FIFO open outlived 10ms I/O budget by at least 300ms; only returned after fixture writer connected: exit=%d error=%q", code, stderr.String())
	}
}

// Actual dispatch advances incident observations with and without mute, without waking a muted terminal.
func TestReviewMutedAlarmObservation(t *testing.T) {
	for _, muted := range []bool{false, true} {
		name := "unmuted-positive"
		if muted {
			name = "muted"
		}
		t.Run(name, func(t *testing.T) {
			environment, app, session, terminal, now := muteTerminalWorld(t)
			_, err := config.UpdateState(environment.paths(), "alice", func(state *config.State) error {
				state.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 1, Owner: "off", Operator: "operator", Room: "alarm-room", Topic: "alarm-topic"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			app.observeMailAlarm(session, "scope-terminal", alarmSample{Count: 1, Oldest: now.Add(-time.Hour), At: *now, Reason: "none"})
			initial := environment.state("alice").MailAlarm.LastObserved
			if !initial.Equal(*now) {
				t.Fatalf("direct nonblocking observation positive failed: got %v, want %v", initial, *now)
			}
			*now = now.Add(time.Minute)
			if muted {
				fixture.Mute(t, fixture.Scope(environment.dir), "review-incident")
			}
			wake := make(chan struct{}, 1)
			for index := 0; index < 3; index++ {
				if err := app.dispatchTerminal(context.Background(), session, "scope-terminal", wake); err != nil {
					t.Fatal(err)
				}
				*now = now.Add(time.Minute)
			}
			state := environment.state("alice")
			if muted && (terminal.observations != 0 || terminal.types != 0 || terminal.enters != 0) {
				t.Fatal("muted incident observation touched terminal input")
			}
			if len(wake) == 0 {
				t.Fatal("dispatch did not notify alarm worker")
			}
			if !state.MailAlarm.LastObserved.After(initial) {
				t.Fatalf("terminal dispatch lost incident monitoring: no new mail-alarm observation after three dispatch cycles; terminal readiness reads=%d, last observation unchanged=%v", terminal.observations, initial)
			}
		})
	}
}

// Cancellation invalidates prior idle credit; two later healthy observations still permit a wake.
func TestReviewCancelledDispatchInvalidatesIdle(t *testing.T) {
	environment, app, session, terminal, now := muteTerminalWorld(t)
	muteDispatch(t, app, session)
	if environment.state("alice").Terminal.Idle == nil || terminal.types != 0 {
		t.Fatal("initial qualification positive did not establish an idle sample without a wake")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	err := app.dispatchTerminal(cancelled, session, "scope-terminal")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("wrong cancellation result: %v", err)
	}
	retained := environment.state("alice").Terminal.Idle != nil
	*now = now.Add(2 * time.Second)
	muteDispatch(t, app, session)
	if retained || terminal.types != 0 || terminal.enters != 0 {
		t.Fatalf("cancelled dispatch retained pre-interruption idle authority=%t; next cycle typed=%d entered=%d after only one fresh observation", retained, terminal.types, terminal.enters)
	}
	*now = now.Add(2 * time.Second)
	muteDispatch(t, app, session)
	if terminal.types != 1 || terminal.enters != 1 {
		t.Fatal("two fresh post-cancellation observations did not wake")
	}
}

// Failed safety cleanup is explicit, never downgraded to a successful dispatch.
// The consumer's existing restart/interruption reset restores two-sample qualification.
func TestReviewCancelledDispatchCleanupFailure(t *testing.T) {
	e, a, s, terminal, now := muteTerminalWorld(t)
	muteDispatch(t, a, s)
	lockPath := filepath.Join(filepath.Dir(e.paths().StateFile("alice")), "alice.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err = a.dispatchTerminal(ctx, s, "scope-terminal")
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "terminal idle invalidation failed") || time.Since(started) > nativeRPCTimeout+2*time.Second {
		t.Fatalf("failed safety cleanup was hidden or unbounded: %v", err)
	}
	if terminal.types != 0 || terminal.enters != 0 {
		t.Fatal("failed cleanup permitted input")
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	// runTerminalWithRefresh starts and retries with this same mandatory reset.
	if err := a.resetTerminalObservations(s, "scope-terminal"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(2 * time.Second)
	muteDispatch(t, a, s)
	if terminal.types != 0 || terminal.enters != 0 {
		t.Fatal("reset reused pre-failure idle qualification")
	}
	*now = now.Add(2 * time.Second)
	muteDispatch(t, a, s)
	if terminal.types != 1 || terminal.enters != 1 {
		t.Fatal("healthy qualification did not recover")
	}
}

// A deadline-now alarm probe attempts a free lock once, never waits behind a
// holder, and never calls its callback after outer-cycle cancellation.
func TestReviewAlarmProbeNonblockingContext(t *testing.T) {
	e, a, _, _, _ := muteTerminalWorld(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.wakeContext = ctx
	called := 0
	fn := func(*config.State) error { called++; return nil }
	if _, err := a.updateStateBy(e.paths(), "alice", time.Now(), fn); err != nil || called != 1 {
		t.Fatalf("free one-shot probe: called=%d err=%v", called, err)
	}
	lockPath := filepath.Join(filepath.Dir(e.paths().StateFile("alice")), "alice.lock")
	f, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = a.updateStateBy(e.paths(), "alice", time.Now(), fn)
	if !errors.Is(err, config.ErrLockTimeout) || called != 1 || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("probe waited or invoked held callback: called=%d err=%v", called, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := a.updateStateBy(e.paths(), "alice", time.Now(), fn); !errors.Is(err, context.Canceled) || called != 1 {
		t.Fatalf("cancelled probe invoked callback: called=%d err=%v", called, err)
	}
}

// A retired generation needs no idle cleanup: its replacement remains untouched,
// and callers retain the established binding-retired rejection contract.
func TestReviewCancelledCleanupRetiredBinding(t *testing.T) {
	e, a, s, terminal, _ := muteTerminalWorld(t)
	muteDispatch(t, a, s)
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.Generation = "replacement"; return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	err = a.dispatchTerminal(context.Background(), s, "scope-terminal")
	if err == nil || err.Error() != "binding retired" {
		t.Fatalf("retired binding contract changed: %v", err)
	}
	after, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || terminal.types != 0 || terminal.enters != 0 {
		t.Fatal("retired cleanup changed replacement authority or input")
	}
}
