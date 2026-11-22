package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const bindTestName = "bind-fixture"

// TestAcquireOrRenewBindLeaseCheckedContextChecksEveryMarkedLeasePath proves the caller's check runs
// against the merged State under the lease lock on both lease paths: the first
// acquisition and the steady renewal inside the margin. A check that refuses
// leaves the sidecar view unchanged.
func TestAcquireOrRenewBindLeaseCheckedContextChecksEveryMarkedLeasePath(t *testing.T) {
	p, state, _ := bindFixture(t, "cursor", false)
	if !BindPartActive(state) {
		t.Fatal("fixture is not an active marked bind part")
	}
	now := time.Now()
	called := false
	view, err := AcquireOrRenewBindLeaseCheckedContext(context.Background(), p, bindTestName, "checked-holder", "host-1", os.Getpid(), now, time.Minute, 0, nil, func(cur *State) error {
		called = true
		if !BindPartMarked(*cur) || cur.Lease != nil {
			t.Fatalf("initial checked state marked=%t lease=%+v", BindPartMarked(*cur), cur.Lease)
		}
		return nil
	})
	if err != nil || !called || view.Lease == nil || view.Lease.Holder != "checked-holder" {
		t.Fatalf("initial checked lease=%+v called=%t err=%v", view.Lease, called, err)
	}
	state, err = LoadState(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	before, err := ReadBindPartView(p, bindTestName, state)
	if err != nil {
		t.Fatal(err)
	}
	denied := errors.New("checked lease denied")
	called = false
	_, err = AcquireOrRenewBindLeaseCheckedContext(context.Background(), p, bindTestName, "checked-holder", "host-1", os.Getpid(), now.Add(time.Second), time.Minute, 0, nil, func(cur *State) error {
		called = true
		if cur.Lease == nil || cur.Lease.Holder != "checked-holder" {
			t.Fatalf("steady checked state lease=%+v", cur.Lease)
		}
		return denied
	})
	if !errors.Is(err, denied) || !called {
		t.Fatalf("steady checked lease called=%t err=%v, want callback refusal", called, err)
	}
	state, err = LoadState(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	after, err := ReadBindPartView(p, bindTestName, state)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("denied checked lease changed bind view\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func bindFixture(t *testing.T, cursor string, terminal bool) (Paths, State, BindPartView) {
	t.Helper()
	p := testPaths(t)
	state := State{NextBatch: cursor, Threads: []string{"$keep"}}
	if terminal {
		state.Terminal = &TerminalBinding{
			Generation: strings.Repeat("a", 32), SessionID: "session-1", Host: "host-1",
			OwnerPID: os.Getpid(), OwnerBirth: "1", Interval: time.Second,
			RetryMax: time.Minute, Outcome: "running",
		}
	}
	if err := SaveState(p, bindTestName, state); err != nil {
		t.Fatal(err)
	}
	state, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	view, err := ReadBindPartView(p, bindTestName, state)
	if err != nil {
		t.Fatal(err)
	}
	return p, state, view
}

// TestBindPartMigrationOwnsProjectionAndKeepsMarker proves the first
// migration moves the cursor into the sidecar, a generic State write of the
// cursor is then refused with ErrBindPartOwned, an ordinary field is still
// written, and that ordinary write keeps the marker on disk.
func TestBindPartMigrationOwnsProjectionAndKeepsMarker(t *testing.T) {
	p, _, view := bindFixture(t, "legacy-cursor", false)
	if view.NextBatch != "legacy-cursor" || view.Revision == 0 {
		t.Fatalf("migrated view = %+v", view)
	}
	if _, err := UpdateState(p, bindTestName, func(state *State) error {
		state.NextBatch = "forbidden-cursor"
		return nil
	}); !errors.Is(err, ErrBindPartOwned) {
		t.Fatalf("generic cursor write = %v, want provider refusal", err)
	}
	if _, err := UpdateState(p, bindTestName, func(state *State) error {
		state.Threads = append(state.Threads, "$ordinary-field")
		return nil
	}); err != nil {
		t.Fatalf("ordinary State write: %v", err)
	}
	state, err := LoadState(p, bindTestName)
	if err != nil || state.NextBatch != "legacy-cursor" || len(state.Threads) != 2 || !BindPartMarked(state) {
		t.Fatalf("merged State next=%q threads=%v marked=%t err=%v", state.NextBatch, state.Threads, BindPartMarked(state), err)
	}
	raw, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	marked, err := bindPartMarkerInInput(raw)
	if err != nil || !marked {
		t.Fatalf("ordinary write lost permanent marker: marked=%t err=%v", marked, err)
	}
}

// TestBindPartPreservesLegacyGenerationAndRequiresFreshReplacementToken proves
// migration keeps a binding's existing generation and session, and that a
// replacement generation is admitted only as a fresh 128-bit hex token: any
// other value is refused with ErrBindPartInvalid.
func TestBindPartPreservesLegacyGenerationAndRequiresFreshReplacementToken(t *testing.T) {
	p := testPaths(t)
	state := State{Terminal: &TerminalBinding{
		Generation: "prior-fixture", SessionID: "session-1", Host: "host-1",
		OwnerPID: os.Getpid(), OwnerBirth: "birth-1",
	}}
	if err := SaveState(p, bindTestName, state); err != nil {
		t.Fatal(err)
	}
	state, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	view, err := ReadBindPartView(p, bindTestName, state)
	if err != nil || view.Generation != "prior-fixture" || view.SessionID != "session-1" {
		t.Fatalf("legacy binding view=%+v err=%v", view, err)
	}
	proof := BindControlProof{TargetVerified: true, OwnerPID: state.Terminal.OwnerPID, OwnerBirth: state.Terminal.OwnerBirth, Target: state.Terminal.Target}
	expected := BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID}
	if _, err := SetBindControl(p, bindTestName, expected, BindControlTarget{Generation: "untrusted-new-value", SessionID: "session-2", Kind: "create"}, proof); !errors.Is(err, ErrBindPartInvalid) {
		t.Fatalf("new non-token generation=%v, want invalid refusal", err)
	}
	target := BindControlTarget{Generation: strings.Repeat("c", 32), SessionID: "session-2", Kind: "create"}
	updated, err := SetBindControl(p, bindTestName, expected, target, proof)
	if err != nil || updated.Generation != target.Generation || updated.SessionID != target.SessionID {
		t.Fatalf("fresh generation from legacy binding=%+v err=%v", updated, err)
	}
}

// TestBindPartLeaseRefusesLiveForeignOwner proves a live foreign holder is
// refused with LeaseHeldError and the lease in the sidecar is unchanged, and
// that the same holder acquires it once the owner has released it.
func TestBindPartLeaseRefusesLiveForeignOwner(t *testing.T) {
	p, _, _ := bindFixture(t, "s0", false)
	now := time.Now().UTC()
	owned, err := AcquireOrRenewBindLease(p, bindTestName, "session-1", "host-1", os.Getpid(), now, time.Minute, 0, nil)
	if err != nil || owned.Lease == nil || owned.Lease.Holder != "session-1" {
		t.Fatalf("acquire own lease=%+v err=%v", owned.Lease, err)
	}
	if _, err := AcquireOrRenewBindLease(p, bindTestName, "session-2", "host-1", os.Getpid()+1, now, time.Minute, 0, func(string) bool { return false }); err == nil {
		t.Fatal("live foreign holder acquired the lease")
	} else {
		var held *LeaseHeldError
		if !errors.As(err, &held) {
			t.Fatalf("foreign lease refusal=%v, want held lease", err)
		}
	}
	state, err := LoadState(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	current, err := ReadBindPartView(p, bindTestName, state)
	if err != nil || current.Lease == nil || *current.Lease != *owned.Lease {
		t.Fatalf("foreign refusal changed lease: before=%+v after=%+v err=%v", owned.Lease, current.Lease, err)
	}
	if changed, err := ReleaseBindLease(p, bindTestName, "session-1", false, now, nil); err != nil || !changed {
		t.Fatalf("release own lease changed=%t err=%v", changed, err)
	}
	updated, err := AcquireOrRenewBindLease(p, bindTestName, "session-2", "host-1", os.Getpid()+1, now.Add(time.Second), time.Minute, 0, nil)
	if err != nil || updated.Lease == nil || updated.Lease.Holder != "session-2" {
		t.Fatalf("available lease was not acquired: %+v err=%v", updated.Lease, err)
	}
}

// TestAcquireBindLeaseReturnsLockedSidecarView proves a lease acquisition
// returns the view it committed under the lock, with one State load and no
// second one to read the result back.
func TestAcquireBindLeaseReturnsLockedSidecarView(t *testing.T) {
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache(bindTestName))
	if err := SaveState(p, bindTestName, State{Threads: []string{"$thread"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, bindTestName); err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	now := time.Now().UTC()
	view, err := AcquireOrRenewBindLease(p, bindTestName, "session-1", "host-1", os.Getpid(), now, time.Minute, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	after := ReadStateDecodeStats()
	if after.LoadStateCalls-before.LoadStateCalls != 1 {
		t.Fatalf("lease result reloaded State after its locked sidecar operation: before=%d after=%d", before.LoadStateCalls, after.LoadStateCalls)
	}
	if view.Revision == 0 || view.Lease == nil || view.Lease.Holder != "session-1" || view.Lease.Host != "host-1" || view.Lease.PID != os.Getpid() || !view.Lease.Expires.Equal(now.Add(time.Minute)) {
		t.Fatalf("lease result does not reflect committed sidecar: %+v", view)
	}
}

// TestLoadBindTerminalAvoidsLargeStateClone proves the terminal projection of
// a cached marked State is returned without a full State load or decode, and
// carries both the State-held and the sidecar-held terminal fields. The cache
// hit may read the file: it compares the bytes with the cached ones, which is
// what keeps a same-stamp overwrite from being served.
func TestLoadBindTerminalAvoidsLargeStateClone(t *testing.T) {
	name := bindTestName
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache(name))
	state := State{
		Threads:      make([]string, 450),
		ThreadEvents: make([]ThreadEvent, 2000),
		Terminal: &TerminalBinding{
			Generation: strings.Repeat("a", 32), SessionID: "session-1", Host: "host-1",
			Outcome: "running", Rung: []string{"$retained"},
		},
	}
	for i := range state.Threads {
		state.Threads[i] = "$thread-" + strings.Repeat("t", 64) + string(rune('a'+i%26))
	}
	for i := range state.ThreadEvents {
		state.ThreadEvents[i] = ThreadEvent{Room: "!" + strings.Repeat("r", 44), EventID: "$" + strings.Repeat("e", 78) + string(rune('a'+i%26)), Root: "$" + strings.Repeat("t", 62)}
	}
	if err := SaveState(p, name, state); err != nil {
		t.Fatal(err)
	}
	marked, err := EnsureBindPart(p, name, BindPartOpenOptions{Holder: "seed", Now: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := ReadBindPartView(p, name, marked)
	if err != nil {
		t.Fatal(err)
	}
	publication := "unavailable: no liveness record (a2a status set)"
	if _, err := LoadState(p, name); err != nil {
		t.Fatal(err)
	} // warm the validated cache before the provider notice update
	if _, err := RecordBindNotice(p, name, provider.Revision, provider.Generation, OwnNoticeObservation{}, publication); err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	terminal, err := LoadBindTerminal(p, name)
	if err != nil {
		t.Fatal(err)
	}
	after := ReadStateDecodeStats()
	if after.LoadStateCalls != before.LoadStateCalls || after.UnmarshalStatePasses != before.UnmarshalStatePasses || after.WriterClones != before.WriterClones {
		t.Fatalf("terminal projection loaded, decoded or cloned the full State: before=%+v after=%+v", before, after)
	}
	if terminal == nil || terminal.Generation != provider.Generation || terminal.SessionID != provider.SessionID || terminal.Outcome != "running" || len(terminal.Rung) != 1 || terminal.Rung[0] != "$retained" || terminal.NoticePublication != publication {
		t.Fatalf("terminal projection lost State or provider fields: %+v", terminal)
	}
	terminal.Rung[0] = "$caller-mutation"
	cached, err := LoadBindTerminal(p, name)
	if err != nil || cached == nil || len(cached.Rung) != 1 || cached.Rung[0] != "$retained" {
		t.Fatalf("terminal projection exposed cached mutable data: %+v err=%v", cached, err)
	}
	if err := os.Remove(p.BindPartFile(name)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBindTerminal(p, name); !errors.Is(err, ErrBindPartMissing) {
		t.Fatalf("missing marked sidecar = %v, want refusal", err)
	}
}

// TestLoadBindTerminalRefreshesAndRejectsChangedState proves the terminal
// projection is not answered from the cache after another writer replaced
// State: the new State is read once in full, and a replacement whose
// provider-owned fields disagree with the sidecar is refused with
// ErrBindPartInvalid, after full validation and without changing the file.
func TestLoadBindTerminalRefreshesAndRejectsChangedState(t *testing.T) {
	name := bindTestName
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache(name))
	state := State{Threads: []string{"$retained"}, Terminal: &TerminalBinding{
		Generation: strings.Repeat("a", 32), SessionID: "session-1", Outcome: "running",
	}}
	if err := SaveState(p, name, state); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBindPart(p, name, BindPartOpenOptions{Holder: "seed", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, name); err != nil {
		t.Fatal(err)
	} // retain the validated legacy body before each foreign replacement

	writeForeignState := func(change func(*State)) []byte {
		t.Helper()
		current, err := LoadState(p, name)
		if err != nil {
			t.Fatal(err)
		}
		change(&current)
		raw, err := sealedState(current)
		if err != nil {
			t.Fatal(err)
		}
		path := p.StateFile(name)
		replacement := path + ".replacement"
		if err := os.WriteFile(replacement, raw, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(replacement, path); err != nil {
			t.Fatal(err)
		}
		return raw
	}

	writeForeignState(func(current *State) { current.Terminal.Outcome = "foreign-but-valid" })
	before := ReadStateDecodeStats()
	refreshed, err := LoadBindTerminal(p, name)
	after := ReadStateDecodeStats()
	if err != nil || refreshed == nil || refreshed.Outcome != "foreign-but-valid" {
		t.Fatalf("changed valid State reused stale terminal data: terminal=%+v err=%v", refreshed, err)
	}
	if after.UnmarshalStatePasses-before.UnmarshalStatePasses != 1 || after.StateReadAttempts-before.StateReadAttempts != 1 {
		t.Fatalf("changed State did not use the full reader exactly once: before=%+v after=%+v", before, after)
	}

	invalid := writeForeignState(func(current *State) { current.Terminal.Generation = strings.Repeat("b", 32) })
	before = ReadStateDecodeStats()
	if _, err := LoadBindTerminal(p, name); !errors.Is(err, ErrBindPartInvalid) {
		t.Fatalf("changed provider shadow = %v, want stale State refusal", err)
	}
	after = ReadStateDecodeStats()
	if after.UnmarshalStatePasses-before.UnmarshalStatePasses != 1 {
		t.Fatalf("stale State refusal skipped full validation: before=%+v after=%+v", before, after)
	}
	got, err := os.ReadFile(p.StateFile(name))
	if err != nil || !bytes.Equal(got, invalid) {
		t.Fatalf("stale State refusal changed its source: equal=%t err=%v", bytes.Equal(got, invalid), err)
	}
}

// TestMarkedStateWriteDoesNotMutateCallbackOwnedTerminal proves a write of a
// marked State does not alter the terminal value its callback owns, while the
// merged State read back carries the sidecar's generation and session.
func TestMarkedStateWriteDoesNotMutateCallbackOwnedTerminal(t *testing.T) {
	p, _, view := bindFixture(t, "s0", false)
	generation := strings.Repeat("d", 32)
	target := BindControlTarget{Generation: generation, SessionID: "session-new", Kind: "create"}
	proof := BindControlProof{TargetVerified: true, OwnerPID: os.Getpid(), OwnerBirth: "birth-new"}
	updated, err := SetBindControl(p, bindTestName,
		BindControlExpectation{Revision: view.Revision}, target, proof)
	if err != nil {
		t.Fatal(err)
	}
	next := &TerminalBinding{Generation: generation, SessionID: target.SessionID, Outcome: "starting"}
	if _, err := UpdateState(p, bindTestName, func(state *State) error {
		state.Terminal = next
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if next.Generation != generation || next.SessionID != target.SessionID {
		t.Fatalf("writer mutated caller-owned terminal: generation_kept=%t session_kept=%t", next.Generation == generation, next.SessionID == target.SessionID)
	}
	state, err := LoadState(p, bindTestName)
	if err != nil || state.Terminal == nil || state.Terminal.Generation != generation || state.Terminal.SessionID != target.SessionID || updated.Generation != generation {
		t.Fatalf("merged terminal state invalid: present=%t generation_kept=%t session_kept=%t err=%v", state.Terminal != nil, state.Terminal != nil && state.Terminal.Generation == generation, state.Terminal != nil && state.Terminal.SessionID == target.SessionID, err)
	}
}

// TestBindPartCursorReservationAndDeadOwnerReplay proves the cursor
// reservation: a second live owner is refused and the cursor stays, a
// reservation of a dead owner may be replayed under a new token, the replaced
// token can no longer commit (ErrBindPartStale), and the new one commits.
func TestBindPartCursorReservationAndDeadOwnerReplay(t *testing.T) {
	p, state, view := bindFixture(t, "s0", false)
	owner1 := BindOperationOwner{Holder: "projector-1", Host: "host-1", PID: 2345, Birth: "birth-1"}
	owner2 := BindOperationOwner{Holder: "projector-2", Host: "host-1", PID: 3456, Birth: "birth-2"}
	token1 := strings.Repeat("1", 32)
	token2 := strings.Repeat("2", 32)
	first, err := BeginBindCursor(p, bindTestName, token1, "projector", view.NextBatch, "", owner1, func(BindOperationOwner) bool { return false })
	if err != nil || first.Cursor != "s0" {
		t.Fatalf("first reservation=%+v err=%v", first, err)
	}
	if _, err := BeginBindCursor(p, bindTestName, token2, "projector", view.NextBatch, "", owner2, func(BindOperationOwner) bool { return false }); !errors.Is(err, ErrBindPartOperation) {
		t.Fatalf("live competing reservation=%v", err)
	}
	unchanged, err := LoadState(p, bindTestName)
	if err != nil || unchanged.NextBatch != "s0" {
		t.Fatalf("refusal changed cursor: next=%q err=%v", unchanged.NextBatch, err)
	}
	if _, err := BeginBindCursor(p, bindTestName, token2, "projector", view.NextBatch, "", owner2, func(owner BindOperationOwner) bool { return owner == owner1 }); err != nil {
		t.Fatalf("dead-owner replay: %v", err)
	}
	if _, err := CommitBindCursor(p, bindTestName, token1, "s1"); !errors.Is(err, ErrBindPartStale) {
		t.Fatalf("replaced token commit=%v, want stale refusal", err)
	}
	committed, err := CommitBindCursor(p, bindTestName, token2, "s1")
	if err != nil || committed.NextBatch != "s1" {
		t.Fatalf("commit=%+v err=%v", committed, err)
	}
	merged, err := LoadState(p, bindTestName)
	if err != nil || merged.NextBatch != "s1" || !BindPartMarked(merged) {
		t.Fatalf("committed merged State next=%q marked=%t err=%v", merged.NextBatch, BindPartMarked(merged), err)
	}
	_ = state
}

// TestBindCursorExcludesMarkedControlMonitorAndLegacyWriters proves that
// while a cursor operation is reserved every other writer of the cursor and of
// the binding is refused - a second cursor owner, a control change and a
// generic State cursor write - and that none of the refusals changes State or
// the sidecar. The lease is not what a cursor reservation protects: the
// monitor's renewal of it is admitted beside the reservation, so that a
// consumer writing its mailbox never costs another one its lease.
func TestBindCursorExcludesMarkedControlMonitorAndLegacyWriters(t *testing.T) {
	p := monitorSeed(t)
	state, err := LoadState(p, "a")
	if err != nil || state.Lease == nil {
		t.Fatalf("monitor fixture lease=%+v err=%v", state.Lease, err)
	}
	marked, err := EnsureBindPart(p, "a", BindPartOpenOptions{Holder: state.Lease.Holder, Now: time.Now()})
	if err != nil || !BindPartActive(marked) {
		t.Fatalf("activate marked monitor fixture: active=%t err=%v", BindPartActive(marked), err)
	}
	record := monitorRecord()
	err = WithMonitorGeneration(p, "a", time.Now().Add(time.Second), MonitorArm, record, func(g *MonitorGeneration) error {
		current, err := LoadState(p, "a")
		if err != nil {
			return err
		}
		view, err := ReadBindPartView(p, "a", current)
		if err != nil {
			return err
		}
		if view.Lease == nil {
			return fmt.Errorf("marked monitor fixture has no lease")
		}
		cursorToken := strings.Repeat("c", 32)
		owner := BindOperationOwner{Holder: "projector", Host: "host-1", PID: 2345, Birth: "birth-1"}
		if _, err := BeginBindCursor(p, "a", cursorToken, "projector", view.NextBatch, "", owner, nil); err != nil {
			return err
		}
		stateBefore, err := os.ReadFile(p.StateFile("a"))
		if err != nil {
			return err
		}
		partBefore, err := os.ReadFile(p.BindPartFile("a"))
		if err != nil {
			return err
		}
		secondOwner := BindOperationOwner{Holder: "delivery", Host: "host-1", PID: 3456, Birth: "birth-2"}
		if _, err := BeginBindCursor(p, "a", strings.Repeat("d", 32), "delivery", view.NextBatch, "", secondOwner, nil); !errors.Is(err, ErrBindPartOperation) {
			return fmt.Errorf("second cursor owner=%v, want operation refusal", err)
		}
		if _, err := SetBindControl(p, "a", BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID}, BindControlTarget{Stopped: true, Kind: "stop"}, BindControlProof{}); !errors.Is(err, ErrBindPartOperation) {
			return fmt.Errorf("marked bind control=%v, want operation refusal", err)
		}
		if _, err := UpdateState(p, "a", func(st *State) error { st.NextBatch = "forbidden-legacy-cursor"; return nil }); !errors.Is(err, ErrBindPartOwned) {
			return fmt.Errorf("legacy State cursor write=%v, want provider refusal", err)
		}
		stateAfter, err := os.ReadFile(p.StateFile("a"))
		if err != nil || !bytes.Equal(stateBefore, stateAfter) {
			return fmt.Errorf("refused writes changed State: err=%v equal=%t", err, bytes.Equal(stateBefore, stateAfter))
		}
		partAfter, err := os.ReadFile(p.BindPartFile("a"))
		if err != nil || !bytes.Equal(partBefore, partAfter) {
			return fmt.Errorf("refused writers changed sidecar: err=%v equal=%t", err, bytes.Equal(partBefore, partAfter))
		}
		if err := g.RenewOwnerLease(record.Generation); err != nil {
			return fmt.Errorf("monitor renewal beside a cursor reservation=%v, want it admitted", err)
		}
		if stored, _, err := readBindPart(p, "a"); err != nil || stored.CursorOp == nil || stored.CursorOp.Token != cursorToken {
			return fmt.Errorf("the reservation did not survive the lease renewal: err=%v", err)
		}
		if _, err := CommitBindCursor(p, "a", cursorToken, "next-cursor"); err != nil {
			return err
		}
		return RenewBindMonitorLease(p, "a", g.record, g.owner, time.Now())
	})
	if err != nil {
		t.Fatalf("cursor ownership contract: %v", err)
	}
}

// TestBindPartOwnStatusRequiresTwoReservationsAndFencesTransfer proves an own
// status publication is reserved in State and in the sidecar, is not reported
// published while pending, blocks a control transfer for its duration, and
// records its publication result when it finishes.
func TestBindPartOwnStatusRequiresTwoReservationsAndFencesTransfer(t *testing.T) {
	p, state, view := bindFixture(t, "s0", true)
	now := time.Now().UTC()
	token := strings.Repeat("3", 32)
	owner := BindOperationOwner{Holder: "status-writer", Host: "host-1", PID: os.Getpid(), Birth: "birth-status"}
	until := now.Add(time.Minute)
	if err := BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, until, now); err != nil {
		t.Fatal(err)
	}
	reserved, err := LoadState(p, bindTestName)
	if err != nil || reserved.OwnStatusWrite == nil || reserved.OwnStatusWrite.Token != token || !reserved.NoticeWriteUncertain {
		t.Fatalf("State reservation=%+v err=%v", reserved.OwnStatusWrite, err)
	}
	part, _, err := readBindPart(p, bindTestName)
	if err != nil || part.StatusOp == nil || part.StatusOp.Token != token {
		t.Fatalf("sidecar reservation=%+v err=%v", part.StatusOp, err)
	}
	if part.projection().NoticePublication == "published" {
		t.Fatal("pending status operation reported published")
	}
	if _, err := SetBindControl(p, bindTestName,
		BindControlExpectation{Revision: part.Revision, Generation: view.Generation, SessionID: view.SessionID},
		BindControlTarget{Generation: strings.Repeat("b", 32), SessionID: "session-2", Kind: "handover"},
		BindControlProof{OwnerPID: state.Terminal.OwnerPID, OwnerBirth: state.Terminal.OwnerBirth, TargetVerified: true}); !errors.Is(err, ErrBindPartOperation) {
		t.Fatalf("control transfer with status owner=%v", err)
	}
	status := &AgentStatus{State: "idle", Doorbell: "armed", OwnNotice: OwnNoticeObservation{}}
	if err := FinishBindOwnStatus(p, bindTestName, BindOwnStatusResult{Token: token, Status: status, Published: true, Publication: "published"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	final, err := LoadState(p, bindTestName)
	if err != nil || final.OwnStatusWrite != nil || final.NoticeWriteUncertain || final.Liveness == nil {
		t.Fatalf("finished State token=%v uncertain=%t liveness=%v err=%v", final.OwnStatusWrite, final.NoticeWriteUncertain, final.Liveness, err)
	}
	view, err = ReadBindPartView(p, bindTestName, final)
	if err != nil || view.NoticePublication != "published" {
		t.Fatalf("finished publication=%q err=%v", view.NoticePublication, err)
	}
}

// TestBindPartControlTransactionResumesExactPendingTarget proves a control
// change whose final write failed stays recorded as a pending transaction and
// completes only when the same target is presented again.
func TestBindPartControlTransactionResumesExactPendingTarget(t *testing.T) {
	p, state, view := bindFixture(t, "s0", true)
	target := BindControlTarget{Generation: strings.Repeat("b", 32), SessionID: "session-2", Kind: "create"}
	proof := BindControlProof{OwnerPID: state.Terminal.OwnerPID, OwnerBirth: state.Terminal.OwnerBirth, Target: state.Terminal.Target, TargetVerified: true}
	expected := BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID}
	prior := bindPartRename
	calls := 0
	bindPartRename = func(old, next string) error {
		calls++
		if calls == 2 {
			return os.ErrPermission
		}
		return os.Rename(old, next)
	}
	if _, err := SetBindControl(p, bindTestName, expected, target, proof); err == nil {
		t.Fatal("injected final control write failure was ignored")
	}
	bindPartRename = prior
	pending, _, err := readBindPart(p, bindTestName)
	if err != nil || pending.ControlTxn == nil || pending.Generation != view.Generation {
		t.Fatalf("pending transaction=%+v generation=%q err=%v", pending.ControlTxn, pending.Generation, err)
	}
	if _, err := SetBindControl(p, bindTestName, expected, target, proof); err != nil {
		t.Fatalf("exact pending transaction did not resume: %v", err)
	}
	current, err := LoadState(p, bindTestName)
	if err != nil || current.Terminal.Generation != target.Generation || current.Terminal.SessionID != target.SessionID {
		t.Fatalf("resumed control generation=%q session=%q err=%v", current.Terminal.Generation, current.Terminal.SessionID, err)
	}
}

// TestBindPartFailedOwnStatusFinalizationStaysUncertain proves a status
// finalization that fails before its rename leaves the publication marked
// uncertain in State and in the sidecar view, never published.
func TestBindPartFailedOwnStatusFinalizationStaysUncertain(t *testing.T) {
	p, _, view := bindFixture(t, "s0", true)
	now := time.Now().UTC()
	token := strings.Repeat("4", 32)
	owner := BindOperationOwner{Holder: "status-writer", Host: "host-1", PID: os.Getpid(), Birth: "birth-status"}
	if err := BeginBindOwnStatus(p, bindTestName, token, view.Revision, view.Generation, view.SessionID, owner, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	prior := bindPartRename
	bindPartRename = func(string, string) error { return os.ErrPermission }
	t.Cleanup(func() { bindPartRename = prior })
	status := &AgentStatus{State: "idle", Doorbell: "armed", OwnNotice: OwnNoticeObservation{}}
	err := FinishBindOwnStatus(p, bindTestName, BindOwnStatusResult{Token: token, Status: status, Published: true, Publication: "published"}, now.Add(time.Second))
	if err == nil {
		t.Fatal("injected pre-rename failure was ignored")
	}
	state, err := LoadState(p, bindTestName)
	if err != nil || !state.NoticeWriteUncertain || state.Terminal.NoticePublication == "published" {
		t.Fatalf("failed finalization State uncertain=%t publication=%q err=%v", state.NoticeWriteUncertain, state.Terminal.NoticePublication, err)
	}
	view, err = ReadBindPartView(p, bindTestName, state)
	if err != nil || view.NoticePublication == "published" {
		t.Fatalf("failed finalization view publication=%q err=%v", view.NoticePublication, err)
	}
}

// TestBindPartRollbackKeepsPermanentTombstone proves rollback keeps the
// marker: export copies the sidecar's cursor into State and admits a legacy
// writer, import adopts that writer's cursor, and a State whose sidecar was
// then removed is refused with ErrBindPartMissing.
func TestBindPartRollbackKeepsPermanentTombstone(t *testing.T) {
	p, _, _ := bindFixture(t, "provider-cursor", false)
	if err := ExportBindPartForRollback(p, bindTestName, time.Now()); err != nil {
		t.Fatal(err)
	}
	legacy, err := LoadState(p, bindTestName)
	if err != nil || legacy.NextBatch != "provider-cursor" || !BindPartMarked(legacy) {
		t.Fatalf("exported State cursor=%q marked=%t err=%v", legacy.NextBatch, BindPartMarked(legacy), err)
	}
	if _, err := UpdateState(p, bindTestName, func(state *State) error { state.NextBatch = "legacy-cursor"; return nil }); err != nil {
		t.Fatalf("legacy rollback writer: %v", err)
	}
	if err := ImportBindPartAfterRollback(p, bindTestName); err != nil {
		t.Fatal(err)
	}
	merged, err := LoadState(p, bindTestName)
	if err != nil || merged.NextBatch != "legacy-cursor" || !BindPartMarked(merged) {
		t.Fatalf("imported cursor=%q marked=%t err=%v", merged.NextBatch, BindPartMarked(merged), err)
	}
	if err := os.Remove(p.BindPartFile(bindTestName)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, bindTestName); !errors.Is(err, ErrBindPartMissing) {
		t.Fatalf("missing sidecar after import=%v", err)
	}
}

// TestBindPartStrictSidecarRejectsAliasesUnknownFieldsAndUnsafeFiles proves
// each malformed or unsafe sidecar in the table - a duplicate key, a case
// alias, an unknown field, a file mode other than 0600 and a symlink - is
// refused when State is loaded, and that the refusal leaves State unchanged.
func TestBindPartStrictSidecarRejectsAliasesUnknownFieldsAndUnsafeFiles(t *testing.T) {
	tests := []struct {
		name   string
		change func(*testing.T, Paths, []byte)
	}{
		{name: "duplicate key", change: func(t *testing.T, p Paths, data []byte) {
			data = bytes.Replace(data, []byte("{\n"), []byte("{\n  \"identity\": \"other\",\n"), 1)
			if err := os.WriteFile(p.BindPartFile(bindTestName), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "case alias", change: func(t *testing.T, p Paths, data []byte) {
			data = bytes.Replace(data, []byte("\"identity\":"), []byte("\"Identity\":"), 1)
			if err := os.WriteFile(p.BindPartFile(bindTestName), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unknown field", change: func(t *testing.T, p Paths, data []byte) {
			data = bytes.Replace(data, []byte("{\n"), []byte("{\n  \"unexpected\": true,\n"), 1)
			if err := os.WriteFile(p.BindPartFile(bindTestName), data, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unsafe mode", change: func(t *testing.T, p Paths, _ []byte) {
			if err := os.Chmod(p.BindPartFile(bindTestName), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "symlink", change: func(t *testing.T, p Paths, _ []byte) {
			path := p.BindPartFile(bindTestName)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(path), "other.bind.json")
			if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, _, _ := bindFixture(t, "s0", false)
			before, err := os.ReadFile(p.StateFile(bindTestName))
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(p.BindPartFile(bindTestName))
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, p, data)
			if _, err := LoadState(p, bindTestName); !errors.Is(err, ErrBindPartInvalid) {
				t.Fatalf("unsafe sidecar accepted: %v", err)
			}
			after, err := os.ReadFile(p.StateFile(bindTestName))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed State: err=%v", err)
			}
		})
	}
}

// TestBindPartNoticeAllowlistMatchesExistingObservationForms proves the
// sidecar's own-notice validation accepts the three observation forms in its
// table (empty, cleared, and suspected orphan with the timestamp spellings
// State accepts) and refuses the malformed forms listed after them. The table
// is written by hand; it is not derived from the writers.
func TestBindPartNoticeAllowlistMatchesExistingObservationForms(t *testing.T) {
	trueValue, falseValue := true, false
	valid := []bindPartOwnNotice{
		{},
		{PossiblyOrphaned: &falseValue, ObservedAt: "2026-11-19T10:20:30Z"},
		{PossiblyOrphaned: &trueValue, Reason: "owned-notice-orphan-suspected", Since: "2026-11-19T10:20:30.1-00:00", ObservedAt: "2026-11-19t10:20:30.100000000000000000000z"},
	}
	for i, notice := range valid {
		if err := validateBindOwnNotice(notice); err != nil {
			t.Errorf("valid notice %d rejected: %v", i, err)
		}
	}
	invalid := []bindPartOwnNotice{
		{PossiblyOrphaned: &trueValue, Reason: "owned-notice-orphan-suspected", Since: "2026-11-19T1:20:30Z", ObservedAt: "2026-11-19T10:20:30Z"},
		{PossiblyOrphaned: &falseValue, Reason: "owned-notice-orphan-suspected", Since: "2026-11-19T10:20:30Z", ObservedAt: "2026-11-19T10:20:31Z"},
		{PossiblyOrphaned: &trueValue, Reason: "other", Since: "2026-11-19T10:20:30Z", ObservedAt: "2026-11-19T10:20:31Z"},
		{ObservedAt: "2026-11-19T10:20:30,5Z"},
		{PossiblyOrphaned: &trueValue, Reason: "owned-notice-orphan-suspected", Since: "2026-11-19T10:20:31Z", ObservedAt: "2026-11-19T10:20:30Z"},
	}
	for i, notice := range invalid {
		if err := validateBindOwnNotice(notice); err == nil {
			t.Errorf("forbidden notice %d accepted: %+v", i, notice)
		}
	}
}

// TestBindPartPublicationAllowlistMatchesExistingResults proves the
// sidecar's publication validation accepts the empty value and the six result
// strings the binding assigns, and refuses an unknown reason and a result with
// an embedded line break. The list is written by hand; it is not derived from
// the writers.
func TestBindPartPublicationAllowlistMatchesExistingResults(t *testing.T) {
	valid := []string{
		"", "published", "unavailable: own notice changed",
		"unavailable: no liveness record (a2a status set)",
		"unavailable: invalid notice retry policy",
		"unavailable: own liveness publication retry pending",
		"unavailable: own liveness publication failed",
	}
	for _, publication := range valid {
		if !validBindNoticePublication(publication) {
			t.Errorf("current publication result refused: %q", publication)
		}
	}
	for _, publication := range []string{"unavailable: unbound", "published\nmallory", "unavailable: new reason"} {
		if validBindNoticePublication(publication) {
			t.Errorf("unknown publication result admitted: %q", publication)
		}
	}
}

// TestBindPartCursorKeepsExistingOpaqueTokenBudget proves an 8 KiB opaque
// sync cursor, the size State accepted before, fits the sidecar and reads back
// unchanged.
func TestBindPartCursorKeepsExistingOpaqueTokenBudget(t *testing.T) {
	cursor := strings.Repeat("c", 8<<10)
	p, _, view := bindFixture(t, "s0", false)
	owner := BindOperationOwner{Holder: "projector", Host: "host-1", PID: os.Getpid(), Birth: "1"}
	token := strings.Repeat("5", 32)
	if _, err := BeginBindCursor(p, bindTestName, token, "projector", view.NextBatch, "", owner, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := CommitBindCursor(p, bindTestName, token, cursor); err != nil {
		t.Fatalf("8 KiB opaque cursor rejected: %v", err)
	}
	if got, err := LoadState(p, bindTestName); err != nil || got.NextBatch != cursor {
		t.Fatalf("opaque cursor round trip len=%d err=%v", len(got.NextBatch), err)
	}
}

// TestMarkedNoopWriteCachesLegacyShadowForNextRead proves an ordinary
// no-op write of a marked State leaves the State bytes unchanged and the next
// cached read still returns the lease held in the sidecar.
func TestMarkedNoopWriteCachesLegacyShadowForNextRead(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	state := State{NextBatch: "s0", Threads: []string{"$keep"}}
	if err := SaveState(p, bindTestName, state); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBindPart(p, bindTestName, BindPartOpenOptions{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}

	// Start with a cold non-durable cache so the ordinary no-op path takes the
	// verified elision route. The current sidecar lease then differs from the
	// migration-time State shadow without changing the legacy State document.
	p = p.WithReadCache(NewReadCache(bindTestName))
	if _, err := LoadState(p, bindTestName); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireOrRenewBindLease(p, bindTestName, "session-1", "host-1", os.Getpid(), time.Now(), time.Minute, 0, nil)
	if err != nil || lease.Lease == nil {
		t.Fatalf("acquire provider lease: lease=%+v err=%v", lease.Lease, err)
	}
	before, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, bindTestName, func(*State) error { return nil }); err != nil {
		t.Fatalf("ordinary no-op write: %v", err)
	}
	after, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("no-op changed legacy State bytes: equal=%t err=%v", bytes.Equal(before, after), err)
	}
	current, err := LoadState(p, bindTestName)
	if err != nil || current.Lease == nil || current.Lease.Holder != lease.Lease.Holder || current.Lease.Host != lease.Lease.Host || current.Lease.PID != lease.Lease.PID || !current.Lease.Expires.Equal(lease.Lease.Expires) {
		t.Fatalf("cached read lost provider lease: lease=%+v want=%+v err=%v", current.Lease, lease.Lease, err)
	}
}

// TestBindPartIdleLeaseRenewalDoesNotDecodeOrRewriteLargeState proves a lease
// that is inside its renewal margin costs no State load, decode, encode or
// write on repeated ticks, and that the short path stays that cheap, and is
// still answered, while another owner's cursor operation is pending: the
// lease is not what a cursor reservation protects. Byte confirmation of the
// cached State is allowed and is not counted against the claim.
func TestBindPartIdleLeaseRenewalDoesNotDecodeOrRewriteLargeState(t *testing.T) {
	name := bindTestName
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache(name))
	state := State{NextBatch: "s0", Threads: make([]string, 450), ThreadEvents: make([]ThreadEvent, 2000)}
	for i := range state.Threads {
		state.Threads[i] = "$thread-" + strings.Repeat("t", 64) + string(rune('a'+i%26))
	}
	for i := range state.ThreadEvents {
		state.ThreadEvents[i] = ThreadEvent{Room: "!" + strings.Repeat("r", 44), EventID: "$" + strings.Repeat("e", 78) + string(rune('a'+i%26)), Root: "$" + strings.Repeat("t", 62)}
	}
	if err := SaveState(p, name, state); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.StateFile(name))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 400<<10 {
		t.Fatalf("fixture State size=%d, want at least 400 KiB", info.Size())
	}
	if _, err := EnsureBindPart(p, name, BindPartOpenOptions{Holder: "seed", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := AcquireOrRenewBindLease(p, name, "holder", "host-1", os.Getpid(), now, LeaseTTL, LeaseTTL/2, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, name); err != nil {
		t.Fatal(err)
	} // warm the validated clone
	before := ReadStateDecodeStats()
	for i := 0; i < 20; i++ {
		if _, err := AcquireOrRenewBindLease(p, name, "holder", "host-1", os.Getpid(), now.Add(time.Duration(i+1)*time.Second), LeaseTTL, LeaseTTL/2, nil); err != nil {
			t.Fatal(err)
		}
	}
	after := ReadStateDecodeStats()
	if d := after.UnmarshalStatePasses - before.UnmarshalStatePasses; d != 0 {
		t.Fatalf("steady lease ticks decoded State %d times", d)
	}
	if d := after.LoadStateCalls - before.LoadStateCalls; d != 0 {
		t.Fatalf("steady lease ticks loaded or cloned State %d times", d)
	}
	if d := after.UnmarshalStatePasses - before.UnmarshalStatePasses; d != 0 {
		t.Fatalf("steady lease ticks decoded State %d times", d)
	}
	if d := after.StateEncodes - before.StateEncodes; d != 0 {
		t.Fatalf("steady lease ticks encoded State %d times", d)
	}
	owner := BindOperationOwner{Holder: "holder", Host: "host-1", PID: os.Getpid(), Birth: "birth"}
	if _, err := BeginBindCursor(p, name, strings.Repeat("a", 32), "bind", "s0", "", owner, nil); err != nil {
		t.Fatal(err)
	}
	before = ReadStateDecodeStats()
	if _, err := AcquireOrRenewBindLease(p, name, "holder", "host-1", os.Getpid(), now.Add(time.Second), LeaseTTL, LeaseTTL/2, nil); err != nil {
		t.Fatalf("the lease's short path beside a pending cursor operation: %v", err)
	}
	after = ReadStateDecodeStats()
	if after.LoadStateCalls != before.LoadStateCalls || after.UnmarshalStatePasses != before.UnmarshalStatePasses {
		t.Fatalf("the short path beside a pending operation fell back to a full State load: before=%+v after=%+v", before, after)
	}
}

// TestBindPartCursorAdvanceReusesLargeStateCache proves a cursor commit to the
// sidecar leaves a large cached State undecoded, unencoded and unwritten, and
// that a later change of the State file by another writer is decoded afresh
// instead of being answered from the cache.
func TestBindPartCursorAdvanceReusesLargeStateCache(t *testing.T) {
	name := bindTestName
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache(name))
	state := State{NextBatch: "s0", Threads: make([]string, 450), ThreadEvents: make([]ThreadEvent, 2000)}
	for i := range state.Threads {
		state.Threads[i] = "$thread-" + strings.Repeat("t", 64) + string(rune('a'+i%26))
	}
	for i := range state.ThreadEvents {
		state.ThreadEvents[i] = ThreadEvent{Room: "!" + strings.Repeat("r", 44), EventID: "$" + strings.Repeat("e", 78) + string(rune('a'+i%26)), Root: "$" + strings.Repeat("t", 62)}
	}
	if err := SaveState(p, name, state); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureBindPart(p, name, BindPartOpenOptions{Holder: "seed", Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.StateFile(name))
	if err != nil || info.Size() < 400<<10 {
		t.Fatalf("fixture State size=%d, want at least 400 KiB: %v", info.Size(), err)
	}
	warm, err := LoadState(p, name)
	if err != nil || !BindPartActive(warm) || warm.NextBatch != "s0" {
		t.Fatalf("warm marked State: active=%t next=%q err=%v", BindPartActive(warm), warm.NextBatch, err)
	}
	stateBefore, err := os.ReadFile(p.StateFile(name))
	if err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	owner := BindOperationOwner{Holder: "holder", Host: "host-1", PID: os.Getpid(), Birth: "1"}
	token := strings.Repeat("6", 32)
	if _, err := BeginBindCursor(p, name, token, "projector", warm.NextBatch, "", owner, nil); err != nil {
		t.Fatal(err)
	}
	committed, err := CommitBindCursor(p, name, token, "s1")
	if err != nil || committed.NextBatch != "s1" {
		t.Fatalf("cursor commit: next=%q err=%v", committed.NextBatch, err)
	}
	loaded, err := LoadState(p, name)
	if err != nil || loaded.NextBatch != "s1" {
		t.Fatalf("fresh cursor projection: next=%q err=%v", loaded.NextBatch, err)
	}
	stateAfter, err := os.ReadFile(p.StateFile(name))
	if err != nil || !bytes.Equal(stateBefore, stateAfter) {
		t.Fatalf("sidecar cursor commit rewrote State: err=%v equal=%t", err, bytes.Equal(stateBefore, stateAfter))
	}
	after := ReadStateDecodeStats()
	// Reads are the cache's byte confirmation of the unchanged State and are
	// not refused; a decode, an encode or a write of it is.
	if d := after.UnmarshalStatePasses - before.UnmarshalStatePasses; d != 0 {
		t.Errorf("sidecar cursor commit decoded State %d times", d)
	}
	if d := after.StateEncodes - before.StateEncodes; d != 0 {
		t.Errorf("sidecar cursor commit encoded State %d times", d)
	}
	if d := after.StateWriteBytes - before.StateWriteBytes; d != 0 {
		t.Errorf("sidecar cursor commit wrote %d State bytes", d)
	}

	// A separate existing writer changes the State inode without updating this
	// consumer's cache. The next read must notice the stamp change and validate
	// the new body while still merging the current provider cursor.
	other := Paths{Base: p.Base}
	if _, err := UpdateState(other, name, func(st *State) error {
		st.Threads = append(st.Threads, "$cache-invalidated")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	beforeReload := ReadStateDecodeStats()
	fresh, err := LoadState(p, name)
	if err != nil || fresh.NextBatch != "s1" || len(fresh.Threads) != 451 || fresh.Threads[450] != "$cache-invalidated" {
		t.Fatalf("changed State stamp did not refresh cached legacy body: next=%q threads=%d err=%v", fresh.NextBatch, len(fresh.Threads), err)
	}
	afterReload := ReadStateDecodeStats()
	if d := afterReload.StateReadAttempts - beforeReload.StateReadAttempts; d == 0 {
		t.Error("changed State stamp reused the old cache without a full read")
	}
	if d := afterReload.UnmarshalStatePasses - beforeReload.UnmarshalStatePasses; d == 0 {
		t.Error("changed State stamp reused the old cache without decoding the new State")
	}
}

// TestBindIdleDeliveryViewUsesValidatedCache proves the cached idle-delivery
// view of a marked State is served without loading or cloning the full State,
// checks the writer seal exactly once per State stamp, and misses after the
// State file changes. The one seal check costs at most one digest: none when
// the seal-digest memo already holds these exact bytes from the preceding load.
func TestBindIdleDeliveryViewUsesValidatedCache(t *testing.T) {
	p, state, _ := bindFixture(t, "s0", true)
	p = p.WithReadCache(NewReadCache(bindTestName))
	if _, err := LoadState(p, bindTestName); err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	view, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName)
	if err != nil || !hit || !view.Marked || !view.Active || view.NextBatch != "s0" || view.Generation != state.Terminal.Generation || view.Stopped || view.HasLegacySubscriptions {
		t.Fatalf("cached bind delivery view=%+v hit=%t err=%v", view, hit, err)
	}
	after := ReadStateDecodeStats()
	if after.LoadStateCalls != before.LoadStateCalls || after.WriterClones != before.WriterClones || after.StateDigests-before.StateDigests > 1 || after.WriterSealChecks-before.WriterSealChecks != 1 {
		t.Fatalf("cached bind view touched full State: before=%+v after=%+v", before, after)
	}
	second, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName)
	if err != nil || !hit || !second.SealValid || second.NextBatch != view.NextBatch {
		t.Fatalf("repeat cached bind view=%+v hit=%t err=%v", second, hit, err)
	}
	if repeated := ReadStateDecodeStats(); repeated.WriterSealChecks != after.WriterSealChecks || repeated.StateDigests != after.StateDigests {
		t.Fatalf("same-stamp hit repeated the State seal hash: first=%+v repeated=%+v", after, repeated)
	}

	// A changed State stamp invalidates this view; the caller must return to the
	// ordinary State path instead of reusing the stale provider projection.
	other := Paths{Base: p.Base}
	if _, err := UpdateState(other, bindTestName, func(current *State) error {
		current.LastInbox = "changed"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName); err != nil || hit {
		t.Fatalf("changed State stamp did not miss cache: hit=%t err=%v", hit, err)
	}
}

// TestTryCommitEmptyBindCursorRequiresEmptyMailbox proves the short cursor
// commit is taken only for an empty mailbox: it advances the sidecar cursor and
// a later cache hit sees the new value, and with pending mail it declines and
// leaves mailbox, State and cursor as they were for the ordinary path.
func TestTryCommitEmptyBindCursorRequiresEmptyMailbox(t *testing.T) {
	p, state, view := bindFixture(t, "s0", true)
	p = p.WithReadCache(NewReadCache(bindTestName))
	if _, err := LoadState(p, bindTestName); err != nil {
		t.Fatal(err)
	}
	snapshot, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName)
	if err != nil || !hit || !snapshot.SealValid {
		t.Fatalf("warm verified idle bind snapshot=%+v hit=%t err=%v", snapshot, hit, err)
	}
	owner := BindOperationOwner{Holder: "holder", Host: "host-1", PID: os.Getpid(), Birth: "birth"}
	firstToken := strings.Repeat("7", 32)
	if _, err := BeginBindCursor(p, bindTestName, firstToken, "bind", "s0", view.Generation, owner, nil); err != nil {
		t.Fatal(err)
	}
	if _, committed, err := TryCommitEmptyBindCursor(p, bindTestName, firstToken, "s1", snapshot); err != nil || !committed {
		t.Fatalf("empty mailbox cursor commit=%t err=%v", committed, err)
	}
	updated, hit, err := TryLoadCachedBindIdleDeliveryState(p, bindTestName)
	if err != nil || !hit || updated.NextBatch != "s1" || !updated.SealValid {
		t.Fatalf("changed sidecar cursor was not refreshed on State cache hit: view=%+v hit=%t err=%v", updated, hit, err)
	}
	current, err := LoadState(p, bindTestName)
	if err != nil || current.NextBatch != "s1" || state.Terminal == nil {
		t.Fatalf("empty fast commit next=%q err=%v", current.NextBatch, err)
	}

	secondToken := strings.Repeat("8", 32)
	if _, err := BeginBindCursor(p, bindTestName, secondToken, "bind", "s1", view.Generation, owner, nil); err != nil {
		t.Fatal(err)
	}
	mailbox, err := OpenMailbox(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := mailbox.Append([]MailEntry{{ID: "$pending", Kind: MailEvent, Room: "!room", Event: []byte(`{"body":"pending"}`)}}); err != nil || n != 1 {
		t.Fatalf("seed pending mailbox: added=%d err=%v", n, err)
	}
	mailBefore, err := os.ReadFile(p.MailboxFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	stateBefore, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	if _, committed, err := TryCommitEmptyBindCursor(p, bindTestName, secondToken, "s2", snapshot); err != nil || committed {
		t.Fatalf("non-empty mailbox fast commit=%t err=%v, want fallback", committed, err)
	}
	mailAfter, mailErr := os.ReadFile(p.MailboxFile(bindTestName))
	stateAfter, stateErr := os.ReadFile(p.StateFile(bindTestName))
	current, loadErr := LoadState(p, bindTestName)
	if mailErr != nil || stateErr != nil || loadErr != nil || !bytes.Equal(mailBefore, mailAfter) || !bytes.Equal(stateBefore, stateAfter) || current.NextBatch != "s1" {
		t.Fatalf("refused fast path changed pending delivery: mailboxErr=%v stateErr=%v loadErr=%v next=%q mailboxSame=%t stateSame=%t", mailErr, stateErr, loadErr, current.NextBatch, bytes.Equal(mailBefore, mailAfter), bytes.Equal(stateBefore, stateAfter))
	}
	if err := AbortBindCursor(p, bindTestName, secondToken); err != nil {
		t.Fatal(err)
	}
}

// TestBindControlToleratesSyncBetweenReadAndWrite proves a control change is
// decided by the binding's generation and session, not by the sidecar revision
// the caller happened to read: after a cursor commit and a lease renewal have
// moved the revision, a stop presented with the earlier revision still
// succeeds. A stop presented for another generation or another session is
// refused with ErrBindPartStale and changes nothing.
//
// Regression witness: with the exact revision required, a stop, a replacement
// or a handover failed whenever a live consumer had synced between the
// caller's read and its write.
func TestBindControlToleratesSyncBetweenReadAndWrite(t *testing.T) {
	p, _, seen := bindFixture(t, "s0", true)
	owner := BindOperationOwner{Holder: "test-holder", Host: "host-1", PID: os.Getpid(), Birth: "1"}
	token, err := NewBindToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginBindCursor(p, bindTestName, token, "bind", seen.NextBatch, seen.Generation, owner, nil); err != nil {
		t.Fatalf("consumer cursor reservation: %v", err)
	}
	if _, err := CommitBindCursor(p, bindTestName, token, "s1"); err != nil {
		t.Fatalf("consumer cursor commit: %v", err)
	}
	if _, err := AcquireOrRenewBindLease(p, bindTestName, "test-holder", "host-1", os.Getpid(), time.Now(), LeaseTTL, 0, nil); err != nil {
		t.Fatalf("consumer lease renewal: %v", err)
	}
	state, err := LoadState(p, bindTestName)
	if err != nil {
		t.Fatal(err)
	}
	moved, err := ReadBindPartView(p, bindTestName, state)
	if err != nil || moved.Revision <= seen.Revision {
		t.Fatalf("premise: sidecar revision %d did not move past %d (err=%v)", moved.Revision, seen.Revision, err)
	}
	stop := BindControlTarget{Generation: seen.Generation, SessionID: seen.SessionID, Stopped: true, Kind: "stop"}

	for name, expected := range map[string]BindControlExpectation{
		"another generation": {Revision: moved.Revision, Generation: strings.Repeat("b", 32), SessionID: seen.SessionID},
		"another session":    {Revision: moved.Revision, Generation: seen.Generation, SessionID: "session-other"},
	} {
		if _, err := SetBindControl(p, bindTestName, expected, stop, BindControlProof{}); !errors.Is(err, ErrBindPartStale) {
			t.Fatalf("stop for %s = %v, want ErrBindPartStale", name, err)
		}
		if after, err := ReadBindPartView(p, bindTestName, state); err != nil || after.Stopped || after.Revision != moved.Revision {
			t.Fatalf("refused stop for %s changed the sidecar: %+v err=%v", name, after, err)
		}
	}

	earlier := BindControlExpectation{Revision: seen.Revision, Generation: seen.Generation, SessionID: seen.SessionID}
	stopped, err := SetBindControl(p, bindTestName, earlier, stop, BindControlProof{})
	if err != nil || !stopped.Stopped || stopped.Generation != seen.Generation || stopped.NextBatch != "s1" {
		t.Fatalf("stop with the earlier revision: view=%+v err=%v", stopped, err)
	}
}

// TestBindOwnStatusOnStoppedBinding proves the sidecar admits an own-status
// publication for a stopped binding and records its result, while a
// publication for another generation or session is still refused as stale.
//
// Regression witness: the reservation refused every stopped binding, so an
// identity whose session had ended could not publish its status.
func TestBindOwnStatusOnStoppedBinding(t *testing.T) {
	p, state, view := bindFixture(t, "s0", true)
	stopped, err := SetBindControl(p, bindTestName,
		BindControlExpectation{Revision: view.Revision, Generation: view.Generation, SessionID: view.SessionID},
		BindControlTarget{Generation: view.Generation, SessionID: view.SessionID, Stopped: true, Kind: "stop"}, BindControlProof{})
	if err != nil || !stopped.Stopped {
		t.Fatalf("premise: stop view=%+v err=%v", stopped, err)
	}
	_ = state
	now := time.Now().UTC()
	owner := BindOperationOwner{Holder: "status-writer", Host: "host-1", PID: os.Getpid(), Birth: "birth-status"}
	for name, c := range map[string][2]string{
		"another generation": {strings.Repeat("b", 32), stopped.SessionID},
		"another session":    {stopped.Generation, "session-other"},
	} {
		if err := BeginBindOwnStatus(p, bindTestName, strings.Repeat("4", 32), stopped.Revision, c[0], c[1], owner, now.Add(time.Minute), now); !errors.Is(err, ErrBindPartStale) {
			t.Fatalf("%s: reservation=%v, want the stale refusal", name, err)
		}
	}
	token := strings.Repeat("5", 32)
	if err := BeginBindOwnStatus(p, bindTestName, token, stopped.Revision, stopped.Generation, stopped.SessionID, owner, now.Add(time.Minute), now); err != nil {
		t.Fatalf("reservation for a stopped binding: %v", err)
	}
	status := &AgentStatus{State: "idle", Doorbell: "armed", OwnNotice: OwnNoticeObservation{}}
	if err := FinishBindOwnStatus(p, bindTestName, BindOwnStatusResult{Token: token, Status: status, Published: true, Publication: "published"}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	final, err := LoadState(p, bindTestName)
	if err != nil || final.OwnStatusWrite != nil || final.NoticeWriteUncertain || final.Liveness == nil || final.Liveness.State != "idle" {
		t.Fatalf("finished State token=%v uncertain=%t liveness=%v err=%v", final.OwnStatusWrite, final.NoticeWriteUncertain, final.Liveness, err)
	}
	after, err := ReadBindPartView(p, bindTestName, final)
	if err != nil || !after.Stopped || after.Generation != stopped.Generation || after.NoticePublication != "published" {
		t.Fatalf("finished view=%+v err=%v", after, err)
	}
}
