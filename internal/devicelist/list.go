package devicelist

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
)

// Bounds and constants of sections 3.1 and 3.2.
const (
	Schema       = "swarma-device-list/1"
	MaxListBytes = 4096
	MaxDevices   = 16
	MaxSeq       = 9007199254740991
	maxLabel     = 64
	maxName      = 252
)

// Outpoint is a seal outpoint: the txid in display byte order, 64 lowercase
// hex, and the output index.
type Outpoint struct {
	TxID string
	Vout uint32
}

// Record is one device record of section 3.1.
type Record struct {
	Label    string
	Alg      string
	Key      string
	Presence bool
	Ceiling  int
	Seal     Outpoint
}

// List is a parsed device list. Only Parse makes one, so every List outside
// this package holds octets that passed the strict reader of section 3.2.
type List struct {
	raw     []byte
	network string
	owner   string
	seq     uint64
	devices []Record
}

// Bytes returns a copy of the exact octets of the list.
func (l *List) Bytes() []byte { return append([]byte(nil), l.raw...) }

// Hash is h(D): the SHA-256 of the exact octets, 64 lowercase hex.
func (l *List) Hash() string { return HashOf(l.raw) }

func (l *List) Network() string { return l.network }
func (l *List) Owner() string   { return l.owner }
func (l *List) Seq() uint64     { return l.seq }

// Devices returns a copy of the records, in list order.
func (l *List) Devices() []Record { return append([]Record(nil), l.devices...) }

// HashOf is h(D) of section 3.2 for the given octets.
func HashOf(octets []byte) string {
	sum := sha256.Sum256(octets)
	return hex.EncodeToString(sum[:])
}

type wireSeal struct {
	TxID string `json:"txid"`
	Vout uint32 `json:"vout"`
}

type wireRecord struct {
	Label    string   `json:"label"`
	Alg      string   `json:"alg"`
	Key      string   `json:"key"`
	Presence bool     `json:"presence"`
	Ceiling  int      `json:"ceiling"`
	Seal     wireSeal `json:"seal"`
}

type wireList struct {
	Schema  string       `json:"schema"`
	Network string       `json:"network"`
	Owner   string       `json:"owner"`
	Seq     uint64       `json:"seq"`
	Devices []wireRecord `json:"devices"`
}

// messengerCeilingOK is the "ceiling exactly 1" rule of a matrix-ed25519
// record (OM1). The codec and step 9 of Admit both call it. It is a variable
// only so that a control can plant its removal and prove that a named
// assertion fails; production never reassigns it.
var messengerCeilingOK = func(ceiling int) bool { return ceiling == 1 }

func invalidList(why string) error { return fmt.Errorf("%w: %s", DeviceListInvalid, why) }

// Parse is the strict reader of section 3.2. It refuses, as
// DEVICE_LIST_INVALID, every input that is not a valid device list in its one
// canonical spelling: the parsed value is re-encoded under the rules of
// LIP-0013 section 2.1 and must give the identical octets, so a reordered,
// missing, extra or duplicated member, whitespace, another number or string
// spelling and a trailing newline all refuse. The reason after the code is a
// fixed label and never holds list content.
func Parse(raw []byte) (*List, error) {
	if len(raw) == 0 {
		return nil, invalidList("empty")
	}
	if len(raw) > MaxListBytes {
		return nil, invalidList("size")
	}
	var w wireList
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&w); err != nil {
		return nil, invalidList("json")
	}
	if why := checkList(w); why != "" {
		return nil, invalidList(why)
	}
	if !bytes.Equal(encodeList(w), raw) {
		return nil, invalidList("not canonical")
	}
	list := &List{raw: append([]byte(nil), raw...), network: w.Network, owner: w.Owner, seq: w.Seq}
	for _, r := range w.Devices {
		list.devices = append(list.devices, Record{Label: r.Label, Alg: r.Alg, Key: r.Key, Presence: r.Presence, Ceiling: r.Ceiling, Seal: Outpoint{TxID: r.Seal.TxID, Vout: r.Seal.Vout}})
	}
	return list, nil
}

// checkList applies the member rules of sections 3.1, 3.2, 3.6 and OM1 and
// returns the fixed label of the first rule that fails, or "".
func checkList(w wireList) string {
	if w.Schema != Schema {
		return "schema"
	}
	if !validNetwork(w.Network) {
		return "network"
	}
	if !validName(w.Owner) {
		return "owner"
	}
	if w.Seq < 1 || w.Seq > MaxSeq {
		return "seq"
	}
	if len(w.Devices) < 1 || len(w.Devices) > MaxDevices {
		return "device count"
	}
	seals := map[Outpoint]bool{}
	previous := ""
	for i, r := range w.Devices {
		if len(r.Label) < 1 || len(r.Label) > maxLabel || !printable(r.Label) {
			return "label"
		}
		entry, ok := registry[r.Alg]
		if !ok {
			return "alg outside the registry"
		}
		key, ok := lowerHex(r.Key, entry.keyBytes)
		if !ok {
			return "key encoding"
		}
		if entry.valid != nil && !entry.valid(key) {
			return "key not valid"
		}
		if r.Ceiling < 0 || r.Ceiling > 2 {
			return "ceiling range"
		}
		if r.Ceiling > entry.highest {
			return "ceiling above the alg"
		}
		if r.Ceiling == 2 && !r.Presence {
			return "ceiling 2 without presence"
		}
		if r.Alg == AlgMatrixEd25519 && (r.Presence || !messengerCeilingOK(r.Ceiling)) {
			return "messenger record"
		}
		if _, ok := lowerHex(r.Seal.TxID, 32); !ok {
			return "seal"
		}
		// Ascending by the key string, strictly: lowercase hex of equal bytes
		// is the equal string, so this is also the uniqueness of O3.3.
		if i > 0 && r.Key <= previous {
			return "device order"
		}
		previous = r.Key
		seal := Outpoint{TxID: r.Seal.TxID, Vout: r.Seal.Vout}
		if seals[seal] {
			return "duplicate seal"
		}
		seals[seal] = true
	}
	return ""
}

// encodeList writes the canonical octets of LIP-0013 section 2.1: compact,
// members in the order of sections 3.1 and 3.2, canonical decimals, and
// strings with the two escapes \" and \\ only. The caller has checked that
// every string is printable ASCII.
func encodeList(w wireList) []byte {
	b := make([]byte, 0, 256+256*len(w.Devices))
	b = append(b, `{"schema":`...)
	b = appendString(b, w.Schema)
	b = append(b, `,"network":`...)
	b = appendString(b, w.Network)
	b = append(b, `,"owner":`...)
	b = appendString(b, w.Owner)
	b = append(b, `,"seq":`...)
	b = strconv.AppendUint(b, w.Seq, 10)
	b = append(b, `,"devices":[`...)
	for i, r := range w.Devices {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, `{"label":`...)
		b = appendString(b, r.Label)
		b = append(b, `,"alg":`...)
		b = appendString(b, r.Alg)
		b = append(b, `,"key":`...)
		b = appendString(b, r.Key)
		b = append(b, `,"presence":`...)
		b = strconv.AppendBool(b, r.Presence)
		b = append(b, `,"ceiling":`...)
		b = strconv.AppendInt(b, int64(r.Ceiling), 10)
		b = append(b, `,"seal":{"txid":`...)
		b = appendString(b, r.Seal.TxID)
		b = append(b, `,"vout":`...)
		b = strconv.AppendUint(b, uint64(r.Seal.Vout), 10)
		b = append(b, `}}`...)
	}
	return append(b, `]}`...)
}

func appendString(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return append(b, '"')
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// lowerHex decodes exactly n bytes of lowercase hex.
func lowerHex(s string, n int) ([]byte, bool) {
	if len(s) != 2*n {
		return nil, false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	decoded, err := hex.DecodeString(s)
	return decoded, err == nil
}

func validNetwork(s string) bool { return s == "main" || s == "regtest" }

// validName is the v3 name syntax of LIP-0013 section 2.1: "id/" and 1 to 252
// bytes of a-z, 0-9, '-', '_' and '.'.
func validName(s string) bool {
	if len(s) < 4 || len(s) > 3+maxName || s[:3] != "id/" {
		return false
	}
	for i := 3; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
