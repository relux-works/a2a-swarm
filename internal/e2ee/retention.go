package e2ee

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math/bits"
	"sort"
)

const ReplayLimit = 4096
const MaxRooms = 32
const MaxRetired = 4096
const MaxVerdicts = 8192
const MaxJournal = 4096
const MaxJournalBytes = 4 << 20
const RetentionAgeMS = int64(7 * 24 * 60 * 60 * 1000)

type ReplayEntry struct {
	Event  string `json:"event"`
	Digest string `json:"digest"`
}
type ReplayWindow struct {
	Floor    uint64                 `json:"rejected_through"`
	HasFloor bool                   `json:"has_floor"`
	Entries  map[uint64]ReplayEntry `json:"entries"`
}

// Reserve is monotonic: eviction cannot turn an old unseen or previously seen
// index into a fresh delivery. Exact rereads require retained event+digest bytes.
func (w *ReplayWindow) Reserve(index uint64, event, digest string) error {
	if event == "" || len(event) > 255 || !hexSize(digest, 32) {
		return ErrInvalid
	}
	if w.HasFloor && index <= w.Floor {
		return ErrExpired
	}
	if old, ok := w.Entries[index]; ok {
		if old.Event == event && old.Digest == digest {
			return nil
		}
		return ErrReplay
	}
	if w.Entries == nil {
		w.Entries = map[uint64]ReplayEntry{}
	}
	w.Entries[index] = ReplayEntry{event, digest}
	if len(w.Entries) > ReplayLimit {
		lowest := index
		for i := range w.Entries {
			if i < lowest {
				lowest = i
			}
		}
		delete(w.Entries, lowest)
		w.Floor = lowest
		w.HasFloor = true
		if lowest == index {
			return ErrExpired
		}
	}
	return nil
}

// expires reports, without mutating, whether Reserve would refuse a fresh
// (not retained) index as expired: the index is at or below the floor, or the
// window is full of higher indices, so the new entry would itself be the one
// evicted. Callers that must not write on refusal ask this before Reserve.
func (w *ReplayWindow) expires(index uint64) bool {
	if w.HasFloor && index <= w.Floor {
		return true
	}
	if len(w.Entries) < ReplayLimit {
		return false
	}
	for i := range w.Entries {
		if i < index {
			return false
		}
	}
	return true
}

// EventReference is what one room event ID was accepted as: the session,
// the authenticated index and the ciphertext digest. Seq orders eviction.
type EventReference struct {
	Session string `json:"session"`
	Index   uint64 `json:"index"`
	Digest  string `json:"digest"`
	Seq     uint64 `json:"seq"`
}

// EventReplay is the per-room event-ID replay identity kept beside the
// per-session ReplayWindow: a homeserver assigns event IDs, so the same ID
// must never name two different accepted payloads, within or across sessions.
// It is bounded by ReplayLimit entries per room; the oldest reservation is
// evicted first. An evicted ID is forgotten: there is no floor, so the same ID
// can be reserved again later for another payload. Event-ID uniqueness
// therefore holds only among the latest ReplayLimit accepted IDs of the room.
// The session index window is bounded separately (per session, with a floor)
// and can still hold the index of an event whose ID was evicted here. The map
// is persisted in Snapshot.Events under the room hash.
type EventReplay struct {
	Next    uint64                    `json:"next"`
	Entries map[string]EventReference `json:"entries"`
}

// Check reports an exact retained reread (true, nil), a fresh ID (false,
// nil) or a collision: the same event ID with another session, index or
// digest is ErrReplay. It never mutates.
func (e *EventReplay) Check(event, session string, index uint64, digest string) (bool, error) {
	if event == "" || len(event) > 255 || !text(session, 255) || !hexSize(digest, 32) {
		return false, ErrInvalid
	}
	ref, ok := e.Entries[event]
	if !ok {
		return false, nil
	}
	if ref.Session != session || ref.Index != index || ref.Digest != digest {
		return false, ErrReplay
	}
	return true, nil
}

// Reserve records a fresh event ID after Check reported no collision,
// evicting the oldest reservation beyond ReplayLimit.
func (e *EventReplay) Reserve(event, session string, index uint64, digest string) error {
	if retained, err := e.Check(event, session, index, digest); err != nil || retained {
		return err
	}
	if e.Entries == nil {
		e.Entries = map[string]EventReference{}
	}
	e.Next++
	e.Entries[event] = EventReference{Session: session, Index: index, Digest: digest, Seq: e.Next}
	if len(e.Entries) > ReplayLimit {
		oldest, oldestSeq := "", ^uint64(0)
		for id, ref := range e.Entries {
			if ref.Seq < oldestSeq {
				oldest, oldestSeq = id, ref.Seq
			}
		}
		delete(e.Entries, oldest)
	}
	return nil
}

type Verdict struct {
	ID        string `json:"id"`
	TimeMS    int64  `json:"time_ms"`
	Protected bool   `json:"protected"`
	Bytes     int    `json:"bytes"`
	Sequence  uint64 `json:"sequence"`
}
type RoomRetention struct {
	Closed   bool            `json:"closed"`
	Retired  map[string]bool `json:"retired"`
	Refusals RefusalFilter   `json:"refusals"`
}
type Retention struct {
	Rooms          map[string]*RoomRetention `json:"rooms"`
	Verdicts       []Verdict                 `json:"verdicts"`
	Journal        []Verdict                 `json:"journal"`
	Sequence       uint64                    `json:"sequence"`
	OldestSequence uint64                    `json:"oldest_sequence"`
}

func (r *Retention) AdmitRoom(roomHash string) error {
	if !hexSize(roomHash, 32) {
		return ErrInvalid
	}
	if room, ok := r.Rooms[roomHash]; ok {
		if room.Closed {
			return ErrCapacity
		}
		return nil
	}
	if len(r.Rooms) >= MaxRooms {
		return ErrCapacity
	}
	if r.Rooms == nil {
		r.Rooms = map[string]*RoomRetention{}
	}
	r.Rooms[roomHash] = &RoomRetention{Retired: map[string]bool{}}
	return nil
}
func (r *Retention) Retire(roomHash, keyHash string, pending bool) error {
	room := r.Rooms[roomHash]
	if room == nil || !hexSize(keyHash, 32) {
		return ErrInvalid
	}
	if pending {
		return ErrConflict
	}
	if room.Retired[keyHash] {
		return nil
	}
	if len(room.Retired) >= MaxRetired {
		return ErrCapacity
	}
	if room.Retired == nil {
		room.Retired = map[string]bool{}
	}
	room.Retired[keyHash] = true
	return nil
}
func (r *Retention) CanImport(roomHash, keyHash string) error {
	room := r.Rooms[roomHash]
	if room == nil {
		return ErrUnavailable
	}
	if room.Closed || room.Retired[keyHash] {
		return ErrRefused
	}
	if len(room.Retired) >= MaxRetired {
		return ErrCapacity
	}
	return nil
}
func (r *Retention) CloseRoom(roomHash string) error {
	room := r.Rooms[roomHash]
	if room == nil {
		return ErrUnavailable
	}
	room.Closed = true
	return nil
}

// Prune keeps all protected pending/prepared/outbox evidence. Capacity errors
// leave the original slices intact; callers suspend before admitting more work.
func (r *Retention) Prune(nowMS int64) error {
	prune := func(in []Verdict, maxCount, maxBytes int) ([]Verdict, error) {
		sorted := append([]Verdict(nil), in...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].TimeMS < sorted[j].TimeMS })
		total := 0
		for _, v := range sorted {
			if v.Bytes < 0 || v.Bytes > MaxJournalBytes {
				return nil, ErrCodec
			}
			total += v.Bytes
		}
		keep := make([]Verdict, 0, len(sorted))
		remaining := len(sorted)
		for _, v := range sorted {
			expired := nowMS >= v.TimeMS && nowMS-v.TimeMS > RetentionAgeMS
			if !v.Protected && (expired || remaining > maxCount || total > maxBytes) {
				remaining--
				total -= v.Bytes
				continue
			}
			keep = append(keep, v)
		}
		if len(keep) > maxCount || total > maxBytes {
			return nil, ErrCapacity
		}
		return keep, nil
	}
	verdicts, err := prune(r.Verdicts, MaxVerdicts, MaxSnapshot-RecoveryReserve)
	if err != nil {
		return err
	}
	journal, err := prune(r.Journal, MaxJournal, MaxJournalBytes)
	if err != nil {
		return err
	}
	r.Verdicts = verdicts
	r.Journal = journal
	r.OldestSequence = r.Sequence
	for _, v := range journal {
		if v.Sequence < r.OldestSequence {
			r.OldestSequence = v.Sequence
		}
	}
	return nil
}

// RefusalFilter is a fixed, nonclearing 64KiB filter. Seven independent domain
// hashes forbid false negatives; 50% occupancy suspends the room permanently.
type RefusalFilter [65536]byte

func refusalBits(digest string) ([7]uint32, error) {
	var out [7]uint32
	raw, err := hex.DecodeString(digest)
	if err != nil || len(raw) != 32 || !hexSize(digest, 32) {
		return out, ErrInvalid
	}
	for i := range out {
		h := sha256.New()
		h.Write([]byte("a2a-e2ee-refusal-v1"))
		h.Write([]byte{byte(i)})
		h.Write(raw)
		out[i] = binary.BigEndian.Uint32(h.Sum(nil)) % (65536 * 8)
	}
	return out, nil
}
func (f *RefusalFilter) Add(digest string) error {
	indices, err := refusalBits(digest)
	if err != nil {
		return err
	}
	for _, i := range indices {
		f[i/8] |= 1 << (i % 8)
	}
	if f.Saturated() {
		return ErrCapacity
	}
	return nil
}
func (f *RefusalFilter) Check(digest string) error {
	indices, err := refusalBits(digest)
	if err != nil {
		return err
	}
	if f.Saturated() {
		return ErrCapacity
	}
	for _, i := range indices {
		if f[i/8]&(1<<(i%8)) == 0 {
			return nil
		}
	}
	return ErrRefused
}
func (f *RefusalFilter) Saturated() bool {
	n := 0
	for _, b := range f {
		n += bits.OnesCount8(b)
	}
	return n >= 65536*4
}
