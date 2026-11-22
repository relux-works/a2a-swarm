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
