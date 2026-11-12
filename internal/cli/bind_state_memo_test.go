package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func memoSession(t *testing.T) (*env, *App, *session) {
	t.Helper()
	e := newEnv(t)
	e.agent("alice")
	a := &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}, Now: time.Now, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client()}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	return e, a, s
}

// CHANGED_CONNECTION_NO_EFFECT. Claim: inside an open epoch with State bytes
// unchanged since a successful guarded write, a changed selected connection
// still refuses the next guarded write at its existing boundary with no state
// change; resident parsed data authorizes nothing. Kills M3 (skip the selected
// checkBinding on a memo hit). Positive control: the first write succeeds.
func TestTerminalStateMemoChangedConnectionNoEffect(t *testing.T) {
	e, a, s := memoSession(t)
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "own", SessionID: "own", Host: machineLabel()}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	memo := config.NewStateDecodeScope("alice")
	s.paths = s.paths.WithStateDecodeScope(memo)
	memo.Open()
	defer memo.Close()
	bindingErr := errors.New("selected connection changed")
	var changed bool
	s.checkBinding = func() error {
		if changed {
			return bindingErr
		}
		return nil
	}
	if err := a.terminalUpdate(s, "own", func(b *config.TerminalBinding) { b.Outcome = "first" }); err != nil {
		t.Fatalf("positive control: %v", err)
	}
	if err := s.checkConnection(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if memo.Stats().Hits == 0 {
		t.Fatal("fixture did not prime a resident frame")
	}
	changed = true
	if err := s.checkConnection(); !errors.Is(err, bindingErr) {
		t.Fatalf("guard on resident data: %v", err)
	}
	if err := a.terminalUpdate(s, "own", func(b *config.TerminalBinding) { b.Outcome = "second" }); !errors.Is(err, bindingErr) {
		t.Fatalf("guarded write: %v", err)
	}
	after, _ := os.ReadFile(e.paths().StateFile("alice"))
	if !bytes.Equal(before, after) {
		t.Fatal("refused connection changed state")
	}
}

type cycleCounts struct {
	cycles                                      int
	loads, reads, parses, seals, sealed, digest float64
	hits                                        int64
}

// runFixtureCycles runs the real terminal consumer loop for n cycles and
// returns per-cycle counter deltas between the end of cycle 1 and cycle n.
func runFixtureCycles(t *testing.T, memo bool, n int) cycleCounts {
	t.Helper()
	prev := terminalStateMemo
	terminalStateMemo = memo
	defer func() { terminalStateMemo = prev }()
	target, _, _ := usabilityTarget(t, "codex")
	e, a, s := memoSession(t)
	initial := config.TerminalBinding{Generation: "own", SessionID: "own", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target, Interval: 50 * time.Millisecond, RetryMax: time.Second, MaxWakes: 6}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &initial
		st.NextBatch = "s0"
		for i := 0; i < 191; i++ {
			st.RememberThreadEvent(config.ThreadEvent{Room: fmt.Sprintf("!room%02d:fixture.invalid", i%16), EventID: fmt.Sprintf("$event%039d", i), Root: fmt.Sprintf("$root%040d", i%64)})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var start, end config.StateDecodeStats
	cycles := 0
	var errLog bytes.Buffer
	a.Stderr = &errLog
	reason, err := a.runTerminalWithRefresh(ctx, s, "own", initial, func() {
		cycles++
		switch cycles {
		case 1:
			start = config.ReadStateDecodeStats()
		case n:
			end = config.ReadStateDecodeStats()
			cancel()
		}
	})
	if err != nil || reason != "cancelled" || cycles != n {
		t.Fatalf("loop exit=%s %v cycles=%d", reason, err, cycles)
	}
	if errLog.Len() != 0 {
		t.Fatalf("fixture cycles failed: %s", errLog.String())
	}
	d := float64(n - 1)
	out := cycleCounts{cycles: n - 1,
		loads:  float64(end.LoadStateCalls-start.LoadStateCalls) / d,
		reads:  float64(end.StateReadAttempts-start.StateReadAttempts) / d,
		parses: float64(end.UnmarshalStatePasses-start.UnmarshalStatePasses) / d,
		seals:  float64(end.WriterSealChecks-start.WriterSealChecks) / d,
		sealed: float64(end.SealedStates-start.SealedStates) / d,
		digest: float64(end.StateDigests-start.StateDigests) / d,
		hits:   end.ScopeHits - start.ScopeHits,
	}
	if sc := s.paths.StateDecodeScope(); memo && (sc == nil || sc.Stats().Epochs < int64(n)) {
		t.Fatal("epoch not opened per cycle")
	}
	return out
}

// Claim: on the same terminal fixture cycle, the memo leaves logical
// LoadState calls, fresh file reads and writer seal/digest/seal-write work per
// cycle unchanged while performing fewer actual typed State parses. Limits:
// one synthetic idle fixture (~40 KB State, no mail), in-process fake server;
// counters include the concurrent alarm worker. Not a CPU measurement.
func TestTerminalStateMemoFixtureCycleCounters(t *testing.T) {
	const n = 6
	off := runFixtureCycles(t, false, n)
	on := runFixtureCycles(t, true, n)
	t.Logf("per cycle off: %+v", off)
	t.Logf("per cycle on:  %+v", on)
	near := func(a, b float64) bool { return a <= b*1.15+1 && b <= a*1.15+1 }
	if !near(on.loads, off.loads) || !near(on.reads, off.reads) {
		t.Fatalf("logical reads changed: off=%+v on=%+v", off, on)
	}
	if !near(on.seals, off.seals) || !near(on.digest, off.digest) || !near(on.sealed, off.sealed) {
		t.Fatalf("writer verification work changed: off=%+v on=%+v", off, on)
	}
	if off.hits != 0 || on.hits == 0 {
		t.Fatalf("memo use: off=%d on=%d", off.hits, on.hits)
	}
	if off.parses < off.reads || on.parses >= off.parses*0.6 {
		t.Fatalf("typed parses not reduced: off=%.2f on=%.2f (reads %.2f)", off.parses, on.parses, off.reads)
	}
}

// Claim: the epoch is open while the cycle (including refresh evaluation)
// runs and closed before the timer wait and after return; a closed scope
// decodes ordinarily.
func TestTerminalStateMemoEpochClosedBeforeWait(t *testing.T) {
	target, _, _ := usabilityTarget(t, "codex")
	e, a, s := memoSession(t)
	initial := config.TerminalBinding{Generation: "own", SessionID: "own", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, Target: target, Interval: 300 * time.Millisecond, RetryMax: time.Second, MaxWakes: 6}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = &initial; st.NextBatch = "s0"; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitChecked := make(chan error, 1)
	checked := false
	reason, err := a.runTerminalWithRefresh(ctx, s, "own", initial, func() {
		if checked {
			return
		}
		checked = true
		sc := s.paths.StateDecodeScope()
		before := sc.Stats()
		if _, err := config.LoadState(s.paths, "alice"); err != nil {
			waitChecked <- err
			return
		}
		if sc.Stats().Hits+sc.Stats().Misses == before.Hits+before.Misses {
			waitChecked <- errors.New("epoch not open during refresh")
			return
		}
		go func() {
			time.Sleep(100 * time.Millisecond) // inside the 300ms timer wait
			b := sc.Stats()
			_, err := config.LoadState(s.paths, "alice")
			if err == nil && sc.Stats().FallbackClosed == b.FallbackClosed {
				err = errors.New("epoch open during timer wait")
			}
			waitChecked <- err
			cancel()
		}()
	})
	if err != nil || reason != "cancelled" {
		t.Fatalf("loop: %s %v", reason, err)
	}
	if err := <-waitChecked; err != nil {
		t.Fatal(err)
	}
	sc := s.paths.StateDecodeScope()
	b := sc.Stats()
	if _, err := config.LoadState(s.paths, "alice"); err != nil || sc.Stats().FallbackClosed != b.FallbackClosed+1 {
		t.Fatal("epoch open after return")
	}
}
