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
// a nil scope, another identity or nothing recorded is always "changed".
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
	// The own operation re-records the files, but the foreign change it found
	// stays evidence until the consumer's next full cycle consumes it.
	if scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("an own operation must not relabel another writer's earlier change")
	}
	scope.ClearForeignWrites()
	if !scope.OwnFilesUnchanged(own, "alice") {
		t.Fatal("after the evidence is consumed, the fresh own record must read unchanged")
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

// Foreign-write evidence survives a later own write: when another writer
// changes the files between two own locked operations, the second own
// operation must not relabel that change as its own, so the files read as
// changed until the evidence is consumed. Neighbour: own, own reads
// unchanged. Calibration: with detection disabled (the earlier behaviour)
// the same sequence wrongly reads unchanged.
func TestStateDecodeScopeForeignBetweenOwnWrites(t *testing.T) {
	for _, detect := range []bool{true, false} {
		foreignWriteDetection = detect
		base := Paths{Base: t.TempDir()}
		scope := NewStateDecodeScope("alice")
		own := base.WithStateDecodeScope(scope)
		write := func(p Paths, v string) {
			t.Helper()
			if _, err := UpdateState(p, "alice", func(st *State) error { st.NextBatch = v; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		write(own, "s1")
		write(own, "s2")
		if !scope.OwnFilesUnchanged(own, "alice") {
			t.Fatal("neighbour: two own writes must read unchanged")
		}
		write(base, "foreign")
		write(own, "s3") // the own locked op that previously relabelled
		if got := scope.OwnFilesUnchanged(own, "alice"); got == detect {
			t.Fatalf("detect=%v: unchanged=%v after a foreign write between own writes", detect, got)
		}
		scope.ClearForeignWrites()
		if detect && !scope.OwnFilesUnchanged(own, "alice") {
			t.Fatal("consumed evidence must leave the current own record")
		}
	}
	foreignWriteDetection = true
}
