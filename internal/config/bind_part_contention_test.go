package config

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// holdBindTestLock takes the State lock of the bind fixture as another
// process would and returns its release.
func holdBindTestLock(t *testing.T, p Paths) func() {
	t.Helper()
	unlock, err := lockFile(filepath.Join(filepath.Dir(p.StateFile(bindTestName)), bindTestName+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	return unlock
}

// TestBindOperationLockWaitIsBounded proves that each typed operation with a
// context (cursor, notice and status, nine in all) gives up its wait for the
// State lock: ErrLockTimeout when the
// context's deadline passes while the lock is held elsewhere, the context's
// own error when it is cancelled, and in both cases neither file changes.
// Control: the same call succeeds once the lock is free.
//
// Regression witness: the operations had no context and waited for as long as
// the lock was held, which held a hook past its budget.
//
// It proves the bound of the lock wait only. It does not time the work an
// operation does once it has the lock.
func TestBindOperationLockWaitIsBounded(t *testing.T) {
	owner := BindOperationOwner{Holder: "holder-1", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	alive := func(BindOperationOwner) bool { return false }
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		// prepare runs with the lock free and returns the operation under test.
		prepare func(t *testing.T, p Paths, view BindPartView) func(context.Context) error
	}{
		{"recover cursor", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			return func(ctx context.Context) error { return RecoverBindCursorContext(ctx, p, bindTestName, alive) }
		}},
		{"recover own status", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			return func(ctx context.Context) error { return RecoverBindOwnStatusContext(ctx, p, bindTestName, alive, now) }
		}},
		{"begin cursor", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			token := strings.Repeat("1", 32)
			return func(ctx context.Context) error {
				_, err := BeginBindCursorContext(ctx, p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive)
				if err == nil {
					err = AbortBindCursor(p, bindTestName, token)
				}
				return err
			}
		}},
		{"commit cursor", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			token := strings.Repeat("2", 32)
			if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error {
				_, err := CommitBindCursorContext(ctx, p, bindTestName, token, "s1")
				return err
			}
		}},
		{"abort cursor", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			token := strings.Repeat("3", 32)
			if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error { return AbortBindCursorContext(ctx, p, bindTestName, token) }
		}},
		{"begin own status", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			token := strings.Repeat("4", 32)
			return func(ctx context.Context) error {
				err := BeginBindOwnStatusContext(ctx, p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now)
				if err == nil {
					err = AbortBindOwnStatus(p, bindTestName, token, now)
				}
				return err
			}
		}},
		{"record notice", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			return func(ctx context.Context) error {
				_, err := RecordBindNoticeContext(ctx, p, bindTestName, view.Revision, view.Generation, OwnNoticeObservation{}, "")
				return err
			}
		}},
		{"finish own status", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			token := strings.Repeat("5", 32)
			if err := BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
				t.Fatal(err)
			}
			status := &AgentStatus{State: "idle", Doorbell: "armed", OwnNotice: OwnNoticeObservation{}}
			return func(ctx context.Context) error {
				return FinishBindOwnStatusContext(ctx, p, bindTestName, BindOwnStatusResult{Token: token, Status: status, Published: true, Publication: "published"}, now.Add(time.Second))
			}
		}},
		{"abort own status", func(t *testing.T, p Paths, view BindPartView) func(context.Context) error {
			token := strings.Repeat("6", 32)
			if err := BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
				t.Fatal(err)
			}
			return func(ctx context.Context) error { return AbortBindOwnStatusContext(ctx, p, bindTestName, token, now) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, view := bindFixture(t, "s0", true)
			op := tc.prepare(t, p, view)
			read := func() (state, sidecar []byte) {
				t.Helper()
				state, err := os.ReadFile(p.StateFile(bindTestName))
				if err != nil {
					t.Fatal(err)
				}
				sidecar, err = os.ReadFile(p.BindPartFile(bindTestName))
				if err != nil {
					t.Fatal(err)
				}
				return state, sidecar
			}
			stateBefore, sidecarBefore := read()
			unlock := holdBindTestLock(t, p)
			released := false
			defer func() {
				if !released {
					unlock()
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			start := time.Now()
			err := op(ctx)
			cancel()
			if !errors.Is(err, ErrLockTimeout) {
				t.Fatalf("deadline while the lock is held: %v, want ErrLockTimeout", err)
			}
			if took := time.Since(start); took < 100*time.Millisecond || took > 5*time.Second {
				t.Fatalf("the wait took %v for a 150ms deadline", took)
			}

			ctx, cancel = context.WithCancel(context.Background())
			go func() { time.Sleep(50 * time.Millisecond); cancel() }()
			if err := op(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation while the lock is held: %v, want context.Canceled", err)
			}
			cancel()

			state, sidecar := read()
			if !bytes.Equal(state, stateBefore) || !bytes.Equal(sidecar, sidecarBefore) {
				t.Fatal("a refused operation changed State or the sidecar")
			}

			released = true
			unlock()
			ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := op(ctx); err != nil {
				t.Fatalf("control with the lock free: %v", err)
			}
		})
	}
}

// TestBindStatusOperationLeavesLeaseAndCursorFree proves that a pending own
// status operation does not shut out the identity's other owners: with it
// pending, the lease is acquired, a cursor is reserved, committed and aborted,
// and the sidecar stays a valid document after every step; the status
// operation then finishes as published although the revision has moved.
//
// Controls: a second status operation is still refused while one is pending;
// a control transaction is still refused while a status operation or a cursor
// reservation is pending; and a pending control transaction refuses the cursor.
//
// Regression witness: the sidecar admitted one pending operation at a time
// and bound a status operation to the document's revision, so a publication in
// flight refused the lease and the cursor of every other command.
func TestBindStatusOperationLeavesLeaseAndCursorFree(t *testing.T) {
	p, state, view := bindFixture(t, "s0", true)
	now := time.Now().UTC()
	statusOwner := BindOperationOwner{Holder: "status-writer", Host: "host-1", PID: os.Getpid(), Birth: "birth-status"}
	cursorOwner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "birth-consumer"}
	alive := func(BindOperationOwner) bool { return false }
	statusToken := strings.Repeat("3", 32)
	valid := func(step string) BindPartView {
		t.Helper()
		st, err := LoadState(p, bindTestName)
		if err != nil {
			t.Fatalf("%s: State and sidecar no longer load: %v", step, err)
		}
		v, err := ReadBindPartView(p, bindTestName, st)
		if err != nil {
			t.Fatalf("%s: sidecar view: %v", step, err)
		}
		return v
	}
	if err := BeginBindOwnStatus(p, bindTestName, statusToken, view.Revision, view.Generation, view.SessionID, statusOwner, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	pending := valid("status begun")

	if err := BeginBindOwnStatus(p, bindTestName, strings.Repeat("5", 32), pending.Revision, view.Generation, view.SessionID, statusOwner, now.Add(time.Minute), now); !errors.Is(err, ErrBindPartOperation) {
		t.Fatalf("control: a second status operation = %v, want ErrBindPartOperation", err)
	}
	handover := func() error {
		_, err := SetBindControl(p, bindTestName,
			BindControlExpectation{Revision: valid("handover").Revision, Generation: view.Generation, SessionID: view.SessionID},
			BindControlTarget{Generation: strings.Repeat("b", 32), SessionID: "session-2", Kind: "handover"},
			BindControlProof{OwnerPID: state.Terminal.OwnerPID, OwnerBirth: state.Terminal.OwnerBirth, TargetVerified: true})
		return err
	}
	if err := handover(); !errors.Is(err, ErrBindPartOperation) {
		t.Fatalf("control: a control transaction beside a status operation = %v, want ErrBindPartOperation", err)
	}

	if _, err := AcquireOrRenewBindLease(p, bindTestName, "session-1", "host-1", os.Getpid(), now, time.Minute, 0, nil); err != nil {
		t.Fatalf("lease beside a status operation: %v", err)
	}
	valid("lease acquired")

	first := strings.Repeat("1", 32)
	if _, err := BeginBindCursor(p, bindTestName, first, "delivery", "s0", "", cursorOwner, alive); err != nil {
		t.Fatalf("cursor reservation beside a status operation: %v", err)
	}
	valid("cursor reserved")
	if err := handover(); !errors.Is(err, ErrBindPartOperation) {
		t.Fatalf("control: a control transaction beside both operations = %v, want ErrBindPartOperation", err)
	}
	if _, err := CommitBindCursor(p, bindTestName, first, "s1"); err != nil {
		t.Fatalf("cursor commit beside a status operation: %v", err)
	}
	if moved := valid("cursor committed"); moved.NextBatch != "s1" || moved.Revision <= pending.Revision {
		t.Fatalf("after the commit: cursor=%q revision %d -> %d", moved.NextBatch, pending.Revision, moved.Revision)
	}
	second := strings.Repeat("2", 32)
	if _, err := BeginBindCursor(p, bindTestName, second, "delivery", "s1", "", cursorOwner, alive); err != nil {
		t.Fatal(err)
	}
	if err := AbortBindCursor(p, bindTestName, second); err != nil {
		t.Fatalf("cursor abort beside a status operation: %v", err)
	}
	valid("cursor aborted")

	status := &AgentStatus{State: "idle", Doorbell: "armed", OwnNotice: OwnNoticeObservation{}}
	if err := FinishBindOwnStatus(p, bindTestName, BindOwnStatusResult{Token: statusToken, Status: status, Published: true, Publication: "published"}, now.Add(time.Second)); err != nil {
		t.Fatalf("the status operation after the revision moved: %v", err)
	}
	final, err := LoadState(p, bindTestName)
	if err != nil || final.OwnStatusWrite != nil || final.NoticeWriteUncertain || final.Liveness == nil || final.NextBatch != "s1" {
		t.Fatalf("finished State: write=%v uncertain=%t liveness=%v cursor=%q err=%v", final.OwnStatusWrite, final.NoticeWriteUncertain, final.Liveness, final.NextBatch, err)
	}
	if done := valid("status finished"); done.NoticePublication != "published" || done.Generation != view.Generation || done.SessionID != view.SessionID {
		t.Fatalf("finished view: %+v", done)
	}

	// A pending control transaction still excludes the cursor.
	if err := handover(); err != nil {
		t.Fatalf("control transaction with nothing pending: %v", err)
	}
	part, _, err := readBindPart(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	if part.ControlTxn != nil {
		if _, err := BeginBindCursor(p, bindTestName, strings.Repeat("6", 32), "delivery", "s1", "", cursorOwner, alive); !errors.Is(err, ErrBindPartOperation) || errors.Is(err, ErrBindPartHeld) {
			t.Fatalf("control: cursor reservation beside a control transaction = %v, want the plain ErrBindPartOperation", err)
		}
	}
}

// TestBindOperationOfLiveOwnerIsHeld proves that an operation whose owner is
// alive is reported as ErrBindPartHeld, which is ErrBindPartOperation as well,
// by the recovery of a cursor and by a competing reservation; that the
// reservation is left in place; and that the same calls discard it once its
// owner is gone.
func TestBindOperationOfLiveOwnerIsHeld(t *testing.T) {
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "consumer-1", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	other := BindOperationOwner{Holder: "consumer-2", Host: "host-1", PID: os.Getpid(), Birth: "birth-2"}
	alive := func(BindOperationOwner) bool { return false }
	gone := func(BindOperationOwner) bool { return true }
	token := strings.Repeat("1", 32)
	if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
		t.Fatal(err)
	}
	held := func(step string, err error) {
		t.Helper()
		if !errors.Is(err, ErrBindPartHeld) || !errors.Is(err, ErrBindPartOperation) {
			t.Fatalf("%s = %v, want ErrBindPartHeld", step, err)
		}
		part, _, readErr := readBindPart(p, bindTestName)
		if readErr != nil || part.CursorOp == nil || part.CursorOp.Token != token {
			t.Fatalf("%s: the live owner's reservation is gone (err=%v)", step, readErr)
		}
	}
	held("recovery with a live owner", RecoverBindCursor(p, bindTestName, alive))
	_, err := BeginBindCursor(p, bindTestName, strings.Repeat("2", 32), "delivery", view.NextBatch, "", other, alive)
	held("a competing reservation with a live owner", err)

	if err := RecoverBindCursor(p, bindTestName, gone); err != nil {
		t.Fatalf("recovery once the owner is gone: %v", err)
	}
	if part, _, err := readBindPart(p, bindTestName); err != nil || part.CursorOp != nil {
		t.Fatalf("after the recovery: reservation=%v err=%v", part.CursorOp, err)
	}
	if errors.Is(ErrBindPartOperation, ErrBindPartHeld) {
		t.Fatal("the plain refusal must not count as a held operation")
	}
}
