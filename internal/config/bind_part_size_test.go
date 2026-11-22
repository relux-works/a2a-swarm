package config

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBindPartHoldsTheCursorBudgetInEveryPhase proves that a sync cursor of
// the full budget, 8 KiB, fits the sidecar through migration, a commit and
// the reservation after it, and that the identity's State loads after every
// step. TestBindPartLargestDocumentFitsInEveryPhase is the same claim for the
// largest document there can be.
//
// Regression witness: the document's limit was 16 KiB, below two copies of
// such a cursor. Migration, or the reservation after a commit, wrote a
// document larger than the reader accepts, and every later load of the State
// refused the identity.
func TestBindPartHoldsTheCursorBudgetInEveryPhase(t *testing.T) {
	cursor := strings.Repeat("c", 8<<10)
	owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
	alive := func(BindOperationOwner) bool { return false }
	loads := func(t *testing.T, p Paths, step, want string) {
		t.Helper()
		st, err := LoadState(p, bindTestName)
		if err != nil {
			info, _ := os.Stat(p.BindPartFile(bindTestName))
			size := int64(-1)
			if info != nil {
				size = info.Size()
			}
			t.Fatalf("%s: the State no longer loads (sidecar of %d bytes): %v", step, size, err)
		}
		if st.NextBatch != want {
			t.Fatalf("%s: cursor of %d bytes, want %d", step, len(st.NextBatch), len(want))
		}
	}

	t.Run("migration", func(t *testing.T) {
		p := testPaths(t)
		if err := SaveState(p, bindTestName, State{NextBatch: cursor}); err != nil {
			t.Fatal(err)
		}
		if _, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Now: time.Now()}); err != nil {
			t.Fatalf("migration of a State with a cursor of the budget: %v", err)
		}
		loads(t, p, "after the migration", cursor)
	})

	t.Run("commit and next reservation", func(t *testing.T) {
		p, _, view := bindFixture(t, "s0", false)
		first, second := strings.Repeat("1", 32), strings.Repeat("2", 32)
		if _, err := BeginBindCursor(p, bindTestName, first, "delivery", view.NextBatch, "", owner, alive); err != nil {
			t.Fatal(err)
		}
		if _, err := CommitBindCursor(p, bindTestName, first, cursor); err != nil {
			t.Fatalf("commit of a cursor of the budget: %v", err)
		}
		loads(t, p, "after the commit", cursor)
		if _, err := BeginBindCursor(p, bindTestName, second, "delivery", cursor, "", owner, alive); err != nil {
			t.Fatalf("reservation that expects a cursor of the budget: %v", err)
		}
		loads(t, p, "with the reservation pending", cursor)
		if err := AbortBindCursor(p, bindTestName, second); err != nil {
			t.Fatal(err)
		}
		loads(t, p, "after the abort", cursor)
	})
}

// TestBindPartRefusesAnOversizedDocumentBeforeReplacing proves the other side
// of the size limit: a write whose document would exceed it is refused before
// the file is replaced. The sidecar's bytes are those from before the write,
// the State still loads with the old cursor, the refused reservation can be
// released, and a cursor that fits is committed afterwards.
//
// Regression witness: the writer replaced the file first and checked the
// readback afterwards, so an oversized document was left in place and the
// identity refused from then on.
func TestBindPartRefusesAnOversizedDocumentBeforeReplacing(t *testing.T) {
	t.Run("migration", func(t *testing.T) {
		// A State whose cursor is one byte over the budget is not migrated:
		// no sidecar appears, and the State stays unmarked and loads.
		p := testPaths(t)
		over := strings.Repeat("c", bindCursorBudget+1)
		if err := SaveState(p, bindTestName, State{NextBatch: over}); err != nil {
			t.Fatal(err)
		}
		_, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Now: time.Now()})
		if !errors.Is(err, ErrBindPartTooLarge) {
			t.Fatalf("migration with a cursor over the budget = %v, want ErrBindPartTooLarge", err)
		}
		if _, statErr := os.Lstat(p.BindPartFile(bindTestName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("the refused migration left a sidecar: %v", statErr)
		}
		st, err := LoadState(p, bindTestName)
		if err != nil || BindPartMarked(st) || st.NextBatch != over {
			t.Fatalf("after the refused migration: marked=%t cursor of %d bytes err=%v", BindPartMarked(st), len(st.NextBatch), err)
		}
	})
	t.Run("a document that does not read back", func(t *testing.T) {
		// A cursor that is not valid UTF-8 changes when it is encoded, so the
		// document would not read back as written. It is within every size
		// limit; the writer finds out by decoding its own bytes first.
		p, _, view := bindFixture(t, "s0", false)
		owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
		token := strings.Repeat("1", 32)
		if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, func(BindOperationOwner) bool { return false }); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(p.BindPartFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		_, err = CommitBindCursor(p, bindTestName, token, "s1\xff")
		if !errors.Is(err, ErrBindPartInvalid) || errors.Is(err, ErrBindPartWrite) {
			t.Fatalf("commit of a cursor that does not survive encoding = %v, want it refused as invalid before any write", err)
		}
		after, readErr := os.ReadFile(p.BindPartFile(bindTestName))
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("the refused write replaced the sidecar (read error %v)", readErr)
		}
		if st, err := LoadState(p, bindTestName); err != nil || st.NextBatch != "s0" {
			t.Fatalf("after the refusal: cursor=%q err=%v", st.NextBatch, err)
		}
	})
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
	alive := func(BindOperationOwner) bool { return false }
	token := strings.Repeat("1", 32)
	if _, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.BindPartFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	tooLarge := strings.Repeat("x", 256<<10)
	_, err = CommitBindCursor(p, bindTestName, token, tooLarge)
	if err == nil {
		t.Fatal("a cursor of 256 KiB was committed")
	}
	after, readErr := os.ReadFile(p.BindPartFile(bindTestName))
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("the refused write replaced the sidecar (%d bytes before, %d after, read error %v); refusal: %v", len(before), len(after), readErr, err)
	}
	if !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("refusal = %v, want it to name the size limit", err)
	}
	st, err := LoadState(p, bindTestName)
	if err != nil || st.NextBatch != "s0" {
		t.Fatalf("after the refusal: cursor=%q err=%v", st.NextBatch, err)
	}
	if err := AbortBindCursor(p, bindTestName, token); err != nil {
		t.Fatalf("release of the reservation after the refusal: %v", err)
	}
	second := strings.Repeat("2", 32)
	if _, err := BeginBindCursor(p, bindTestName, second, "delivery", "s0", "", owner, alive); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitBindCursor(p, bindTestName, second, "s1"); err != nil {
		t.Fatalf("a cursor that fits, after the refusal: %v", err)
	}
	if st, err := LoadState(p, bindTestName); err != nil || st.NextBatch != "s1" {
		t.Fatalf("after the fitting commit: cursor=%q err=%v", st.NextBatch, err)
	}
}

// largestBindState returns a State whose every field that the sidecar takes
// over is as long as the sidecar admits, in the form that grows most when it
// is encoded: a cursor of the budget, a lease, a binding and a notice.
func largestBindState(t *testing.T) State {
	t.Helper()
	wide := func(n int) string { return strings.Repeat("<", n) }
	stamp := func(second string) string {
		s := "2026-01-02T03:04:" + second + "."
		return s + strings.Repeat("9", bindNoticeTimestampMax-len(s)-1) + "Z"
	}
	suspected := true
	publication := ""
	for _, v := range bindNoticePublications {
		if len(v) > len(publication) {
			publication = v
		}
	}
	return State{
		NextBatch: wide(bindCursorBudget),
		Lease:     &Lease{Holder: wide(200), Host: wide(128), PID: os.Getpid(), Expires: time.Now().Add(time.Hour)},
		Terminal: &TerminalBinding{
			Generation: wide(128), SessionID: wide(200), Host: "host-1",
			OwnerPID: os.Getpid(), OwnerBirth: "1", Interval: time.Second, RetryMax: time.Minute, Outcome: "running",
			OwnNotice:         OwnNoticeObservation{PossiblyOrphaned: &suspected, Reason: "owned-notice-orphan-suspected", Since: stamp("05"), ObservedAt: stamp("06")},
			NoticePublication: publication,
		},
	}
}

// TestBindPartLargestDocumentFitsInEveryPhase builds the largest document the
// sidecar can be asked to hold and takes it through every phase that writes
// it: migration, a reservation beside a pending status operation, a commit of
// another cursor of the budget, a stop and a handover with its lease. Every
// field is as long as the validator admits and filled with a character the
// encoder turns into six bytes. Each step succeeds, the State loads after it,
// and the file is within the limit.
//
// The limit is derived from the cursor budget and the largest document
// without a cursor (bindPartMaxBytes). This test does not use that
// derivation: it reaches the documents through the operations, so it fails if
// the derivation leaves something out.
//
// It does not prove that the homeserver's cursors stay within the budget; a
// larger one is refused before the write (the test after this one).
func TestBindPartLargestDocumentFitsInEveryPhase(t *testing.T) {
	p := testPaths(t)
	state := largestBindState(t)
	if err := SaveState(p, bindTestName, state); err != nil {
		t.Fatal(err)
	}
	largest := int64(0)
	fits := func(step string) BindPartView {
		t.Helper()
		st, err := LoadState(p, bindTestName)
		if err != nil {
			t.Fatalf("%s: the State no longer loads: %v", step, err)
		}
		info, err := os.Stat(p.BindPartFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() > int64(bindPartMaxBytes) {
			t.Fatalf("%s: a sidecar of %d bytes, over the limit of %d", step, info.Size(), bindPartMaxBytes)
		}
		if info.Size() > largest {
			largest = info.Size()
		}
		view, err := ReadBindPartView(p, bindTestName, st)
		if err != nil {
			t.Fatalf("%s: sidecar view: %v", step, err)
		}
		return view
	}
	wide := func(n int) string { return strings.Repeat("<", n) }
	alive := func(BindOperationOwner) bool { return false }
	owner := BindOperationOwner{Holder: wide(200), Host: wide(128), PID: os.Getpid(), Birth: wide(128)}
	now := time.Now().UTC()

	if _, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: state.Lease.Holder, Now: now}); err != nil {
		t.Fatalf("migration of the largest State: %v", err)
	}
	view := fits("after the migration")

	if err := BeginBindOwnStatus(p, bindTestName, strings.Repeat("3", 32), view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
		t.Fatalf("status operation: %v", err)
	}
	first := strings.Repeat("1", 32)
	if _, err := BeginBindCursor(p, bindTestName, first, "projector", state.NextBatch, view.Generation, owner, alive); err != nil {
		t.Fatalf("reservation beside the status operation: %v", err)
	}
	fits("with a reservation and a status operation pending")
	other := strings.Repeat("&", bindCursorBudget)
	if _, err := CommitBindCursor(p, bindTestName, first, other); err != nil {
		t.Fatalf("commit of another cursor of the budget: %v", err)
	}
	fits("after the commit")
	if err := AbortBindOwnStatus(p, bindTestName, strings.Repeat("3", 32), now); err != nil {
		t.Fatalf("release of the status operation: %v", err)
	}
	view = fits("after the status operation")

	st, err := LoadState(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	proof := BindControlProof{OwnerPID: st.Terminal.OwnerPID, OwnerBirth: st.Terminal.OwnerBirth, Target: st.Terminal.Target, TargetVerified: true}
	expected := BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID}
	if _, err := TransferBindSessionAndLease(p, bindTestName, expected,
		BindControlTarget{Generation: view.Generation, SessionID: strings.Repeat(">", 200), Stopped: view.Stopped, Kind: "handover"},
		BindLeaseTransfer{OldHolder: state.Lease.Holder, NewHolder: strings.Repeat(">", 200), Host: wide(128), PID: os.Getpid(), Now: now, TTL: time.Hour, Dead: func(string) bool { return true }},
		proof); err != nil {
		t.Fatalf("handover with its lease: %v", err)
	}
	view = fits("after the handover")

	if _, err := SetBindControl(p, bindTestName,
		BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID},
		BindControlTarget{Generation: view.Generation, SessionID: view.SessionID, Stopped: true, Kind: "stop"}, BindControlProof{}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if view = fits("after the stop"); !view.Stopped {
		t.Fatal("the stop did not take effect")
	}
	// Two cursors of the budget alone encode to twelve times the budget.
	if largest < int64(2*6*bindCursorBudget) {
		t.Fatalf("the largest sidecar was %d bytes: the cursors were not at their longest form", largest)
	}
	t.Logf("largest sidecar %d bytes, limit %d", largest, bindPartMaxBytes)
}

// TestBindPartSizeLimitIsExact proves where the limit is drawn: with the
// limit one byte below a document's size the write is refused and the
// sidecar's bytes are those from before; with the limit at its size the same
// write is admitted.
func TestBindPartSizeLimitIsExact(t *testing.T) {
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
	alive := func(BindOperationOwner) bool { return false }
	token := strings.Repeat("1", 32)
	reserve := func() error {
		_, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive)
		return err
	}
	size := func() int {
		t.Helper()
		data, err := os.ReadFile(p.BindPartFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		return len(data)
	}
	// The same reservation, taken and released once, tells the size it has.
	if err := reserve(); err != nil {
		t.Fatal(err)
	}
	reserved := size()
	if err := AbortBindCursor(p, bindTestName, token); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.BindPartFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	prior := bindPartMaxBytes
	t.Cleanup(func() { bindPartMaxBytes = prior })

	bindPartMaxBytes = reserved - 1
	err = reserve()
	if !errors.Is(err, ErrBindPartTooLarge) {
		t.Fatalf("a document one byte over the limit = %v, want ErrBindPartTooLarge", err)
	}
	after, readErr := os.ReadFile(p.BindPartFile(bindTestName))
	if readErr != nil || !bytes.Equal(before, after) {
		t.Fatalf("the refused write replaced the sidecar (%d bytes before, %d after, read error %v)", len(before), len(after), readErr)
	}
	bindPartMaxBytes = prior
	if _, err := LoadState(p, bindTestName); err != nil {
		t.Fatalf("after the refusal the State no longer loads: %v", err)
	}

	bindPartMaxBytes = reserved
	if err := reserve(); err != nil {
		t.Fatalf("a document exactly at the limit: %v", err)
	}
	if got := size(); got != reserved {
		t.Fatalf("the reservation is %d bytes this time, %d before: the two writes are not the same document", got, reserved)
	}
}

// TestBindCursorReservationKeepsTheWholeDigest proves that a reservation
// records the SHA-256 of the exact bytes of the cursor it was taken for, and
// that the commit compares all of it. With the cursor in the sidecar replaced
// by another of the same length, differing in its last byte, in its first
// byte, or by one trailing byte, the commit is refused and nothing moves; with
// the reserved cursor back, the commit goes through.
//
// The other cursor is put in place by a direct write of the document, which
// no operation does: a reservation's cursor moves only through its own commit.
func TestBindCursorReservationKeepsTheWholeDigest(t *testing.T) {
	cursor := strings.Repeat("c", bindCursorBudget)
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "consumer", Host: "host-1", PID: os.Getpid(), Birth: "start-1"}
	alive := func(BindOperationOwner) bool { return false }
	first, second := strings.Repeat("1", 32), strings.Repeat("2", 32)
	if _, err := BeginBindCursor(p, bindTestName, first, "delivery", view.NextBatch, "", owner, alive); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitBindCursor(p, bindTestName, first, cursor); err != nil {
		t.Fatal(err)
	}
	if _, err := BeginBindCursor(p, bindTestName, second, "delivery", cursor, "", owner, alive); err != nil {
		t.Fatal(err)
	}
	part, _, err := readBindPart(p, bindTestName)
	if err != nil || part.CursorOp == nil {
		t.Fatalf("reservation: %+v err=%v", part.CursorOp, err)
	}
	sum := sha256.Sum256([]byte(cursor))
	if want := hex.EncodeToString(sum[:]); part.CursorOp.ExpectedCursorDigest != want {
		t.Fatalf("the reservation records %q, want the SHA-256 of the cursor %q", part.CursorOp.ExpectedCursorDigest, want)
	}
	raw, err := os.ReadFile(p.BindPartFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte(cursor)); n != 1 {
		t.Fatalf("the document holds the cursor %d times, want once: the reservation must not repeat it", n)
	}
	put := func(next string) {
		t.Helper()
		current, _, err := readBindPart(p, bindTestName)
		if err != nil {
			t.Fatal(err)
		}
		current.NextBatch = next
		if err := writeBindPart(p, bindTestName, current); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, other string }{
		{"last byte differs", cursor[:len(cursor)-1] + "d"},
		{"first byte differs", "d" + cursor[1:]},
		{"one byte shorter", cursor[:len(cursor)-1]},
	} {
		put(tc.other)
		if _, err := CommitBindCursor(p, bindTestName, second, "next"); !errors.Is(err, ErrBindPartStale) {
			t.Fatalf("%s: commit over another cursor = %v, want ErrBindPartStale", tc.name, err)
		}
		if st, err := LoadState(p, bindTestName); err != nil || st.NextBatch != tc.other {
			t.Fatalf("%s: the refused commit moved the cursor (err=%v)", tc.name, err)
		}
	}
	put(cursor)
	if _, err := CommitBindCursor(p, bindTestName, second, "next"); err != nil {
		t.Fatalf("commit with the reserved cursor back in place: %v", err)
	}
	if st, err := LoadState(p, bindTestName); err != nil || st.NextBatch != "next" {
		t.Fatalf("after the commit: cursor=%q err=%v", st.NextBatch, err)
	}
}
