//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// quiescentMuteWorld is muteTerminalWorld with an optional pending message, so
// the zero-pending composition of host mute and quiescence can be exercised.
func quiescentMuteWorld(t *testing.T, pending bool) (*env, *App, *session, *muteTerminal, *time.Time, string) {
	t.Helper()
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	if pending {
		runtimeMail(e, room, "pending")
	}
	now := time.Now()
	e.now = func() time.Time { return now }
	a, s := muteApp(e)
	target := tmuxwake.Target{Runtime: "codex", OwnerPID: 42, OwnerBirth: "fixture-birth", Pane: "%1", Socket: "/fixture-only", SocketID: "fixture-socket"}
	b := config.TerminalBinding{Generation: "scope-terminal", SessionID: "fixture-session", Host: machineLabel(), OwnerPID: 42, OwnerBirth: target.OwnerBirth, DaemonPID: 43, DaemonBirth: "fixture-consumer", Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 30}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &b
		st.Lease = &config.Lease{Holder: b.SessionID, Host: b.Host, PID: 42, Expires: now.Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fake := &muteTerminal{b: b}
	a.terminalFixture = func(binding config.TerminalBinding) terminalEffects { fake.b = binding; return fake }
	fixture.Enroll(t, wakemute.Scope{{Root: e.dir, Connections: []wakemute.Connection{{Path: "."}}}})
	return e, a, s, fake, &now, room
}

// quietAfterDispatch is the exact predicate runTerminalLoop evaluates after a
// dispatch: a failed dispatch is never quiet, a successful one is quiet only
// with no un-rung pending entry and a settled binding (consumer PID 43 here).
func quietAfterDispatch(e *env, s *session, dispatchErr error, now time.Time) bool {
	if dispatchErr != nil {
		return false
	}
	return s.terminalUndelivered == 0 && terminalQuiescent(e.state("alice"), "scope-terminal", 43, now)
}

// quietWhy names the first predicate term that is false, for failure messages.
func quietWhy(e *env, s *session, now time.Time) string {
	st := e.state("alice")
	b := st.Terminal
	if b == nil {
		return "no binding"
	}
	return fmt.Sprintf("undelivered=%d refresh=%q outcome=%q idle=%t hooks_paused=%t not_before=%s",
		s.terminalUndelivered, terminalRefreshBlockReason(st, "scope-terminal", b.DaemonBirth, machineLabel(), 43, now),
		b.Outcome, b.Idle != nil, st.HooksPaused, st.Notification.NotBefore)
}

// Proves, for an ACTIVE mute: no cycle touches the terminal; a cycle with an
// un-rung pending entry is never quiet (the entry is counted before mute
// admission); and mail arriving DURING the mute in a previously settled
// zero-pending world turns the next cycle not quiet, still without typing.
// After thaw the held mail is delivered (positive control).
// Two API boundaries are asserted separately: the store admission refuses
// with ErrMuted, while dispatchTerminal reports a healthy mute refusal as nil.
// Limit: whether a zero-pending consumer may stay quiescent while muted is
// left to the contract and only reported, not asserted.
func TestQuiescentMuteActive(t *testing.T) {
	for _, pending := range []bool{true, false} {
		t.Run(map[bool]string{true: "pending", false: "zero-pending-then-mail"}[pending], func(t *testing.T) {
			e, a, s, fake, now, room := quiescentMuteWorld(t, pending)
			muteDispatch(t, a, s)
			*now = now.Add(2 * time.Second)
			operation := fixture.Mute(t, fixture.Scope(e.dir), "quiescent-active")
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			admitted := false
			if err := wakemute.Admission(ctx, e.dir, ".", func(uint64) error { admitted = true; return nil }); !errors.Is(err, wakemute.ErrMuted) || admitted {
				t.Fatalf("store admission under mute: err=%v admitted=%t", err, admitted)
			}
			cancel()
			cycle := func(i int) bool {
				err := a.dispatchTerminal(t.Context(), s, "scope-terminal")
				if err != nil {
					t.Fatalf("cycle %d: dispatch must report a healthy mute refusal as nil, got %v", i, err)
				}
				q := quietAfterDispatch(e, s, err, *now)
				*now = now.Add(2 * time.Second)
				return q
			}
			for i := 0; i < 3; i++ {
				q := cycle(i)
				if pending && q {
					t.Fatalf("cycle %d: held wake with pending mail counted as quiet", i)
				}
				if !pending {
					t.Logf("zero-pending muted cycle %d quiet=%t (%s)", i, q, quietWhy(e, s, *now))
				}
			}
			if !pending {
				runtimeMail(e, room, "arrives-while-muted")
				for i := 3; i < 6; i++ {
					if cycle(i) {
						t.Fatalf("cycle %d: mail arriving under mute counted as quiet", i)
					}
				}
			}
			if fake.types != 0 || fake.enters != 0 {
				t.Fatal("terminal admitted while muted")
			}
			fixture.Unmute(t, operation)
			for i := 0; i < 3; i++ {
				if err := a.dispatchTerminal(t.Context(), s, "scope-terminal"); err != nil {
					t.Fatalf("post-thaw cycle %d: %v", i, err)
				}
				*now = now.Add(2 * time.Second)
			}
			if fake.types != 1 || fake.enters != 1 {
				t.Fatalf("post-thaw delivery positive refused: types=%d enters=%d", fake.types, fake.enters)
			}
			if quietAfterDispatch(e, s, nil, *now) {
				t.Fatal("outstanding notice counted as quiet")
			}
		})
	}
}

// Proves: with zero pending, an unmuted consumer is quiet (positive control);
// a mute PULSE (mute then thaw between cycles) makes the next cycle suppressed
// and not quiet, and the following cycle in the same scope generation returns
// to quiet. With pending mail the pulse never yields quiet, because the
// un-rung entry is counted before mute admission.
func TestQuiescentMutePulse(t *testing.T) {
	t.Run("zero-pending", func(t *testing.T) {
		e, a, s, fake, now, _ := quiescentMuteWorld(t, false)
		err := a.dispatchTerminal(t.Context(), s, "scope-terminal")
		if !quietAfterDispatch(e, s, err, *now) {
			t.Fatalf("unmuted zero-pending control not quiet: %v; %s", err, quietWhy(e, s, *now))
		}
		op := fixture.Mute(t, fixture.Scope(e.dir), "quiescent-pulse")
		fixture.Unmute(t, op)
		*now = now.Add(2 * time.Second)
		err = a.dispatchTerminal(t.Context(), s, "scope-terminal")
		if quietAfterDispatch(e, s, err, *now) {
			t.Fatal("first cycle after a pulse counted as quiet")
		}
		if got := e.state("alice").Terminal.Outcome; got != "suppressed: scope generation changed; fresh readiness required" {
			t.Fatalf("pulse outcome %q", got)
		}
		*now = now.Add(2 * time.Second)
		err = a.dispatchTerminal(t.Context(), s, "scope-terminal")
		if !quietAfterDispatch(e, s, err, *now) {
			t.Fatalf("same-generation cycle after a pulse not quiet: %v; %s", err, quietWhy(e, s, *now))
		}
		if fake.types != 0 || fake.enters != 0 {
			t.Fatal("zero-pending world touched the terminal")
		}
	})
	t.Run("pending", func(t *testing.T) {
		e, a, s, _, now, _ := quiescentMuteWorld(t, true)
		muteDispatch(t, a, s)
		op := fixture.Mute(t, fixture.Scope(e.dir), "quiescent-pulse-pending")
		fixture.Unmute(t, op)
		*now = now.Add(2 * time.Second)
		err := a.dispatchTerminal(t.Context(), s, "scope-terminal")
		if s.terminalUndelivered < 1 {
			t.Fatalf("pending entry not counted before mute requalification: %d", s.terminalUndelivered)
		}
		if quietAfterDispatch(e, s, err, *now) {
			t.Fatal("held wake with pending mail counted as quiet")
		}
	})
}
