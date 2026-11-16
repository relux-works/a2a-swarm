package devicelist

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

var contractFixtureDigests = map[string]string{
	"observations": "fe41ee76c570d1134d07771c8ee95287c5f56e49f6ccc662ffdb55506ec0bd25",
	"vectors":      "229ac2993422836363ba6d450868bffef8c4081ff6f71209ffaf2a0f10ee508d",
}

func readContractBytes(t testing.TB, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + "-d1cab6f8.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func fixtureDigest(raw []byte) string {
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func readContractFixture(t testing.TB, name string, out any) {
	t.Helper()
	raw := readContractBytes(t, name)
	if fixtureDigest(raw) != contractFixtureDigests[name] {
		t.Fatal("fixture digest mismatch:", name)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
	}
}

// Claim: both vendored fixtures have the pinned SHA-256, and a one-byte change fails that comparison.
func TestContractFixtureDigests(t *testing.T) {
	for name, want := range contractFixtureDigests {
		raw := readContractBytes(t, name)
		if got := fixtureDigest(raw); got != want {
			t.Fatalf("%s digest %s, want %s", name, got, want)
		}
		raw[0] ^= 1
		if fixtureDigest(raw) == want {
			t.Fatal("modified bytes matched pinned digest:", name)
		}
	}
}

type contractObservation struct {
	Name, Kind string
	Snapshot   string
	Now        int64
	Clock      bool
	KnownTip   *Tip `json:"known_tip"`
	Label      Freshness
	Config     SnapshotConfig
	History    []struct {
		OK     bool
		Tip    Tip
		Result struct {
			State         SealState
			TxID          string
			Vout          uint32
			Confirmations uint64
			ScriptPubKey  string `json:"script_pubkey"`
		}
	}
	Decisions []struct {
		RefuseCommand     bool `json:"refuse_command"`
		ObserveClosed     bool `json:"observe_closed"`
		EnforcementClosed bool `json:"enforcement_closed"`
		SeenUnspent       bool `json:"seen_unspent"`
	}
}

// Claim: all 21 pinned label vectors agree with the real snapshot label and their expected construction error class.
func TestSharedLabelVectors(t *testing.T) {
	var vectors []contractObservation
	readContractFixture(t, "observations", &vectors)
	count := 0
	for _, v := range vectors {
		if v.Kind != "label" {
			continue
		}
		count++
		t.Run(v.Name, func(t *testing.T) {
			control := newScenario(t)
			if control.chain.Freshness(control.at) != LabelUnknown {
				t.Fatal("label control")
			}
			config := v.Config
			config.Path = "/snapshot.json"
			config.Owner = config.Names[0]
			var wantError error
			switch v.Name {
			case "label_missing_seal":
				// This vector has an empty configured seal set, so configuration
				// validation precedes the watch-set comparison.
				wantError = ErrInvalid
			case "label_consistency_witnessed", "label_consistency_other", "label_consistency_None",
				"label_foreign_network", "label_foreign_owner", "label_extra_seal",
				"label_binding_before_unknown_clock", "label_invalid_schema_before_clock":
				wantError = ErrSnapshotInvalid
			}
			view, err := newSnapshot([]byte(v.Snapshot), config)
			if err != wantError || (err != nil && view != nil) {
				t.Fatalf("construction error %v, want %v", err, wantError)
			}
			label := LabelInvalid
			if err == nil {
				label = view.Freshness(Check{Now: time.Unix(v.Now, 0), ClockTrusted: v.Clock, LiveTip: v.KnownTip})
			}
			if label != v.Label {
				t.Fatalf("vector mismatch: got %s (%v), want %s", label, err, v.Label)
			}
		})
	}
	if count != 21 {
		t.Fatalf("label vector count %d", count)
	}
}

// Claim: exactly 45 schema and 3 size vectors drive parseSnapshotFile, with size acceptance distinguished from schema validity.
func TestSharedSnapshotVectors(t *testing.T) {
	var vectors []struct {
		Name, Kind, Wire, WireB64 string
		Valid                     bool
	}
	readContractFixture(t, "vectors", &vectors)
	var control []byte
	for _, v := range vectors {
		if v.Name == "snapshot_valid" {
			control = []byte(v.Wire)
		}
	}
	if len(control) == 0 {
		t.Fatal("missing shared control")
	}
	counts := map[string]int{}
	for _, v := range vectors {
		if v.Kind != "snapshot" && v.Kind != "snapshot_size" {
			continue
		}
		counts[v.Kind]++
		t.Run(v.Name, func(t *testing.T) {
			if _, err := parseSnapshotFile(control); err != nil {
				t.Fatal("control before:", err)
			}
			raw := []byte(v.Wire)
			if v.WireB64 != "" {
				var err error
				raw, err = base64.StdEncoding.DecodeString(v.WireB64)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := parseSnapshotFile(raw)
			accepted := err == nil
			if v.Kind == "snapshot_size" {
				// These payloads are not JSON. The real parser must distinguish
				// bounded-but-invalid schema from byte overflow.
				wantError := ErrCapacity
				if v.Valid {
					wantError = ErrSnapshotInvalid
				}
				if err != wantError {
					t.Fatalf("size error %v, want %v", err, wantError)
				}
				accepted = err != ErrCapacity
			}
			if accepted != v.Valid {
				t.Fatalf("vector mismatch: accepted %v, want %v", accepted, v.Valid)
			}
			if _, err := parseSnapshotFile(control); err != nil {
				t.Fatal("control after:", err)
			}
		})
	}
	if counts["snapshot"] != 45 || counts["snapshot_size"] != 3 {
		t.Fatalf("snapshot/size vector counts: %v", counts)
	}
}

// Claim: all 29 histories (64 steps) match current-read refusal, observe closure and seen state, with every result marked observe-only.
func TestSharedSealHistoryVectors(t *testing.T) {
	var vectors []contractObservation
	readContractFixture(t, "observations", &vectors)
	count, steps := 0, 0
	for _, v := range vectors {
		if v.Kind != "seal_history" {
			continue
		}
		count++
		t.Run(v.Name, func(t *testing.T) {
			if len(v.History) != len(v.Decisions) {
				t.Fatal("history length")
			}
			seal := Outpoint{TxID: strings.Repeat("a", 64), Vout: 2}
			record := rec("device", AlgMatrixEd25519, keyEdE, false, 1, 2)
			record.Seal = wireSeal{TxID: seal.TxID, Vout: seal.Vout}
			list := mustParse(t, encodeList(wireList{Schema: Schema, Network: "regtest", Owner: "id/example", Seq: 5, Devices: []wireRecord{record}}))
			makeView := func(state SealState, confirmations uint64, script string, tip Tip) *Snapshot {
				data := snapshotData(nameValue(pinOf(list)), list)
				data.Network = "regtest"
				data.Names[0].Op = opAt(90)
				data.Tip = tip
				data.Seals[0] = snapshotSealRecord{Outpoint: seal, State: state, Confirmations: confirmations, ScriptPubKey: script}
				return viewOf(t, data)
			}
			tip := Tip{Hash: strings.Repeat("1", 64), Height: 100, Time: 1699999990}
			s := newScenario(t)
			s.list = list
			s.lists = fixedLists{list}
			s.chain = makeView(SealUnspent, 1, "51", tip)
			wantObservedInstruct(t, s)
			s.journal = NewObserveJournal() // Independent instruct control does not prime the vector.
			for i, entry := range v.History {
				steps++
				s.next()
				if entry.OK {
					if entry.Result.TxID != seal.TxID || entry.Result.Vout != seal.Vout {
						t.Fatal("fixture outpoint changed")
					}
					s.chain = makeView(entry.Result.State, entry.Result.Confirmations, entry.Result.ScriptPubKey, entry.Tip)
				} else {
					s.chain = failingSeal{makeView(SealUnspent, 1, "51", tip)}
				}
				// The oracle's refusal flag concerns only the current read, not
				// the sticky observe-journal simulation of the full consumer check.
				current, readErr := s.chain.Seal(seal)
				refuseCommand := readErr != nil || current != SealUnspent
				result, err := s.admit()
				want := v.Decisions[i]
				if entry.OK && err != nil {
					t.Fatal(err)
				}
				if !entry.OK && err != ErrChainView {
					t.Fatalf("unavailable error: %v", err)
				}
				closed := s.journal.closedFor("regtest", "id/example", keyEdE, seal)
				// There is no enforcement state or closure exporter. Even a
				// closure-recording Result must retain the observe-only mark.
				if !result.ObserveOnly || want.EnforcementClosed {
					t.Fatalf("enforcement closure is forbidden: %+v, want %+v", result, want)
				}
				if refuseCommand != want.RefuseCommand || closed != want.ObserveClosed || s.journal.SeenUnspent("regtest", "id/example", seal) != want.SeenUnspent {
					t.Fatalf("vector mismatch step %d: %+v %v, current refusal %v, closed %v, want %+v", i, result, err, refuseCommand, closed, want)
				}
				if result.ObservedInstruct() != (!want.RefuseCommand && !want.ObserveClosed) {
					t.Fatalf("wrong consumer observation: %+v", result)
				}
				if entry.OK && (want.RefuseCommand || want.ObserveClosed) && (result.Code != DeviceRevoked || result.Step != 8) {
					t.Fatalf("wrong observed refusal: %+v", result)
				}
			}
		})
	}
	if count != 29 || steps != 64 {
		t.Fatalf("seal history count %d, steps %d", count, steps)
	}
}
