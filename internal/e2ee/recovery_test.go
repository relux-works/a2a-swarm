//go:build darwin || linux

package e2ee

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

type diskArtifacts struct {
	lease *LockedStore
	apply int
}

func (d *diskArtifacts) Apply(p Prepared) error {
	d.apply++
	return d.lease.publish("test-artifacts", p.Evidence, false)
}
func (d *diskArtifacts) Verify(p Prepared) error {
	b, err := d.lease.read("test-artifacts", RecoveryReserve)
	if err != nil {
		return err
	}
	if !bytes.Equal(b, p.Evidence) {
		return ErrIntegrity
	}
	return nil
}

// Recovery uses the sealed, already-ratcheted account for ordinary and
// to-device-only batches. It never invokes Olm again, even after cursor rename
// succeeds but its directory fsync or final snapshot replacement is interrupted.
func TestPreparedRecoverySkipsOlm(t *testing.T) {
	for _, toDeviceOnly := range []bool{false, true} {
		for _, step := range []string{"none", "test-artifacts:file-sync", "test-artifacts:dir-sync", "cursor.json:write", "cursor.json:file-sync", "cursor.json:publish", "cursor.json:dir-sync", "state.bin:file-sync", "state.bin:dir-sync"} {
			name := step
			if toDeviceOnly {
				name = "to-device-only/" + name
			}
			t.Run(name, func(t *testing.T) {
				s, l, c, p, inv := fixture(t)
				root := mustPointerRoot(t, c, p, inv)
				codec := codecFor(t, root, inv)
				bob, key, sender, ciphertext, plain := prekey(t)
				fork, err := bob.Fork(key)
				if err != nil {
					t.Fatal(err)
				}
				_, got, err := fork.DecryptPrekey(sender, ciphertext)
				if err != nil || !bytes.Equal(got, plain) {
					t.Fatal("Olm prepare", err)
				}
				ratcheted, err := fork.Checkpoint(key)
				if err != nil {
					t.Fatal(err)
				}
				evidence := json.RawMessage(`{"accepted":["event-digest"],"refusals":[]}`)
				if toDeviceOnly {
					evidence = json.RawMessage(`{"accepted":[],"refusals":[]}`)
				}
				digest := TupleHash("batch")
				initial := Cursor{"since", 3, TupleHash("previous")}
				next := Cursor{"next", 4, digest}
				// Seed the existing cursor through its normal durable serialization seam.
				cursorBytes, _ := json.Marshal(initial)
				if err = l.publish("cursor.json", cursorBytes, true); err != nil {
					t.Fatal(err)
				}
				snapshot := Snapshot{Version: 1, Revision: 8, Engine: ratcheted, Prepared: &Prepared{p.Binding.Generation, initial, next, digest, evidence}}
				if err = l.SaveSnapshot(codec, p.Binding, snapshot, true); err != nil {
					t.Fatal(err)
				}
				artifacts := &diskArtifacts{lease: l}
				reached := false
				if step != "none" {
					s.fault = func(label string) error {
						if label == step {
							reached = true
							return ErrIO
						}
						return nil
					}
				}
				recovered, did, err := l.Recover(codec, p.Binding, artifacts)
				if step != "none" {
					if err != ErrIO || !reached {
						t.Fatalf("fault not exercised: %v", err)
					}
					s.fault = nil
					// Reopen independent handles and reread persisted state, not crypto state.
					l = reopenLease(t, s, l)
					artifacts.lease = l
					recovered, did, err = l.Recover(codec, p.Binding, artifacts)
					// A failure after final state rename already cleared preparation safely.
					if step == "state.bin:dir-sync" && !did && err == nil {
						did = true
					}
				}
				if err != nil || !did || recovered.Prepared != nil {
					t.Fatal("recovery failed", err)
				}
				if !reflect.DeepEqual(recovered.Engine, ratcheted) {
					t.Fatal("recovery changed ratchets")
				}
				reopened, err := bob.engine.Restore(recovered.Engine, key)
				if err != nil {
					t.Fatal(err)
				}
				if len(reopened.account.OTKeys) != 0 || len(bob.account.OTKeys) != 1 {
					t.Fatal("OTK recovery mismatch")
				}
				cursor, err := l.ReadCursor()
				if err != nil || cursor != next {
					t.Fatal("cursor mismatch", err)
				}
				if err = artifacts.Verify(*snapshot.Prepared); err != nil {
					t.Fatal("lost effects", err)
				}
				again, did, err := l.Recover(codec, p.Binding, artifacts)
				if err != nil || did || again.Prepared != nil {
					t.Fatal("cleanup not idempotent", err)
				}
			})
		}
	}
}

// An unrelated cursor cannot consume prepared state or apply effects; the
// matching cursor is the adjacent positive control in the recovery test above.
func TestPreparedCursorConflict(t *testing.T) {
	_, l, c, p, inv := fixture(t)
	root := mustPointerRoot(t, c, p, inv)
	codec := codecFor(t, root, inv)
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	digest := TupleHash("batch")
	pRecord := Prepared{p.Binding.Generation, Cursor{"since", 0, ""}, Cursor{"next", 1, digest}, digest, json.RawMessage(`{}`)}
	snapshot := Snapshot{Version: 1, Revision: 1, Engine: state, Prepared: &pRecord}
	if err = l.SaveSnapshot(codec, p.Binding, snapshot, true); err != nil {
		t.Fatal(err)
	}
	if err = l.CreateCursor(Cursor{Token: "unrelated"}); err != nil {
		t.Fatal(err)
	}
	effects := &diskArtifacts{lease: l}
	if _, _, err = l.Recover(codec, p.Binding, effects); err != ErrConflict {
		t.Fatalf("unrelated cursor: %v", err)
	}
	if effects.apply != 0 {
		t.Fatal("effects before cursor validation")
	}
	actual, err := l.LoadSnapshot(codec, p.Binding)
	if err != nil || actual.Prepared == nil || !reflect.DeepEqual(actual.Engine, state) {
		t.Fatal("conflict mutated state", err)
	}
}

// A valid prepared record is readable, while duplicate-key evidence is refused
// before publication and leaves the previous ratcheted snapshot byte-identical.
func TestPreparedEvidenceCodecRefusal(t *testing.T) {
	_, l, c, p, inv := fixture(t)
	root := mustPointerRoot(t, c, p, inv)
	codec := codecFor(t, root, inv)
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	digest := TupleHash("batch")
	record := Prepared{p.Binding.Generation, Cursor{"since", 0, ""}, Cursor{"next", 1, digest}, digest, json.RawMessage(`{"accepted":[]}`)}
	snapshot := Snapshot{Version: 1, Revision: 1, Engine: state, Prepared: &record}
	if err = l.SaveSnapshot(codec, p.Binding, snapshot, true); err != nil {
		t.Fatal(err)
	}
	before, err := l.read("state.bin", MaxSnapshot+envelopeOverhead)
	if err != nil {
		t.Fatal(err)
	}
	record.Evidence = json.RawMessage(`{"accepted":[],"accepted":["altered"]}`)
	if err = l.SaveSnapshot(codec, p.Binding, snapshot, false); err != ErrCodec {
		t.Fatalf("duplicate evidence: %v", err)
	}
	after, err := l.read("state.bin", MaxSnapshot+envelopeOverhead)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed codec replaced snapshot", err)
	}
}
