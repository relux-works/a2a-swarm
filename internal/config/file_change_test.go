//go:build linux || darwin

package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestStateDirectoryChangeTimeStampProvesChanges verifies that the platform
// change-time source qualifies directory stamps, keeps an unchanged stamp
// stable, and observes a namespace mutation.
func TestStateDirectoryChangeTimeStampProvesChanges(t *testing.T) {
	dir := t.TempDir()
	before := stampStateDirectory(dir)
	if !before.known {
		t.Fatal("platform change time did not qualify the directory stamp")
	}
	if !before.Equal(stampStateDirectory(dir)) {
		t.Fatal("positive control: unchanged directory stamp was not stable")
	}
	if err := os.WriteFile(filepath.Join(dir, "entry"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := stampStateDirectory(dir)
	if !after.known {
		t.Fatal("directory stamp became unknown after a namespace mutation")
	}
	if before.Equal(after) {
		// Accommodate filesystems that expose change times at one-second
		// resolution while still requiring the mutation to be observable.
		time.Sleep(time.Second)
		if err := os.WriteFile(filepath.Join(dir, "second-entry"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		after = stampStateDirectory(dir)
	}
	if before.Equal(after) {
		t.Fatal("negative control: directory namespace mutation kept the same stamp")
	}
}
