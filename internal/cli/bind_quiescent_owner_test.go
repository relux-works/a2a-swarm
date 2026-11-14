//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// nth returns the time of the n-th event with prefix (1-based), waiting up to
// within for it to happen.
func (l *terminalTraceLog) nth(t *testing.T, prefix string, n int, within time.Duration) time.Time {
	t.Helper()
	until := time.Now().Add(within)
	for {
		l.mu.Lock()
		seen := 0
		for i, e := range l.events {
			if strings.HasPrefix(e, prefix) {
				seen++
				if seen == n {
					at := l.at[i]
					l.mu.Unlock()
					return at
				}
			}
		}
		events := append([]string(nil), l.events...)
		l.mu.Unlock()
		if time.Now().After(until) {
			t.Fatalf("trace %q #%d not observed; events=%v", prefix, n, events)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ownerBoundary describes one cycle-boundary replacement shape. The native
// start token and the canonical Birth seams answer from the number of full
// cycles that have BEGUN, so a change becomes visible exactly at a cycle
// boundary (between two waits), never inside a wait that already read it.
type ownerBoundary struct {
	gated     bool // HooksPaused: ACTIVE waits only
	shiftFrom int  // first cycle whose waits see a different valid token (0: never)
	unknownAt int  // a cycle whose waits read an unknown token (0: none)
	birthFrom int  // first cycle whose canonical registration Birth is unavailable (0: never)
}

// runOwnerBoundary runs the real consumer loop (1s Interval, 100ms tick, 1s
// quiescent reconciliation) over the live orphan fixture owner with the
// boundary seams installed before the consumer starts.
func runOwnerBoundary(t *testing.T, b ownerBoundary) *consumerRun {
	t.Helper()
	quiescentTiming(t, 0, time.Second, 20*time.Second, 100*time.Millisecond)
	var trace atomic.Pointer[terminalTraceLog]
	cycles := func() int {
		if l := trace.Load(); l != nil {
			return l.count("cycle", time.Time{}, time.Now().Add(time.Hour))
		}
		return 0
	}
	processSeams(t, func(ctx context.Context, pid int) (string, error) {
		if b.birthFrom > 0 && cycles() >= b.birthFrom {
			return "", errors.New("canonical inspection unavailable")
		}
		return tmuxwake.Birth(ctx, pid)
	}, func(pid int) (string, error) {
		v, err := processStart(pid)
		if err != nil {
			return v, err
		}
		n := cycles()
		switch {
		case b.unknownAt > 0 && n == b.unknownAt:
			return "", errors.New("transient inspection failure")
		case b.shiftFrom > 0 && n >= b.shiftFrom:
			return shiftedStart(v), nil
		}
		return v, nil
	})
	f := newOrphanDispatchFixture(t)
	if b.gated {
		f.update(func(st *config.State) { st.HooksPaused = true })
	}
	run := startQuiescentConsumer(t, f, time.Second, nil)
	trace.Store(run.trace)
	return run
}

// ownerBoundaryCases are the review 5898673841 boundary shapes
// (native-pid-reuse-adopted, repeat across waits), each with the cycle from
// which the different token is visible:
//   - active-to-active: a gated lane; the token changes between two ACTIVE
//     waits (from cycle 2);
//   - active-unknown-then-replaced: the waits of cycle 2 read unknown, those
//     of cycle 3 a different token; the unknown read must not erase the
//     identity, so the change is still a replacement;
//   - quiescent-reregistration: a quiet lane reconciles after 1s and
//     re-registers in cycle 2 with a different token;
//   - quiescent-to-active-canonical-unavailable: from cycle 2 the canonical
//     registration Birth is unavailable, so the loop falls back to the ACTIVE
//     wait, whose native read returns a different token.
var ownerBoundaryCases = []struct {
	name string
	b    ownerBoundary
	from int // the cycle whose start begins the 3s stop bound
}{
	{"active-to-active", ownerBoundary{gated: true, shiftFrom: 2}, 2},
	{"active-unknown-then-replaced", ownerBoundary{gated: true, unknownAt: 2, shiftFrom: 3}, 3},
	{"quiescent-reregistration", ownerBoundary{shiftFrom: 2}, 2},
	{"quiescent-to-active-canonical-unavailable", ownerBoundary{shiftFrom: 2, birthFrom: 2}, 2},
}

func ownerBoundaryError(t *testing.T, name string, b ownerBoundary, from int) error {
	t.Helper()
	run := runOwnerBoundary(t, b)
	at := run.trace.nth(t, "cycle", from, 20*time.Second)
	return stopWithin(run, at, 3*time.Second, name+": owner replaced across a wait boundary")
}

// Review 5898673841 (native-pid-reuse-adopted across waits). The owner's
// native identity is kept for the consumer's lifetime: a different valid
// token first seen in a LATER wait, in either scheduler mode or across a mode
// change, is a known replacement and stops the consumer within 3s.
// Limit: reuse is modelled through the native-inspection seam, not real
// kernel PID reuse.
func TestQuiescentOwnerIdentityAcrossWaits(t *testing.T) {
	isolateBindTest(t)
	for _, c := range ownerBoundaryCases {
		t.Run(c.name, func(t *testing.T) {
			if err := ownerBoundaryError(t, c.name, c.b, c.from); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Controls for the lifetime identity: an unchanged token across many cycles
// in both modes, and an unknown read between cycles followed by the SAME
// token, never stop a consumer whose owner is live.
func TestQuiescentOwnerIdentityAcrossWaitsControls(t *testing.T) {
	isolateBindTest(t)
	for _, c := range []struct {
		name string
		b    ownerBoundary
	}{
		{"active-unchanged", ownerBoundary{gated: true}},
		{"quiescent-unchanged", ownerBoundary{}},
		{"active-unknown-then-same", ownerBoundary{gated: true, unknownAt: 2}},
		{"quiescent-canonical-unavailable-same", ownerBoundary{birthFrom: 2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := runOwnerBoundary(t, c.b)
			run.trace.nth(t, "cycle", 4, 20*time.Second)
			select {
			case <-run.done:
				t.Fatalf("a live, unchanged owner stopped the consumer: %q", run.outcome)
			default:
			}
		})
	}
}

// Calibration (narrowing mutant): the watch and teardown stay, but the owner
// identity is forgotten at every wait. The UNCHANGED boundary witness must
// fail on its intended assertion (not stopped within 3s) for every shape.
func TestQuiescentOwnerIdentityAcrossWaitsCalibration(t *testing.T) {
	isolateBindTest(t)
	terminalOwnerIdentityPerWait = true
	t.Cleanup(func() { terminalOwnerIdentityPerWait = false })
	for _, c := range ownerBoundaryCases {
		t.Run(c.name, func(t *testing.T) {
			err := ownerBoundaryError(t, c.name, c.b, c.from)
			if err == nil || !strings.Contains(err.Error(), "not stopped within") {
				t.Fatalf("witness did not fail on its intended assertion under the mutant: %v", err)
			}
		})
	}
}

// initialAbsenceRun starts a quiet consumer whose FIRST native owner read
// answers firstErr; every later read returns the live fixture owner's token,
// shifted when reuse is set (a reused PID with the same canonical Birth).
func initialAbsenceRun(t *testing.T, firstErr error, reuse bool) (*consumerRun, time.Time) {
	t.Helper()
	quiescentTiming(t, 0, time.Minute, 20*time.Second, 100*time.Millisecond)
	var calls atomic.Int64
	processSeams(t, tmuxwake.Birth, func(pid int) (string, error) {
		if calls.Add(1) == 1 {
			return "", firstErr
		}
		v, err := processStart(pid)
		if err == nil && reuse {
			v = shiftedStart(v)
		}
		return v, err
	})
	f := newOrphanDispatchFixture(t)
	from := time.Now()
	return startQuiescentConsumer(t, f, time.Second, nil), from
}

func initialAbsenceError(t *testing.T) error {
	t.Helper()
	run, from := initialAbsenceRun(t, errProcessGone, true)
	return stopWithin(run, from, 3*time.Second, "owner absent at the first native read")
}

// Review 5900358239 finding 1 (initial-owner-absence-ignored). The owner's
// absence at the consumer's first native read is a known end: the consumer
// stops within 3s and never adopts the reused PID's token. Control: an
// UNKNOWN first read followed by the live owner's token keeps running.
func TestQuiescentOwnerInitialAbsence(t *testing.T) {
	isolateBindTest(t)
	t.Run("gone-before-first-cycle", func(t *testing.T) {
		if err := initialAbsenceError(t); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unknown-first-control", func(t *testing.T) {
		run, _ := initialAbsenceRun(t, errors.New("transient inspection failure"), false)
		run.trace.nth(t, "cycle", 3, 20*time.Second)
		select {
		case <-run.done:
			t.Fatalf("an unknown first read stopped a live owner's consumer: %q", run.outcome)
		default:
		}
	})
}

// Calibration for finding 1: with the first read's absence ignored, the
// UNCHANGED witness must fail on its intended assertion.
func TestQuiescentOwnerInitialAbsenceCalibration(t *testing.T) {
	isolateBindTest(t)
	terminalInitialOwnerAbsenceStops = false
	t.Cleanup(func() { terminalInitialOwnerAbsenceStops = true })
	err := initialAbsenceError(t)
	if err == nil || !strings.Contains(err.Error(), "not stopped within") {
		t.Fatalf("witness did not fail on its intended assertion under the mutant: %v", err)
	}
}

// registrationShape: from cycle 2 on, the native token read INSIDE a
// registration's canonical bracket is shifted when changed is set, and the
// bracket's SECOND canonical Birth probe either fails (slow false) or takes
// 4s (slow true). Native reads outside a bracket answer unknown in the
// failing shape, so only the registration itself can see the change.
type registrationShape struct{ changed, slow bool }

func registrationRun(t *testing.T, shape registrationShape) (*consumerRun, func() time.Time) {
	t.Helper()
	quiescentTiming(t, 0, time.Second, 20*time.Second, 100*time.Millisecond)
	var trace atomic.Pointer[terminalTraceLog]
	cycles := func() int {
		if l := trace.Load(); l != nil {
			return l.count("cycle", time.Time{}, time.Now().Add(time.Hour))
		}
		return 0
	}
	var inBracket atomic.Bool
	var births atomic.Int64
	processSeams(t, func(ctx context.Context, pid int) (string, error) {
		if cycles() < 2 {
			return tmuxwake.Birth(ctx, pid)
		}
		if births.Add(1)%2 == 1 { // the bracket's first probe
			inBracket.Store(true)
			return tmuxwake.Birth(ctx, pid)
		}
		inBracket.Store(false) // the bracket's second probe
		if shape.slow {
			time.Sleep(4 * time.Second)
			return tmuxwake.Birth(ctx, pid)
		}
		return "", errors.New("canonical inspection unavailable")
	}, func(pid int) (string, error) {
		v, err := processStart(pid)
		if err != nil || cycles() < 2 {
			return v, err
		}
		if inBracket.Load() {
			if shape.changed {
				return shiftedStart(v), nil
			}
			return v, nil
		}
		if shape.slow {
			return v, nil
		}
		return "", errors.New("transient inspection failure")
	})
	f := newOrphanDispatchFixture(t)
	run := startQuiescentConsumer(t, f, time.Second, nil)
	trace.Store(run.trace)
	return run, func() time.Time { return run.trace.nth(t, "cycle", 2, 20*time.Second) }
}

func registrationError(t *testing.T, name string, shape registrationShape) error {
	t.Helper()
	run, second := registrationRun(t, shape)
	return stopWithin(run, second(), 3*time.Second, name+": known replacement at registration")
}

// Review 5900358239 finding 2 (known-replacement-gated-by-registration). A
// changed valid native token seen while registering is a known replacement
// that stops within 3s, whether the bracket's second canonical probe fails
// (changed-then-unknown) or takes 4s (changed-slow-second-birth). Controls:
// an unchanged token with the same failing or slow probe keeps running.
func TestQuiescentOwnerReplacementAtRegistration(t *testing.T) {
	isolateBindTest(t)
	for _, c := range []struct {
		name  string
		shape registrationShape
	}{
		{"changed-then-unknown", registrationShape{changed: true}},
		{"changed-slow-second-birth", registrationShape{changed: true, slow: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := registrationError(t, c.name, c.shape); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, c := range []struct {
		name  string
		shape registrationShape
	}{
		{"unchanged-then-unknown", registrationShape{}},
		{"unchanged-slow-second-birth", registrationShape{slow: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			run, second := registrationRun(t, c.shape)
			at := second()
			time.Sleep(time.Until(at.Add(5 * time.Second)))
			select {
			case <-run.done:
				t.Fatalf("an unchanged owner stopped the consumer: %q", run.outcome)
			default:
			}
		})
	}
}

// Calibration for finding 2: with the native comparison deferred until both
// canonical probes succeed, the UNCHANGED witness must fail on its intended
// assertion for both changed shapes.
func TestQuiescentOwnerReplacementAtRegistrationCalibration(t *testing.T) {
	isolateBindTest(t)
	terminalRegistrationNativeFirst = false
	t.Cleanup(func() { terminalRegistrationNativeFirst = true })
	for _, c := range []struct {
		name  string
		shape registrationShape
	}{
		{"changed-then-unknown", registrationShape{changed: true}},
		{"changed-slow-second-birth", registrationShape{changed: true, slow: true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := registrationError(t, c.name, c.shape)
			if err == nil || !strings.Contains(err.Error(), "not stopped within") {
				t.Fatalf("witness did not fail on its intended assertion under the mutant: %v", err)
			}
		})
	}
}
