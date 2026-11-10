package e2ee

import (
	"fmt"
	"testing"
)

// Retained rereads are idempotent; altered event/digest and expired indices
// refuse. Pruning raises a permanent floor even for unseen delayed history.
func TestReplayWindowPruning(t *testing.T) {
	var w ReplayWindow
	digest := TupleHash("event")
	if err := w.Reserve(10, "event", digest); err != nil {
		t.Fatal(err)
	}
	if err := w.Reserve(10, "event", digest); err != nil {
		t.Fatal("reread", err)
	}
	for _, v := range []ReplayEntry{{"other", digest}, {"event", TupleHash("altered")}} {
		if err := w.Reserve(10, v.Event, v.Digest); err != ErrReplay {
			t.Fatal("replay admitted", err)
		}
	}
	for i := uint64(11); i < ReplayLimit+11; i++ {
		if err := w.Reserve(i, fmt.Sprint(i), digest); err != nil {
			t.Fatal(err)
		}
	}
	if !w.HasFloor || w.Floor != 10 || len(w.Entries) != ReplayLimit {
		t.Fatal("window bound")
	}
	if err := w.Reserve(10, "event", digest); err != ErrExpired {
		t.Fatal("expired reread reopened", err)
	}
	if err := w.Reserve(5, "delayed", digest); err != ErrExpired {
		t.Fatal("unseen delayed history admitted", err)
	}
	if err := w.Reserve(ReplayLimit+11, "new", digest); err != nil {
		t.Fatal("positive new index", err)
	}
}

// Pending evidence cannot be retired or pruned; retirement and closed-room
// tombstones survive detail compaction and lifetime budget pressure.
func TestRetentionProtectedAndTombstones(t *testing.T) {
	var r Retention
	room := TupleHash("room")
	key := TupleHash("session")
	if err := r.AdmitRoom(room); err != nil {
		t.Fatal(err)
	}
	if err := r.CanImport(room, key); err != nil {
		t.Fatal(err)
	}
	if err := r.Retire(room, key, true); err != ErrConflict {
		t.Fatal("retired pending key", err)
	}
	if err := r.CanImport(room, key); err != nil {
		t.Fatal("failed retire changed state", err)
	}
	if err := r.Retire(room, key, false); err != nil {
		t.Fatal(err)
	}
	if err := r.CanImport(room, key); err != ErrRefused {
		t.Fatal("reimported retired key", err)
	}
	r.Sequence = 100
	r.Verdicts = []Verdict{{ID: "pending", TimeMS: 1, Protected: true}, {ID: "completed", TimeMS: 1}}
	r.Journal = []Verdict{{ID: "pending", TimeMS: 1, Protected: true, Sequence: 3}, {ID: "completed", TimeMS: 1, Sequence: 4}}
	if err := r.Prune(RetentionAgeMS + 10); err != nil {
		t.Fatal(err)
	}
	if len(r.Verdicts) != 1 || r.Verdicts[0].ID != "pending" || len(r.Journal) != 1 || r.OldestSequence != 3 || !r.Rooms[room].Retired[key] {
		t.Fatal("lost protected evidence")
	}
	if err := r.CloseRoom(room); err != nil {
		t.Fatal(err)
	}
	if err := r.AdmitRoom(room); err != ErrCapacity {
		t.Fatal("reopened closed room", err)
	}
	for i := 1; i < MaxRooms; i++ {
		if err := r.AdmitRoom(TupleHash(fmt.Sprint(i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.AdmitRoom(TupleHash("overflow")); err != ErrCapacity {
		t.Fatal("lifetime room limit", err)
	}
	old := append([]Verdict(nil), r.Journal...)
	r.Journal = append(r.Journal, Verdict{ID: "large pending", Protected: true, Bytes: MaxJournalBytes})
	r.Journal[0].Bytes = 1
	if err := r.Prune(RetentionAgeMS + 20); err != ErrCapacity {
		t.Fatal("evicted protected evidence", err)
	}
	if len(r.Journal) != len(old)+1 {
		t.Fatal("failed prune changed evidence")
	}
}

// The fixed Bloom filter never forgets an inserted refusal; saturation blocks
// the room instead of clearing bits. A fresh nonhit is the positive neighbour.
func TestRefusalFilterNonclearing(t *testing.T) {
	var f RefusalFilter
	digest := TupleHash("refused")
	if err := f.Check(digest); err != nil {
		t.Fatal("fresh filter", err)
	}
	if err := f.Add(digest); err != nil {
		t.Fatal(err)
	}
	if err := f.Check(digest); err != ErrRefused {
		t.Fatal("forgot refusal", err)
	}
	if err := f.Check(TupleHash("unrefused-positive-neighbour")); err != nil {
		t.Fatal("nonhit refused after insertion", err)
	}
	for i := range f {
		f[i] = 0xff
	}
	if err := f.Check(TupleHash("new")); err != ErrCapacity {
		t.Fatal("saturation admitted traffic", err)
	}
	if err := f.Add(digest); err != ErrCapacity {
		t.Fatal("saturation cleared", err)
	}
}
