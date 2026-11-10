//go:build darwin || linux

package e2ee

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// BenchmarkSnapshot measures actual seal+file fsync+rename+directory fsync+
// read+unseal and operation-scoped secret inventory at 1MiB and the 64MiB payload
// cap. Cold reopens codec/store; warm reuses the open locked capability (no
// plaintext reuse). OS cache eviction is not portable/authorized: "cold" means
// cold handles/AEAD, not cold physical disk. CI records platform and GOTOOLCHAIN.
// A separate bounded sample of 20 operations reports p50/p95 without retaining
// an unbounded duration slice as b.N grows. The normal loop gives ns/op.
func BenchmarkSnapshot(b *testing.B) {
	for _, size := range []int{1 << 20, MaxSnapshot} {
		for _, cold := range []bool{true, false} {
			mode := "warm"
			if cold {
				mode = "cold"
			}
			b.Run(fmt.Sprintf("%s/%dMiB", mode, size>>20), func(b *testing.B) {
				dir := filepath.Join(testRoot(b), "crypto")
				s, err := OpenFileStore(dir)
				if err != nil {
					b.Fatal(err)
				}
				lease, err := s.Lock(context.Background())
				if err != nil {
					b.Fatal(err)
				}
				binding := Binding{Binding: TupleHash("bench"), Identity: "@bench:example.test", Device: "BENCH", Generation: "0123456789abcdef0123456789abcdef"}
				key := bytes.Repeat([]byte{0x77}, 32)
				raw := bytes.Repeat([]byte("s"), size)
				// The first store file is create-only; subsequent operations replace it.
				initial, _ := NewCodec(key, new(Inventory))
				if err = lease.WriteSealed(initial, binding, raw, true); err != nil {
					b.Fatal(err)
				}
				operation := func() {
					if cold {
						lease.Close()
						s.Close()
						s, err = OpenFileStore(dir)
						if err != nil {
							b.Fatal(err)
						}
						lease, err = s.Lock(context.Background())
						if err != nil {
							b.Fatal(err)
						}
					}
					inv := new(Inventory)
					codec, err := NewCodec(key, inv)
					if err != nil {
						b.Fatal(err)
					}
					if err = lease.WriteSealed(codec, binding, raw, false); err != nil {
						b.Fatal(err)
					}
					got, err := lease.ReadSealed(codec, binding)
					if err != nil || !bytes.Equal(got, raw) {
						b.Fatal("snapshot readback", err)
					}
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
				lease.Close()
				s.Close()
				if p95 > 500*time.Millisecond {
					b.Errorf("snapshot p95 exceeds 500ms admission gate: %v", p95)
				}
			})
		}
	}
}
