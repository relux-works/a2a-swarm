package devicelist

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// PinMember is the top-level member of the owner's name value that pins the
// device list (O3.4).
const PinMember = "swarma-devices"

// MaxNameValue bounds the name value octets: 520 bytes, the bound of
// LIP-0010 section 2 rule 1 and of the chain gateway contract.
const MaxNameValue = 520

// Pin is the value of swarma-devices: the version and h(D) of the list.
type Pin struct {
	Seq    uint64
	SHA256 string
}

func (p Pin) valid() bool {
	_, ok := lowerHex(p.SHA256, 32)
	return ok && p.Seq >= 1 && p.Seq <= MaxSeq
}

func encodePin(p Pin) []byte {
	b := append([]byte(nil), `{"seq":`...)
	b = strconv.AppendUint(b, p.Seq, 10)
	b = append(b, `,"sha256":`...)
	b = appendString(b, p.SHA256)
	return append(b, '}')
}

// PinFromNameValue reads swarma-devices from the octets of the owner's name
// value (O3.4, O3.5, LIP-0010 section 2 and SH6). The member is matched by
// its exact octets. It refuses, as DEVICE_PIN_INVALID:
//
//   - a value over 520 bytes, a value that is not one JSON object starting at
//     its first octet, and any octet after the object, white space included;
//   - a value without the member, and a duplicate of the member;
//   - a top-level member name that equals swarma-devices after JSON string
//     decoding or under Unicode simple case folding and is not spelled with
//     exactly the octets "swarma-devices": an escaped spelling of the name
//     and a fold alias (also U+017F for s) both refuse, so that two readers
//     cannot disagree on whether the name has a pin;
//   - a member that is not exactly {"seq":<seq>,"sha256":"<h(D)>"} in that
//     order and compact.
//
// It reads no other member and gives no opinion on swarma or nostr; white
// space between the members of the top level, and an escaped name of a member
// that is not an alias of swarma-devices, do not refuse.
func PinFromNameValue(value []byte) (Pin, error) {
	if len(value) == 0 || len(value) > MaxNameValue || value[0] != '{' {
		return Pin{}, DevicePinInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(value))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return Pin{}, DevicePinInvalid
	}
	exact := []byte(`"` + PinMember + `"`)
	var member json.RawMessage
	found := false
	for decoder.More() {
		before := decoder.InputOffset()
		token, err := decoder.Token()
		if err != nil {
			return Pin{}, DevicePinInvalid
		}
		name, ok := token.(string)
		after := decoder.InputOffset()
		if !ok || before < 0 || after > int64(len(value)) || before > after {
			return Pin{}, DevicePinInvalid
		}
		// The octets of the name as written: what lies between the end of the
		// previous token and the end of this one, less the comma and the
		// white space in front of it.
		spelled := bytes.TrimLeft(value[before:after], ", \t\r\n")
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return Pin{}, DevicePinInvalid
		}
		if !strings.EqualFold(name, PinMember) {
			continue
		}
		if !bytes.Equal(spelled, exact) || found {
			return Pin{}, DevicePinInvalid
		}
		found, member = true, raw
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return Pin{}, DevicePinInvalid
	}
	if !found || decoder.InputOffset() != int64(len(value)) {
		return Pin{}, DevicePinInvalid
	}
	var w struct {
		Seq    uint64 `json:"seq"`
		SHA256 string `json:"sha256"`
	}
	if json.Unmarshal(member, &w) != nil {
		return Pin{}, DevicePinInvalid
	}
	pin := Pin{Seq: w.Seq, SHA256: w.SHA256}
	if !pin.valid() || !bytes.Equal(encodePin(pin), member) {
		return Pin{}, DevicePinInvalid
	}
	return pin, nil
}
