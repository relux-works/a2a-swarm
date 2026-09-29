package config

import (
	"strconv"
	"testing"
)

// Proves Ack moves the cursor to the pending batch's next_batch, adds its
// events to the acked ring exactly once, and clears the batch; an Ack with
// nothing pending changes nothing (negative control).
func TestStateAck(t *testing.T) {
	s := State{NextBatch: "s1"}
	if s.Ack() != nil || s.NextBatch != "s1" || len(s.Acked) != 0 {
		t.Fatalf("empty ack changed state: %+v", s)
	}
	s.Pending = &Pending{NextBatch: "s5", Events: []string{"$a", "$b"}, CloseSubs: []string{"sub-1"}}
	p := s.Ack()
	if p == nil || p.CloseSubs[0] != "sub-1" || s.NextBatch != "s5" || s.Pending != nil {
		t.Fatalf("ack = %+v, state %+v", p, s)
	}
	if !s.IsAcked("$a") || !s.IsAcked("$b") || s.IsAcked("$c") {
		t.Fatalf("ring = %v", s.Acked)
	}
	s.AddAcked("$a", "")
	if len(s.Acked) != 2 {
		t.Fatalf("duplicate or empty id added: %v", s.Acked)
	}
}

// Proves the acked ring keeps only the newest AckedMax ids: the oldest ones
// fall out, the newest stays.
func TestStateAckedRingBound(t *testing.T) {
	var s State
	for i := 0; i < AckedMax+5; i++ {
		s.AddAcked("$e" + strconv.Itoa(i))
	}
	if len(s.Acked) != AckedMax {
		t.Fatalf("ring size = %d", len(s.Acked))
	}
	if s.IsAcked("$e4") || !s.IsAcked("$e5") || !s.IsAcked("$e"+strconv.Itoa(AckedMax+4)) {
		t.Fatal("ring kept the wrong end")
	}
}

// Proves the pending batch and the legacy subscription map round-trip through
// the 0600 state file.
func TestStatePendingRoundTrip(t *testing.T) {
	p := testPaths(t)
	want := State{NextBatch: "s1", Pending: &Pending{NextBatch: "s2", Events: []string{"$e"}},
		LegacySubscriptions: map[string][]string{LegacyAllRooms: {"system"}}}
	if err := SaveState(p, "a", want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "a")
	if err != nil || got.Pending == nil || got.Pending.NextBatch != "s2" || got.LegacySubscriptions["*"][0] != "system" {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
}
