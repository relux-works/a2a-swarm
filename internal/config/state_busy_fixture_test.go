package config

// This file deliberately uses only APIs that already exist before the
// unchanged-transaction elision, so an authorized seat can copy it onto the
// base commit for a before/after work and benchmark comparison. On the base,
// TestUpdateStateUnchangedWorkCount is the expected-red regression witness.

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	busyStateName           = "busy"
	syntheticThreadRecords  = 2000
	syntheticBusyOutcome    = "suppressed: busy, draft or unknown input"
	syntheticIdleOutcome    = "armed: no new mail"
	syntheticIDBytes        = 44
	syntheticStateMinBytes  = 300_000
	syntheticStateMaxBytes  = 600_000
	syntheticNoopIterations = 3
)

// syntheticID returns a Matrix-shaped identifier of a fixed 44 bytes, close to
// a real room/event ID, made only of synthetic text.
func syntheticID(sigil, kind string, n int) string {
	id := fmt.Sprintf("%ssynthetic-%s-%06d-", sigil, kind, n)
	return id + strings.Repeat("x", syntheticIDBytes-len(id))
}

// syntheticBusyState is an ordinary consumer state whose size is dominated by
// 2,000 retained thread records, like the observed busy consumer. It holds no
// live room, event, host, session or credential value.
func syntheticBusyState() State {
	s := State{NextBatch: "s-synthetic-batch"}
	s.ThreadEvents = make([]ThreadEvent, 0, syntheticThreadRecords)
	for i := 0; i < syntheticThreadRecords; i++ {
		s.ThreadEvents = append(s.ThreadEvents, ThreadEvent{Room: syntheticID("!", "room", i%16), EventID: syntheticID("$", "event", i), Root: syntheticID("$", "root", i%64)})
	}
	for i := 0; i < 14; i++ {
		s.Threads = append(s.Threads, syntheticID("$", "thread", i))
	}
	s.Terminal = &TerminalBinding{Generation: strings.Repeat("0", 32), SessionID: "synthetic-session", Host: "synthetic-host", OwnerPID: 2, OwnerBirth: "synthetic-owner-birth", DaemonPID: 3, DaemonBirth: "synthetic-daemon-birth", NoticePublication: "unavailable: synthetic", Interval: 2 * time.Second, RetryMax: 30 * time.Second, MaxWakes: 6, Outcome: syntheticBusyOutcome}
	return s
}

// seedSyntheticBusyState writes the fixture through the ordinary sealed writer
// and returns its paths and exact disk bytes.
func seedSyntheticBusyState(tb testing.TB) (Paths, []byte) {
	tb.Helper()
	p := Paths{Base: tb.TempDir()}
	if err := SaveState(p, busyStateName, syntheticBusyState()); err != nil {
		tb.Fatal(err)
	}
	raw, err := os.ReadFile(p.StateFile(busyStateName))
	if err != nil {
		tb.Fatal(err)
	}
	return p, raw
}

// stateReplaced runs op while holding the current state file open, so its
// inode cannot be freed and reused, and reports whether the path names a
// different file afterwards. Every ordinary state write replaces the path by
// rename, so this detects a rewrite even when the new bytes are identical.
func stateReplaced(tb testing.TB, p Paths, name string, op func() error) (bool, error) {
	tb.Helper()
	path := p.StateFile(name)
	held, err := os.Open(path)
	if err != nil {
		tb.Fatal(err)
	}
	defer held.Close()
	opErr := op()
	before, err := held.Stat()
	if err != nil {
		tb.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		tb.Fatal(err)
	}
	return !os.SameFile(before, after), opErr
}

// countStateRewrites sums stateReplaced over ops; any op error fails the test.
func countStateRewrites(tb testing.TB, p Paths, name string, ops ...func() error) int {
	tb.Helper()
	rewrites := 0
	for i, op := range ops {
		replaced, err := stateReplaced(tb, p, name, op)
		if err != nil {
			tb.Fatalf("op %d: %v", i, err)
		}
		if replaced {
			rewrites++
		}
	}
	return rewrites
}

// busyCycleNoops mirrors, at the State level, the callbacks a busy/no-mail
// bind cycle enters with nothing to change: a read-only pending scan, the
// rung/attempt prune, the notice/idle reset and the stable outcome. Callers
// also replace writer metadata, which every save overrides.
func busyCycleNoops(outcome string) []func(*State) error {
	return []func(*State) error{
		func(*State) error { return nil },
		func(st *State) error {
			b := st.Terminal
			b.Rung = slices.DeleteFunc(b.Rung, func(string) bool { return true })
			b.AttemptIDs = slices.Clone(b.AttemptIDs)
			return nil
		},
		func(st *State) error {
			st.Terminal.Idle = nil
			st.Terminal.OwnNotice.PossiblyOrphaned = nil
			return nil
		},
		func(st *State) error { st.Terminal.Idle = nil; st.Terminal.Outcome = outcome; return nil },
		func(st *State) error { st.Writer = &StateWriter{Protocol: 1, Seal: "caller-supplied"}; return nil },
	}
}

// updateRoutes are the ordinary transaction entry points used by the bind
// consumer (UpdateState via App.updateState, UpdateStateContext for daemon
// and lease paths, UpdateStateBy for bounded observers).
func updateRoutes(p Paths, name string) map[string]func(func(*State) error) (State, error) {
	return map[string]func(func(*State) error) (State, error){
		"UpdateState": func(fn func(*State) error) (State, error) { return UpdateState(p, name, fn) },
		"UpdateStateBy": func(fn func(*State) error) (State, error) {
			return UpdateStateBy(p, name, time.Now().Add(time.Second), fn)
		},
		"UpdateStateUntil": func(fn func(*State) error) (State, error) { return UpdateStateUntil(p, name, time.Time{}, fn) },
		"UpdateStateContext": func(fn func(*State) error) (State, error) {
			return UpdateStateContext(context.Background(), p, name, time.Now().Add(time.Second), fn)
		},
	}
}

// Proves the synthetic fixture is comparable to the observed state: exactly
// 2,000 thread records and a total size within 300-600KB. It does not prove
// that the fixture reproduces the live consumer's CPU profile.
func TestSyntheticBusyStateSize(t *testing.T) {
	p, raw := seedSyntheticBusyState(t)
	st, err := LoadState(p, busyStateName)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.ThreadEvents) != syntheticThreadRecords || len(raw) < syntheticStateMinBytes || len(raw) > syntheticStateMaxBytes {
		t.Fatalf("fixture has %d records and %d bytes", len(st.ThreadEvents), len(raw))
	}
	if !validWriterSeal(st) || !st.Writer.ForeignSeen.IsZero() {
		t.Fatal("fixture is not a compatible sealed input")
	}
}

// Proves, as a deterministic work count on the 2,000-record fixture, that
// repeated busy-cycle transactions with no effective change replace the state
// file zero times on every ordinary entry point and leave its bytes intact,
// while a changed outcome still replaces it exactly once. Expected red on the
// base commit, which replaces the file on every transaction.
func TestUpdateStateUnchangedWorkCount(t *testing.T) {
	for route := range updateRoutes(Paths{}, "") {
		t.Run(route, func(t *testing.T) {
			p, raw := seedSyntheticBusyState(t)
			update := updateRoutes(p, busyStateName)[route]
			var ops []func() error
			for i := 0; i < syntheticNoopIterations; i++ {
				for _, fn := range busyCycleNoops(syntheticBusyOutcome) {
					ops = append(ops, func() error { _, err := update(fn); return err })
				}
			}
			if n := countStateRewrites(t, p, busyStateName, ops...); n != 0 {
				t.Fatalf("%d unchanged transactions replaced the state %d times", len(ops), n)
			}
			after, err := os.ReadFile(p.StateFile(busyStateName))
			if err != nil || string(after) != string(raw) {
				t.Fatal("unchanged transactions altered the state bytes", err)
			}
			changed := func() error {
				_, err := update(func(st *State) error { st.Terminal.Outcome = syntheticIdleOutcome; return nil })
				return err
			}
			if n := countStateRewrites(t, p, busyStateName, changed); n != 1 {
				t.Fatalf("changed outcome replaced the state %d times, want 1", n)
			}
			if st, err := LoadState(p, busyStateName); err != nil || st.Terminal.Outcome != syntheticIdleOutcome || !validWriterSeal(st) || len(st.ThreadEvents) != syntheticThreadRecords {
				t.Fatal("changed outcome was not persisted as a sealed state", err)
			}
		})
	}
}

// legacyUpdateState is a bounded control plant: the transaction body as it was
// before elision (independent working copy, fence, unconditional sealed save).
// It exists only to show that the rewrite detector sees the old behavior.
func legacyUpdateState(p Paths, name string, fn func(*State) error) (State, error) {
	return withStateLock(p, name, lockFile, func(old State) (State, error) {
		if old.Migration != nil {
			return old, ErrIdentityMigrated
		}
		fence := ordinaryWriterFence(old, time.Now())
		var s State
		if len(old.rawState) != 0 {
			if err := unmarshalState(old.rawState, &s); err != nil {
				return old, err
			}
		}
		if err := fn(&s); err != nil {
			return old, err
		}
		return saveStateLocked(p, name, old, s, fence, false, false)
	})
}

// Proves the rewrite detector is not vacuous: the restored always-write plant
// is counted once per no-op transaction even though it rewrites byte-identical
// content, so a byte comparison alone would miss it; a read-only load is
// counted zero times. This validates the instrument, not product behavior.
func TestStateRewriteDetectorControl(t *testing.T) {
	p, raw := seedSyntheticBusyState(t)
	var ops []func() error
	for _, fn := range busyCycleNoops(syntheticBusyOutcome) {
		ops = append(ops, func() error { _, err := legacyUpdateState(p, busyStateName, fn); return err })
	}
	if n := countStateRewrites(t, p, busyStateName, ops...); n != len(ops) {
		t.Fatalf("detector counted %d of %d restored rewrites", n, len(ops))
	}
	after, err := os.ReadFile(p.StateFile(busyStateName))
	if err != nil || string(after) != string(raw) {
		t.Fatal("restored rewrites were expected to keep identical bytes", err)
	}
	read := func() error { _, err := LoadState(p, busyStateName); return err }
	if n := countStateRewrites(t, p, busyStateName, read, read); n != 0 {
		t.Fatalf("detector counted %d rewrites for read-only loads", n)
	}
}

// Benchmarks for an authorized before/after comparison on the same synthetic
// 2,000-record state. They measure one in-process transaction; they are not
// a measurement of the installed consumer's CPU share.
func BenchmarkUpdateStateUnchanged2000(b *testing.B) {
	p, _ := seedSyntheticBusyState(b)
	fn := func(st *State) error { st.Terminal.Idle = nil; st.Terminal.Outcome = syntheticBusyOutcome; return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := UpdateState(p, busyStateName, fn); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUpdateStateChanged2000(b *testing.B) {
	p, _ := seedSyntheticBusyState(b)
	outcomes := [2]string{syntheticIdleOutcome, syntheticBusyOutcome}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		outcome := outcomes[i%2]
		if _, err := UpdateState(p, busyStateName, func(st *State) error { st.Terminal.Outcome = outcome; return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoadState2000(b *testing.B) {
	p, _ := seedSyntheticBusyState(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := LoadState(p, busyStateName); err != nil {
			b.Fatal(err)
		}
	}
}
