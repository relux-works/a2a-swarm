package devicelist

import (
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strings"
	"testing"
	"time"
)

// exampleList is the example of LIP-0029 section 3.2 without its display line
// breaks, and exampleHash the h(D) that the section states for it.
const (
	exampleList = `{"schema":"swarma-device-list/1","network":"main","owner":"id/alisa","seq":5,"devices":[` +
		`{"label":"phone P","alg":"p256","key":"036b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296","presence":true,"ceiling":2,"seal":{"txid":"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1","vout":0}},` +
		`{"label":"laptop M","alg":"bip340","key":"79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798","presence":false,"ceiling":1,"seal":{"txid":"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1","vout":1}},` +
		`{"label":"Element on P","alg":"matrix-ed25519","key":"d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a","presence":false,"ceiling":1,"seal":{"txid":"a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1","vout":2}}]}`
	exampleHash = "b726e9dd98d9ff7fa6cb98541f4473dcb521d6c872ac17b9ebdcc0c52372ae79"
)

// Public test keys only: the generator points and RFC 8032 test keys 1 to 3.
// No device may use them.
const (
	keyP256G = "036b17d1f2e12c4247f8bce6e563a440f277037d812deb33a0f4a13945d898c296"
	keySecpG = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	keyEdE   = "d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a"
	keyEd2   = "3d4017c3e843895a92b70aa74d1b7ebc9c982ccf2ec4968cc0cd55f12af4660c"
	keyEd3   = "fc51cd8e6218a1a38da47ed00230f0580816ed13ba3303ac5deb911548908025"

	testRoom  = "!owner:example.org"
	writtenAt = uint64(1700000000)
)

var (
	sealTxID = strings.Repeat("a1", 32)
	sealE    = Outpoint{TxID: sealTxID, Vout: 2}
	// testTip is the tip of every fixture snapshot; its block is 600 s older
	// than the write time. testOp is the name operation that carries the pin,
	// 45 blocks below the tip.
	testTip = Tip{Hash: strings.Repeat("0b", 32), Height: 812345, Time: writtenAt - 600}
	testOp  = NameOp{TxID: strings.Repeat("c3", 32), Height: 812300, BlockHash: strings.Repeat("0d", 32)}
	// now is 30 s after the write, and at is the check of a host that has
	// only the snapshot file: a trusted clock and no independent live tip, so
	// the label of a young fixture snapshot is unknown.
	now = time.Unix(int64(writtenAt)+30, 0)
	at  = Check{Now: now, ClockTrusted: true}
)

// opAt is testOp moved to another height, in another block.
func opAt(height uint64) NameOp {
	return NameOp{TxID: strings.Repeat("c4", 32), Height: height, BlockHash: strings.Repeat("0e", 32)}
}

func rec(label, alg, key string, presence bool, ceiling int, vout uint32) wireRecord {
	return wireRecord{Label: label, Alg: alg, Key: key, Presence: presence, Ceiling: ceiling, Seal: wireSeal{TxID: sealTxID, Vout: vout}}
}

// listOctets encodes a list of id/alisa on main with the records sorted by
// key, as section 3.2 orders them.
func listOctets(seq uint64, records ...wireRecord) []byte {
	sorted := append([]wireRecord(nil), records...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	return encodeList(wireList{Schema: Schema, Network: "main", Owner: "id/alisa", Seq: seq, Devices: sorted})
}

func mustParse(t testing.TB, octets []byte) *List {
	t.Helper()
	list, err := Parse(octets)
	if err != nil {
		t.Fatal("fixture list does not parse:", err)
	}
	return list
}

// replaced applies one textual change to the example and fails the test when
// the text to change is absent, so that a mutation cannot silently be a no-op.
func replaced(t testing.TB, old, new string) []byte {
	t.Helper()
	if !strings.Contains(exampleList, old) {
		t.Fatalf("mutation target %q is not in the example", old)
	}
	return []byte(strings.Replace(exampleList, old, new, 1))
}

func wireKey(t testing.TB, hexKey string) string {
	t.Helper()
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawStdEncoding.EncodeToString(raw)
}

func pinOf(list *List) Pin { return Pin{Seq: list.Seq(), SHA256: list.Hash()} }

func nameValue(pin Pin) []byte {
	return []byte(`{"swarma":{},"swarma-devices":` + string(encodePin(pin)) + `}`)
}

// snapshotFor is the content of a snapshot file that watches one name, owner,
// with the given value, and the seal of every device of the list: unspent,
// except the ones named as absent.
func snapshotFor(owner string, value []byte, list *List, absent ...Outpoint) SnapshotData {
	data := SnapshotData{
		Consistency: ConsistencyBracketed,
		Network:     "main",
		Names:       []NameRecord{{Name: owner, Value: value, Op: testOp}},
		Tip:         testTip,
		WrittenAt:   writtenAt,
	}
	for _, device := range list.Devices() {
		record := SealRecord{Outpoint: device.Seal, State: SealUnspent, Confirmations: 1, ScriptPubKey: "51"}
		for _, a := range absent {
			if a == device.Seal {
				record.State = SealNotInUTXOSet
				record.Confirmations, record.ScriptPubKey = 0, ""
			}
		}
		data.Seals = append(data.Seals, record)
	}
	sort.Slice(data.Seals, func(i, j int) bool {
		a, b := data.Seals[i].Outpoint, data.Seals[j].Outpoint
		return a.TxID < b.TxID || (a.TxID == b.TxID && a.Vout < b.Vout)
	})
	return data
}

// snapshotData is snapshotFor the owner of the list.
func snapshotData(value []byte, list *List, absent ...Outpoint) SnapshotData {
	return snapshotFor(list.Owner(), value, list, absent...)
}

// configOf is the host configuration whose watch set is exactly what the
// data holds, serving the first name of the data.
func configOf(data SnapshotData) SnapshotConfig {
	config := SnapshotConfig{Path: "/var/lib/chain-snapshot/snapshot.json", Network: data.Network, Owner: data.Names[0].Name}
	for _, name := range data.Names {
		config.Names = append(config.Names, name.Name)
	}
	for _, seal := range data.Seals {
		config.Seals = append(config.Seals, seal.Outpoint)
	}
	return config
}

// viewOf makes the view through the one parser: the data is encoded as a
// snapshot file and read back under its own watch set.
func viewOf(t testing.TB, data SnapshotData) *Snapshot {
	t.Helper()
	view, err := newSnapshot(encodeSnapshotFile(data), configOf(data))
	if err != nil {
		t.Fatal("fixture snapshot:", err)
	}
	return view
}

func snapshotOf(t testing.TB, list *List, absent ...Outpoint) *Snapshot {
	t.Helper()
	return viewOf(t, snapshotData(nameValue(pinOf(list)), list, absent...))
}

func mustCache(t testing.TB, max int) *Cache {
	t.Helper()
	cache, err := NewCache(max)
	if err != nil {
		t.Fatal("fixture cache:", err)
	}
	return cache
}

// fixedLists answers every lookup with one list, whatever hash is asked: a
// cache that lies about what it holds under a hash.
type fixedLists struct{ list *List }

func (f fixedLists) Get(string) (*List, bool) { return f.list, f.list != nil }

// tampered returns a List with the octets of list and edited records. Parse
// can never return such a value; controls use it to reach the checks of Admit
// that the codec already makes.
func tampered(list *List, edit func([]Record)) *List {
	copied := *list
	copied.devices = list.Devices()
	edit(copied.devices)
	return &copied
}

// scenario is one call of Admit. newScenario is the valid control of every
// negative: Alisa's Matrix device E, of the example list, sends an encrypted
// event to her owner room, the list is cached under the pinned hash, and the
// seal of E is unspent (S29-17).
type scenario struct {
	event   Event
	room    string
	at      Check
	lists   Lists
	chain   ChainView
	journal *ObserveJournal
	list    *List
}

func newScenario(t testing.TB) *scenario {
	t.Helper()
	list := mustParse(t, []byte(exampleList))
	cache := mustCache(t, 0)
	if _, err := cache.Add([]byte(exampleList), pinOf(list)); err != nil {
		t.Fatal("fixture cache:", err)
	}
	return &scenario{
		event:   Event{OriginatorEd25519: wireKey(t, keyEdE), SessionID: "session-1", Index: 0, Room: testRoom, KeySource: KeySourceDirectOlm},
		room:    testRoom,
		at:      at,
		lists:   cache,
		chain:   snapshotOf(t, list),
		journal: NewObserveJournal(),
		list:    list,
	}
}

// use replaces the list of the scenario: cached under its hash and pinned by
// a new snapshot.
func (s *scenario) use(t testing.TB, octets []byte, absent ...Outpoint) {
	t.Helper()
	s.list = mustParse(t, octets)
	cache := mustCache(t, 0)
	if _, err := cache.Add(octets, pinOf(s.list)); err != nil {
		t.Fatal("fixture cache:", err)
	}
	s.lists, s.chain = cache, snapshotOf(t, s.list, absent...)
}

// useAt is use with the pin carried by the given name operation.
func (s *scenario) useAt(t testing.TB, octets []byte, op NameOp) {
	t.Helper()
	s.use(t, octets)
	s.edit(t, func(d *SnapshotData) { d.Names[0].Op = op })
}

// edit replaces the view by a snapshot of the scenario's list with one
// change to its content.
func (s *scenario) edit(t testing.TB, change func(*SnapshotData)) {
	t.Helper()
	data := snapshotData(nameValue(pinOf(s.list)), s.list)
	change(&data)
	s.chain = viewOf(t, data)
}

// next gives the event a message index that no earlier admission used.
func (s *scenario) next() { s.event.Index++ }

func (s *scenario) admit() (Result, error) {
	return Admit(s.event, s.room, s.at, s.lists, s.chain, s.journal)
}
