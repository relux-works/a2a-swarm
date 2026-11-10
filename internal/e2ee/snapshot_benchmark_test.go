//go:build darwin || linux

package e2ee

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// snapshotFixture constructs real account/Olm/Megolm pickles, replay windows
// and bounded accepted-message evidence. Padding is in legitimate bounded event
// IDs and <=512KiB accepted bodies, never a non-JSON substitute for Snapshot.
// Engine checkpoint generation is setup; Save/Load, JSON, inventory and IO are timed.
func snapshotFixture(b testing.TB, target int, binding Binding) Snapshot {
	b.Helper()
	_, w, key := working(b)
	_, peer, _ := working(b)
	_, peer2, _ := working(b)
	otks, err := peer.GenerateOneTimeKeys(8)
	if err != nil {
		b.Fatal(err)
	}
	_, curve, err := peer.IdentityKeys()
	if err != nil {
		b.Fatal(err)
	}
	for _, otk := range otks {
		if _, err = w.NewOutboundOlm(curve, otk); err != nil {
			b.Fatal(err)
		}
	}
	ids := make([]string, 0, 96)
	for _, source := range []*Working{w, peer, peer2} {
		for range 32 {
			sid, sharing, err := source.NewOutboundMegolm()
			if err != nil {
				b.Fatal(err)
			}
			if _, err = w.AcceptDirectMegolm(sharing); err != nil {
				b.Fatal(err)
			}
			ids = append(ids, string(sid))
		}
	}
	state, err := w.Checkpoint(key)
	if err != nil {
		b.Fatal(err)
	}
	s := Snapshot{Version: 1, Revision: 1, Engine: state, Replay: map[string]ReplayWindow{}}
	for _, sid := range ids {
		s.Replay[sid] = ReplayWindow{Entries: map[uint64]ReplayEntry{}}
	}
	raw, err := json.Marshal(s)
	if err != nil {
		b.Fatal(err)
	}
	baseGoal := target - 4096
	if target == MaxSnapshot {
		baseGoal = MaxSnapshot - RecoveryReserve
	}
	size := len(raw)
	var lastEntries map[uint64]ReplayEntry
	var lastIndex uint64
	finished := false
	for sessionIndex, sid := range ids {
		entries := s.Replay[sid].Entries
		for index := uint64(0); index < ReplayLimit; index++ {
			remaining := baseGoal - size
			if remaining == 0 {
				finished = true
				break
			}
			prefix := fmt.Sprintf("$bench-%02d-%04d-", sessionIndex, index)
			entry := ReplayEntry{Event: prefix + strings.Repeat("e", 128-len(prefix)), Digest: TupleHash(sid, strconv.FormatUint(index, 10))}
			encoded, _ := json.Marshal(entry)
			delta := len(strconv.Quote(strconv.FormatUint(index, 10))) + 1 + len(encoded)
			if len(entries) > 0 {
				delta++
			}
			if remaining < delta {
				minDelta := delta - (128 - len(prefix))
				if remaining >= minDelta {
					entry.Event = entry.Event[:128-(delta-remaining)]
					entries[index] = entry
				} else {
					if lastEntries == nil {
						b.Fatal("benchmark base exceeds target")
					}
					previous := lastEntries[lastIndex]
					if len(previous.Event)+remaining > 255 {
						b.Fatal("benchmark event budget")
					}
					previous.Event += strings.Repeat("e", remaining)
					lastEntries[lastIndex] = previous
				}
				size = baseGoal
				finished = true
				break
			}
			entries[index] = entry
			size += delta
			lastEntries, lastIndex = entries, index
		}
		if finished {
			break
		}
	}
	if size != baseGoal {
		b.Fatal("benchmark replay capacity insufficient")
	}
	// Sixteen ordinary accepted messages allow full-cap recovery evidence while
	// keeping every body below the adapter's 1MiB application-payload boundary.
	type accepted struct {
		Event string `json:"event"`
		Body  string `json:"body"`
	}
	evidence := struct {
		Accepted []accepted `json:"accepted"`
		Refusals []string   `json:"refusals"`
	}{Accepted: make([]accepted, 16), Refusals: []string{}}
	for i := range evidence.Accepted {
		evidence.Accepted[i].Event = fmt.Sprintf("$accepted-%d", i)
	}
	body, _ := json.Marshal(evidence)
	digest := TupleHash("bench-batch")
	s.Prepared = &Prepared{Generation: binding.Generation, Since: Cursor{Token: "since"}, Next: Cursor{Token: "next", Revision: 1, Batch: digest}, Digest: digest, Evidence: body}
	raw, err = json.Marshal(s)
	if err != nil {
		b.Fatal(err)
	}
	remaining := target - len(raw)
	if remaining < 0 {
		b.Fatal("benchmark fixture exceeds target")
	}
	for i := range evidence.Accepted {
		n := min(remaining, 512<<10)
		evidence.Accepted[i].Body = strings.Repeat("m", n)
		remaining -= n
	}
	if remaining != 0 {
		b.Fatal("benchmark evidence capacity insufficient")
	}
	s.Prepared.Evidence, _ = json.Marshal(evidence)
	raw, err = s.Marshal()
	if err != nil || len(raw) != target {
		b.Fatal("benchmark snapshot bounds/size", err)
	}
	return s
}

// BenchmarkSnapshot measures real SaveSnapshot/LoadSnapshot end to end: bounds,
// Marshal/strict Decode, inventory, fresh AEAD, write/fsync/rename/directory fsync,
// read/unseal. Equality is checked outside all timed operations. Cold reopens handles; OS disk-cache eviction
// is not claimed. Twenty bounded samples report nearest-rank p50/p95; b.N reports
// ns/op. The operational 1/8MiB bound is 500ms p95; full-cap 64MiB has a
// capacity-only 10s bound, not an interactive latency promise or U4 hook budget.
func BenchmarkSnapshot(b *testing.B) {
	for _, size := range []int{1 << 20, 8 << 20, MaxSnapshot} {
		for _, cold := range []bool{true, false} {
			mode := "warm"
			if cold {
				mode = "cold"
			}
			b.Run(fmt.Sprintf("%s/%dMiB", mode, size>>20), func(b *testing.B) {
				binding := Binding{Binding: TupleHash("bench"), Identity: "@bench:example.test", Device: "BENCH", Generation: "0123456789abcdef0123456789abcdef"}
				snapshot := snapshotFixture(b, size, binding)
				dir := filepath.Join(testRoot(b), "crypto")
				store, err := OpenFileStore(dir)
				if err != nil {
					b.Fatal(err)
				}
				lease, err := store.Lock(b.Context())
				if err != nil {
					store.Close()
					b.Fatal(err)
				}
				defer func() { lease.Close(); store.Close() }()
				key := bytes.Repeat([]byte{0x42}, 32)
				inv := new(Inventory)
				codec, err := NewCodec(key, inv)
				if err != nil {
					b.Fatal(err)
				}
				if err = lease.SaveSnapshot(codec, binding, snapshot, true); err != nil {
					b.Fatal(err)
				}
				operation := func() Snapshot {
					if cold {
						if lease.Close() != nil || store.Close() != nil {
							b.Fatal("close benchmark handles")
						}
						store, err = OpenFileStore(dir)
						if err != nil {
							b.Fatal(err)
						}
						lease, err = store.Lock(b.Context())
						if err != nil {
							b.Fatal(err)
						}
					}
					inv.Reset()
					codec, err = NewCodec(key, inv)
					if err != nil {
						b.Fatal(err)
					}
					if err = lease.SaveSnapshot(codec, binding, snapshot, false); err != nil {
						b.Fatal(err)
					}
					got, err := lease.LoadSnapshot(codec, binding)
					if err != nil {
						b.Fatal("snapshot readback", err)
					}
					if inv.Err() != nil {
						b.Fatal("benchmark inventory capacity")
					}
					return got
				}
				// This real round trip is outside both percentile samples and b.N timing.
				if !reflect.DeepEqual(operation(), snapshot) {
					b.Fatal("untimed snapshot equality")
				}
				durations := make([]time.Duration, 20)
				for i := range durations {
					start := time.Now()
					operation()
					durations[i] = time.Since(start)
				}
				sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
				p50, p95 := durations[9], durations[18]
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					operation()
				}
				b.StopTimer()
				b.ReportMetric(float64(p50.Microseconds())/1000, "p50-ms")
				b.ReportMetric(float64(p95.Microseconds())/1000, "p95-ms")
				b.SetBytes(int64(size))
				bound := 500 * time.Millisecond
				if size == MaxSnapshot {
					bound = 10 * time.Second
				}
				if p95 > bound {
					b.Fatalf("snapshot %dMiB p95 %v exceeds its %v bound", size>>20, p95, bound)
				}
			})
		}
	}
}
