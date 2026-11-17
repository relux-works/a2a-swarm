package config

import (
	"bytes"
	"os"
	"testing"
)

// TestSealDigestMemoDifferentBytes proves the digest memo returns the direct
// digest for two different inputs, reuses a computed digest for byte-
// identical input without recomputing, and never validates a seal against
// different bytes than the ones sealed.
func TestSealDigestMemoDifferentBytes(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	if err := SaveState(p, "fixture", State{NextBatch: "memo-batch-aaaa", Threads: []string{"$root"}}); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if !validWriterSeal(st) {
		t.Fatal("sealed control does not verify")
	}
	before := ReadStateDecodeStats()
	if !validWriterSeal(st) || !validWriterSeal(st) {
		t.Fatal("sealed control does not verify on repeat")
	}
	if d := ReadStateDecodeStats().StateDigests - before.StateDigests; d != 0 {
		t.Fatalf("repeat verification recomputed %d digests", d)
	}
	memo, err := memoizedStateDigest(st.rawState)
	if err != nil {
		t.Fatal(err)
	}
	direct, err := stateDigest(st.rawState)
	if err != nil {
		t.Fatal(err)
	}
	if memo != direct || memo != st.Writer.Seal {
		t.Fatal("memoized digest differs from the direct digest")
	}
	tamperedRaw := bytes.Replace(st.rawState, []byte("memo-batch-aaaa"), []byte("memo-batch-bbbb"), 1)
	if bytes.Equal(tamperedRaw, st.rawState) {
		t.Fatal("tamper control did not change bytes")
	}
	tamperedDigest, err := memoizedStateDigest(tamperedRaw)
	if err != nil {
		t.Fatal(err)
	}
	directTampered, err := stateDigest(tamperedRaw)
	if err != nil {
		t.Fatal(err)
	}
	if tamperedDigest != directTampered || tamperedDigest == direct {
		t.Fatal("digest was reused across different bytes")
	}
	tampered := st
	tampered.rawState = tamperedRaw
	if validWriterSeal(tampered) {
		t.Fatal("seal validated against different bytes")
	}
}

// TestSealDigestForeignWriter proves a foreign writer's stale-seal State is
// still detected (invalid seal, sticky fence on the next update), beside an
// untampered control that verifies and takes no fence.
func TestSealDigestForeignWriter(t *testing.T) {
	setup := func(t *testing.T) Paths {
		t.Helper()
		p := Paths{Base: t.TempDir()}
		if err := SaveState(p, "fixture", State{NextBatch: "foreign-batch-aa", Threads: []string{"$root"}}); err != nil {
			t.Fatal(err)
		}
		return p
	}
	control := setup(t)
	st, err := LoadState(control, "fixture")
	if err != nil || !validWriterSeal(st) {
		t.Fatalf("untampered control: err=%v seal-ok=%v", err, validWriterSeal(st))
	}
	got, err := UpdateState(control, "fixture", func(*State) error { return nil })
	if err != nil || !got.Writer.ForeignSeen.IsZero() {
		t.Fatalf("untampered control took a foreign fence: err=%v fence=%v", err, got.Writer.ForeignSeen)
	}
	foreign := setup(t)
	path := foreign.StateFile("fixture")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(raw, []byte("foreign-batch-aa"), []byte("foreign-batch-bb"), 1)
	if bytes.Equal(tampered, raw) {
		t.Fatal("foreign control did not change bytes")
	}
	if err := os.WriteFile(path, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(foreign, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if st.NextBatch != "foreign-batch-bb" || validWriterSeal(st) {
		t.Fatal("foreign writer's State was not detected by its seal")
	}
	got, err = UpdateState(foreign, "fixture", func(*State) error { return nil })
	if err != nil || got.Writer.ForeignSeen.IsZero() || !validWriterSeal(got) {
		t.Fatalf("foreign update: err=%v fence=%v seal-ok=%v", err, got.Writer.ForeignSeen, validWriterSeal(got))
	}
}
