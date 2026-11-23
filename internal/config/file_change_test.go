//go:build linux || darwin

package config

import (
	"bytes"
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

// TestReadCacheQualifiedDirectoryChangeTimeRejectsNamespaceABA proves a
// qualified ctime change invalidates own-write durability after an unobserved
// rename-away and rename-back, even when file identity and bytes are restored.
func TestReadCacheQualifiedDirectoryChangeTimeRejectsNamespaceABA(t *testing.T) {
	p := cachedPaths(t)
	path := p.StateFile("fixture")
	dir := filepath.Dir(path)
	if !p.readCache.durable {
		t.Fatal("positive control: own synced write did not mint qualified durability")
	}
	beforeFile := StampFile(path)
	beforeDir := stampStateDirectory(dir)
	if !beforeDir.known {
		t.Fatal("positive control: directory change time is not qualified")
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moveAwayAndBack := func() {
		t.Helper()
		moved := path + ".aba-moved"
		if err := os.Rename(path, moved); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(moved, path); err != nil {
			t.Fatal(err)
		}
	}
	moveAwayAndBack()
	afterDir := stampStateDirectory(dir)
	if beforeDir.Equal(afterDir) {
		// Retry after a full second for filesystems with coarse ctime resolution.
		time.Sleep(time.Second)
		moveAwayAndBack()
		afterDir = stampStateDirectory(dir)
	}
	current, err := os.ReadFile(path)
	if err != nil || beforeDir.Equal(afterDir) || !beforeFile.Equal(StampFile(path)) || !bytes.Equal(original, current) {
		t.Fatalf("ABA control did not preserve file while changing directory evidence: err=%v sameDir=%v sameFile=%v sameBytes=%v", err, beforeDir.Equal(afterDir), beforeFile.Equal(StampFile(path)), bytes.Equal(original, current))
	}
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 || !p.readCache.durable {
		t.Fatalf("ABA no-op skipped revalidation syncs or failed to restore own proof: syncs=%+v durable=%v", rec, p.readCache.durable)
	}
}
