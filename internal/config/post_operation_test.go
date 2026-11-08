package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func operationFixture(t *testing.T) (Paths, *PostJournal) {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("durable journal supports macOS and Linux only")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := Paths{Base: base}
	j, err := OpenPostJournal(p, "alice", false, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return p, j
}

func operationCandidate(id string, n int) PostOperation {
	data := json.RawMessage(`{"body":"` + strings.Repeat("x", n) + `"}`)
	return PostOperation{Schema: 1, ID: id, Binding: PostBinding{Fingerprint: strings.Repeat("1", 64), UserID: "@alice:example.invalid", DeviceID: "synthetic-device", Homeserver: "https://matrix.example.invalid"},
		RoomID: "!room:example.invalid", RootID: "$root", EventType: "m.room.message", TransactionID: "txn_" + id, IntentDigest: PostOperationDigest([]byte(id)), ContentDigest: PostOperationDigest(data), RequestLength: len(data), Request: data, Outcome: "not_submitted"}
}

func mustPrepareOperation(t *testing.T, j *PostJournal, r PostOperation) PostOperation {
	t.Helper()
	got, created, err := j.Prepare(r)
	if err != nil || !created {
		t.Fatalf("prepare: created=%v err=%v", created, err)
	}
	return got
}

// Scenarios 1, 4 and 9: exact disk state survives independently reopened roots;
// no transition can downgrade a retained ACK or uncertainty.
func TestPostJournalDurableStateAndRetention(t *testing.T) {
	p, j := operationFixture(t)
	r := mustPrepareOperation(t, j, operationCandidate("stable", 20))
	for _, outcome := range []string{"not_submitted", "unknown", "acknowledged"} {
		switch outcome {
		case "unknown":
			var err error
			r, err = j.ReserveAttempt(r)
			if err != nil {
				t.Fatal(err)
			}
		case "acknowledged":
			var err error
			r, err = j.RecordACK(r, "$ack", 200, 19)
			if err != nil {
				t.Fatal(err)
			}
		}
		reopened, err := OpenPostJournal(p, "alice", false, false)
		if err != nil {
			t.Fatal(err)
		}
		got, ok, err := reopened.Read(r.ID)
		reopened.Close()
		if err != nil || !ok || got.Outcome != outcome || got.TransactionID != r.TransactionID {
			t.Fatalf("restart %s: %+v %v", outcome, got, err)
		}
		raw, err := j.root.ReadFile(r.ID + ".json")
		if err != nil {
			t.Fatal(err)
		}
		if outcome == "acknowledged" {
			if len(got.Request) != 0 || bytes.Contains(raw, []byte(`"request":`)) || bytes.Contains(raw, []byte(strings.Repeat("x", 20))) || got.ContentDigest == "" || got.EventID != "$ack" {
				t.Fatal("ACK did not drop request and retain metadata")
			}
		} else if !bytes.Equal(got.Request, r.Request) {
			t.Fatal("request changed")
		}
	}
	original, _ := j.root.ReadFile(r.ID + ".json")
	if _, err := j.ReserveAttempt(r); err == nil {
		t.Fatal("ACK downgraded")
	}
	if _, err := j.RecordACK(r, "$other", 200, 19); err == nil {
		t.Fatal("ACK replaced")
	}
	after, _ := j.root.ReadFile(r.ID + ".json")
	if !bytes.Equal(original, after) {
		t.Fatal("monotonic record changed")
	}
}

// Scenario 6: cooperative first writers produce one creator and one reservation.
func TestPostJournalConcurrentCreation(t *testing.T) {
	p, j := operationFixture(t)
	var creators, reservations atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, err := OpenPostJournal(p, "alice", false, true)
			if err != nil {
				t.Error(err)
				return
			}
			defer other.Close()
			r, created, err := other.Prepare(operationCandidate("contended", 1))
			if err != nil {
				t.Error(err)
				return
			}
			if created {
				creators.Add(1)
				if _, err = other.ReserveAttempt(r); err != nil {
					t.Error(err)
				} else {
					reservations.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if creators.Load() != 1 || reservations.Load() != 1 {
		t.Fatalf("creators=%d reservations=%d", creators.Load(), reservations.Load())
	}
	r, _, _ := j.Read("contended")
	if r.Outcome != "unknown" {
		t.Fatal(r.Outcome)
	}
}

// Scenario 5: each pre-mutation persistence failure prevents a send grant.
func TestPostJournalPersistenceFailures(t *testing.T) {
	for _, phase := range []string{"prepare", "reserve", "ack"} {
		for _, stage := range []string{"lock_sync", "create", "write", "file_sync", "publish", "directory_sync"} {
			t.Run(phase+"/"+stage, func(t *testing.T) {
				_, j := operationFixture(t)
				r := operationCandidate("failure", 10)
				if phase != "prepare" {
					r = mustPrepareOperation(t, j, r)
				}
				if phase == "ack" {
					var err error
					r, err = j.ReserveAttempt(r)
					if err != nil {
						t.Fatal(err)
					}
				}
				j.fault = func(s string) error {
					if s == stage {
						return errors.New("inert injected failure")
					}
					return nil
				}
				var err error
				switch phase {
				case "prepare":
					_, _, err = j.Prepare(r)
				case "reserve":
					_, err = j.ReserveAttempt(r)
				case "ack":
					_, err = j.RecordACK(r, "$ack", 200, 19)
				}
				if err == nil {
					t.Fatal("fault supplied a success/send grant")
				}
				j.fault = nil
				got, ok, readErr := j.Read(r.ID)
				if readErr != nil {
					t.Fatal(readErr)
				}
				want := "unknown"
				if stage == "directory_sync" {
					want = "acknowledged"
				} // rename succeeded; Read seals it
				if phase == "ack" && (!ok || got.Outcome != want) {
					t.Fatalf("ACK failure recovery state differs: %+v", got)
				}
				if phase != "ack" && ok && got.Outcome == "acknowledged" {
					t.Fatal("pre-I/O fault invented ACK")
				}
			})
		}
	}
}

// Scenario 7: accepted exact request bound has a neighboring max+1 refusal.
func TestPostJournalBounds(t *testing.T) {
	_, j := operationFixture(t)
	exact := operationCandidate("exact", PostRequestLimit-len(`{"body":""}`))
	if len(exact.Request) != PostRequestLimit {
		t.Fatal(len(exact.Request))
	}
	mustPrepareOperation(t, j, exact)
	over := operationCandidate("over", PostRequestLimit-len(`{"body":""}`)+1)
	if _, _, err := j.Prepare(over); err == nil {
		t.Fatal("oversize request accepted")
	}
	for i := 1; i < PostRecordCountLimit; i++ {
		r := mustPrepareOperation(t, j, operationCandidate(fmt.Sprintf("item_%d", i), 1))
		if _, err := j.ReserveAttempt(r); err != nil {
			t.Fatal(err)
		}
	}
	before, err := j.root.ReadFile("item_1.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Prepare(operationCandidate("overflow", 1)); err == nil {
		t.Fatal("record count limit ignored")
	}
	after, _ := j.root.ReadFile("item_1.json")
	if !bytes.Equal(before, after) {
		t.Fatal("UNKNOWN evicted")
	}
	// Unexpected entries are never silently dropped to make room.
	_, other := operationFixture(t)
	if err := other.root.WriteFile("unexpected", []byte("inert"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := other.Prepare(operationCandidate("total", 1)); err == nil {
		t.Fatal("unexpected entry ignored")
	}
}

func TestPostJournalMalformedAndUnsafe(t *testing.T) {
	for _, mode := range []string{"duplicate", "escaped_duplicate", "digest", "unknown_schema", "unknown_field", "oversize", "mode", "symlink", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			_, j := operationFixture(t)
			r := mustPrepareOperation(t, j, operationCandidate("unsafe", 10))
			raw, _ := j.root.ReadFile(r.ID + ".json")
			switch mode {
			case "duplicate":
				raw = append([]byte(`{"schema":1,`), raw[1:]...)
			case "escaped_duplicate":
				raw = append([]byte(`{"\u0073chema":1,`), raw[1:]...)
			case "digest":
				raw = bytes.Replace(raw, []byte(r.ContentDigest), []byte(strings.Repeat("0", 64)), 1)
			case "unknown_schema":
				raw = bytes.Replace(raw, []byte(`"schema":1`), []byte(`"schema":2`), 1)
			case "unknown_field":
				raw = append([]byte(`{"unexpected":true,`), raw[1:]...)
			case "oversize":
				raw = append(raw, bytes.Repeat([]byte(" "), PostRecordLimit)...)
			case "mode":
				if err := j.root.Chmod(r.ID+".json", 0644); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := j.root.Rename(r.ID+".json", "original"); err != nil {
					t.Fatal(err)
				}
				if err := j.root.Symlink("original", r.ID+".json"); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				r.Binding.DeviceID = "other-device"
				r.RecordDigest = r.digest()
				raw, _ = json.Marshal(r)
			}
			if mode != "mode" && mode != "symlink" {
				if err := j.root.WriteFile(r.ID+".json", raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := j.root.ReadFile(r.ID + ".json")
			got, _, err := j.Read(r.ID)
			if mode == "foreign" {
				if err != nil || MatchesPostOperation(got, operationCandidate("unsafe", 10).Binding, r.RoomID, r.RootID, r.IntentDigest) {
					t.Fatal("foreign binding matched")
				}
			} else if err == nil {
				t.Fatal("unsafe record accepted")
			}
			if _, _, err := j.Prepare(operationCandidate("unsafe", 10)); err == nil {
				t.Fatal("unsafe record overwritten")
			}
			after, _ := j.root.ReadFile(r.ID + ".json")
			if !bytes.Equal(before, after) {
				t.Fatal("refusal overwrote record")
			}
		})
	}
}

func TestPostJournalPathAndLockSafety(t *testing.T) {
	p, j := operationFixture(t)
	if err := j.root.Remove(".lock"); err != nil {
		t.Fatal(err)
	}
	if err := j.root.Symlink("missing", ".lock"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Prepare(operationCandidate("badlock", 1)); err == nil {
		t.Fatal("symlink lock accepted")
	}
	if _, err := j.root.Lstat("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("symlink refusal created its target")
	}
	if _, _, err := j.Read("badlock"); err == nil {
		t.Fatal("status accepted symlink lock")
	}
	if err := j.root.Remove(".lock"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Prepare(operationCandidate("missinglock", 1)); err == nil {
		t.Fatal("missing lock recreated")
	}
	other, err := OpenPostJournal(p, "alice", false, true)
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
	mustPrepareOperation(t, j, operationCandidate("goodlock", 1))
	if err := os.Chmod(filepath.Join(p.Base, "post-operations"), 0755); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenPostJournal(p, "alice", false, false); err == nil {
		other.Close()
		t.Fatal("unsafe directory accepted")
	}
	if err := os.Chmod(filepath.Join(p.Base, "post-operations"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(p.Base, "alias")
	if err := os.Symlink(p.Base, alias); err != nil {
		t.Fatal(err)
	}
	if other, err := OpenPostJournal(Paths{Base: alias}, "alice", false, true); err == nil {
		other.Close()
		t.Fatal("symlink ancestor accepted")
	}
	if _, ok, err := j.Read("absent"); err != nil || ok {
		t.Fatal("missing record invented evidence")
	}
}

func TestPostJournalExactRecordAndTotalBounds(t *testing.T) {
	_, j := operationFixture(t)
	r := mustPrepareOperation(t, j, operationCandidate("record", 1))
	raw, err := j.root.ReadFile(r.ID + ".json")
	if err != nil {
		t.Fatal(err)
	}
	padded := append(raw, bytes.Repeat([]byte(" "), PostRecordLimit-len(raw))...)
	if err = j.root.WriteFile(r.ID+".json", padded, 0600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := j.Read(r.ID); err != nil || !ok {
		t.Fatal("exact record limit refused")
	}
	if err = j.root.WriteFile(r.ID+".json", append(padded, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := j.Read(r.ID); err == nil {
		t.Fatal("record limit+1 accepted")
	}
	_, other := operationFixture(t)
	for i := 0; i < PostRecordCountLimit; i++ {
		r := mustPrepareOperation(t, other, operationCandidate(fmt.Sprintf("total_%d", i), 1))
		encoded, err := other.root.ReadFile(r.ID + ".json")
		if err != nil {
			t.Fatal(err)
		}
		encoded = append(encoded, bytes.Repeat([]byte(" "), PostRecordLimit-len(encoded))...)
		if err := other.root.WriteFile(r.ID+".json", encoded, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := other.capacity("total_0.json", PostRecordLimit); err != nil {
		t.Fatal("exact journal total refused")
	}
	if err := other.capacity("total_0.json", PostRecordLimit+1); err == nil {
		t.Fatal("journal total+1 accepted")
	}
	full, _, err := other.Read("total_0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.ReserveAttempt(full); err == nil {
		t.Fatal("replacement peak bytes not counted")
	}
}

func TestPostJournalUnsupportedLockRefuses(t *testing.T) {
	_, j := operationFixture(t)
	j.lock = func(*os.File) (func(), error) { return nil, errors.New("synthetic unsupported lock") }
	if _, _, err := j.Prepare(operationCandidate("blocked", 1)); err == nil {
		t.Fatal("unsupported lock allowed reservation")
	}
	j.lock = nil
	if _, exists, err := j.Read("blocked"); err != nil || exists {
		t.Fatal("unsupported lock created record")
	}
	j.lock = nil
	mustPrepareOperation(t, j, operationCandidate("allowed", 1))
}

func TestPostOperationIDGrammar(t *testing.T) {
	for _, id := range []string{"a", "A_9-z", strings.Repeat("Z", 64)} {
		if !ValidPostOperationID(id) {
			t.Fatal("valid opaque ID refused")
		}
	}
	for _, id := range []string{"", strings.Repeat("z", 65), "../escape", "a/b", "a.b", "a:b", "a\n", "nonascii_é"} {
		if ValidPostOperationID(id) {
			t.Fatal("unsafe opaque ID accepted")
		}
	}
}
