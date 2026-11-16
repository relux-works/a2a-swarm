package devicelist

import (
	"bytes"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"
)

// snapshotText is the fixture snapshot spelled by hand, member by member, as
// the chain gateway contract at 62a2fcf7 (section 5, part (B)) writes it. It does not come from the parser's encoder,
// so it pins the spelling of the file from outside.
func snapshotText(value []byte) string {
	a1, hash, block, txid := sealTxID, testTip.Hash, testOp.BlockHash, testOp.TxID
	return `{"consistency":"bracketed","mode":"observe_only",` +
		`"names":[{"expired":false,"height":812300,"name":"id/alisa","name_block_hash":"` + block + `","txid":"` + txid + `","value_b64":"` + base64.StdEncoding.EncodeToString(value) + `","vout":0}],` +
		`"network":"main",` +
		`"seals":[{"confirmations":1,"script_pubkey":"51","state":"unspent","txid":"` + a1 + `","vout":0},{"confirmations":1,"script_pubkey":"51","state":"unspent","txid":"` + a1 + `","vout":1},{"state":"not_in_utxo_set","txid":"` + a1 + `","vout":2}],` +
		`"tip":{"hash":"` + hash + `","height":812345,"time":1699999400},"v":1,"written_at":1700000000}`
}

// Claim: the parser accepts the hand-spelled contract file and refuses noncanonical or schema-invalid mutations. The
// hand-written contract fixture parses to the expected value, and the encoder
// writes the same octets back. Every other spelling and every member outside
// its rule refuses as ErrSnapshotInvalid: the canonical-JSON refusals of the
// contract (duplicate, escaped, case-alias, unknown, missing and reordered
// members, null, float, exponent, sign, leading zero, white space, BOM, LF,
// trailing bytes) and the closed values. A file over the size bound is
// ErrCapacity. Control: the unchanged octets parse in the same run, and each
// mutation is checked to have changed them.
// Claim: canonical fixture bytes round-trip exactly, while the listed lexical, schema and relation mutations refuse.
func TestSnapshotFileStrict(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	value := nameValue(pinOf(list))
	data := snapshotData(value, list, sealE)
	text := snapshotText(value)
	raw := []byte(text)
	parsed, err := parseSnapshotFile(raw)
	if err != nil || !reflect.DeepEqual(parsed, data) {
		t.Fatalf("control: %+v %v", parsed, err)
	}
	if encoded := encodeSnapshotFile(data); !bytes.Equal(encoded, raw) {
		t.Fatalf("the encoder does not write the contract spelling:\n%s\n%s", encoded, raw)
	}
	b64 := base64.StdEncoding.EncodeToString(value)
	if !strings.HasSuffix(b64, "=") {
		t.Fatal("fixture: the value has no base64 padding to remove")
	}
	mut := func(old, new string) []byte {
		t.Helper()
		if strings.Count(text, old) < 1 {
			t.Fatalf("mutation target %q is not in the fixture", old)
		}
		return []byte(strings.Replace(text, old, new, 1))
	}
	upperTip := strings.ToUpper(testTip.Hash)
	sealsStart, sealsEnd := strings.Index(text, `"seals":`)+len(`"seals":`), strings.Index(text, `,"tip"`)
	if sealsStart >= sealsEnd {
		t.Fatal("fixture: the seals array was not found")
	}
	sealsArray := text[sealsStart:sealsEnd]
	for name, octets := range map[string][]byte{
		// Canonical JSON.
		"empty":                      nil,
		"not an object":              []byte(`[]`),
		"empty object":               []byte(`{}`),
		"trailing newline":           []byte(text + "\n"),
		"leading space":              []byte(" " + text),
		"BOM":                        []byte("\xef\xbb\xbf" + text),
		"trailing value":             []byte(text + text),
		"trailing brace":             []byte(text + "}"),
		"truncated":                  raw[:len(raw)-1],
		"LF inside":                  mut(`,"mode"`, ",\n\"mode\""),
		"space inside":               mut(`"v":1`, `"v": 1`),
		"unknown member":             []byte(`{"a":1,` + text[1:]),
		"unknown member of a record": mut(`"script_pubkey":"51",`, `"script_pubkey":"51","spent":false,`),
		"missing consistency":        mut(`"consistency":"bracketed",`, ``),
		"missing written_at":         mut(`,"written_at":1700000000`, ``),
		"missing expired":            mut(`"expired":false,`, ``),
		"missing tip time":           mut(`,"time":1699999400`, ``),
		"duplicate member":           mut(`"mode":"observe_only",`, `"mode":"observe_only","mode":"observe_only",`),
		"duplicate, other value":     mut(`"network":"main"`, `"network":"regtest","network":"main"`),
		"reordered members":          mut(`"consistency":"bracketed","mode":"observe_only"`, `"mode":"observe_only","consistency":"bracketed"`),
		"case alias of a key":        mut(`"mode"`, `"Mode"`),
		"Unicode fold alias":         mut(`"seals"`, `"ſeals"`),
		"escaped key":                mut(`"mode"`, `"m\u006fde"`),
		"escaped value":              mut(`"observe_only"`, `"observe\u005fonly"`),
		"escaped slash":              mut(`"id/alisa"`, `"id\/alisa"`),
		"null tip":                   mut(`"tip":{"hash":"`+testTip.Hash+`","height":812345,"time":1699999400}`, `"tip":null`),
		"null boolean":               mut(`"expired":false`, `"expired":null`),
		"null array":                 mut(sealsArray, `null`),
		"empty array":                mut(sealsArray, `[]`),
		"float":                      mut(`"height":812345`, `"height":812345.0`),
		"exponent":                   mut(`"height":812345`, `"height":8.12345e5`),
		"negative":                   mut(`"written_at":1700000000`, `"written_at":-1`),
		"negative zero":              mut(`"vout":0}]`, `"vout":-0}]`),
		"leading zero":               mut(`"v":1`, `"v":01`),
		"number as a string":         mut(`"v":1`, `"v":"1"`),
		"boolean as a number":        mut(`"expired":false`, `"expired":0`),
		// Closed values.
		"another mode":                mut(`observe_only`, `enforce`),
		"another version":             mut(`"v":1`, `"v":2`),
		"another network":             mut(`"network":"main"`, `"network":"testnet"`),
		"consistency witnessed":       mut(`"consistency":"bracketed"`, `"consistency":"witnessed"`),
		"consistency empty":           mut(`"consistency":"bracketed"`, `"consistency":""`),
		"consistency in another case": mut(`"consistency":"bracketed"`, `"consistency":"Bracketed"`),
		"consistency null":            mut(`"consistency":"bracketed"`, `"consistency":null`),
		"seal state spent":            mut(`"state":"not_in_utxo_set"`, `"state":"spent"`),
		// Types.
		"uppercase tip hash":        mut(testTip.Hash, upperTip),
		"short tip hash":            mut(testTip.Hash, testTip.Hash[2:]),
		"tip height over the bound": mut(`"height":812345`, `"height":9007199254740992`),
		"tip time over the bound":   mut(`"time":1699999400`, `"time":9007199254740992`),
		"write time over the bound": mut(`"written_at":1700000000`, `"written_at":9007199254740992`),
		"uppercase name":            mut(`"id/alisa"`, `"id/Alisa"`),
		"name with a leading slash": mut(`"id/alisa"`, `"/alisa"`),
		"empty name":                mut(`"id/alisa"`, `""`),
		"uppercase name block hash": mut(testOp.BlockHash, strings.ToUpper(testOp.BlockHash)),
		"short name txid":           mut(testOp.TxID, testOp.TxID[2:]),
		"vout over 32 bits":         mut(`"vout":0}]`, `"vout":4294967296}]`),
		"unpadded value":            mut(b64, strings.TrimRight(b64, "=")),
		"value that is not base64":  mut(b64, "!"+b64[1:]),
		"short seal txid":           mut(sealTxID+`","vout":1`, sealTxID[2:]+`","vout":1`),
		// Relations inside the file.
		"name above the tip":        mut(`"height":812300`, `"height":812346`),
		"name in another tip block": mut(`"height":812300`, `"height":812345`),
		"seals out of order":        mut(`"vout":0},{"confirmations":1,"script_pubkey":"51","state":"unspent","txid":"`+sealTxID+`","vout":1}`, `"vout":1},{"confirmations":1,"script_pubkey":"51","state":"unspent","txid":"`+sealTxID+`","vout":0}`),
		"seal twice":                mut(`"vout":1}`, `"vout":0}`),
	} {
		if bytes.Equal(octets, raw) {
			t.Fatalf("%s: mutation did not change the octets", name)
		}
		if parsed, err := parseSnapshotFile(octets); err != ErrSnapshotInvalid || !reflect.DeepEqual(parsed, SnapshotData{}) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	large := append(append([]byte(nil), raw...), bytes.Repeat([]byte(" "), MaxSnapshotBytes)...)
	if _, err := parseSnapshotFile(large); err != ErrCapacity {
		t.Fatal("oversize:", err)
	}
}

// Claim: snapshot name, seal and value bounds and numeric ordering hold, each
// negative beside the control at the bound. Names: 1 to 16, strictly sorted
// by name. Seals: 1 to 256, strictly sorted by txid and then by the NUMERIC
// vout. Value: 0 to 520 bytes of padded standard base64, never base64url. A
// tip of height 0 is valid, and a name operation in the tip block must name
// the tip's hash.
// Claim: name, seal and value bounds, numeric outpoint sorting and name-operation tip binding accept controls and reject violations.
func TestSnapshotFileBounds(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	base := func() SnapshotData { return snapshotData(nameValue(pinOf(list)), list) }
	names := func(n int) []NameRecord {
		var out []NameRecord
		for i := 0; i < n; i++ {
			out = append(out, NameRecord{Name: "id/n" + string(rune('a'+i)), Value: []byte{}, Op: testOp})
		}
		return out
	}
	seals := func(n int) []SealRecord {
		var out []SealRecord
		for i := 0; i < n; i++ {
			out = append(out, SealRecord{Outpoint: Outpoint{TxID: sealTxID, Vout: uint32(i)}, State: SealUnspent, Confirmations: 1})
		}
		return out
	}
	lower := strings.Repeat("a0", 32)
	cases := []struct {
		name   string
		valid  bool
		change func(d *SnapshotData)
	}{
		{"control", true, func(d *SnapshotData) {}},
		{"16 names", true, func(d *SnapshotData) { d.Names = names(16) }},
		{"17 names", false, func(d *SnapshotData) { d.Names = names(17) }},
		{"no name", false, func(d *SnapshotData) { d.Names = nil }},
		{"names out of order", false, func(d *SnapshotData) { n := names(2); d.Names = []NameRecord{n[1], n[0]} }},
		{"name twice", false, func(d *SnapshotData) { n := names(1); d.Names = []NameRecord{n[0], n[0]} }},
		{"256 seals", true, func(d *SnapshotData) { d.Seals = seals(256) }},
		{"257 seals", false, func(d *SnapshotData) { d.Seals = seals(257) }},
		{"no seal", false, func(d *SnapshotData) { d.Seals = nil }},
		// 2 before 10 is the numeric order; as strings "10" sorts first.
		{"vout 2 then 10", true, func(d *SnapshotData) { s := seals(11); d.Seals = []SealRecord{s[2], s[10]} }},
		{"vout 10 then 2", false, func(d *SnapshotData) { s := seals(11); d.Seals = []SealRecord{s[10], s[2]} }},
		{"txid ascending", true, func(d *SnapshotData) {
			d.Seals = []SealRecord{{Outpoint: Outpoint{TxID: lower, Vout: 9}, State: SealUnspent, Confirmations: 1}, seals(1)[0]}
		}},
		{"txid descending", false, func(d *SnapshotData) {
			d.Seals = []SealRecord{seals(1)[0], {Outpoint: Outpoint{TxID: lower, Vout: 9}, State: SealUnspent, Confirmations: 1}}
		}},
		{"empty value", true, func(d *SnapshotData) { d.Names[0].Value = []byte{} }},
		{"value of 520 bytes", true, func(d *SnapshotData) { d.Names[0].Value = bytes.Repeat([]byte("x"), 520) }},
		{"value of 521 bytes", false, func(d *SnapshotData) { d.Names[0].Value = bytes.Repeat([]byte("x"), 521) }},
		{"tip of height 0", true, func(d *SnapshotData) {
			d.Tip.Height = 0
			d.Names[0].Op.Height, d.Names[0].Op.BlockHash = 0, d.Tip.Hash
		}},
		{"name in the tip block", true, func(d *SnapshotData) {
			d.Names[0].Op.Height, d.Names[0].Op.BlockHash = d.Tip.Height, d.Tip.Hash
		}},
		{"name at the tip height in another block", false, func(d *SnapshotData) { d.Names[0].Op.Height = d.Tip.Height }},
		{"expired name", true, func(d *SnapshotData) { d.Names[0].Expired = true }},
	}
	for _, c := range cases {
		data := base()
		c.change(&data)
		parsed, err := parseSnapshotFile(encodeSnapshotFile(data))
		if c.valid && (err != nil || !reflect.DeepEqual(parsed, data)) {
			t.Fatalf("%s: control refused: %v", c.name, err)
		}
		if !c.valid && (err != ErrSnapshotInvalid || !reflect.DeepEqual(parsed, SnapshotData{})) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	// Base64url is not the value encoding. Control: the same octets in
	// standard base64 parse.
	data := base()
	data.Names[0].Value = []byte{0xfb, 0xff, 0xfe}
	std := string(encodeSnapshotFile(data))
	if !strings.Contains(std, `"+//+"`) {
		t.Fatal("fixture: the value does not use the two characters that base64url replaces")
	}
	if _, err := parseSnapshotFile([]byte(std)); err != nil {
		t.Fatal("control: standard base64:", err)
	}
	if _, err := parseSnapshotFile([]byte(strings.Replace(std, `"+//+"`, `"-__-"`, 1))); err != ErrSnapshotInvalid {
		t.Fatal("base64url value:", err)
	}
}
