package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// The public happy path must cross both replacements and return phase digests
// that equal the actual verified disk record. The old head could not do this.
func TestPostJournalAtomicReplaceSequence(t *testing.T) {
	_, j := operationFixture(t)
	r := mustPrepareOperation(t, j, operationCandidate("sequence", 20))
	for _, outcome := range []string{"unknown", "acknowledged"} {
		var stages []string
		j.fault = func(stage string) error {
			stages = append(stages, stage)
			if stage == "publish" {
				st, err := j.root.Lstat(r.ID + ".tmp")
				if err != nil || !st.Mode().IsRegular() || !postOperationOwner(st, true) {
					t.Fatal("exclusive private temporary not present before rename")
				}
			}
			if stage == "renamed" || stage == "directory_sync" || stage == "directory_synced" {
				if _, err := j.root.Lstat(r.ID + ".tmp"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("renamed temporary name still exists")
				}
				got, ok, err := j.read(r.ID) // already inside the writer lock
				if err != nil || !ok || got.Outcome != outcome {
					t.Fatal("directory sync did not follow successor rename")
				}
			}
			return nil
		}
		var err error
		if outcome == "unknown" {
			r, err = j.ReserveAttempt(r)
		} else {
			r, err = j.RecordACK(r, "$ack", 200, 19)
		}
		if err != nil {
			t.Fatal(err)
		}
		j.fault = nil
		want := []string{"lock_sync", "create", "write", "file_sync", "publish", "renamed", "directory_sync", "directory_synced"}
		if !reflect.DeepEqual(stages, want) {
			t.Fatalf("stages=%v", stages)
		}
		got, ok, err := j.Read(r.ID)
		if err != nil || !ok || !samePostRecord(got, r) || r.RecordDigest != r.digest() {
			t.Fatal("returned phase is not pinned to the saved record")
		}
	}
}

// After rename a failure is not rollback: no second Remove(tmp) or removal of
// the target is allowed. A fresh reader may seal the visible ACK, but the
// failed writer must never return a successful reservation or ACK grant.
func TestPostJournalPostRenameFailure(t *testing.T) {
	for _, phase := range []string{"reserve", "ack"} {
		for _, stage := range []string{"renamed", "directory_sync", "directory_synced"} {
			t.Run(phase+"/"+stage, func(t *testing.T) {
				_, j := operationFixture(t)
				r := mustPrepareOperation(t, j, operationCandidate("postrename", 20))
				var err error
				want := "unknown"
				if phase == "ack" {
					r, err = j.ReserveAttempt(r)
					if err != nil {
						t.Fatal(err)
					}
					want = "acknowledged"
				}
				j.fault = func(s string) error {
					if s == stage {
						return errors.New("synthetic post-rename failure")
					}
					return nil
				}
				if phase == "reserve" {
					_, err = j.ReserveAttempt(r)
				} else {
					_, err = j.RecordACK(r, "$ack", 200, 19)
				}
				if err == nil {
					t.Fatal("post-rename failure returned success")
				}
				j.fault = nil
				got, ok, err := j.Read(r.ID)
				if err != nil || !ok || got.Outcome != want {
					t.Fatal("successor lost or masked")
				}
				if _, err := j.root.Lstat(r.ID + ".tmp"); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("temporary survived rename")
				}
				_, created, err := j.Prepare(operationCandidate(r.ID, 20))
				if err != nil || created {
					t.Fatal("retained operation became a new send grant")
				}
				if phase == "ack" && len(got.Request) != 0 {
					t.Fatal("ACK retained request bytes")
				}
			})
		}
	}
}

// Crash the already-built CI test binary with a synced temporary, or just
// after rename. No local/nested build, transport or provider is involved.
func TestPostJournalReplacementCrashRecovery(t *testing.T) {
	for _, stage := range []string{"publish", "renamed"} {
		t.Run(stage, func(t *testing.T) {
			p, j := operationFixture(t)
			r := mustPrepareOperation(t, j, operationCandidate("crash", 20))
			cmd := exec.Command(os.Args[0], "-test.run=^TestPostJournalReplacementCrashHelper$")
			cmd.Env = []string{"A2A_JOURNAL_HELPER=1", "A2A_JOURNAL_BASE=" + p.Base, "A2A_JOURNAL_STAGE=" + stage, "HOME=" + p.Base, "TMPDIR=" + p.Base}
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			var exit *exec.ExitError
			if err := cmd.Run(); !errors.As(err, &exit) || exit.ExitCode() != 87 {
				t.Fatalf("child did not reach crash boundary: %v %s", err, &output)
			}
			got, ok, err := j.Read(r.ID)
			if err != nil || !ok {
				t.Fatal(err)
			}
			if stage == "publish" {
				if got.Outcome != "not_submitted" {
					t.Fatal("temporary masked old record")
				}
				if _, err := j.root.Lstat(r.ID + ".tmp"); err != nil {
					t.Fatal("crash left no temporary")
				}
				// A status read ignored the leftover; the next writer removes it
				// under flock and completes a fresh, exclusive replacement.
				got, err = j.ReserveAttempt(r)
				if err != nil {
					t.Fatal("leftover prevented recovery", err)
				}
			} else if got.Outcome != "unknown" {
				t.Fatal("renamed record lost or masked")
			}
			if _, err := j.root.Lstat(r.ID + ".tmp"); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("leftover not cleaned")
			}
			ack, err := j.RecordACK(got, "$ack", 200, 19)
			if err != nil || ack.Outcome != "acknowledged" || len(ack.Request) != 0 {
				t.Fatal("crash recovery did not reach durable ACK", err)
			}
		})
	}
}

func TestPostJournalReplacementCrashHelper(t *testing.T) {
	if os.Getenv("A2A_JOURNAL_HELPER") != "1" {
		t.Skip("CI subprocess helper only")
	}
	j, err := OpenPostJournal(Paths{Base: os.Getenv("A2A_JOURNAL_BASE")}, "alice", false, false)
	if err != nil {
		os.Exit(88)
	}
	r, ok, err := j.Read("crash")
	if err != nil || !ok {
		os.Exit(88)
	}
	j.fault = func(stage string) error {
		if stage == os.Getenv("A2A_JOURNAL_STAGE") {
			os.Exit(87)
		}
		return nil
	}
	_, _ = j.ReserveAttempt(r)
	os.Exit(88)
}

func TestPostJournalTemporarySafety(t *testing.T) {
	for _, kind := range []string{"leftover", "symlink", "mode", "oversize", "directory"} {
		t.Run(kind, func(t *testing.T) {
			_, j := operationFixture(t)
			r := mustPrepareOperation(t, j, operationCandidate("safe", 1))
			original, _ := j.root.ReadFile(r.ID + ".json")
			switch kind {
			case "symlink":
				if err := j.root.Symlink(r.ID+".json", r.ID+".tmp"); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := j.root.Mkdir(r.ID+".tmp", 0700); err != nil {
					t.Fatal(err)
				}
			default:
				if err := j.root.WriteFile(r.ID+".tmp", []byte("partial crash bytes"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "mode" {
					if err := j.root.Chmod(r.ID+".tmp", 0644); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "oversize" {
					f, err := j.root.OpenFile(r.ID+".tmp", os.O_WRONLY, 0)
					if err != nil {
						t.Fatal(err)
					}
					err = f.Truncate(PostRecordLimit + 1)
					f.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			if got, ok, err := j.Read(r.ID); err != nil || !ok || !samePostRecord(got, r) {
				t.Fatal("reader used temporary")
			}
			_, err := j.ReserveAttempt(r)
			if kind == "leftover" {
				if err != nil {
					t.Fatal("safe leftover blocked next write", err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe temporary accepted")
				}
				after, _ := j.root.ReadFile(r.ID + ".json")
				if !bytes.Equal(original, after) {
					t.Fatal("unsafe temporary changed protected record")
				}
			}
		})
	}
}

// A non-cooperating replacement may recompute all public digests and keep
// semantic intent/transaction unchanged. It still must not pass either gate.
// This is a before-gate race model, not protection from arbitrary same-UID code.
func TestPostJournalPinsCompleteRecordAtBothGates(t *testing.T) {
	mutations := map[string]func(*PostOperation){
		"request_bytes": func(r *PostOperation) {
			r.Request = bytes.ReplaceAll(r.Request, []byte("x"), []byte("y"))
			r.ContentDigest = PostOperationDigest(r.Request)
		},
		"request_length": func(r *PostOperation) {
			r.Request = bytes.ReplaceAll(r.Request, []byte("x"), []byte("yy"))
			r.RequestLength = len(r.Request)
			r.ContentDigest = PostOperationDigest(r.Request)
		},
		"content_digest": func(r *PostOperation) { r.ContentDigest = strings.Repeat("2", 64) },
		"event_type":     func(r *PostOperation) { r.EventType = "m.room.other" },
		"transaction_id": func(r *PostOperation) { r.TransactionID = "another_txn" },
		"record_digest":  func(r *PostOperation) { r.RecordDigest = strings.Repeat("0", 64) },
		"fingerprint":    func(r *PostOperation) { r.Binding.Fingerprint = strings.Repeat("2", 64) },
		"account":        func(r *PostOperation) { r.Binding.UserID = "@other:example.invalid" },
		"device":         func(r *PostOperation) { r.Binding.DeviceID = "other-device" },
		"endpoint":       func(r *PostOperation) { r.Binding.Homeserver = "https://other.example.invalid" },
		"room":           func(r *PostOperation) { r.RoomID = "!other:example.invalid" },
		"root":           func(r *PostOperation) { r.RootID = "$other" },
		"id":             func(r *PostOperation) { r.ID = "another_id" },
		"intent":         func(r *PostOperation) { r.IntentDigest = strings.Repeat("2", 64) },
		"phase": func(r *PostOperation) {
			if r.Outcome == "unknown" {
				r.Outcome = "not_submitted"
			} else {
				r.Outcome = "unknown"
			}
		},
	}
	for _, phase := range []string{"reserve", "ack"} {
		for name, mutate := range mutations {
			t.Run(phase+"/"+name, func(t *testing.T) {
				_, j := operationFixture(t)
				expected := mustPrepareOperation(t, j, operationCandidate("pinned", 10))
				if phase == "ack" {
					var err error
					expected, err = j.ReserveAttempt(expected)
					if err != nil {
						t.Fatal(err)
					}
				}
				swapped := expected
				swapped.Request = bytes.Clone(expected.Request)
				mutate(&swapped)
				if name != "record_digest" {
					swapped.RecordDigest = swapped.digest()
				}
				raw, err := json.Marshal(swapped)
				if err != nil {
					t.Fatal(err)
				}
				// Deliberately bypass flock, modeling replacement before the gate.
				if err := j.root.WriteFile("swap.fixture", raw, 0600); err != nil {
					t.Fatal(err)
				}
				if err := j.root.Rename("swap.fixture", expected.ID+".json"); err != nil {
					t.Fatal(err)
				}
				if phase == "reserve" {
					_, err = j.ReserveAttempt(expected)
				} else {
					_, err = j.RecordACK(expected, "$observed", 200, 19)
				}
				if err == nil {
					t.Fatal("immutable replacement passed gate")
				}
				after, err := j.root.ReadFile(expected.ID + ".json")
				if err != nil || !bytes.Equal(raw, after) {
					t.Fatal("mismatch overwrote evidence")
				}
			})
		}
	}
}
