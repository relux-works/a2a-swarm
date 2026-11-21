package config

import (
	"bytes"
	"errors"
	"os"
	"testing"
)

// TestMarkedStateReplacementKeepsMarker proves that the ordinary writer does
// not replace a marked State with a value that lacks the permanent marker,
// through SaveState and through an UpdateState callback that assigns a State
// built from scratch. Both are refused with ErrBindPartOwned, neither file
// changes, and the identity still loads. The replacement's provider-owned
// fields equal the sidecar's, so only the missing marker can refuse it.
//
// Controls: the loaded State, changed in an ordinary field, is saved and stays
// marked; the same from-scratch value is written over an unmarked State.
//
// It does not cover a writer outside this package's API: an image that drops
// unknown State keys is TestBindPartOldImageWriterIsRefused.
func TestMarkedStateReplacementKeepsMarker(t *testing.T) {
	p, _, _ := bindFixture(t, "", false)
	read := func() (state, sidecar []byte) {
		t.Helper()
		state, err := os.ReadFile(p.StateFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		sidecar, err = os.ReadFile(p.BindPartFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		return state, sidecar
	}
	stateBefore, sidecarBefore := read()
	scratch := State{Threads: []string{"$replaced"}}

	for _, tc := range []struct {
		name  string
		write func() error
	}{
		{"SaveState", func() error { return SaveState(p, bindTestName, scratch) }},
		{"UpdateState", func() error {
			_, err := UpdateState(p, bindTestName, func(st *State) error { *st = scratch; return nil })
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.write(); !errors.Is(err, ErrBindPartOwned) {
				t.Fatalf("replacement without the marker = %v, want ErrBindPartOwned", err)
			}
			state, sidecar := read()
			if !bytes.Equal(state, stateBefore) || !bytes.Equal(sidecar, sidecarBefore) {
				t.Fatal("the refused replacement changed State or the sidecar")
			}
			loaded, err := LoadState(p, bindTestName)
			if err != nil || !BindPartMarked(loaded) {
				t.Fatalf("after the refusal: marked=%t err=%v", BindPartMarked(loaded), err)
			}
		})
	}

	t.Run("control: the loaded State is saved", func(t *testing.T) {
		loaded, err := LoadState(p, bindTestName)
		if err != nil {
			t.Fatal(err)
		}
		loaded.Threads = []string{"$kept"}
		if err := SaveState(p, bindTestName, loaded); err != nil {
			t.Fatalf("ordinary save of the loaded State: %v", err)
		}
		after, err := LoadState(p, bindTestName)
		if err != nil || !BindPartMarked(after) || len(after.Threads) != 1 || after.Threads[0] != "$kept" {
			t.Fatalf("after the ordinary save: marked=%t threads=%v err=%v", BindPartMarked(after), after.Threads, err)
		}
	})

	t.Run("control: an unmarked State is replaced", func(t *testing.T) {
		q := testPaths(t)
		if err := SaveState(q, bindTestName, State{Threads: []string{"$first"}}); err != nil {
			t.Fatal(err)
		}
		if err := SaveState(q, bindTestName, scratch); err != nil {
			t.Fatalf("replacement of an unmarked State: %v", err)
		}
		after, err := LoadState(q, bindTestName)
		if err != nil || BindPartMarked(after) || len(after.Threads) != 1 || after.Threads[0] != "$replaced" {
			t.Fatalf("after the replacement: marked=%t threads=%v err=%v", BindPartMarked(after), after.Threads, err)
		}
	})
}
