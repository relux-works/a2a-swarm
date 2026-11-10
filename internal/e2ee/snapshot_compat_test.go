//go:build darwin || linux

package e2ee

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// These independent reference functions retain the r3 Marshal/strict decoder
// semantics, including re-encoding and the original token-based strictJSON.
// They are deliberately test-only, never selected by a production flag.
func legacySnapshotMarshal(s Snapshot) ([]byte, error) {
	if s.Version != 1 || s.Revision == 0 || s.Engine.Version != 1 {
		return nil, ErrCodec
	}
	if err := s.validateBounds(); err != nil {
		return nil, err
	}
	for _, pin := range s.Pins {
		if err := pin.Validate(); err != nil {
			return nil, err
		}
	}
	// Reserve space independently of current prepared size. Protected evidence is
	// never evicted to fit a batch; the caller must prune or stop before mutation.
	base := s
	base.Prepared = nil
	raw, err := json.Marshal(base)
	if err != nil {
		return nil, ErrCodec
	}
	if len(raw) > MaxSnapshot-RecoveryReserve {
		return nil, ErrCapacity
	}
	if s.Prepared != nil {
		p, err := json.Marshal(s.Prepared)
		if err != nil {
			return nil, ErrCodec
		}
		if len(p) > RecoveryReserve || !s.Prepared.valid() {
			return nil, ErrCapacity
		}
	}
	raw, err = json.Marshal(s)
	if err != nil {
		return nil, ErrCodec
	}
	if len(raw) > MaxSnapshot {
		return nil, ErrCapacity
	}
	// Opaque prepared evidence must not make our own output unreadable (e.g.
	// duplicate fields or excessive nesting). Validate before any publication.
	var check Snapshot
	if err := strictJSON(raw, &check, MaxSnapshot); err != nil {
		return nil, err
	}
	return raw, nil
}
func legacyDecodeSnapshot(raw []byte) (Snapshot, error) {
	var s Snapshot
	if err := strictJSON(raw, &s, MaxSnapshot); err != nil {
		return Snapshot{}, err
	}
	if _, err := legacySnapshotMarshal(s); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

// Control: new Save bytes and Load values retain the r3 strict decoder semantics
// at exact 1/8/64MiB, including real crypto state and prepared replay evidence.
// Witness: a one-byte version change and duplicate top-level version are rejected
// by both decoders; the oracle does not merely accept every generated fixture.
func TestSnapshotStrictRoundTripEquivalence(t *testing.T) {
	for _, size := range []int{1 << 20, 8 << 20, MaxSnapshot} {
		t.Run(fmt.Sprintf("%dMiB", size>>20), func(t *testing.T) {
			_, lease, c, p, inv := fixture(t)
			root := mustPointerRoot(t, c, p, inv)
			codec := codecFor(t, root, inv)
			snapshot := snapshotFixture(t, size, p.Binding)
			expected, err := legacySnapshotMarshal(snapshot)
			if err != nil || len(expected) != size {
				t.Fatal("reference fixture", err)
			}
			if err = lease.SaveSnapshot(codec, p.Binding, snapshot, true); err != nil {
				t.Fatal(err)
			}
			sealed, err := lease.read("state.bin", MaxSnapshot+envelopeOverhead)
			if err != nil {
				t.Fatal(err)
			}
			actual, err := codec.unseal(p.Binding, sealed)
			if err != nil || !bytes.Equal(expected, actual) {
				t.Fatal("new save changed legacy bytes", err)
			}
			legacy, err := legacyDecodeSnapshot(actual)
			if err != nil {
				t.Fatal("legacy strict load", err)
			}
			loaded, err := lease.LoadSnapshot(codec, p.Binding)
			if err != nil {
				t.Fatal("new load", err)
			}
			for _, value := range []Snapshot{legacy, loaded} {
				raw, err := legacySnapshotMarshal(value)
				if err != nil || !bytes.Equal(raw, expected) {
					t.Fatal("round-trip byte equivalence", err)
				}
			}
			for _, bad := range [][]byte{
				bytes.Replace(expected, []byte(`"v":1`), []byte(`"v":9`), 1),
				append([]byte(`{"v":1,`), expected[1:]...),
			} {
				if _, err = legacyDecodeSnapshot(bad); err != ErrCodec {
					t.Fatal("reference rejection witness", err)
				}
				if _, err = DecodeSnapshot(bad); err != ErrCodec {
					t.Fatal("new rejection witness", err)
				}
			}
		})
	}
}

// Control: exact encoding/json lengths include HTML/Unicode escaping, integer
// boundaries, nil/empty collections, base64 pickles and numeric refusal arrays.
// Witness: UTF-8 key normalization collisions and opaque duplicate evidence
// remain ErrCodec; compact input that expands beyond the reserve stays refused.
func TestSnapshotCanonicalSizeEquivalence(t *testing.T) {
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	base := Snapshot{Version: 1, Revision: 1, Engine: state}
	digest := TupleHash("size")
	prepared := Prepared{Generation: strings.Repeat("a", 32), Since: Cursor{Token: "before"}, Next: Cursor{Token: "after", Revision: 1, Batch: digest}, Digest: digest, Evidence: json.RawMessage(` { "raw" : [1e+09, -0, "<>&", "\u2028"] } `)}
	cases := []Snapshot{base, base, base, base}
	cases[1].Prepared = &prepared
	cases[1].Pins = []PinRecord{shapePin()}
	room := &RoomRetention{Closed: true, Retired: map[string]bool{TupleHash("old"): true}}
	for i, b := range []byte{0, 1, 9, 10, 99, 100, 255} {
		room.Refusals[i] = b
	}
	cases[1].Retention = Retention{Rooms: map[string]*RoomRetention{TupleHash("room"): room}, Sequence: ^uint64(0), OldestSequence: 1, Verdicts: []Verdict{{ID: "verdict", Bytes: 17, Sequence: 1, Protected: true}}, Journal: []Verdict{}}
	cases[1].Replay = map[string]ReplayWindow{"session": {Entries: map[uint64]ReplayEntry{^uint64(0): {Event: "$event", Digest: digest}}}}
	cases[2].Engine = EngineState{Version: 1, Account: []byte{}, Olm: map[string][]byte{"<>&\"\n\u2028\u2029\xff": {0, 128, 255}}, Inbound: map[string][]byte{}, Outbound: map[string][]byte{}}
	cases[2].Replay = map[string]ReplayWindow{}
	cases[3].Prepared = &Prepared{Generation: prepared.Generation, Since: prepared.Since, Next: prepared.Next, Digest: digest, Evidence: json.RawMessage("null")}
	for i, s := range cases {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		b := s
		b.Prepared = nil
		baseRaw, _ := json.Marshal(b)
		baseSize, preparedSize, total, err := snapshotSizes(s)
		if err != nil || baseSize != len(baseRaw) || total != len(raw) {
			t.Fatalf("size control %d", i)
		}
		if s.Prepared != nil {
			p, _ := json.Marshal(s.Prepared)
			if preparedSize != len(p) {
				t.Fatal("prepared size control")
			}
		}
		old, err := legacySnapshotMarshal(s)
		if err != nil {
			t.Fatal("legacy control", err)
		}
		new, err := s.Marshal()
		if err != nil || !bytes.Equal(old, new) {
			t.Fatal("canonical bytes control", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*Snapshot)
		want error
	}{
		{"normalized-key-collision", func(s *Snapshot) {
			s.Engine = EngineState{Version: 1, Account: state.Account, Olm: map[string][]byte{"\xff": {1}, "\ufffd": {2}}}
		}, ErrCodec},
		{"duplicate-evidence", func(s *Snapshot) { p := prepared; p.Evidence = json.RawMessage(`{"a":1,"\u0061":2}`); s.Prepared = &p }, ErrCodec},
		{"expanded-evidence", func(s *Snapshot) {
			p := prepared
			p.Evidence = json.RawMessage(`{"body":"` + strings.Repeat("<", RecoveryReserve/6) + `"}`)
			s.Prepared = &p
		}, ErrCapacity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.edit(&s)
			if _, err = legacySnapshotMarshal(s); err != tc.want {
				t.Fatal("legacy witness", err)
			}
			if _, err = s.Marshal(); err != tc.want {
				t.Fatal("new witness", err)
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = legacyDecodeSnapshot(raw); err != tc.want {
				t.Fatal("legacy decode witness", err)
			}
			if _, err = DecodeSnapshot(raw); err != tc.want {
				t.Fatal("new decode witness", err)
			}
		})
	}
}

// The new allocation-light scanner must agree with the original token strict
// reader on grammar, UTF-8, escaped duplicate keys and exact depth boundaries.
// Compact size is checked independently against json.RawMessage's encoder.
func TestSnapshotJSONScannerCompatibility(t *testing.T) {
	good := []string{`null`, `true`, `false`, `-0`, `1.25e+9`, `1e9999`, `"<>&"`, `"\u003c\u2028\/"`, "\"a\u2028\u2029b\"", ` { "a": [0, "x", {}] } `, strings.Repeat("[", 32) + "0" + strings.Repeat("]", 32)}
	bad := []string{"", `{"a":0,"\u0061":1}`, `{"\ud800":0,"\ufffd":1}`, `{"a":[]`, `[0,]`, `{"a":1,}`, `01`, `-01`, `1.`, `1e-`, `NaN`, `true false`, `[1x]`, `"\x00"`, string([]byte{'"', 0xff, '"'}), strings.Repeat("[", 33) + "0" + strings.Repeat("]", 33)}
	for _, group := range []struct {
		values []string
		accept bool
	}{{good, true}, {bad, false}} {
		for i, raw := range group.values {
			var reference json.RawMessage
			old := strictJSON([]byte(raw), &reference, MaxSnapshot)
			size, err := scanJSON([]byte(raw), 0)
			if (old == nil) != group.accept || (err == nil) != group.accept {
				t.Fatalf("scanner control accept=%t case=%d", group.accept, i)
			}
			if group.accept {
				encoded, e := json.Marshal(json.RawMessage(raw))
				if e != nil || size != len(encoded) {
					t.Fatalf("compact control %d", i)
				}
			}
		}
	}
}

// A new private scanner also carries serialized secrets. Control: every fmt
// verb returns its fixed label; witness: no verb can select its planted data.
func TestSnapshotScannerValueFreeFormatting(t *testing.T) {
	value := jsonScan{data: bytes.Repeat([]byte{128, 9, 171, 127}, 8)}
	for _, input := range []any{value, &value} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
			if fmt.Sprintf(verb, input) != "<e2ee-json-scan>" {
				t.Fatal("scanner fmt witness")
			}
		}
	}
}

// Witness unknown fields, trailing data, invalid bytes and excessive evidence
// nesting still fail the same strict codec gate; whitespace is a valid neighbor.
// A numeric replay-key spelling alias retains the old typed decoder behavior.
func TestSnapshotStrictDecoderRefusals(t *testing.T) {
	_, w, key := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	base := Snapshot{Version: 1, Revision: 1, Engine: state}
	raw, err := legacySnapshotMarshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range [][]byte{raw, append(append([]byte(" \n"), raw...), []byte("\t ")...)} {
		for _, decode := range []func([]byte) (Snapshot, error){legacyDecodeSnapshot, DecodeSnapshot} {
			if _, err = decode(valid); err != nil {
				t.Fatal("strict decoder control", err)
			}
		}
	}
	inputs := [][]byte{
		append([]byte(`{"unknown":0,`), raw[1:]...),
		bytes.Replace(raw, []byte(`"engine":{`), []byte(`"engine":{"unknown":0,`), 1),
		append(append([]byte(nil), raw...), []byte(" null")...),
		append([]byte{'"', 0xff, '"'}, raw...),
	}
	digest := TupleHash("depth")
	p := Prepared{Generation: strings.Repeat("a", 32), Since: Cursor{Token: "before"}, Next: Cursor{Token: "after", Revision: 1, Batch: digest}, Digest: digest, Evidence: json.RawMessage(strings.Repeat("[", 31) + "0" + strings.Repeat("]", 31))}
	deep := base
	deep.Prepared = &p
	encoded, _ := json.Marshal(deep)
	inputs = append(inputs, encoded)
	for i, bad := range inputs {
		if _, err = legacyDecodeSnapshot(bad); err != ErrCodec {
			t.Fatalf("reference strict witness %d", i)
		}
		if _, err = DecodeSnapshot(bad); err != ErrCodec {
			t.Fatalf("new strict witness %d", i)
		}
	}
	entry := ReplayEntry{Event: "$alias", Digest: digest}
	base.Replay = map[string]ReplayWindow{"session": {Entries: map[uint64]ReplayEntry{1: entry}}}
	raw, _ = json.Marshal(base)
	alias := bytes.Replace(raw, []byte(`"1":`), []byte(`"01":`), 1)
	expected, err := legacyDecodeSnapshot(alias)
	if err != nil {
		t.Fatal("alias reference", err)
	}
	actual, err := DecodeSnapshot(alias)
	if err != nil {
		t.Fatal("alias decoder", err)
	}
	a, _ := json.Marshal(expected)
	b, _ := json.Marshal(actual)
	if !bytes.Equal(a, b) {
		t.Fatal("alias semantics changed")
	}
}

// Typed inventory must cover every registration the previous generic observer
// made, including decoded pickles, ratchet byte arrays and opaque evidence.
// Control: a valid typed load scrubs planted secrets and preserves public text.
// Witness: rejected snapshot versions still inventory their secret-bearing input
// via the error path, using a fresh inventory so the writer cannot mask a gap.
func TestSnapshotInventoryCoverage(t *testing.T) {
	_, lease, _, p, _ := fixture(t)
	key := bytes.Repeat([]byte{0x42}, 32)
	secret := bytes.Repeat([]byte{128, 9, 171, 127}, 8)
	_, w, _ := working(t)
	state, err := w.Checkpoint(key)
	if err != nil {
		t.Fatal(err)
	}
	state.Olm = map[string][]byte{"pickle": secret}
	room := &RoomRetention{}
	copy(room.Refusals[:], secret)
	digest := TupleHash("inventory")
	snapshot := Snapshot{Version: 1, Revision: 1, Engine: state, Retention: Retention{Rooms: map[string]*RoomRetention{TupleHash("room"): room}}, Prepared: &Prepared{Generation: p.Binding.Generation, Since: Cursor{Token: "since"}, Next: Cursor{Token: "next", Revision: 1, Batch: digest}, Digest: digest, Evidence: json.RawMessage(`{"bytes":` + numericBytes(secret) + `}`)}}
	raw, err := snapshot.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	reference := new(Inventory)
	reference.ObserveJSON(raw)
	direct := new(Inventory)
	direct.observeSnapshot(snapshot, raw)
	for hash := range reference.seen {
		if !direct.seen[hash] {
			t.Fatal("typed inventory omitted registration")
		}
	}
	writer, _ := NewCodec(key, new(Inventory))
	if err = lease.SaveSnapshot(writer, p.Binding, snapshot, true); err != nil {
		t.Fatal(err)
	}
	for _, rejected := range []bool{false, true} {
		input := raw
		if rejected {
			input = bytes.Replace(raw, []byte(`"v":1`), []byte(`"v":9`), 1)
			if err = lease.WriteSealed(writer, p.Binding, input, false); err != nil {
				t.Fatal(err)
			}
		}
		inv := new(Inventory)
		codec, _ := NewCodec(key, inv)
		_, err = lease.LoadSnapshot(codec, p.Binding)
		if (!rejected && err != nil) || (rejected && err != ErrCodec) {
			t.Fatal("inventory load outcome", err)
		}
		reference := new(Inventory)
		reference.ObserveJSON(input)
		for hash := range reference.seen {
			if !inv.seen[hash] {
				t.Fatal("load inventory omitted registration")
			}
		}
		if strings.Contains(inv.Scrub(numericBytes(secret)), numericBytes(secret)) || inv.Scrub("public diagnostic") != "public diagnostic" {
			t.Fatal("inventory scrub witness/control")
		}
	}
}

// Differential seeds exercise both acceptance and rejection. CI may fuzz this
// bounded scanner against r3's independent token-based strict reader; neither
// outcome alone is a sufficient oracle, and accepted compact sizes must agree.
func FuzzSnapshotJSONScannerCompatibility(f *testing.F) {
	for _, seed := range []string{`{"a":1}`, `{"a":1,"\u0061":2}`, `["<>&",1e9999]`, `"\ud800"`, string([]byte{'"', 0xff, '"'}), `[0,]`} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 64<<10 {
			t.Skip("bounded scanner corpus")
		}
		var reference json.RawMessage
		old := strictJSON(raw, &reference, 64<<10)
		size, err := scanJSON(raw, 0)
		if (old == nil) != (err == nil) {
			t.Fatal("strict scanner differential")
		}
		if err == nil {
			encoded, e := json.Marshal(json.RawMessage(raw))
			if e != nil || len(encoded) != size {
				t.Fatal("compact size differential")
			}
		}
	})
}
