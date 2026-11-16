package e2ee

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// eventsSnapshot is a minimal valid snapshot around a real engine checkpoint.
func eventsSnapshot(t *testing.T) Snapshot {
	t.Helper()
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	return Snapshot{Version: 1, Revision: 1, Engine: state}
}

// The event-ID identity is part of the snapshot schema: a snapshot carrying
// two rooms' identities marshals, decodes to an equal map (same keys,
// references, Seq and Next) and marshals again to the identical bytes. The
// member is additive: the same snapshot encoded without any "events" member,
// as a writer from before this member produced it, still decodes, with an
// empty identity. Control for that second claim: the edited bytes really lack
// the member and differ from the encoding that has it.
func TestSnapshotEventsRoundTrip(t *testing.T) {
	s := eventsSnapshot(t)
	without, err := s.Marshal()
	if err != nil {
		t.Fatal("empty identity control", err)
	}
	digest := TupleHash("ciphertext")
	s.Events = map[string]EventReplay{
		TupleHash("!one:example.test"): {Next: 7, Entries: map[string]EventReference{
			"$a": {Session: "session-a", Index: 0, Digest: digest, Seq: 2},
			"$b": {Session: "session-b", Index: math.MaxUint32, Digest: digest, Seq: 7},
		}},
		TupleHash("!two:example.test"): {Next: 1, Entries: map[string]EventReference{
			"$a": {Session: "session-c", Index: 5, Digest: TupleHash("other"), Seq: 1},
		}},
	}
	raw, err := s.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSnapshot(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded.Events, s.Events) {
		t.Fatal("event-ID identity changed in the round trip")
	}
	again, err := decoded.Marshal()
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("round trip is not byte-identical", err)
	}
	wantMember, _ := json.Marshal(s.Events)
	if !bytes.Contains(raw, append([]byte(`"events":`), wantMember...)) {
		t.Fatal("events member is not the encoded map")
	}
	// A snapshot from before the member existed has no "events" at all.
	legacy := bytes.Replace(without, []byte(`"events":null,`), nil, 1)
	if bytes.Equal(legacy, without) || bytes.Contains(legacy, []byte(`"events"`)) {
		t.Fatal("legacy fixture still carries the member")
	}
	old, err := DecodeSnapshot(legacy)
	if err != nil || len(old.Events) != 0 {
		t.Fatal("snapshot without the events member refused", err)
	}
}

// Load and save both refuse an event-ID identity outside its bounds: more
// than MaxRooms rooms and more than ReplayLimit entries in a room are
// capacity errors; a duplicate Seq, a Seq above the room's Next, an event ID
// that is not a "$" ID, a room key that is not a room hash, an empty session
// and a malformed digest are codec errors. Controls in the same harness: a
// small valid identity, and an identity exactly at both limits (MaxRooms
// rooms, one of them holding ReplayLimit entries), decode and marshal.
// Load and save refuse Index > MaxUint32 as ErrCodec; Index == MaxUint32 and Seq == 0 remain valid.
func TestSnapshotEventsBounds(t *testing.T) {
	base := eventsSnapshot(t)
	digest := TupleHash("ciphertext")
	ref := func(seq uint64) EventReference {
		return EventReference{Session: "session", Index: seq, Digest: digest, Seq: seq}
	}
	room := TupleHash("!dm:example.test")
	full := func(n int) EventReplay {
		entries := make(map[string]EventReference, n)
		for i := 1; i <= n; i++ {
			entries[fmt.Sprintf("$e-%d", i)] = ref(uint64(i))
		}
		return EventReplay{Next: uint64(n), Entries: entries}
	}
	rooms := func(n int) map[string]EventReplay {
		out := make(map[string]EventReplay, n)
		for i := 0; i < n; i++ {
			out[TupleHash(fmt.Sprint("room", i))] = EventReplay{}
		}
		return out
	}
	for _, tc := range []struct {
		name   string
		events map[string]EventReplay
		want   error
	}{
		{"control-valid", map[string]EventReplay{room: {Next: 2, Entries: map[string]EventReference{"$a": ref(1), "$b": ref(2)}}}, nil},
		{"control-at-limits", func() map[string]EventReplay {
			out := rooms(MaxRooms - 1)
			out[room] = full(ReplayLimit)
			return out
		}(), nil},
		{"control-zero-seq", map[string]EventReplay{room: {Entries: map[string]EventReference{"$a": ref(0)}}}, nil},
		{"control-max-index", map[string]EventReplay{room: {Entries: map[string]EventReference{"$a": {Session: "session", Index: math.MaxUint32, Digest: digest}}}}, nil},
		{"index-above-max", map[string]EventReplay{room: {Entries: map[string]EventReference{"$a": {Session: "session", Index: uint64(math.MaxUint32) + 1, Digest: digest}}}}, ErrCodec},
		{"too-many-rooms", rooms(MaxRooms + 1), ErrCapacity},
		{"too-many-entries", map[string]EventReplay{room: full(ReplayLimit + 1)}, ErrCapacity},
		{"duplicate-seq", map[string]EventReplay{room: {Next: 2, Entries: map[string]EventReference{"$a": ref(1), "$b": ref(1)}}}, ErrCodec},
		{"seq-above-next", map[string]EventReplay{room: {Next: 1, Entries: map[string]EventReference{"$a": ref(2)}}}, ErrCodec},
		{"malformed-event-id", map[string]EventReplay{room: {Next: 1, Entries: map[string]EventReference{"a": ref(1)}}}, ErrCodec},
		{"room-key-not-a-hash", map[string]EventReplay{"!dm:example.test": {Next: 1, Entries: map[string]EventReference{"$a": ref(1)}}}, ErrCodec},
		{"empty-session", map[string]EventReplay{room: {Next: 1, Entries: map[string]EventReference{"$a": {Digest: digest, Seq: 1}}}}, ErrCodec},
		{"malformed-digest", map[string]EventReplay{room: {Next: 1, Entries: map[string]EventReference{"$a": {Session: "session", Digest: "abc", Seq: 1}}}}, ErrCodec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			candidate.Events = tc.events
			if _, err := candidate.Marshal(); err != tc.want {
				t.Fatal("save bounds", err)
			}
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSnapshot(raw); err != tc.want {
				t.Fatal("load bounds", err)
			}
		})
	}
}
