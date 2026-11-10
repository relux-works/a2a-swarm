package e2ee

import (
	"encoding/binary"

	"maunium.net/go/mautrix/crypto/goolm/libolmpickle"
)

// Library unpicklers trust embedded allocation counts. Authenticate/decrypt with
// the library, then validate the exact v0.30.0 binary layout before those counts
// reach its decoders. These are format/size gates, not Olm cryptography. Only the
// versions emitted by this adapter are accepted; migration needs a new codec.
// Layout sources: account/account.go:395; session/olm_session.go:379;
// ratchet/olm.go:378, ratchet/chain.go:131; session/megolm_*_session.go.
func checkedPickle(kind string, pickled, key []byte, secrets *Inventory) ([]byte, error) {
	if len(pickled) == 0 || len(pickled) > 1<<20 {
		return nil, ErrCodec
	}
	secrets.Register(pickled)
	raw, err := libolmpickle.Unpickle(key, pickled)
	if err != nil {
		return nil, engineError(err)
	}
	secrets.Register(raw)
	if !validPickleLayout(kind, raw) {
		return nil, ErrCodec
	}
	return raw, nil
}
func validPickleLayout(kind string, raw []byte) bool {
	if len(raw) < 4 {
		return false
	}
	version := binary.BigEndian.Uint32(raw[:4])
	count := func(offset int) (int, bool) {
		if offset < 0 || offset > len(raw)-4 {
			return 0, false
		}
		return int(binary.BigEndian.Uint32(raw[offset : offset+4])), true
	}
	switch kind {
	case "account":
		if version != 4 {
			return false
		}
		n, ok := count(164)
		if !ok || n > 100 {
			return false
		}
		fallbackAt := 168 + n*69
		if fallbackAt >= len(raw) {
			return false
		}
		fallback := int(raw[fallbackAt])
		return fallback <= 2 && len(raw) == fallbackAt+1+fallback*69+4
	case "olm":
		if version != 1 {
			return false
		}
		senders, ok := count(133)
		if !ok || senders > 1 {
			return false
		}
		offset := 137 + senders*100
		receivers, ok := count(offset)
		if !ok || receivers > 5 {
			return false
		}
		offset += 4 + receivers*68
		skipped, ok := count(offset)
		if !ok || skipped > 40 {
			return false
		}
		return len(raw) == offset+4+skipped*68
	case "inbound":
		return version == 2 && len(raw) == 301
	case "outbound":
		return version == 1 && len(raw) == 232
	}
	return false
}
