//go:build linux || darwin

package courierenroll

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRollbackPreservesPublishedReader(t *testing.T) {
	dir := target(t)
	if Install(dir, strings.NewReader(fixture("old", 41)), false) != nil {
		t.Fatal("setup")
	}
	var pinned *os.File
	err := install(dir, strings.NewReader(fixture("new", 42)), true, func(phase string, _ *os.File) error {
		if phase != "published" {
			return nil
		}
		var err error
		pinned, err = os.Open(dir)
		if err != nil {
			t.Fatal("pinning new generation failed")
		}
		return errors.New("durability failure")
	})
	if err != errWrite || pinned == nil {
		t.Fatal("rollback injection failed")
	}
	defer pinned.Close()
	checkGeneration(t, dir, "old", 41)
	if verify(pinned, [][]byte{[]byte("/run/a2a/courier.sock\n"), []byte("new\n"), bytes.Repeat([]byte{42}, 32)}) != nil {
		t.Fatal("rollback broke an already-pinned reader")
	}
}

func TestRollbackFailureIsExplicitlyUncertain(t *testing.T) {
	dir := target(t)
	home := filepath.Dir(dir)
	if Install(dir, strings.NewReader(fixture("old", 43)), false) != nil {
		t.Fatal("setup")
	}
	err := install(dir, strings.NewReader(fixture("new", 44)), true, func(phase string, stage *os.File) error {
		if phase != "published" {
			return nil
		}
		// Simulate losing the rollback namespace without changing either set
		// of bytes. The command must refuse to claim successful rollback.
		if os.Rename(filepath.Join(home, stage.Name()), filepath.Join(home, "retained-old")) != nil {
			t.Fatal("rollback failure setup")
		}
		return errors.New("injected")
	})
	if err != errRecovery {
		t.Fatal("lost rollback was not reported as uncertain")
	}
	checkGeneration(t, dir, "new", 44)
	checkGeneration(t, filepath.Join(home, "retained-old"), "old", 43)
}

func TestWrongModeAfterPublicationRollsBack(t *testing.T) {
	dir := target(t)
	if Install(dir, strings.NewReader(fixture("old", 45)), false) != nil {
		t.Fatal("setup")
	}
	err := install(dir, strings.NewReader(fixture("new", 46)), true, func(phase string, _ *os.File) error {
		if phase == "published" {
			return os.Chmod(filepath.Join(dir, "station.key"), 0644)
		}
		return nil
	})
	if err == nil {
		t.Fatal("wrong mode after publication reported success")
	}
	checkGeneration(t, dir, "old", 45)
}
