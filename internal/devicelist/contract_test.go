package devicelist

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func readContractFixture(t testing.TB, name string, out any) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name + "-62a2fcf7.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatal(err)
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
		Admit, Closed bool
		SeenUnspent   bool `json:"seen_unspent"`
	}
}

// Claim: all 21 pinned label vectors agree with the parser, protected bindings and real snapshot label function.
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
			view, err := newSnapshot([]byte(v.Snapshot), config)
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

// Claim: every pinned snapshot schema and size vector drives the actual parser or its independent byte-bound check.
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
	count := 0
	for _, v := range vectors {
		if v.Kind != "snapshot" && v.Kind != "snapshot_size" {
			continue
		}
		count++
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
			accepted := len(raw) <= MaxSnapshotBytes
			if v.Kind == "snapshot" {
				_, err := parseSnapshotFile(raw)
				accepted = err == nil
			}
			if accepted != v.Valid {
				t.Fatalf("vector mismatch: accepted %v, want %v", accepted, v.Valid)
			}
			if _, err := parseSnapshotFile(control); err != nil {
				t.Fatal("control after:", err)
			}
		})
	}
	t.Logf("checked %d snapshot/size vectors", count)
}

// Claim: all 29 pinned seal histories agree with Admit's step-8 verdict, closure and seen-unspent state at every step.
func TestSharedSealHistoryVectors(t *testing.T) {
	var vectors []contractObservation
	readContractFixture(t, "observations", &vectors)
	count := 0
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
				data.Seals[0] = SealRecord{Outpoint: seal, State: state, Confirmations: confirmations, ScriptPubKey: script}
				return viewOf(t, data)
			}
			tip := Tip{Hash: strings.Repeat("1", 64), Height: 100, Time: 1699999990}
			s := newScenario(t)
			s.list = list
			s.lists = fixedLists{list}
			s.chain = makeView(SealUnspent, 1, "51", tip)
			wantAdmitted(t, s)
			s.journal = NewObserveJournal() // Independent admitted control does not prime the vector.
			for i, entry := range v.History {
				s.next()
				if entry.OK {
					if entry.Result.TxID != seal.TxID || entry.Result.Vout != seal.Vout {
						t.Fatal("fixture outpoint changed")
					}
					s.chain = makeView(entry.Result.State, entry.Result.Confirmations, entry.Result.ScriptPubKey, entry.Tip)
				} else {
					s.chain = failingSeal{makeView(SealUnspent, 1, "51", tip)}
				}
				result, err := s.admit()
				want := v.Decisions[i]
				if entry.OK && err != nil {
					t.Fatal(err)
				}
				if !entry.OK && err != ErrChainView {
					t.Fatalf("unavailable error: %v", err)
				}
				if result.Admitted() != want.Admit || s.journal.closedFor("regtest", "id/example", keyEdE, seal) != want.Closed || s.journal.SeenUnspent("regtest", "id/example", seal) != want.SeenUnspent {
					t.Fatalf("vector mismatch step %d: %+v %v, want %+v", i, result, err, want)
				}
				if entry.OK && !want.Admit && (result.Code != DeviceRevoked || result.Step != 8) {
					t.Fatalf("wrong refusal: %+v", result)
				}
			}
		})
	}
	if count != 29 {
		t.Fatalf("seal history vector count %d", count)
	}
}
