package e2ee

import (
	"encoding/base64"
	"encoding/binary"
	"math"
	"testing"

	"maunium.net/go/mautrix/crypto/goolm/message"
)

// groupMessageWire renders the group-message layout a genuine encoder emits:
// version 0x03, index tag 0x08 with the given already-encoded varint, tag 0x12
// with a length-prefixed ciphertext, then an 8-byte MAC and a 64-byte
// signature (zeroes: neither parser reads them).
func groupMessageWire(indexVarint []byte) []byte {
	ciphertext := []byte("sixteen byte body")
	raw := append([]byte{0x03, 0x08}, indexVarint...)
	raw = append(raw, 0x12)
	raw = binary.AppendUvarint(raw, uint64(len(ciphertext)))
	raw = append(raw, ciphertext...)
	return append(raw, make([]byte, 8+64)...)
}

// The header reader agrees with the library decoder (goolm
// message.GroupMessage.Decode) on the index of a genuinely encoded message at
// one-byte and multi-byte varint boundaries: 0, 127, 128, 16383, 16384, 2^21
// and 2^32-1 parse in both to the same index. Beside those controls, the two
// encodings the library refuses are refused here too: an index of 2^32 (above
// the 32-bit range; its neighbour 2^32-1 is accepted) and a ten-byte varint
// whose last byte overflows 64 bits (its low bits alone would read as 1; the
// non-overflowing two-byte spelling of 1 is accepted by both). The test
// compares parsers only; it says nothing about decryption.
func TestMegolmHeaderIndexMatchesGoolm(t *testing.T) {
	parse := func(raw []byte) (uint64, bool) {
		return megolmHeaderIndex(base64.RawStdEncoding.EncodeToString(raw))
	}
	for _, index := range []uint64{0, 127, 128, 16383, 16384, 1 << 21, math.MaxUint32} {
		raw := groupMessageWire(binary.AppendUvarint(nil, index))
		var reference message.GroupMessage
		if err := reference.Decode(raw); err != nil || !reference.HasMessageIndex || uint64(reference.MessageIndex) != index {
			t.Fatalf("library control at index %d: %v", index, err)
		}
		if got, ok := parse(raw); !ok || got != index {
			t.Fatalf("header index %d read as %d (ok=%v)", index, got, ok)
		}
	}
	overflow := []byte{0x81, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02}
	for name, indexVarint := range map[string][]byte{
		"index-above-uint32": binary.AppendUvarint(nil, uint64(math.MaxUint32)+1),
		"varint-overflow":    overflow,
	} {
		raw := groupMessageWire(indexVarint)
		var reference message.GroupMessage
		if err := reference.Decode(raw); err == nil {
			t.Fatalf("%s: library control accepted the encoding", name)
		}
		if got, ok := parse(raw); ok {
			t.Fatalf("%s: header reader accepted index %d", name, got)
		}
	}
	// Control for the overflow case: the same low bits without the overflow.
	raw := groupMessageWire([]byte{0x81, 0x00})
	var reference message.GroupMessage
	if err := reference.Decode(raw); err != nil || reference.MessageIndex != 1 {
		t.Fatal("library control for the two-byte spelling", err)
	}
	if got, ok := parse(raw); !ok || got != 1 {
		t.Fatal("two-byte spelling of 1 refused", got, ok)
	}
}
