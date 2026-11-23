package config

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"
)

// failBindPartDirectorySync makes the directory syncs of sidecar writes fail
// from the first-th to the last-th, counted from this call, and every other
// one run as usual. It returns the count of syncs so far.
func failBindPartDirectorySync(t *testing.T, first, last int) *int {
	t.Helper()
	prior := bindPartSyncDirectory
	calls := 0
	bindPartSyncDirectory = func(dir string) error {
		calls++
		if calls >= first && calls <= last {
			return errors.New("planted directory-sync failure")
		}
		return prior(dir)
	}
	t.Cleanup(func() { bindPartSyncDirectory = prior })
	return &calls
}

// TestBindPartWriteWithoutDirectorySyncIsUncertain proves what a sidecar write
// answers, and leaves behind, when the directory cannot be synced after the
// file was replaced. For every kind of operation:
//   - the operation fails with ErrBindPartWrite, "write outcome is uncertain".
//     It is never reported as done;
//   - the state it leaves is the one written down for it in the row. For an
//     operation that changes or ends something, the new document is in place,
//     valid, and what every later reader gets. For one that only begins
//     something (a reservation, a status publication) what it began is taken
//     back, also when the directory stays unhealthy for that second write:
//     its caller was told it failed and would never end it;
//   - State and the sidecar load together afterwards;
//   - the row's next step, with the directory healthy again, succeeds, and its
//     write syncs the directory again.
//
// Control, per row: with a healthy directory the same operation succeeds (the
// fixture's own writes, and the next step, go through the same seam).
//
// Regression witness: the writer read the new file back and, when it matched,
// returned success although the directory sync had failed.
//
// The failure is planted in the writer's seam. No crash is simulated: the
// test shows what is reported and what is in place, not what survives a power
// loss. That is the point of the answer: nothing is claimed about it.
func TestBindPartWriteWithoutDirectorySyncIsUncertain(t *testing.T) {
	owner := BindOperationOwner{Holder: "holder-1", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	alive := func(BindOperationOwner) bool { return false }
	now := time.Now().UTC()
	token := strings.Repeat("c", 32)
	expect := func(view BindPartView) BindControlExpectation {
		return BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID}
	}
	stop := func(p Paths, view BindPartView) error {
		_, err := SetBindControl(p, bindTestName, expect(view),
			BindControlTarget{Generation: view.Generation, SessionID: view.SessionID, Stopped: true, Kind: "stop"}, BindControlProof{})
		return err
	}
	current := func(t *testing.T, p Paths) (bindPartDisk, BindPartView) {
		t.Helper()
		state, err := LoadState(p, bindTestName)
		if err != nil {
			t.Fatalf("State and the sidecar no longer load together: %v", err)
		}
		view, err := ReadBindPartView(p, bindTestName, state)
		if err != nil {
			t.Fatal(err)
		}
		part, _, err := readBindPart(p, bindTestName)
		if err != nil {
			t.Fatal(err)
		}
		return part, view
	}
	reserve := func(p Paths, view BindPartView) error {
		_, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive)
		return err
	}
	notReserved := func(t *testing.T, part bindPartDisk, before, after BindPartView) {
		if part.CursorOp != nil || after.NextBatch != before.NextBatch {
			t.Fatalf("the reservation was not taken back, or the cursor moved: %+v", part.CursorOp)
		}
	}
	// Another live owner reserves at once: nothing of the failed one is held.
	reserveAsAnother := func(p Paths, view BindPartView) error {
		other := BindOperationOwner{Holder: "holder-2", Host: "host-1", PID: os.Getpid(), Birth: "birth-2"}
		_, err := BeginBindCursor(p, bindTestName, strings.Repeat("d", 32), "delivery", view.NextBatch, "", other, alive)
		return err
	}
	beginStatus := func(p Paths, view BindPartView) error {
		return BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now)
	}
	noStatusPending := func(t *testing.T, part bindPartDisk, before, after BindPartView) {
		if part.StatusOp != nil || after.NoticePublication != before.NoticePublication {
			t.Fatalf("the status operation was not taken back: op=%+v publication=%q, was %q", part.StatusOp, after.NoticePublication, before.NoticePublication)
		}
	}
	beginStatusAgain := func(p Paths, view BindPartView) error {
		return BeginBindOwnStatus(p, bindTestName, strings.Repeat("d", 32), view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now)
	}
	for _, tc := range []struct {
		name string
		// first and last are the sidecar writes of the operation, counted from
		// its start, whose directory sync fails.
		first, last int
		prepare     func(t *testing.T, p Paths, view BindPartView)
		do          func(p Paths, view BindPartView) error
		// left checks the state the failed operation leaves, against the view before it.
		left func(t *testing.T, part bindPartDisk, before, after BindPartView)
		// next is the step that follows, from a fresh read.
		next func(p Paths, view BindPartView) error
	}{
		{"reserve the cursor", 1, 1, nil, reserve, notReserved, reserveAsAnother},
		{"reserve the cursor, the directory stays unhealthy", 1, 2, nil, reserve, notReserved, reserveAsAnother},
		{"commit the cursor", 1, 1, func(t *testing.T, p Paths, view BindPartView) {
			if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
				t.Fatal(err)
			}
		}, func(p Paths, view BindPartView) error {
			_, err := CommitBindCursor(p, bindTestName, token, "s1")
			return err
		}, func(t *testing.T, part bindPartDisk, before, after BindPartView) {
			if part.CursorOp != nil || after.NextBatch != "s1" {
				t.Fatalf("the cursor did not move, or stayed reserved: cursor=%q op=%+v", after.NextBatch, part.CursorOp)
			}
		}, func(p Paths, view BindPartView) error {
			// The reservation is gone, so its abort has nothing to release; the
			// next reservation is taken on the new cursor.
			if err := AbortBindCursor(p, bindTestName, token); !errors.Is(err, ErrBindPartStale) {
				return errors.Join(errors.New("abort after the uncertain commit: want stale"), err)
			}
			if _, err := BeginBindCursor(p, bindTestName, strings.Repeat("d", 32), "delivery", "s1", "", owner, alive); err != nil {
				return err
			}
			return AbortBindCursor(p, bindTestName, strings.Repeat("d", 32))
		}},
		{"release the cursor", 1, 1, func(t *testing.T, p Paths, view BindPartView) {
			if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
				t.Fatal(err)
			}
		}, func(p Paths, view BindPartView) error { return AbortBindCursor(p, bindTestName, token) },
			func(t *testing.T, part bindPartDisk, before, after BindPartView) {
				if part.CursorOp != nil || after.NextBatch != before.NextBatch {
					t.Fatalf("the reservation is still there, or the cursor moved: %+v", part.CursorOp)
				}
			}, func(p Paths, view BindPartView) error {
				_, err := BeginBindCursor(p, bindTestName, strings.Repeat("d", 32), "delivery", view.NextBatch, "", owner, alive)
				return err
			}},
		{"take the lease", 1, 1, nil, func(p Paths, view BindPartView) error {
			_, err := AcquireOrRenewBindLease(p, bindTestName, "holder-1", "host-1", os.Getpid(), now, time.Minute, 0, nil)
			return err
		}, func(t *testing.T, part bindPartDisk, before, after BindPartView) {
			if after.Lease == nil || after.Lease.Holder != "holder-1" {
				t.Fatalf("the lease is not in place: %+v", after.Lease)
			}
		}, func(p Paths, view BindPartView) error {
			// Another holder is refused as by any held lease; its holder releases it.
			if _, err := AcquireOrRenewBindLease(p, bindTestName, "holder-2", "host-1", os.Getpid(), now, time.Minute, 0, nil); err == nil {
				return errors.New("another holder took a lease that is in place")
			}
			released, err := ReleaseBindLeaseContext(context.Background(), p, bindTestName, "holder-1", false, now, nil)
			if err == nil && !released {
				err = errors.New("the holder could not release the lease that is in place")
			}
			return err
		}},
		{"record a notice", 1, 1, nil, func(p Paths, view BindPartView) error {
			_, err := RecordBindNotice(p, bindTestName, view.Revision, view.Generation, OwnNoticeObservation{}, "unavailable: own liveness publication failed")
			return err
		}, func(t *testing.T, part bindPartDisk, before, after BindPartView) {
			if after.NoticePublication != "unavailable: own liveness publication failed" {
				t.Fatalf("the notice record is not in place: %q", after.NoticePublication)
			}
		}, func(p Paths, view BindPartView) error {
			_, err := RecordBindNotice(p, bindTestName, view.Revision, view.Generation, OwnNoticeObservation{}, "")
			return err
		}},
		{"begin a status publication", 1, 1, nil, beginStatus, noStatusPending, beginStatusAgain},
		{"begin a status publication, the directory stays unhealthy", 1, 2, nil, beginStatus, noStatusPending, beginStatusAgain},
		{"finish a status publication", 1, 1, func(t *testing.T, p Paths, view BindPartView) {
			if err := BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
				t.Fatal(err)
			}
		}, func(p Paths, view BindPartView) error {
			status := &AgentStatus{State: "idle", Doorbell: "armed", OwnNotice: OwnNoticeObservation{}}
			return FinishBindOwnStatus(p, bindTestName, BindOwnStatusResult{Token: token, Status: status, Published: true, Publication: "published"}, now.Add(time.Second))
		}, func(t *testing.T, part bindPartDisk, before, after BindPartView) {
			if part.StatusOp != nil || after.NoticePublication != "published" {
				t.Fatalf("the publication is not recorded: op=%+v publication=%q", part.StatusOp, after.NoticePublication)
			}
		}, func(p Paths, view BindPartView) error {
			return BeginBindOwnStatus(p, bindTestName, strings.Repeat("d", 32), view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now)
		}},
		{"stop, at the record of the change", 1, 1, nil, stop,
			func(t *testing.T, part bindPartDisk, before, after BindPartView) {
				// The record is in place and nothing of the change is applied. The
				// next holder of the lock discards it; here that is the stop below.
				if part.ControlTxn == nil || after.Stopped || after.Generation != before.Generation {
					t.Fatalf("the binding changed, or the record is not in place: %+v stopped=%t", part.ControlTxn, after.Stopped)
				}
			}, stop},
		{"stop, at the change itself", 2, 2, nil, stop,
			func(t *testing.T, part bindPartDisk, before, after BindPartView) {
				if part.ControlTxn != nil || !after.Stopped || after.Generation != before.Generation {
					t.Fatalf("the stop is not in place: %+v stopped=%t", part.ControlTxn, after.Stopped)
				}
			}, func(p Paths, view BindPartView) error {
				// The same stop again finds it done and writes nothing; the next
				// write of the identity is another operation.
				if err := stop(p, view); err != nil {
					return err
				}
				_, err := RecordBindNotice(p, bindTestName, view.Revision, view.Generation, OwnNoticeObservation{}, "unavailable: own liveness publication failed")
				return err
			}},
		{"handover with its lease, at the change itself", 2, 2, nil, func(p Paths, view BindPartView) error {
			_, err := TransferBindSessionAndLeaseContext(context.Background(), p, bindTestName, expect(view),
				BindControlTarget{Generation: view.Generation, SessionID: "session-2", Stopped: view.Stopped, Kind: "handover"},
				BindLeaseTransfer{OldHolder: "test-holder", NewHolder: "session-2", Host: "host-1", PID: os.Getpid(), Now: now, TTL: time.Minute, Dead: func(string) bool { return true }},
				BindControlProof{OwnerPID: os.Getpid(), OwnerBirth: "1", TargetVerified: true})
			return err
		}, func(t *testing.T, part bindPartDisk, before, after BindPartView) {
			if after.SessionID != "session-2" || after.Lease == nil || after.Lease.Holder != "session-2" || after.Generation != before.Generation {
				t.Fatalf("the handover is not in place: session=%q lease=%+v", after.SessionID, after.Lease)
			}
		}, stop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, before := bindFixture(t, "s0", true)
			if tc.prepare != nil {
				tc.prepare(t, p, before)
				_, before = current(t, p)
			}
			calls := failBindPartDirectorySync(t, tc.first, tc.last)
			err := tc.do(p, before)
			if !errors.Is(err, ErrBindPartWrite) {
				t.Fatalf("the operation answered %v, want ErrBindPartWrite", err)
			}
			if *calls < tc.last {
				t.Fatalf("premise: the operation synced the directory %d times, the failure is planted up to the %d", *calls, tc.last)
			}
			part, after := current(t, p)
			if st, err := LoadState(p, bindTestName); err != nil || st.OwnStatusWrite != nil && part.StatusOp == nil {
				t.Fatalf("State records a status publication the sidecar does not: %+v err=%v", st.OwnStatusWrite, err)
			}
			tc.left(t, part, before, after)
			synced := *calls
			if err := tc.next(p, after); err != nil {
				t.Fatalf("the next step, with the directory healthy: %v", err)
			}
			if *calls == synced {
				t.Fatal("the next step did not sync the directory again")
			}
			current(t, p)
		})
	}
}

// TestBindMigrationWithoutDirectorySyncLeavesTheIdentityUnmarked is the
// migration's row of TestBindPartWriteWithoutDirectorySyncIsUncertain. The
// migration writes the sidecar first and the marker second. When the
// directory sync of the sidecar fails, the migration answers ErrBindPartWrite,
// takes the sidecar away again and writes no marker: State is byte for byte
// as it was, loads as an unmarked State, and no sidecar is left. A sidecar
// left without its marker would refuse every command of the identity.
// Control: the next migration, with the directory healthy, succeeds.
func TestBindMigrationWithoutDirectorySyncLeavesTheIdentityUnmarked(t *testing.T) {
	p := testPaths(t)
	if err := SaveState(p, bindTestName, State{NextBatch: "s0", Threads: []string{"$keep"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	calls := failBindPartDirectorySync(t, 1, 1)
	_, err = EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
	if !errors.Is(err, ErrBindPartWrite) {
		t.Fatalf("the migration answered %v, want ErrBindPartWrite", err)
	}
	if *calls < 1 {
		t.Fatal("premise: the migration did not sync the directory")
	}
	after, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the failed migration changed State: err=%v", err)
	}
	if _, err := os.Lstat(p.BindPartFile(bindTestName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the failed migration left its sidecar: %v", err)
	}
	if st, err := LoadState(p, bindTestName); err != nil || BindPartMarked(st) || st.NextBatch != "s0" {
		t.Fatalf("the identity after the failed migration: marked=%t cursor=%q err=%v", BindPartMarked(st), st.NextBatch, err)
	}
	st, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
	if err != nil || !BindPartActive(st) {
		t.Fatalf("control: the next migration: active=%t err=%v", BindPartActive(st), err)
	}
}

// TestBindReservationThatCannotBeTakenBackStaysWithItsOwner shows the limit of
// the take-back, so that it is written down and not discovered. The directory
// sync of a reservation fails, and the write that takes the reservation back
// cannot replace the file at all. The reservation then stays in place, the
// beginning still answers ErrBindPartWrite and names the token beside that
// error, another live owner is refused as by any held reservation, and the
// owner releases it by its token.
//
// The token beside the error is what the caller keeps it by: an error that
// left nothing behind (here: the refusal of the other owner) comes with an
// empty reservation. The caller that keeps it is shown in the cli package
// (TestCursorReservationThatStaysIsReleasedByItsOwner).
func TestBindReservationThatCannotBeTakenBackStaysWithItsOwner(t *testing.T) {
	p, _, view := bindFixture(t, "s0", true)
	owner := BindOperationOwner{Holder: "holder-1", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	other := BindOperationOwner{Holder: "holder-2", Host: "host-1", PID: os.Getpid(), Birth: "birth-2"}
	alive := func(BindOperationOwner) bool { return false }
	token := strings.Repeat("c", 32)
	failBindPartDirectorySync(t, 1, 1)
	restore := failSecondBindPartRename(t)
	left, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive)
	restore()
	if !errors.Is(err, ErrBindPartWrite) {
		t.Fatalf("the reservation answered %v, want ErrBindPartWrite", err)
	}
	if left.Token != token {
		t.Fatalf("the failed reservation names the token %q beside its error, want %q", left.Token, token)
	}
	part, _, err := readBindPart(p, bindTestName)
	if err != nil || part.CursorOp == nil || part.CursorOp.Token != token {
		t.Fatalf("premise: the reservation is not in place: %+v err=%v", part.CursorOp, err)
	}
	refused, err := BeginBindCursor(p, bindTestName, strings.Repeat("d", 32), "delivery", view.NextBatch, "", other, alive)
	if !errors.Is(err, ErrBindPartHeld) {
		t.Fatalf("another live owner beside the reservation: %v, want ErrBindPartHeld", err)
	}
	if refused != (BindCursorReservation{}) {
		t.Fatalf("a refusal that wrote nothing names a reservation: %+v", refused)
	}
	if err := AbortBindCursor(p, bindTestName, token); err != nil {
		t.Fatalf("the owner's release by its token: %v", err)
	}
	if _, err := BeginBindCursor(p, bindTestName, strings.Repeat("d", 32), "delivery", view.NextBatch, "", other, alive); err != nil {
		t.Fatalf("control: after the release: %v", err)
	}
}
