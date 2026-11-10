//go:build darwin || linux

package e2ee

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func shapePin() PinRecord {
	master := bytes.Repeat([]byte{1}, 32)
	usk := bytes.Repeat([]byte{2}, 32)
	masterHash, uskHash := sha256.Sum256(master), sha256.Sum256(usk)
	masterPublic := base64.RawStdEncoding.EncodeToString(master)
	uskPublic := base64.RawStdEncoding.EncodeToString(usk)
	return PinRecord{
		Revision: 1, Digest: TupleHash("host-pin-artifact"), Owner: "@owner:example.test",
		MasterKeyID: "ed25519:" + masterPublic, MasterPublicKey: masterPublic,
		MasterSHA256:     hex.EncodeToString(masterHash[:]),
		UserSigningKeyID: "ed25519:" + uskPublic, UserSigningPublicKey: uskPublic,
		UserSigningSHA256:          hex.EncodeToString(uskHash[:]),
		UserSigningMasterSignature: base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 64)),
	}
}

// A complete public owner MSK/USK record round-trips through the sealed store;
// missing/malformed evidence and mismatched fingerprints refuse on save without
// replacing the previous snapshot, and planted invalid sealed input refuses on
// load without repair. Signature bytes are shape fixtures,
// not a cryptographic proof: U2a must verify both signatures before granting trust.
func TestOwnerPinEvidenceShape(t *testing.T) {
	_, lease, custody, pointer, inv := fixture(t)
	root := mustPointerRoot(t, custody, pointer, inv)
	codec := codecFor(t, root, inv)
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	pin := shapePin()
	if err = pin.Validate(); err != nil {
		t.Fatal("complete public shape", err)
	}
	snapshot := Snapshot{Version: 1, Revision: 1, Engine: state, Pins: []PinRecord{pin}}
	if err = lease.SaveSnapshot(codec, pointer.Binding, snapshot, true); err != nil {
		t.Fatal(err)
	}
	loaded, err := lease.LoadSnapshot(codec, pointer.Binding)
	if err != nil || !reflect.DeepEqual(loaded.Pins, snapshot.Pins) {
		t.Fatal("public evidence readback", err)
	}
	before, err := lease.read("state.bin", MaxSnapshot+envelopeOverhead)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*PinRecord)
		want error
	}{
		{"missing-master", func(p *PinRecord) { p.MasterPublicKey = "" }, ErrCodec},
		{"missing-usk", func(p *PinRecord) { p.UserSigningPublicKey = "" }, ErrCodec},
		{"missing-usk-signature", func(p *PinRecord) { p.UserSigningMasterSignature = "" }, ErrCodec},
		{"missing-usk-fingerprint", func(p *PinRecord) { p.UserSigningSHA256 = "" }, ErrCodec},
		{"missing-master-fingerprint", func(p *PinRecord) { p.MasterSHA256 = "" }, ErrCodec},
		{"wrong-master-fingerprint", func(p *PinRecord) { p.MasterSHA256 = strings.Repeat("0", 64) }, ErrIntegrity},
		{"wrong-usk-fingerprint", func(p *PinRecord) { p.UserSigningSHA256 = strings.Repeat("0", 64) }, ErrIntegrity},
		{"wrong-master-key-id", func(p *PinRecord) { p.MasterKeyID = p.UserSigningKeyID }, ErrCodec},
		{"wrong-usk-algorithm", func(p *PinRecord) { p.UserSigningKeyID = "curve25519:" + p.UserSigningPublicKey }, ErrCodec},
		{"padded-usk", func(p *PinRecord) { p.UserSigningPublicKey += "=" }, ErrCodec},
		{"noncanonical-usk", func(p *PinRecord) {
			p.UserSigningPublicKey = strings.Repeat("A", 42) + "B"
			p.UserSigningKeyID = "ed25519:" + p.UserSigningPublicKey
		}, ErrCodec},
		{"short-usk", func(p *PinRecord) { p.UserSigningPublicKey = base64.RawStdEncoding.EncodeToString(make([]byte, 31)) }, ErrCodec},
		{"signature-newline", func(p *PinRecord) { p.UserSigningMasterSignature += "\n" }, ErrCodec},
		{"short-signature", func(p *PinRecord) {
			p.UserSigningMasterSignature = base64.RawStdEncoding.EncodeToString(make([]byte, 63))
		}, ErrCodec},
		{"malformed-signature", func(p *PinRecord) { p.UserSigningMasterSignature = strings.Repeat("!", 86) }, ErrCodec},
		{"zero-revision", func(p *PinRecord) { p.Revision = 0 }, ErrCodec},
		{"bad-owner", func(p *PinRecord) { p.Owner = "owner" }, ErrCodec},
		{"bad-artifact-digest", func(p *PinRecord) { p.Digest = "bad" }, ErrCodec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := pin
			tc.edit(&bad)
			if err := bad.Validate(); err != tc.want {
				t.Fatal("shape rejection", err)
			}
			candidate := snapshot
			candidate.Pins = []PinRecord{bad}
			if err := lease.SaveSnapshot(codec, pointer.Binding, candidate, false); err != tc.want {
				t.Fatal("save rejection", err)
			}
			// Bypass Marshal's gate to exercise validation of already-decoded disk input.
			raw, err := json.Marshal(candidate)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = DecodeSnapshot(raw); err != tc.want {
				t.Fatal("decode rejection", err)
			}
			after, err := lease.read("state.bin", MaxSnapshot+envelopeOverhead)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("refusal modified snapshot", err)
			}
			// Deliberately bypass the typed writer to plant authenticated malformed
			// evidence. Loading must reject its shape without silently repairing it.
			if err = lease.WriteSealed(codec, pointer.Binding, raw, false); err != nil {
				t.Fatal(err)
			}
			planted, err := lease.read("state.bin", MaxSnapshot+envelopeOverhead)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = lease.LoadSnapshot(codec, pointer.Binding); err != tc.want {
				t.Fatal("load rejection", err)
			}
			after, err = lease.read("state.bin", MaxSnapshot+envelopeOverhead)
			if err != nil || !bytes.Equal(planted, after) {
				t.Fatal("load repaired malformed evidence", err)
			}
			if err = lease.publish("state.bin", before, false); err != nil {
				t.Fatal("restore positive fixture", err)
			}
		})
	}
	loaded, err = lease.LoadSnapshot(codec, pointer.Binding)
	if err != nil || !reflect.DeepEqual(loaded.Pins, []PinRecord{pin}) {
		t.Fatal("valid evidence no longer readable", err)
	}
}
