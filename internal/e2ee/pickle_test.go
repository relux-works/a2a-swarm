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
