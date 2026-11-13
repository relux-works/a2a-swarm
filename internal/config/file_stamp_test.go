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
// another writer's update and for a mailbox change; a nil scope, another
// identity or nothing recorded is always "changed".
func TestStateDecodeScopeOwnFiles(t *testing.T) {
	base := Paths{Base: t.TempDir()}
	scope := NewStateDecodeScope("alice")
	own := base.WithStateDecodeScope(scope)
	if scope.OwnFilesUnchanged(own, "alice") || scope.OwnFilesRecorded() {
		t.Fatal("nothing recorded must read as changed")
	}
	if _, err := UpdateState(own, "alice", func(st *State) error { st.NextBatch = "s1"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("positive control: our own locked write must read as unchanged")
	}
	if _, err := UpdateState(own, "alice", func(st *State) error { st.NextBatch = "s2"; return nil }); err != nil {
		t.Fatal(err)
	}
	if !scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("a second own write must read as unchanged")
	}
	if _, err := UpdateState(base, "alice", func(st *State) error { st.NextBatch = "s3"; return nil }); err != nil {
		t.Fatal(err)
	}
	if scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("another writer's update must read as changed")
	}
	if _, err := UpdateState(own, "alice", func(st *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if !scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("a fresh own locked operation must re-record")
	}
	if err := os.WriteFile(own.MailboxFile("alice"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("an unlocked mailbox change must read as changed")
	}
	var none *StateDecodeScope
	if none.OwnFilesUnchanged(own, "alice") || scope.OwnFilesUnchanged(own, "bob") {
		t.Fatal("nil scope or another identity must read as changed")
	}
}
