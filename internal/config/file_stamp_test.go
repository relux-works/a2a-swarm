//go:build unix

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A stamp changes for every observable version change: content growth, a
// same-size rewrite through atomic replacement, deletion and recreation. A
// missing path is a known absent stamp, and an unknown stamp equals nothing.
func TestFileStampVersions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f")
	absent := StampFile(path)
	if !absent.Known || absent.Present || !absent.Equal(StampFile(path)) {
		t.Fatalf("missing path must be a stable known absent stamp: %+v", absent)
	}
	if err := os.WriteFile(path, []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	first := StampFile(path)
	if !first.Present || first.Equal(absent) || !first.Equal(StampFile(path)) {
		t.Fatalf("positive control: an unchanged file keeps its stamp: %+v", first)
	}
	replacement := filepath.Join(dir, "f.next")
	if err := os.WriteFile(replacement, []byte("two"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if StampFile(path).Equal(first) {
		t.Fatal("atomic same-size replacement kept the stamp")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	before := StampFile(path)
	if _, err := f.WriteString("+"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if StampFile(path).Equal(before) {
		t.Fatal("append kept the stamp")
	}
	if (FileStamp{}).Equal(FileStamp{}) {
		t.Fatal("unknown stamps must never be equal")
	}
}

// Own-write stamps are recorded under the state lock by the scope's own
// writer. They stay unchanged across that writer's own updates and change for
// another writer's update and for a mailbox change; another writer's change
// stays visible through a later own operation until the evidence is consumed;
