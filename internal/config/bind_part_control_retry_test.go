package config

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// failSecondBindPartRename makes the second replacement of the sidecar fail
// and returns the restore function. A control change writes the sidecar
// twice under one hold of the State lock: its record, then the result.
func failSecondBindPartRename(t *testing.T) func() {
	t.Helper()
	prior := bindPartRename
	calls := 0
	bindPartRename = func(from, to string) error {
		calls++
		if calls == 2 {
			return os.ErrPermission
		}
		return os.Rename(from, to)
	}
	restore := func() { bindPartRename = prior }
	t.Cleanup(restore)
	return restore
}

// TestBindControlAfterAnInterruptedChange proves what a new invocation finds
// after a control change whose second write failed. It has only what it reads
// afresh: no revision or token of the failed call. For a stop, a create and a
// handover with its lease:
//   - the State loads and shows the binding as it was before the change;
//   - the same change, presented from a fresh read, is performed;
//   - or, instead, other commands come first: a lease, a cursor reservation
//     and a status operation are each admitted, the first of them removes the
//     record the interrupted change left, the binding is still the old one,
//     and the change is performed after them.
//
// Regression witness: the interrupted change stayed recorded with the
// revision its caller had read. A fresh read shows a later revision, so the
// same change was refused as held by another owner, a create with its new
// generation could never match, and the lease of every other command was
// refused as well. Only the failed call's own revision could finish it.
func TestBindControlAfterAnInterruptedChange(t *testing.T) {
	type attempt func(p Paths, state State, view BindPartView) (BindPartView, error)
	proofOf := func(state State) BindControlProof {
		return BindControlProof{OwnerPID: state.Terminal.OwnerPID, OwnerBirth: state.Terminal.OwnerBirth, Target: state.Terminal.Target, TargetVerified: true}
	}
	expect := func(view BindPartView) BindControlExpectation {
		return BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID}
	}
	now := time.Now().UTC()
	for _, tc := range []struct {
		name string
		do   attempt
		done func(before, after BindPartView) bool
	}{
		{"stop", func(p Paths, state State, view BindPartView) (BindPartView, error) {
			return SetBindControl(p, bindTestName, expect(view),
				BindControlTarget{Generation: view.Generation, SessionID: view.SessionID, Stopped: true, Kind: "stop"}, BindControlProof{})
		}, func(before, after BindPartView) bool {
			return after.Stopped && after.Generation == before.Generation && after.SessionID == before.SessionID
		}},
		{"create", func(p Paths, state State, view BindPartView) (BindPartView, error) {
			// A new invocation makes a new generation: it cannot repeat the old one.
			generation, err := NewBindToken()
			if err != nil {
				return BindPartView{}, err
			}
			return SetBindControl(p, bindTestName, expect(view),
				BindControlTarget{Generation: generation, SessionID: "session-2", Kind: "create"}, proofOf(state))
		}, func(before, after BindPartView) bool {
			return !after.Stopped && after.Generation != before.Generation && after.SessionID == "session-2"
		}},
		{"handover with its lease", func(p Paths, state State, view BindPartView) (BindPartView, error) {
			return TransferBindSessionAndLease(p, bindTestName, expect(view),
				BindControlTarget{Generation: view.Generation, SessionID: "session-2", Stopped: view.Stopped, Kind: "handover"},
				BindLeaseTransfer{OldHolder: "test-holder", NewHolder: "session-2", Host: "host-1", PID: os.Getpid(), Now: now, TTL: time.Minute, Dead: func(string) bool { return true }},
				proofOf(state))
		}, func(before, after BindPartView) bool {
			return after.Generation == before.Generation && after.SessionID == "session-2" && after.Lease != nil && after.Lease.Holder == "session-2"
		}},
	} {
		for _, first := range []string{"the same change again", "other commands first"} {
			t.Run(tc.name+"/"+first, func(t *testing.T) {
				p, state, before := bindFixture(t, "s0", true)
				restore := failSecondBindPartRename(t)
				if _, err := tc.do(p, state, before); err == nil {
					t.Fatal("premise: the change's second write did not fail")
				}
				restore()

				fresh := func(step string) (State, BindPartView) {
					t.Helper()
					st, err := LoadState(p, bindTestName)
					if err != nil {
						t.Fatalf("%s: the State no longer loads: %v", step, err)
					}
					v, err := ReadBindPartView(p, bindTestName, st)
					if err != nil {
						t.Fatalf("%s: sidecar view: %v", step, err)
					}
					return st, v
				}
				st, view := fresh("after the interrupted change")
				if view.Generation != before.Generation || view.SessionID != before.SessionID || view.Stopped != before.Stopped {
					t.Fatalf("after the interrupted change the binding is %q/%q stopped=%t, want it as before", view.Generation, view.SessionID, view.Stopped)
				}

				if first == "other commands first" {
					holder := "test-holder"
					if view.Lease != nil {
						holder = view.Lease.Holder
					}
					if _, err := AcquireOrRenewBindLease(p, bindTestName, holder, "host-1", os.Getpid(), now, time.Minute, 0, func(string) bool { return true }); err != nil {
						t.Fatalf("another command's lease after the interrupted change: %v", err)
					}
					if part, _, err := readBindPart(p, bindTestName); err != nil || part.ControlTxn != nil {
						t.Fatalf("the first operation after the interrupted change left its record: %+v err=%v", part.ControlTxn, err)
					}
					owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
					alive := func(BindOperationOwner) bool { return false }
					_, view = fresh("after the lease")
					cursorToken := strings.Repeat("1", 32)
					if _, err := BeginBindCursor(p, bindTestName, cursorToken, "delivery", view.NextBatch, "", owner, alive); err != nil {
						t.Fatalf("a cursor reservation after the interrupted change: %v", err)
					}
					if err := AbortBindCursor(p, bindTestName, cursorToken); err != nil {
						t.Fatal(err)
					}
					_, view = fresh("after the reservation")
					statusToken := strings.Repeat("3", 32)
					if err := BeginBindOwnStatus(p, bindTestName, statusToken, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
						t.Fatalf("a status operation after the interrupted change: %v", err)
					}
					if err := AbortBindOwnStatus(p, bindTestName, statusToken, now); err != nil {
						t.Fatal(err)
					}
					st, view = fresh("after the other commands")
					if view.Generation != before.Generation || view.SessionID != before.SessionID || view.Stopped != before.Stopped {
						t.Fatalf("the other commands changed the binding to %q/%q stopped=%t", view.Generation, view.SessionID, view.Stopped)
					}
				}

				after, err := tc.do(p, st, view)
				if err != nil {
					t.Fatalf("the change from a fresh read (revision %d, the interrupted call read %d): %v", view.Revision, before.Revision, err)
				}
				if !tc.done(before, after) {
					t.Fatalf("the change did not take effect: before %+v after %+v", before, after)
				}
				_, final := fresh("after the change")
				if !tc.done(before, final) {
					t.Fatalf("a fresh read does not show the change: %+v", final)
				}
				part, _, err := readBindPart(p, bindTestName)
				if err != nil || part.ControlTxn != nil {
					t.Fatalf("a control record is left in the sidecar: %+v err=%v", part.ControlTxn, err)
				}
			})
		}
	}
}

// TestBindControlFailedSecondWriteReportsTheFailure is the control for the
// test above: the injected failure is the failure the caller of the
// interrupted change gets, and no further write is attempted by that call.
func TestBindControlFailedSecondWriteReportsTheFailure(t *testing.T) {
	p, _, view := bindFixture(t, "s0", true)
	failSecondBindPartRename(t)
	_, err := SetBindControl(p, bindTestName,
		BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID},
		BindControlTarget{Generation: view.Generation, SessionID: view.SessionID, Stopped: true, Kind: "stop"}, BindControlProof{})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("the interrupted change = %v, want the injected write failure", err)
	}
	if strings.Contains(err.Error(), "held by another owner") {
		t.Fatalf("the interrupted change was reported as held: %v", err)
	}
}

// TestBindStatusOperationOfALiveOwnerExpires proves that a status operation
// whose owner is alive is held only until the deadline it was begun with.
// Before the deadline, recovery leaves it and answers that it is held. After
// it, recovery clears it: the State token is gone, the publication is recorded
// as failed and uncertain, never as published, and the next status operation
// is admitted.
//
// Regression witness: recovery released a live owner's operation never. A
// writer that could neither finish nor abort, for the same fault that failed
// its write, kept every later status write of the identity pending for as
// long as its process ran, and a bound consumer runs for days.
//
// Control: an owner that is gone is recovered before the deadline, as before.
func TestBindStatusOperationOfALiveOwnerExpires(t *testing.T) {
	alive := func(BindOperationOwner) bool { return false }
	gone := func(BindOperationOwner) bool { return true }
	owner := BindOperationOwner{Holder: "status-writer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
	for _, tc := range []struct {
		name string
		dead func(BindOperationOwner) bool
		late time.Duration
	}{
		{"live owner, after the deadline", alive, 2 * time.Minute},
		{"control: owner gone, before the deadline", gone, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _, view := bindFixture(t, "s0", true)
			now := time.Now().UTC()
			token := strings.Repeat("3", 32)
			if err := BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
				t.Fatal(err)
			}
			if err := RecoverBindOwnStatus(p, bindTestName, alive, now.Add(time.Second)); !errors.Is(err, ErrBindPartHeld) {
				t.Fatalf("recovery of a live owner's operation before its deadline = %v, want ErrBindPartHeld", err)
			}
			if part, _, err := readBindPart(p, bindTestName); err != nil || part.StatusOp == nil {
				t.Fatalf("the held operation is gone: err=%v", err)
			}
			if err := RecoverBindOwnStatus(p, bindTestName, tc.dead, now.Add(tc.late)); err != nil {
				t.Fatalf("recovery: %v", err)
			}
			st, err := LoadState(p, bindTestName)
			if err != nil || st.OwnStatusWrite != nil || !st.NoticeWriteUncertain {
				t.Fatalf("after the recovery: token=%v uncertain=%t err=%v", st.OwnStatusWrite, st.NoticeWriteUncertain, err)
			}
			cleared, err := ReadBindPartView(p, bindTestName, st)
			if err != nil || cleared.NoticePublication == "published" || cleared.Generation != view.Generation {
				t.Fatalf("after the recovery: publication=%q generation=%q err=%v", cleared.NoticePublication, cleared.Generation, err)
			}
			if part, _, err := readBindPart(p, bindTestName); err != nil || part.StatusOp != nil {
				t.Fatalf("the operation was not cleared: err=%v", err)
			}
			if err := BeginBindOwnStatus(p, bindTestName, strings.Repeat("4", 32), cleared.Revision, cleared.Generation, cleared.SessionID, owner, now.Add(time.Hour), now.Add(tc.late)); err != nil {
				t.Fatalf("the next status operation after the recovery: %v", err)
			}
		})
	}
}
