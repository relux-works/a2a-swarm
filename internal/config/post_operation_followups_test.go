package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"
)

func acknowledgeOperation(t *testing.T, j *PostJournal, id string) PostOperation {
	t.Helper()
	r := mustPrepareOperation(t, j, operationCandidate(id, 1))
	r, err := j.ReserveAttempt(r)
	if err != nil {
		t.Fatal(err)
	}
	r, err = j.RecordACK(r, "$ack", 200, 1)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type operationEntrySnapshot struct {
	Mode os.FileMode
	Data string
	Link string
}

// Snapshot without following symlinks so refusal checks cover entries and targets.
func operationInventorySnapshot(t *testing.T, j *PostJournal) map[string]operationEntrySnapshot {
	t.Helper()
	d, err := j.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string]operationEntrySnapshot)
	for _, entry := range entries {
		st, err := j.root.Lstat(entry.Name())
		if err != nil {
			t.Fatal(err)
		}
		item := operationEntrySnapshot{Mode: st.Mode()}
		switch {
		case st.Mode().IsRegular():
			data, err := j.root.ReadFile(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
			item.Data = string(data)
		case st.Mode()&os.ModeSymlink != 0:
			item.Link, err = j.root.Readlink(entry.Name())
			if err != nil {
				t.Fatal(err)
			}
		}
		snapshot[entry.Name()] = item
	}
	return snapshot
}

// Explicit pruning removes ACKs only; prepared/UNKNOWN bytes, metadata and
// temporaries survive, and a repeated prune is harmless on this local fixture.
func TestPostJournalPruneAcknowledgedOnly(t *testing.T) {
	_, j := operationFixture(t)
	acknowledgeOperation(t, j, "ack")
	pending := mustPrepareOperation(t, j, operationCandidate("pending", 1))
	unknown := mustPrepareOperation(t, j, operationCandidate("unknown", 1))
	if _, err := j.ReserveAttempt(unknown); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stale.tmp", ".DS_Store", "._ack.json"} {
		if err := j.root.WriteFile(name, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := operationInventorySnapshot(t, j)
	removed, retained, err := j.PruneAcknowledged()
	if err != nil || removed != 1 || retained != 2 {
		t.Fatalf("removed=%d retained=%d err=%v", removed, retained, err)
	}
	delete(before, "ack.json")
	if after := operationInventorySnapshot(t, j); !reflect.DeepEqual(before, after) {
		t.Fatal("prune changed something other than acknowledged evidence")
	}
	if got, ok, err := j.Read(pending.ID); err != nil || !ok || !samePostRecord(got, pending) {
		t.Fatal("pending changed", err)
	}
	if removed, retained, err := j.PruneAcknowledged(); err != nil || removed != 0 || retained != 2 {
		t.Fatal("repeat prune not idempotent", err)
	}
}

// Unsafe neighbors refuse before any removal, including stale-temp cleanup;
// removing that neighbor allows pruning. Foreign here means an unexpected entry,
// not a real chown: foreign-UID rejection is covered by synthetic owner tests.
func TestPostJournalPruneUnsafeInventoryUnchanged(t *testing.T) {
	for _, kind := range []string{"symlink", "foreign_entry", "invalid_record", "public_record", "metadata_symlink", "metadata_directory"} {
		t.Run(kind, func(t *testing.T) {
			_, j := operationFixture(t)
			acknowledgeOperation(t, j, "ack")
			mustPrepareOperation(t, j, operationCandidate("pending", 1))
			u := mustPrepareOperation(t, j, operationCandidate("unknown", 1))
			if _, err := j.ReserveAttempt(u); err != nil {
				t.Fatal(err)
			}
			if err := j.root.WriteFile("safe.tmp", []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			name := "foreign"
			var err error
			switch kind {
			case "symlink":
				name = "unsafe.json"
				err = j.root.Symlink("ack.json", name)
			case "metadata_symlink":
				name = ".DS_Store"
				err = j.root.Symlink("ack.json", name)
			case "metadata_directory":
				name = "._x"
				err = j.root.Mkdir(name, 0700)
			case "invalid_record", "public_record":
				name = "unsafe.json"
				data, e := j.root.ReadFile("ack.json")
				if e != nil {
					t.Fatal(e)
				}
				mode := os.FileMode(0600)
				if kind == "public_record" {
					mode = 0644
					// Keep content valid so only the mode can cause this refusal.
					r, ok, e := j.read("ack")
					if e != nil || !ok {
						t.Fatal("valid mode-control record unavailable", e)
					}
					r.ID = "unsafe"
					r.RecordDigest = r.digest()
					data, e = json.Marshal(r)
					if e != nil {
						t.Fatal(e)
					}
				}
				err = j.root.WriteFile(name, data, mode)
			default:
				err = j.root.WriteFile(name, []byte("unrecognized"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := operationInventorySnapshot(t, j)
			if _, _, err := j.PruneAcknowledged(); !errors.Is(err, ErrPostOperation) {
				t.Fatal("unsafe inventory not refused", err)
			}
			if after := operationInventorySnapshot(t, j); !reflect.DeepEqual(before, after) {
				t.Fatal("refusal deleted or changed an entry")
			}
			if err := j.root.Remove(name); err != nil {
				t.Fatal(err)
			}
			if removed, retained, err := j.PruneAcknowledged(); err != nil || removed != 1 || retained != 2 {
				t.Fatal("valid neighbor did not prune", err)
			}
		})
	}
}

// Only the named regular metadata files are ignored by read/write/prune and
// left intact. Symlink/directory lookalikes and other names refuse; these local
// fixtures do not establish protection against arbitrary same-UID replacement.
func TestPostJournalMetadataExceptions(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		allowed    bool
	}{
		{".DS_Store", "regular", true},
		{"._x", "regular", true},
		{"._x.tmp", "regular", true},
		{".DS_Store", "symlink", false},
		{"._x", "symlink", false},
		{"._x", "directory", false},
		{".DS_Store", "directory", false},
		{".other", "regular", false},
		{"unexpected", "regular", false},
	} {
		t.Run(tc.name+"/"+tc.kind, func(t *testing.T) {
			_, j := operationFixture(t)
			r := acknowledgeOperation(t, j, "ack")
			var err error
			switch tc.kind {
			case "regular":
				err = j.root.WriteFile(tc.name, []byte("OS metadata"), 0644)
			case "symlink":
				err = j.root.Symlink("ack.json", tc.name)
			case "directory":
				err = j.root.Mkdir(tc.name, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			before := operationInventorySnapshot(t, j)
			got, exists, err := j.Read(r.ID)
			if tc.allowed {
				if err != nil || !exists || !samePostRecord(got, r) {
					t.Fatal("regular metadata blocked read", err)
				}
			} else if !errors.Is(err, ErrPostOperation) {
				t.Fatal("unsafe neighbor allowed read", err)
			}
			_, created, err := j.Prepare(operationCandidate("next", 1))
			if tc.allowed {
				if err != nil || !created {
					t.Fatal("regular metadata blocked post", err)
				}
			} else if !errors.Is(err, ErrPostOperation) || created {
				t.Fatal("unsafe neighbor allowed write", err)
			}
			removed, retained, err := j.PruneAcknowledged()
			if tc.allowed {
				if err != nil || removed != 1 || retained != 1 {
					t.Fatal("regular metadata blocked prune", err)
				}
				after := operationInventorySnapshot(t, j)
				if after[tc.name] != before[tc.name] {
					t.Fatal("metadata was modified/deleted")
				}
			} else {
				if !errors.Is(err, ErrPostOperation) {
					t.Fatal("unsafe neighbor allowed prune", err)
				}
				if after := operationInventorySnapshot(t, j); !reflect.DeepEqual(before, after) {
					t.Fatal("unsafe inventory changed on refusal")
				}
				if err := j.root.Remove(tc.name); err != nil {
					t.Fatal(err)
				}
				mustPrepareOperation(t, j, operationCandidate("next", 1))
			}
		})
	}
}

// Prune failures never claim success; an unlink-stage fault preserves all
// records, while a sync-stage fault may already have retired the ACK. Retry
// succeeds and pending bytes survive. Faults model I/O, not actual power loss.
func TestPostJournalPruneFailures(t *testing.T) {
	for _, stage := range []string{"prune_remove", "prune_directory_sync"} {
		t.Run(stage, func(t *testing.T) {
			_, j := operationFixture(t)
			acknowledgeOperation(t, j, "ack")
			mustPrepareOperation(t, j, operationCandidate("pending", 1))
			before, err := j.root.ReadFile("pending.json")
			if err != nil {
				t.Fatal(err)
			}
			j.fault = func(s string) error {
				if s == stage {
					return os.ErrPermission
				}
				return nil
			}
			if _, _, err := j.PruneAcknowledged(); !errors.Is(err, ErrPostOperation) {
				t.Fatal("injected prune fault claimed success", err)
			}
			j.fault = nil
			_, exists, err := j.Read("ack")
			if err != nil || exists != (stage == "prune_remove") {
				t.Fatal("unexpected post-failure ACK state", err)
			}
			if _, retained, err := j.PruneAcknowledged(); err != nil || retained != 1 {
				t.Fatal("retry failed", err)
			}
			after, err := j.root.ReadFile("pending.json")
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("prune fault changed pending bytes", err)
			}
		})
	}
}

// awaitOperationSignal makes concurrency handshakes fail on worker errors or a
// bounded deadline instead of waiting forever for a signal that cannot arrive.
func awaitOperationSignal(t *testing.T, done <-chan struct{}, failure <-chan error) {
	t.Helper()
	select {
	case <-done:
	case err := <-failure:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("concurrency handshake timed out")
	}
}

// Concurrent prune/prepare use independent journal handles and serialize in
// both lock orders without losing pending/UNKNOWN bytes or the new operation.
// Only cooperating goroutines are covered; concurrency across processes is out
// of scope, as are arbitrary same-UID writers and power-loss durability.
func TestPostJournalPruneRacesPost(t *testing.T) {
	for _, first := range []string{"post", "prune"} {
		t.Run(first, func(t *testing.T) {
			p, j := operationFixture(t)
			acknowledgeOperation(t, j, "ack")
			mustPrepareOperation(t, j, operationCandidate("pending", 1))
			u := mustPrepareOperation(t, j, operationCandidate("unknown", 1))
			if _, err := j.ReserveAttempt(u); err != nil {
				t.Fatal(err)
			}
			before := operationInventorySnapshot(t, j)
			other, err := OpenPostJournal(p, "alice", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			held, attempting, release := make(chan struct{}), make(chan struct{}), make(chan struct{}, 1)
			done, failure := make(chan struct{}, 2), make(chan error, 2)
			defer close(release)
			leader, follower := j, other
			stage := "directory_synced"
			if first == "prune" {
				stage = "prune_remove"
			}
			leader.fault = func(s string) error {
				if s == stage {
					close(held)
					select {
					case <-release:
					case <-time.After(5 * time.Second):
						return errors.New("leader release timed out")
					}
				}
				return nil
			}
			follower.lock = func(f *os.File) (func(), error) {
				close(attempting)
				return lockPostOperation(f)
			}
			post := func(j *PostJournal) error {
				_, created, err := j.Prepare(operationCandidate("fresh", 1))
				if err == nil && !created {
					return errors.New("racing post was not created")
				}
				return err
			}
			prune := func(j *PostJournal) error {
				removed, _, err := j.PruneAcknowledged()
				if err == nil && removed != 1 {
					return fmt.Errorf("removed=%d want=1", removed)
				}
				return err
			}
			launch := func(j *PostJournal, fn func(*PostJournal) error) {
				go func() {
					if err := fn(j); err != nil {
						failure <- err
						return
					}
					done <- struct{}{}
				}()
			}
			if first == "post" {
				launch(leader, post)
			} else {
				launch(leader, prune)
			}
			awaitOperationSignal(t, held, failure)
			if first == "post" {
				launch(follower, prune)
			} else {
				launch(follower, post)
			}
			awaitOperationSignal(t, attempting, failure)
			// Until the leader releases, the follower must not finish its work.
			select {
			case <-done:
				t.Fatal("follower bypassed journal lock")
			case err := <-failure:
				t.Fatal(err)
			case <-time.After(20 * time.Millisecond):
			}
			release <- struct{}{}
			awaitOperationSignal(t, done, failure)
			awaitOperationSignal(t, done, failure)
			leader.fault, follower.lock = nil, nil
			if got, ok, err := j.Read("fresh"); err != nil || !ok || got.Outcome != "not_submitted" {
				t.Fatal("racing post lost", err)
			}
			after := operationInventorySnapshot(t, j)
			delete(before, "ack.json")
			delete(after, "fresh.json")
			if !reflect.DeepEqual(before, after) {
				t.Fatal("race lost pending/UNKNOWN evidence or retained the ACK")
			}
		})
	}
}
