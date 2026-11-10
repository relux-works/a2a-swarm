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
