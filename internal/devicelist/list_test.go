package devicelist

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Claim: the codec computes h(D) exactly as section 3.2 defines it. The
// example of the section is 789 bytes and hashes to the value the section
// states; the parsed members are the example's; the octets survive unchanged.
// Negative: the same list with one label byte changed is still a valid list
// and has another hash, so the vector is not a constant.
// Claim: the specification example has the expected members and exact hash, and a changed label changes its hash.
func TestExampleVector(t *testing.T) {
	if len(exampleList) != 789 {
		t.Fatalf("example is %d bytes, section 3.2 says 789", len(exampleList))
	}
	if got := HashOf([]byte(exampleList)); got != exampleHash {
		t.Fatalf("h(D) = %s, section 3.2 says %s", got, exampleHash)
	}
	list, err := Parse([]byte(exampleList))
	if err != nil {
		t.Fatal("the section 3.2 example does not parse:", err)
	}
	if list.Hash() != exampleHash || !bytes.Equal(list.Bytes(), []byte(exampleList)) {
		t.Fatal("parsed list does not keep the exact octets")
	}
	if list.Network() != "main" || list.Owner() != "id/alisa" || list.Seq() != 5 {
		t.Fatal("parsed head members differ from the example")
	}
	devices := list.Devices()
	if len(devices) != 3 || devices[0].Key != keyP256G || devices[1].Key != keySecpG || devices[2].Key != keyEdE {
		t.Fatal("parsed devices differ from the example")
	}
	element := devices[2]
	if element.Label != "Element on P" || element.Alg != AlgMatrixEd25519 || element.Presence || element.Ceiling != 1 || element.Seal != sealE {
		t.Fatal("parsed matrix record differs from the example")
	}
	other := mustParse(t, replaced(t, "phone P", "phone Q"))
	if other.Hash() == exampleHash {
		t.Fatal("a changed list kept the hash of the example")
	}
	if want := (Pin{Seq: 5, SHA256: exampleHash}); pinOf(list) != want {
		t.Fatal("the pin of the example differs from section 3.2")
	}
	if string(encodePin(pinOf(list))) != `{"seq":5,"sha256":"`+exampleHash+`"}` {
		t.Fatal("the pin member differs from the spelling of section 3.2")
	}
}

// Claim: the reader refuses every spelling but the canonical one, and every
// member outside its rule, as DEVICE_LIST_INVALID, for the reason named.
// Control: the unchanged example parses in the same run, and each mutation is
// checked to have changed the octets.
// Claim: the canonical example parses while the listed lexical and schema mutations refuse.
func TestParseStrict(t *testing.T) {
	if _, err := Parse([]byte(exampleList)); err != nil {
		t.Fatal("control:", err)
	}
	unsorted := encodeList(wireList{Schema: Schema, Network: "main", Owner: "id/alisa", Seq: 5, Devices: []wireRecord{
		rec("b", "bip340", keySecpG, false, 1, 0), rec("a", "p256", keyP256G, true, 2, 1)}})
	sameKey := encodeList(wireList{Schema: Schema, Network: "main", Owner: "id/alisa", Seq: 5, Devices: []wireRecord{
		rec("a", "bip340", keySecpG, false, 1, 0), rec("b", "ssh-ed25519", keySecpG, false, 1, 1)}})
	cases := []struct {
		name   string
		octets []byte
		why    string
	}{
		{"empty", nil, "empty"},
		{"trailing newline", []byte(exampleList + "\n"), "not canonical"},
		{"leading space", []byte(" " + exampleList), "not canonical"},
		{"trailing value", []byte(exampleList + "{}"), "not canonical"},
		{"space after colon", replaced(t, `"seq":5`, `"seq": 5`), "not canonical"},
		{"reordered members", replaced(t, `"schema":"swarma-device-list/1","network":"main"`, `"network":"main","schema":"swarma-device-list/1"`), "not canonical"},
		{"duplicated member", replaced(t, `"seq":5,`, `"seq":5,"seq":5,`), "not canonical"},
		{"missing member", replaced(t, `"presence":false,`, ``), "not canonical"},
		{"null member", replaced(t, `"presence":false`, `"presence":null`), "not canonical"},
		{"unicode escape", replaced(t, `"phone P"`, `"phone \u0050"`), "not canonical"},
		{"escaped slash", replaced(t, `"id/alisa"`, `"id\/alisa"`), "not canonical"},
		{"extra member", replaced(t, `"seq":5,`, `"seq":5,"exp":1,`), "json"},
		{"seq leading zero", replaced(t, `"seq":5`, `"seq":05`), "json"},
		{"seq fraction", replaced(t, `"seq":5`, `"seq":5.0`), "json"},
		{"seq exponent", replaced(t, `"seq":5`, `"seq":5e0`), "json"},
		{"seq string", replaced(t, `"seq":5`, `"seq":"5"`), "json"},
		{"negative vout", replaced(t, `"vout":0`, `"vout":-1`), "json"},
		{"seq zero", replaced(t, `"seq":5`, `"seq":0`), "seq"},
		{"seq above the bound", replaced(t, `"seq":5`, `"seq":9007199254740992`), "seq"},
		{"schema", replaced(t, `swarma-device-list/1`, `swarma-device-list/2`), "schema"},
		{"network", replaced(t, `"network":"main"`, `"network":"test"`), "network"},
		{"owner case", replaced(t, `id/alisa`, `id/Alisa`), "owner"},
		{"owner prefix", replaced(t, `id/alisa`, `alisa`), "owner"},
		{"non-ASCII label", replaced(t, `phone P`, "phon\u00e9 P"), "label"},
		{"control character in label", replaced(t, `phone P`, `phone\tP`), "label"},
		{"empty label", replaced(t, `phone P`, ``), "label"},
		{"label over 64 bytes", replaced(t, `phone P`, strings.Repeat("p", 65)), "label"},
		{"uppercase key", replaced(t, `79be667e`, `79BE667E`), "key encoding"},
		{"ceiling 3", replaced(t, `"presence":false,"ceiling":1`, `"presence":false,"ceiling":3`), "ceiling range"},
		{"uppercase txid", replaced(t, `"txid":"a1`, `"txid":"A1`), "seal"},
		{"duplicate seal", replaced(t, `"vout":1`, `"vout":0`), "duplicate seal"},
		{"devices null", []byte(`{"schema":"swarma-device-list/1","network":"main","owner":"id/alisa","seq":5,"devices":null}`), "device count"},
		{"devices empty", []byte(`{"schema":"swarma-device-list/1","network":"main","owner":"id/alisa","seq":5,"devices":[]}`), "device count"},
		{"devices unsorted", unsorted, "device order"},
		{"two records with one key", sameKey, "device order"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if bytes.Equal(c.octets, []byte(exampleList)) {
				t.Fatal("mutation did not change the octets")
			}
			list, err := Parse(c.octets)
			if list != nil || !errors.Is(err, DeviceListInvalid) {
				t.Fatalf("want DEVICE_LIST_INVALID, got list=%v err=%v", list != nil, err)
			}
			if want := string(DeviceListInvalid) + ": " + c.why; err.Error() != want {
				t.Fatalf("refused for %q, want %q", err.Error(), want)
			}
		})
	}
	if _, err := Parse(replaced(t, `"seq":5`, `"seq":9007199254740991`)); err != nil {
		t.Fatal("control: the largest seq must parse:", err)
	}
}

// Claim: the bounds of section 3.2 hold: 16 devices parse and 17 refuse; a
// list over 4096 bytes refuses. Control: the 16-device list is within the
// byte bound, so the 17th device is refused by count and not by size.
// Claim: 16 devices fit, while 17 devices and an oversized list refuse for their respective bounds.
func TestParseBounds(t *testing.T) {
	records := func(n int, label string) []wireRecord {
		var out []wireRecord
		for i := 0; i < n; i++ {
			key := strings.Repeat("0", 62) + string("0123456789abcdef"[i/16]) + string("0123456789abcdef"[i%16])
			out = append(out, rec(label, "ssh-ed25519", key, false, 1, uint32(i)))
		}
		return out
	}
	sixteen := listOctets(1, records(16, "d")...)
	if len(sixteen) > MaxListBytes {
		t.Fatalf("fixture: 16 short records are %d bytes", len(sixteen))
	}
	if list, err := Parse(sixteen); err != nil || len(list.Devices()) != 16 {
		t.Fatal("control: 16 devices must parse:", err)
	}
	seventeen := listOctets(1, records(17, "d")...)
	if len(seventeen) > MaxListBytes {
		t.Fatalf("fixture: 17 short records are %d bytes", len(seventeen))
	}
	if _, err := Parse(seventeen); !errors.Is(err, DeviceListInvalid) || !strings.HasSuffix(err.Error(), "device count") {
		t.Fatal("17 devices not refused by count:", err)
	}
	large := listOctets(1, records(16, strings.Repeat("d", 64))...)
	if len(large) <= MaxListBytes {
		t.Fatalf("fixture: the large list is only %d bytes", len(large))
	}
	if _, err := Parse(large); !errors.Is(err, DeviceListInvalid) || !strings.HasSuffix(err.Error(), "size") {
		t.Fatal("a list over 4096 bytes not refused by size:", err)
	}
}

// Claim: the registry of section 3.6 decides which records a list may hold.
// An alg outside the registry refuses the list (O3.15, DEVICE_LIST_INVALID);
// a reserved alg is admitted in a list (O3.17); a key must have the encoding
// and validity of its alg (section 3.1); a ceiling above the highest level of
// the alg refuses (O3.16); ceiling 2 needs presence (O3.2); a matrix-ed25519
// record has no presence and ceiling exactly 1 (OM1). Each refusal sits
// beside a valid control that differs in the one member the claim names.
// Claim: the algorithm registry admits the listed valid records and refuses their invalid neighbors.
func TestRegistry(t *testing.T) {
	zeros := strings.Repeat("0", 62)
	cases := []struct {
		name   string
		record wireRecord
		why    string // "" for a valid control.
	}{
		{"control reserved alg", rec("d", "ssh-ed25519", keyEd2, false, 1, 0), ""},
		{"unknown alg", rec("d", "telegram", keyEd2, false, 1, 0), "alg outside the registry"},
		{"empty alg", rec("d", "", keyEd2, false, 1, 0), "alg outside the registry"},
		{"reserved alg with a key of another length", rec("d", "ssh-ed25519", keyP256G, false, 1, 0), "key encoding"},

		{"control bip340 x=1", rec("d", "bip340", zeros+"01", false, 1, 0), ""},
		{"bip340 x=5 is on no point", rec("d", "bip340", zeros+"05", false, 1, 0), "key not valid"},
		{"bip340 x above the field prime", rec("d", "bip340", strings.Repeat("f", 64), false, 1, 0), "key not valid"},
		{"bip340 ceiling 2", rec("d", "bip340", keySecpG, true, 2, 0), "ceiling above the alg"},

		{"control p256 x=5", rec("d", "p256", "02"+zeros+"05", true, 2, 0), ""},
		{"p256 x=1 is on no point", rec("d", "p256", "02"+zeros+"01", true, 2, 0), "key not valid"},
		{"p256 uncompressed prefix", rec("d", "p256", "04"+zeros+"05", true, 2, 0), "key not valid"},
		{"p256 ceiling 2 without presence", rec("d", "p256", keyP256G, false, 2, 0), "ceiling 2 without presence"},
		{"control p256 ceiling 1 without presence", rec("d", "p256", keyP256G, false, 1, 0), ""},

		{"control p384 ceiling 1", rec("d", "p384", "02"+strings.Repeat("ab", 48), false, 1, 0), ""},
		{"p384 ceiling 2", rec("d", "p384", "02"+strings.Repeat("ab", 48), true, 2, 0), "ceiling above the alg"},
		{"control fido2-es256 ceiling 2", rec("d", "fido2-es256", keyP256G, true, 2, 0), ""},

		{"control matrix key 2", rec("d", AlgMatrixEd25519, keyEd2, false, 1, 0), ""},
		{"control matrix key 3", rec("d", AlgMatrixEd25519, keyEd3, false, 1, 0), ""},
		{"matrix identity point", rec("d", AlgMatrixEd25519, "01"+zeros, false, 1, 0), "key not valid"},
		{"matrix point of order 2", rec("d", AlgMatrixEd25519, "ec"+strings.Repeat("ff", 30)+"7f", false, 1, 0), "key not valid"},
		{"matrix point of order 4", rec("d", AlgMatrixEd25519, "00"+zeros, false, 1, 0), "key not valid"},
		{"matrix point of order 8", rec("d", AlgMatrixEd25519, "26e8958fc2b227b045c3f489f2ef98f0d5dfac05d3c63339b13802886d53fc05", false, 1, 0), "key not valid"},
		{"matrix y not below the field prime", rec("d", AlgMatrixEd25519, "ed"+strings.Repeat("ff", 30)+"7f", false, 1, 0), "key not valid"},
		{"matrix sign bit on x=0", rec("d", AlgMatrixEd25519, "01"+strings.Repeat("00", 30)+"80", false, 1, 0), "key not valid"},
		{"matrix y on no point", rec("d", AlgMatrixEd25519, "02"+zeros, false, 1, 0), "key not valid"},
		{"matrix presence", rec("d", AlgMatrixEd25519, keyEd2, true, 1, 0), "messenger record"},
		{"matrix ceiling 0", rec("d", AlgMatrixEd25519, keyEd2, false, 0, 0), "messenger record"},
		{"matrix ceiling 2", rec("d", AlgMatrixEd25519, keyEd2, true, 2, 0), "ceiling above the alg"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			list, err := Parse(listOctets(1, c.record))
			if c.why == "" {
				if err != nil || list == nil {
					t.Fatal("valid control refused:", err)
				}
				return
			}
			if list != nil || !errors.Is(err, DeviceListInvalid) {
				t.Fatalf("want DEVICE_LIST_INVALID, got list=%v err=%v", list != nil, err)
			}
			if want := string(DeviceListInvalid) + ": " + c.why; err.Error() != want {
				t.Fatalf("refused for %q, want %q", err.Error(), want)
			}
		})
	}
}
