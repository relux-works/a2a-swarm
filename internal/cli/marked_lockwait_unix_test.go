//go:build unix

package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// TestIdleSyncCommitGivesUpAtItsDeadline proves that the bound consumer's
// sync step does not wait for the State lock past its deadline when its sync
// returned nothing. The step has its reservation and an empty response; then
// another process takes the State lock and keeps it. The step answers
// ErrLockTimeout within its budget and the sidecar is byte for byte as it was.
//
// Control: once the lock is free the consumer's next step succeeds, and
// nothing is left reserved: another owner reserves the cursor at once.
//
// Regression witness: the empty commit had no context and waited in a
// blocking flock for as long as the lock was held; the watchdog here ends
// that wait after twelve seconds and fails the test.
//
// The lock is held by this test's process through a second descriptor, which
// flock treats as another holder. It is not a second a2a process.
func TestIdleSyncCommitGivesUpAtItsDeadline(t *testing.T) {
	e, a, s, generation, _ := markedBindDeliveryFixture(t, false)
	ctx := context.Background()
	start, subs, err := s.startBindDelivery(ctx, a.Now())
	if err != nil || !start.cached {
		t.Fatalf("premise: the stable binding's cached view: cached=%t err=%v", start.cached, err)
	}
	const budget = 2 * time.Second
	deadline := time.Now().Add(budget)
	token, err := s.beginBindCursorView(ctx, start.view, "delivery", deadline)
	if err != nil {
		t.Fatalf("premise: the reservation: %v", err)
	}
	sidecarBefore := readFile(t, e.paths().BindPartFile("alice"))

	release := e.holdStateLock("alice")
	released := false
	defer func() {
		if !released {
			release()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := s.finishIdleBindSync(ctx, start.view, token, matrix.SyncResponse{NextBatch: "s-after-nothing"}, subs.Subs, deadline)
		done <- err
	}()
	select {
	case err = <-done:
	case <-time.After(budget + 10*time.Second):
		released = true
		release()
		<-done
		t.Fatal("the sync step was still waiting for the State lock ten seconds after its deadline")
	}
	if !errors.Is(err, config.ErrLockTimeout) {
		t.Fatalf("the sync step under a held State lock: %v, want ErrLockTimeout", err)
	}
	if got := readFile(t, e.paths().BindPartFile("alice")); got != sidecarBefore {
		t.Fatal("the sidecar changed under a held State lock")
	}

	released = true
	release()
	if _, err := s.syncIdleBindNowOrSyncNow(ctx, a); err != nil {
		t.Fatalf("control: the next sync step with the lock free: %v", err)
	}
	st := e.state("alice")
	view, err := config.ReadBindPartView(e.paths(), "alice", st)
	if err != nil || view.Generation != generation || view.Stopped {
		t.Fatalf("control: the binding afterwards: %+v err=%v", view, err)
	}
	other := config.BindOperationOwner{Holder: "another-owner", Host: machineLabel(), PID: os.Getpid(), Birth: "another-start"}
	otherToken := strings.Repeat("9", 32)
	if _, err := config.BeginBindCursor(e.paths(), "alice", otherToken, "bind", view.NextBatch, generation, other, func(config.BindOperationOwner) bool { return false }); err != nil {
		t.Fatalf("control: a reservation is left behind: %v", err)
	}
	if err := config.AbortBindCursor(e.paths(), "alice", otherToken); err != nil {
		t.Fatal(err)
	}
}

// TestCursorReleaseNeverOutlivesItsCaller proves that the release of a cursor
// reservation waits for the State lock no longer than the step that took the
// reservation may: not past that step's deadline, and not after its context
// was cancelled.
//
// Row "deadline": the context the release waits with has a deadline, and it
// is never later than the caller's, before that deadline and after it. With
// the State lock held past the deadline the release answers ErrLockTimeout and
// the sidecar is unchanged.
//
// Row "cancelled": a reservation taken without a deadline is released by a
// cancelled caller while the lock is held elsewhere. The release returns the
// cancellation. The lock is never freed during the call, and the watchdog
// would end a wait after ten seconds.
//
// Controls, both rows: with the lock free again the same release succeeds,
// although its deadline has passed or its caller is cancelled: it gets one
// attempt. And the reservation that could not be released does not shut the
// process out: its next reservation releases the leftover first.
//
// Regression witness: the release waited on a context of its own, at least
// 250 ms long and unlimited for a reservation without a deadline, and never
// ended by its caller's cancellation.
func TestCursorReleaseNeverOutlivesItsCaller(t *testing.T) {
	for _, row := range []string{"deadline", "cancelled"} {
		t.Run(row, func(t *testing.T) {
			e, a, s, _, _ := markedBindDeliveryFixture(t, false)
			start, _, err := s.startBindDelivery(context.Background(), a.Now())
			if err != nil || !start.cached {
				t.Fatalf("premise: the stable binding's cached view: cached=%t err=%v", start.cached, err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var deadline time.Time
			want := context.Canceled
			if row == "deadline" {
				deadline = time.Now().Add(1500 * time.Millisecond)
				want = config.ErrLockTimeout
			}
			token, err := s.beginBindCursorView(ctx, start.view, "delivery", deadline)
			if err != nil {
				t.Fatalf("premise: the reservation: %v", err)
			}
			notLater := func(when string) {
				t.Helper()
				releaseCtx, done := s.cursorReleaseContext(token)
				defer done()
				got, ok := releaseCtx.Deadline()
				if !ok || got.After(deadline) {
					t.Fatalf("%s: the release waits until %v (set=%t); the caller's deadline is %v", when, got, ok, deadline)
				}
			}
			if row == "deadline" {
				notLater("before the caller's deadline")
				time.Sleep(time.Until(deadline) + 10*time.Millisecond)
				notLater("after the caller's deadline")
			} else {
				cancel()
				releaseCtx, done := s.cursorReleaseContext(token)
				if releaseCtx.Err() == nil {
					t.Fatal("the release's context outlives its cancelled caller")
				}
				done()
			}

			sidecarBefore := readFile(t, e.paths().BindPartFile("alice"))
			release := e.holdStateLock("alice")
			aborted := make(chan error, 1)
			go func() { aborted <- s.abortBindCursor(token) }()
			select {
			case err = <-aborted:
			case <-time.After(10 * time.Second):
				release()
				<-aborted
				t.Fatal("the release was still waiting for the State lock ten seconds after its caller had ended")
			}
			if !errors.Is(err, want) {
				release()
				t.Fatalf("the release under a held lock: %v, want %v", err, want)
			}
			if got := readFile(t, e.paths().BindPartFile("alice")); got != sidecarBefore {
				release()
				t.Fatal("the sidecar changed under a held State lock")
			}
			release()

			// Controls. The next reservation of this process releases the leftover
			// first; without that it would be refused as held by a live owner.
			next, err := s.beginBindCursorView(context.Background(), start.view, "delivery", time.Now().Add(5*time.Second))
			if err != nil {
				t.Fatalf("control: the next reservation after a release that timed out: %v", err)
			}
			// Its release gets one attempt at the free lock on an ended context.
			ended, cancelEnded := context.WithCancel(context.Background())
			cancelEnded()
			s.holdCursor(ended, next, time.Now().Add(-time.Second))
			if err := s.abortBindCursor(next); err != nil {
				t.Fatalf("control: the release at a free lock after its caller ended: %v", err)
			}
			other := config.BindOperationOwner{Holder: "another-owner", Host: machineLabel(), PID: os.Getpid(), Birth: "another-start"}
			otherToken := strings.Repeat("9", 32)
			if _, err := config.BeginBindCursor(e.paths(), "alice", otherToken, "bind", start.view.NextBatch, start.view.Generation, other, func(config.BindOperationOwner) bool { return false }); err != nil {
				t.Fatalf("control: a reservation is left behind: %v", err)
			}
			if err := config.AbortBindCursor(e.paths(), "alice", otherToken); err != nil {
				t.Fatal(err)
			}
		})
	}
}
