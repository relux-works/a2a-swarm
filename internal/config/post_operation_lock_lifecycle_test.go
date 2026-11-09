//go:build darwin || linux

package config

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Claim: Open creates a private empty lock and reuses its inode; serial opens on a local filesystem only.
func TestPostJournalLockCreatedAtOpen(t *testing.T) {
	p, j := operationFixture(t)
	st, err := j.root.Lstat(".lock")
	if err != nil || !st.Mode().IsRegular() || !postOperationOwner(st, true) || st.Size() != 0 {
		t.Fatal("open did not create a private regular empty lock", err)
	}
	other, err := OpenPostJournal(p, "alice", false, true)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	after, err := other.root.Lstat(".lock")
	if err != nil || !os.SameFile(st, after) {
		t.Fatal("existing lock replaced", err)
	}
}

// Claim: Missing later lock refuses reads/transitions without recreation or record changes; isolated unlink, not hostile continuous replacement.
func TestPostJournalMissingLaterLockRefuses(t *testing.T) {
	for _, phase := range []string{"prepare", "reserve", "ack", "read_empty", "read_record"} {
		t.Run(phase, func(t *testing.T) {
			p, j := operationFixture(t)
			r := operationCandidate("retained", 1)
			if phase == "reserve" || phase == "ack" || phase == "read_record" {
				r = mustPrepareOperation(t, j, r)
			}
			if phase == "ack" {
				var err error
				r, err = j.ReserveAttempt(r)
				if err != nil {
					t.Fatal(err)
				}
			}
			before, _ := j.root.ReadFile("retained.json")
			if err := j.root.Remove(".lock"); err != nil {
				t.Fatal(err)
			}
			var err error
			switch phase {
			case "prepare":
				_, _, err = j.Prepare(r)
			case "reserve":
				_, err = j.ReserveAttempt(r)
			case "ack":
				_, err = j.RecordACK(r, "$ack", 200, 1)
			default:
				_, _, err = j.Read(r.ID)
			}
			if err == nil {
				t.Fatal("missing later lock accepted")
			}
			if _, err = j.root.Lstat(".lock"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("later acquisition recreated lock", err)
			}
			status, err := OpenPostJournal(p, "alice", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer status.Close()
			if _, _, err = status.Read(r.ID); err == nil {
				t.Fatal("status accepted missing lock")
			}
			if _, err = j.root.Lstat(".lock"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("status recreated lock", err)
			}
			after, _ := j.root.ReadFile("retained.json")
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed record")
			}
		})
	}
}

// Claim: Only absent/valid initial locks open; unsafe neighbors refuse unchanged; local fixture types, synthetic foreign-UID coverage elsewhere.
func TestPostJournalInitialLockNeighbors(t *testing.T) {
	for _, neighbor := range []string{"missing", "existing", "symlink", "dangling", "mode", "directory", "fifo", "content"} {
		t.Run(neighbor, func(t *testing.T) {
			p, j := operationFixture(t)
			if err := j.root.Remove(".lock"); err != nil {
				t.Fatal(err)
			}
			switch neighbor {
			case "existing":
				if err := j.root.WriteFile(".lock", nil, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := j.root.WriteFile("target", []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := j.root.Symlink("target", ".lock"); err != nil {
					t.Fatal(err)
				}
			case "dangling":
				if err := j.root.Symlink("target", ".lock"); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := j.root.WriteFile(".lock", nil, 0644); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := j.root.Mkdir(".lock", 0700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(p.Base, "post-operations", "agents", "alice", ".lock"), 0600); err != nil {
					t.Fatal(err)
				}
			case "content":
				if err := j.root.WriteFile(".lock", []byte("unchanged"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := j.root.Lstat(".lock")
			other, err := OpenPostJournal(p, "alice", false, true)
			valid := neighbor == "missing" || neighbor == "existing"
			if other != nil {
				other.Close()
			}
			if (err == nil) != valid {
				t.Fatalf("valid=%v err=%v", valid, err)
			}
			if !valid {
				if _, _, err := j.Prepare(operationCandidate("refused", 1)); err == nil {
					t.Fatal("later writer accepted unsafe lock")
				}
				if _, _, err := j.Read("refused"); err == nil {
					t.Fatal("status accepted unsafe lock")
				}
			}
			after, _ := j.root.Lstat(".lock")
			if before != nil && (after == nil || !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != after.Size()) {
				t.Fatal("neighbor changed")
			}
			if neighbor == "symlink" {
				data, e := j.root.ReadFile("target")
				if e != nil || string(data) != "unchanged" {
					t.Fatal("target changed", e)
				}
			}
			if neighbor == "dangling" {
				if _, e := j.root.Lstat("target"); !errors.Is(e, os.ErrNotExist) {
					t.Fatal("dangling target created", e)
				}
			}
		})
	}
}

// Claim: Success and injected failures unlock before close exactly once; callback observation, not cross-process scheduling.
func TestPostJournalLockReleaseOrder(t *testing.T) {
	for _, stage := range []string{"success", "lock_sync", "create"} {
		t.Run(stage, func(t *testing.T) {
			_, j := operationFixture(t)
			var held *os.File
			unlocks := 0
			j.lock = func(f *os.File) (func(), error) {
				held = f
				unlock, err := lockPostOperation(f)
				if err != nil {
					return nil, err
				}
				return func() {
					if _, err := f.Stat(); err != nil {
						t.Error("descriptor closed before unlock", err)
					}
					unlocks++
					unlock()
				}, nil
			}
			j.fault = func(s string) error {
				if s == stage {
					return os.ErrPermission
				}
				return nil
			}
			_, _, err := j.Prepare(operationCandidate("release", 1))
			if (err == nil) != (stage == "success") || unlocks != 1 || held == nil {
				t.Fatalf("stage=%s err=%v unlocks=%d", stage, err, unlocks)
			}
			if _, err := held.Stat(); !errors.Is(err, os.ErrClosed) {
				t.Fatal("descriptor retained after release", err)
			}
		})
	}
}

// Claim: Foreign UID metadata fails the lock owner predicate; synthetic stat, not a real chown.
func TestPostJournalForeignLockOwnerPredicate(t *testing.T) {
	_, j := operationFixture(t)
	st, err := j.root.Lstat(".lock")
	if err != nil {
		t.Fatal(err)
	}
	foreign := operationForeignInfo{FileInfo: st, stat: *st.Sys().(*syscall.Stat_t)}
	foreign.stat.Uid = uint32(os.Geteuid()) + 1
	if postOperationOwner(foreign, true) {
		t.Fatal("foreign regular lock owner accepted")
	}
}

// Claim: Fresh cooperating goroutines open one usable journal and retain the record; concurrency across processes is out of scope.
func TestPostJournalConcurrentFreshOpen(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Paths{Base: base}
	done, failure := make(chan struct{}, 12), make(chan error, 12)
	for range 12 {
		go func() {
			j, e := OpenPostJournal(p, "alice", false, true)
			if e != nil {
				failure <- e
				return
			}
			defer j.Close()
			if _, _, e = j.Prepare(operationCandidate("fresh", 1)); e != nil {
				failure <- e
				return
			}
			done <- struct{}{}
		}()
	}
	for range 12 {
		awaitOperationSignal(t, done, failure)
	}
	j, err := OpenPostJournal(p, "alice", false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	if _, ok, err := j.Read("fresh"); err != nil || !ok {
		t.Fatal("concurrent fresh creation lost record", err)
	}
}
