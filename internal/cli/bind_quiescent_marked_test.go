//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// TestQuiescentMarkedConsumerBoundedWork proves the quiet window of a marked
// binding does the bounded work of quietWindowError (no extra full cycle, no
// owner re-registration, no self-write wake), that every completed long-poll
// is persisted through the sidecar and keeps the consumer waiting, and that
// the window neither clones, decodes, encodes, seals nor writes State.
//
// Regression witness: when the exchange persisted a marked binding's response
// through the generic path, the response was refused for want of a cursor
// reservation, the wait ended with "ingest" and a full cycle followed every
// long-poll. Limits: fixture timing with shortened seams against the local
// fake homeserver; this is not the CPU measurement.
func TestQuiescentMarkedConsumerBoundedWork(t *testing.T) {
	isolateBindTest(t)
	quiescentTiming(t, 0, 4*time.Second, time.Second, 100*time.Millisecond)
	f := newOrphanDispatchFixture(t)
	legacy := f.e.state("alice")
	if legacy.Lease == nil {
		t.Fatal("setup: delivery lease missing before migration")
	}
	if _, err := config.EnsureBindPart(f.e.paths(), "alice", config.BindPartOpenOptions{Holder: legacy.Lease.Holder, Now: f.now}); err != nil {
		t.Fatal("activate marked binding:", err)
	}
	revision := func() uint64 {
		t.Helper()
		state, err := config.LoadState(f.e.paths(), "alice")
		if err != nil {
			t.Fatal(err)
		}
		view, err := config.ReadBindPartView(f.e.paths(), "alice", state)
		if err != nil {
			t.Fatal(err)
		}
		return view.Revision
	}

	// The fixture App carries no context; the marked lease path must wait as
	// the unmarked one does instead of failing on it.
	run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
	from := run.trace.await(t, "poll", time.Time{}, 20*time.Second)
	revisionBefore := revision()
	statsBefore := config.ReadStateDecodeStats()
	time.Sleep(2500 * time.Millisecond)
	statsAfter := config.ReadStateDecodeStats()
	to := from.Add(2500 * time.Millisecond)

	if err := quietWindowError(run.trace, from, to, time.Second); err != nil {
		t.Fatal(err)
	}
	if n := run.trace.count("wake:ingest", from, to); n != 0 {
		t.Fatalf("%d long-poll responses of a marked binding were refused instead of persisted", n)
	}
	if n := run.trace.count("exchanged", from, to); n == 0 {
		t.Fatal("no long-poll response was persisted while the consumer kept waiting")
	}
	if after := revision(); after <= revisionBefore {
		t.Fatalf("sidecar revision %d did not advance past %d: the exchange did not go through the sidecar", after, revisionBefore)
	}
	// revision() above loads State once more without a cache; the counters are
	// read before it, so they cover the consumer's quiet window only.
	d := statsAfter
	if d.WriterClones != statsBefore.WriterClones || d.UnmarshalStatePasses != statsBefore.UnmarshalStatePasses ||
		d.StateEncodes != statsBefore.StateEncodes || d.SealedStates != statsBefore.SealedStates || d.StateWriteBytes != statsBefore.StateWriteBytes {
		t.Fatalf("quiet window of a marked binding cloned, decoded, encoded, sealed or wrote State: before=%+v after=%+v", statsBefore, statsAfter)
	}
}

// TestQuiescentOwnerExitRecordsStop proves a quiescent consumer whose owner is
// killed returns "owner gone or replaced" and records the stop where the
// binding keeps it: in State for an unmarked binding (control), in the sidecar
// for a marked one, read back through the merged State in both cases. With
// the State lock held by another writer a marked consumer still returns
// within the bound and records nothing.
//
// Regression witness: the stop of a marked binding was written through the
// generic State path, refused there as a provider-owned field, and lost.
func TestQuiescentOwnerExitRecordsStop(t *testing.T) {
	isolateBindTest(t)
	for _, tc := range []struct {
		name               string
		marked, lockedHeld bool
	}{{"unmarked", false, false}, {"marked", true, false}, {"marked, State lock held", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			quiescentTiming(t, 0, time.Minute, 20*time.Second, time.Second)
			f := newOrphanDispatchFixture(t)
			if tc.marked {
				legacy := f.e.state("alice")
				if legacy.Lease == nil {
					t.Fatal("setup: delivery lease missing before migration")
				}
				if _, err := config.EnsureBindPart(f.e.paths(), "alice", config.BindPartOpenOptions{Holder: legacy.Lease.Holder, Now: f.now}); err != nil {
					t.Fatal("activate marked binding:", err)
				}
			}
			run := startQuiescentConsumer(t, f, 300*time.Millisecond, nil)
			run.trace.await(t, "poll", time.Time{}, 40*time.Second)
			owner := f.binding().OwnerPID
			release := make(chan struct{})
			releaseLock := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseLock)
			released := make(chan struct{})
			if tc.lockedHeld {
				held := make(chan struct{})
				go func() {
					defer close(released)
					config.UpdateStateContext(context.Background(), f.e.paths(), "alice", time.Time{}, func(*config.State) error {
						close(held)
						<-release
						return errors.New("released")
					})
				}()
				<-held
			} else {
				close(released)
			}
			if err := syscall.Kill(owner, syscall.SIGKILL); err != nil {
				t.Fatal(err)
			}
			run.wait(t, 3*time.Second)
			releaseLock()
			<-released
			if run.err != nil || run.outcome != "owner gone or replaced" {
				t.Fatalf("outcome=%q err=%v", run.outcome, run.err)
			}
			state, err := config.LoadState(f.e.paths(), "alice")
			if err != nil || state.Terminal == nil {
				t.Fatalf("State after the owner's exit: terminal=%+v err=%v", state.Terminal, err)
			}
			if tc.lockedHeld {
				if state.Terminal.Stopped {
					t.Fatal("stop was recorded although the State lock was held for the whole stop path")
				}
				return
			}
			if !state.Terminal.Stopped || state.Terminal.Outcome != "target exited or replaced" {
				t.Fatalf("stop not recorded: stopped=%t outcome=%q", state.Terminal.Stopped, state.Terminal.Outcome)
			}
		})
	}
}
