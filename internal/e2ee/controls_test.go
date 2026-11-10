//go:build darwin || linux

package e2ee

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Witness constant-nonce encryption and each omitted AAD component separately;
// control: both fresh ciphertexts decrypt under the original complete binding.
func TestNonceAndEachAADField(t *testing.T) {
	_, _, c, p, inv := fixture(t)
	r := mustPointerRoot(t, c, p, inv)
	codec := codecFor(t, r, inv)
	raw := []byte(`{"body":"same plaintext"}`)
	first, err := codec.Seal(p.Binding, raw)
	if err != nil {
		t.Fatal(err)
	}
	second, err := codec.Seal(p.Binding, raw)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, second) || bytes.Equal(first[12:24], second[12:24]) {
		t.Fatal("fresh nonce witness")
	}
	for _, sealed := range [][]byte{first, second} {
		got, err := codec.Unseal(p.Binding, sealed)
		if err != nil || !bytes.Equal(got, raw) {
			t.Fatal("positive decrypt", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*Binding)
	}{
		{"mxid", func(b *Binding) { b.Identity = "@other:example.test" }},
		{"device", func(b *Binding) { b.Device = "OTHER" }},
		{"generation", func(b *Binding) { b.Generation = strings.Repeat("b", 32) }},
		{"binding", func(b *Binding) { b.Binding = strings.Repeat("b", 64) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := p.Binding
			tc.edit(&bad)
			if _, err := codec.Unseal(bad, first); err != ErrIntegrity {
				t.Fatal("AAD witness", err)
			}
		})
	}
}

// Witness inventory overflow suppresses even previously unregistered secrets;
// control: Reset retires the old operation and a fresh inventory scrubs normally.
// Byte-budget boundary is planted in accounting, without a 256MiB allocation.
func TestInventoryBoundAndReset(t *testing.T) {
	inv := new(Inventory)
	secret := []byte("inventory-planted-secret")
	inv.Register(secret)
	if inv.Err() != nil || strings.Contains(inv.Scrub(string(secret)), string(secret)) {
		t.Fatal("positive registration")
	}
	inv.used = MaxInventoryBytes - 1
	count := len(inv.values)
	inv.Register([]byte("new-unregistered-secret"))
	if inv.Err() != ErrCapacity || len(inv.values) != count || inv.used != MaxInventoryBytes-1 {
		t.Fatal("inventory byte bound")
	}
	if got := inv.Scrub("new-unregistered-secret"); got != "<e2ee-diagnostic-suppressed>" {
		t.Fatal("overflow emitted diagnostic")
	}
	inv.Reset()
	if inv.Err() != nil || inv.used != 0 || len(inv.values) != 0 || len(inv.seen) != 0 || inv.Scrub("public") != "public" {
		t.Fatal("reset control")
	}
	inv.Register(secret)
	if strings.Contains(inv.Scrub(string(secret)), string(secret)) {
		t.Fatal("fresh operation not scrubbed")
	}
	inv.Reset()
	inv.values = make([]string, MaxInventoryEntries)
	inv.Register(secret)
	if inv.Err() != ErrCapacity || len(inv.values) != MaxInventoryEntries {
		t.Fatal("inventory entry bound")
	}
	inv.Reset()
}

// Witness background-context reentry returns ErrLock without a polling deadline,
// including a second handle for the same inode. Release is the positive control.
// Every goroutine wait selects on completion or bounded failure.
func TestLockImmediateReentry(t *testing.T) {
	s, lease, _, _, _ := fixture(t)
	for _, store := range []*FileStore{s, {dir: s.dir}} {
		result := make(chan error, 1)
		go func() {
			next, err := store.Lock(context.Background())
			if next != nil {
				next.Close()
			}
			result <- err
		}()
		select {
		case err := <-result:
			if err != ErrLock {
				t.Fatal("reentry witness", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("self-lock waited")
		}
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := s.Lock(t.Context())
	if err != nil {
		t.Fatal("release control", err)
	}
	next.Close()
}

// Witness a real partial stage write (short nil error or ENOSPC) cannot publish;
// next-lock cleanup removes the partial stage and the positive retry succeeds.
func TestPartialStageWriteRecovery(t *testing.T) {
	for _, failure := range []string{"short-write", "ENOSPC"} {
		t.Run(failure, func(t *testing.T) {
			s, lease, _, _, _ := fixture(t)
			if err := lease.publish("partial-test", []byte("old committed"), true); err != nil {
				t.Fatal(err)
			}
			raw := bytes.Repeat([]byte("partial-stage"), 32)
			called := false
			s.writeStage = func(file *os.File, data []byte) (int, error) {
				called = true
				n, err := file.Write(data[:len(data)/2])
				if err != nil {
					return n, err
				}
				if failure == "ENOSPC" {
					return n, syscall.ENOSPC
				}
				return n, nil
			}
			if err := lease.publish("partial-test", raw, false); err != ErrIO || !called {
				t.Fatal("writer witness", err)
			}
			stages := stageContents(t, s)
			if len(stages) != 1 || !bytes.Equal(stages[0], raw[:len(raw)/2]) {
				t.Fatal("partial stage was not retained")
			}
			got, err := lease.read("partial-test", 100)
			if err != nil || string(got) != "old committed" {
				t.Fatal("partial write changed published item", err)
			}
			s.writeStage = nil
			lease = reopenLease(t, s, lease)
			if len(stageContents(t, s)) != 0 {
				t.Fatal("recovery retained partial stage")
			}
			if err = lease.publish("partial-test", raw, false); err != nil {
				t.Fatal("positive retry", err)
			}
			got, err = lease.read("partial-test", len(raw))
			if err != nil || !bytes.Equal(got, raw) {
				t.Fatal("retry readback", err)
			}
		})
	}
}

func stageContents(t testing.TB, s *FileStore) [][]byte {
	t.Helper()
	fd, err := unix.Openat(int(s.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := os.NewFile(uintptr(fd), "e2ee-test-stage-scan")
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	var contents [][]byte
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") {
			raw, err := (&LockedStore{store: s}).read(entry.Name(), 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			contents = append(contents, raw)
		}
	}
	return contents
}

// Witness an orphan cursor prevents minting a replacement generation; control:
// a fresh directory can publish its pointer and an ordinary cursor in order.
func TestOrphanCursorRefusesPointer(t *testing.T) {
	_, lease, c, p, _ := fixture(t)
	if err := lease.CreateCursor(Cursor{Token: "existing"}); err != nil {
		t.Fatal(err)
	}
	before, err := lease.read("cursor.json", 20000)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.CreatePointer(p); err != ErrConflict {
		t.Fatal("orphan cursor witness", err)
	}
	if _, err = lease.read("generation.json", 4096); err != ErrUnavailable {
		t.Fatal("orphan refusal wrote pointer", err)
	}
	after, err := lease.read("cursor.json", 20000)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("orphan cursor modified", err)
	}
	_, fresh, fc, fp, _ := fixture(t)
	if err = fc.CreatePointer(fp); err != nil {
		t.Fatal("fresh pointer control", err)
	}
	if err = fresh.CreateCursor(Cursor{Token: "initial"}); err != nil {
		t.Fatal("fresh cursor control", err)
	}
}

// Witness persisted over-bound collections cannot be loaded or silently pruned;
// control: an actual account and normal replay/retention state round-trip first.
func TestPersistedBoundsRefuse(t *testing.T) {
	_, lease, c, p, inv := fixture(t)
	root := mustPointerRoot(t, c, p, inv)
	codec := codecFor(t, root, inv)
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	base := Snapshot{Version: 1, Revision: 1, Engine: state, Replay: map[string]ReplayWindow{"session": {Entries: map[uint64]ReplayEntry{1: {Event: "$event", Digest: TupleHash("event")}}}}}
	if err = lease.SaveSnapshot(codec, p.Binding, base, true); err != nil {
		t.Fatal(err)
	}
	if _, err = lease.LoadSnapshot(codec, p.Binding); err != nil {
		t.Fatal("positive bounds", err)
	}
	for _, tc := range []struct {
		name  string
		plant func(*Snapshot)
		want  error
	}{
		{"replay-window", func(s *Snapshot) {
			entries := map[uint64]ReplayEntry{}
			for i := uint64(0); i <= ReplayLimit; i++ {
				entries[i] = ReplayEntry{Event: "$event", Digest: TupleHash("event")}
			}
			s.Replay = map[string]ReplayWindow{"session": {Entries: entries}}
		}, ErrCapacity},
		{"replay-sessions", func(s *Snapshot) {
			s.Replay = map[string]ReplayWindow{}
			for i := 0; i < 129; i++ {
				s.Replay[fmt.Sprint(i)] = ReplayWindow{}
			}
		}, ErrCapacity},
		{"replay-floor", func(s *Snapshot) {
			s.Replay = map[string]ReplayWindow{"session": {Floor: 2, HasFloor: true, Entries: map[uint64]ReplayEntry{1: {Event: "$event", Digest: TupleHash("event")}}}}
		}, ErrCodec},
		{"rooms", func(s *Snapshot) {
			s.Retention.Rooms = map[string]*RoomRetention{}
			for i := 0; i <= MaxRooms; i++ {
				s.Retention.Rooms[TupleHash(fmt.Sprint(i))] = &RoomRetention{}
			}
		}, ErrCapacity},
		{"retired", func(s *Snapshot) {
			room := &RoomRetention{Retired: map[string]bool{}}
			for i := 0; i <= MaxRetired; i++ {
				room.Retired[TupleHash(fmt.Sprint(i))] = true
			}
			s.Retention.Rooms = map[string]*RoomRetention{TupleHash("room"): room}
		}, ErrCapacity},
		{"verdict-count", func(s *Snapshot) { s.Retention.Verdicts = make([]Verdict, MaxVerdicts+1) }, ErrCapacity},
		{"journal-count", func(s *Snapshot) { s.Retention.Journal = make([]Verdict, MaxJournal+1) }, ErrCapacity},
		{"journal-bytes", func(s *Snapshot) {
			s.Retention.Journal = []Verdict{{ID: "a", Bytes: MaxJournalBytes}, {ID: "b", Bytes: 1}}
		}, ErrCapacity},
		{"journal-negative", func(s *Snapshot) { s.Retention.Journal = []Verdict{{ID: "a", Bytes: -1}} }, ErrCodec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := base
			tc.plant(&candidate)
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = DecodeSnapshot(raw); err != tc.want {
				t.Fatal("decode bounds witness", err)
			}
			if err = lease.WriteSealed(codec, p.Binding, raw, false); err != nil {
				t.Fatal(err)
			}
			before, err := lease.read("state.bin", MaxSnapshot+envelopeOverhead)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = lease.LoadSnapshot(codec, p.Binding); err != tc.want {
				t.Fatal("load bounds witness", err)
			}
			after, err := lease.read("state.bin", MaxSnapshot+envelopeOverhead)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("load repaired invalid bounds", err)
			}
			if err = lease.SaveSnapshot(codec, p.Binding, base, false); err != nil {
				t.Fatal("positive reset", err)
			}
		})
	}
}
