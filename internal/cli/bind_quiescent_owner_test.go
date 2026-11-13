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
