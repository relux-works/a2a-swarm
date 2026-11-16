package e2ee

import (
	"encoding/base64"
	"math"
)

// megolmHeaderIndex reads the message index from the unauthenticated Megolm
// group-message header: version byte 0x03, then tag/value fields where tag
// 0x08 carries the index as a base-128 varint and tag 0x12 a length-prefixed
// ciphertext, followed by an 8-byte MAC and a 64-byte signature. Nothing here
// is authenticated: the result may only refuse (replay floor at stage 5) and
// is cross-checked against the authenticated index after decryption. A header
// that does not parse returns ok=false and leaves the verdict to stage 6. Like
// the library decoder, it refuses a varint that overflows 64 bits (a tenth
// byte above 1, or more than ten bytes) and an index above the 32-bit range.
func megolmHeaderIndex(ciphertext string) (index uint64, ok bool) {
	raw, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil || len(raw) < 1+8+64 || raw[0] != 0x03 {
		return 0, false
	}
	body := raw[1 : len(raw)-8-64]
	pos := 0
	readVarint := func() (uint64, bool) {
		var value uint64
		for shift := uint(0); shift < 64; shift += 7 {
			if pos >= len(body) {
				return 0, false
			}
			b := body[pos]
			pos++
			if shift == 63 && b > 1 {
				// The tenth byte holds only bit 63; anything more overflows.
				return 0, false
			}
			value |= uint64(b&0x7f) << shift
			if b&0x80 == 0 {
				return value, true
			}
		}
		return 0, false
	}
	seen := false
	for pos < len(body) {
		tag := body[pos]
		pos++
		switch tag {
		case 0x08:
			if seen {
				return 0, false
			}
			v, good := readVarint()
			if !good || v > math.MaxUint32 {
				return 0, false
			}
			index, seen = v, true
		case 0x12:
			n, good := readVarint()
			if !good || n > uint64(len(body)-pos) {
				return 0, false
			}
			pos += int(n)
		default:
			return 0, false
		}
	}
	return index, seen
}
