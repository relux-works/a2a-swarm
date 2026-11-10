package e2ee

import (
	"encoding/binary"
	"testing"

	"maunium.net/go/mautrix/crypto/goolm/libolmpickle"
)

// Actual emitted account pickles restore; a planted 2^32-1 OTK count, unknown
// version and truncated body refuse before the library allocates from the count.
func TestPickleAllocationGate(t *testing.T) {
	engine, w, key := working(t)
	if _, err := w.GenerateOneTimeKeys(2); err != nil {
		t.Fatal(err)
	}
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = engine.Restore(state, key); err != nil {
		t.Fatal("valid pickle", err)
	}
	raw := w.account.PickleLibOlm()
	if !validPickleLayout("account", raw) {
		t.Fatal("valid account layout rejected")
	}
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { binary.BigEndian.PutUint32(b[164:168], ^uint32(0)); return b },
		func(b []byte) []byte { binary.BigEndian.PutUint32(b[:4], 99); return b },
		func(b []byte) []byte { return b[:len(b)-1] },
	} {
		bad := mutate(append([]byte(nil), raw...))
		encoded, err := libolmpickle.Pickle(key, bad)
		if err != nil {
			t.Fatal(err)
		}
		changed := state
		changed.Account = encoded
		if _, err = engine.Restore(changed, key); err != ErrCodec {
			t.Fatalf("allocation control missed: %v", err)
		}
	}
}

// Witness authenticated malformed Olm/Megolm layouts never reach count-trusting
// library decoders. Control: each actual emitted session restores unchanged.
func TestSessionPickleLayoutMutations(t *testing.T) {
	w, key, sender, ciphertext, _ := prekey(t)
	if _, _, err := w.DecryptPrekey(sender, ciphertext); err != nil {
		t.Fatal(err)
	}
	sid, sharing, err := w.NewOutboundMegolm()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.AcceptDirectMegolm(sharing); err != nil {
		t.Fatal(err)
	}
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.engine.Restore(state, key); err != nil {
		t.Fatal("session restore control", err)
	}
	var olmID string
	for id := range state.Olm {
		olmID = id
	}
	for _, tc := range []struct {
		kind, id string
		pickle   []byte
	}{
		{"olm", olmID, state.Olm[olmID]},
		{"inbound", string(sid), state.Inbound[string(sid)]},
		{"outbound", string(sid), state.Outbound[string(sid)]},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			raw, err := libolmpickle.Unpickle(key, tc.pickle)
			if err != nil || !validPickleLayout(tc.kind, raw) {
				t.Fatal("emitted layout control", err)
			}
			mutations := map[string]func([]byte) []byte{
				"version":   func(b []byte) []byte { binary.BigEndian.PutUint32(b[:4], 99); return b },
				"truncated": func(b []byte) []byte { return b[:len(b)-1] },
				"trailing":  func(b []byte) []byte { return append(b, 0) },
			}
			if tc.kind == "olm" {
				senders := int(binary.BigEndian.Uint32(raw[133:137]))
				receiverOffset := 137 + senders*100
				receivers := int(binary.BigEndian.Uint32(raw[receiverOffset : receiverOffset+4]))
				skippedOffset := receiverOffset + 4 + receivers*68
				for name, offset := range map[string]int{"senders": 133, "receivers": receiverOffset, "skipped": skippedOffset} {
					mutations[name] = func(b []byte) []byte { binary.BigEndian.PutUint32(b[offset:offset+4], ^uint32(0)); return b }
				}
			}
			for name, mutate := range mutations {
				t.Run(name, func(t *testing.T) {
					bad := mutate(append([]byte(nil), raw...))
					encoded, err := libolmpickle.Pickle(key, bad)
					if err != nil {
						t.Fatal(err)
					}
					candidate := state
					// Replace only the selected map; never corrupt the positive state's maps.
					clone := map[string][]byte{tc.id: encoded}
					switch tc.kind {
					case "olm":
						candidate.Olm = clone
					case "inbound":
						candidate.Inbound = clone
					case "outbound":
						candidate.Outbound = clone
					}
					if _, err = w.engine.Restore(candidate, key); err != ErrCodec {
						t.Fatal("session layout witness", err)
					}
					if _, err = w.engine.Restore(state, key); err != nil {
						t.Fatal("mutation changed control", err)
					}
				})
			}
		})
	}
}
