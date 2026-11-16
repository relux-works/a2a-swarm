package devicelist

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Claim: combining labels never improves either input, following invalid > stale > unknown > fresh.
func TestLabelOrder(t *testing.T) {
	labels := []Freshness{LabelFresh, LabelUnknown, LabelStale, LabelInvalid}
	for i, a := range labels {
		for k, b := range labels {
			want := a
			if k > i {
				want = b
			}
			if got := worseLabel(a, b); got != want {
				t.Fatalf("%s + %s = %s, want %s", a, b, got, want)
			}
		}
	}
	if worseLabel(Freshness("unrecognized"), LabelFresh) != LabelInvalid {
		t.Fatal("unknown label improved")
	}
}

// Claim: no produced LIP-0029 code is local and exactly the two local refusal classes are marked local.
func TestCodeLocalClasses(t *testing.T) {
	for _, c := range []Code{DeviceListInvalid, DeviceListMismatch, DeviceListRollback, DevicePinInvalid, ListUnknown, DeviceUnknown, DeviceAlgMismatch, DeviceAlgUnsupported, DeviceRevoked, MessengerUnauthenticated, NotForMe, Replay} {
		if c.IsLocal() {
			t.Fatal("specification code marked local:", c)
		}
	}
	for _, c := range []Code{ChainViewUnavailable, JournalFull} {
		if !c.IsLocal() {
			t.Fatal("local class not marked local:", c)
		}
	}
}

// Claim: an older observing tip cannot close or lower the head, while a non-older absence and reorganisation can.
func TestOlderObservationDoesNotMutateJournal(t *testing.T) {
	for _, older := range []bool{false, true} {
		t.Run(map[bool]string{false: "non-older control", true: "older observation"}[older], func(t *testing.T) {
			s := newScenario(t)
			s.useAt(t, replaced(t, `"seq":5`, `"seq":6`), opAt(812310))
			s.edit(t, func(d *SnapshotData) { d.Names[0].Op = opAt(812310); d.Tip.Height = 812350 })
			wantAdmitted(t, s)
			head, _ := s.journal.Head("main", "id/alisa")
			observed := s.journal.owners[journalOwner{"main", "id/alisa"}].seenUnspent[sealE]
			if observed.Height != 812350 || observed.Hash != testTip.Hash {
				t.Fatalf("observing tip not journaled %+v", observed)
			}
			s.next()
			s.use(t, []byte(exampleList), sealE)
			if !older {
				s.edit(t, func(d *SnapshotData) {
					d.Tip.Height = 812351
					d.Seals[2] = SealRecord{Outpoint: sealE, State: SealNotInUTXOSet}
				})
			}
			wantRefused(t, s, DeviceRevoked, 8)
			if isClosed(s, keyEdE, sealE) == older {
				t.Fatal("closure ordering incorrect")
			}
			got, _ := s.journal.Head("main", "id/alisa")
			if older {
				if got != head {
					t.Fatalf("older observation lowered head: %+v", got)
				}
			} else if got.Seq != 5 || got.ObservedAt.Height != 812351 {
				t.Fatalf("newer reorganisation control did not take head: %+v", got)
			}
		})
	}
}

// Claim: stale views write no owners, replay keys, heads, seen-unspent records or closures, beside young-view controls.
func TestStaleObservationWritesNothing(t *testing.T) {
	s := newScenario(t)
	s.at.Now = now.Add(time.Minute)
	wantAdmitted(t, s)
	if len(s.journal.owners) != 0 || s.journal.replayCount != 0 {
		t.Fatal("stale admission wrote journal state")
	}
	s.at = at
	wantAdmitted(t, s)
	state := s.journal.owners[journalOwner{"main", "id/alisa"}]
	head := state.head
	seen := map[Outpoint]Tip{}
	for seal, tip := range state.seenUnspent {
		seen[seal] = tip
	}
	replays := s.journal.replayCount
	s.next()
	s.at.Now = now.Add(time.Minute)
	s.chain = snapshotOf(t, s.list, sealE)
	r := wantRefused(t, s, DeviceRevoked, 8)
	if r.Label != LabelStale || isClosed(s, keyEdE, sealE) || state.head != head || !reflect.DeepEqual(state.seenUnspent, seen) || s.journal.replayCount != replays {
		t.Fatal("stale absence mutated journal")
	}
	s.chain = snapshotOf(t, s.list)
	wantAdmitted(t, s)
	if state.head != head || s.journal.replayCount != replays || !reflect.DeepEqual(state.seenUnspent, seen) {
		t.Fatal("stale unspent mutated journal")
	}
	s.at = at
	s.chain = snapshotOf(t, s.list, sealE)
	wantRefused(t, s, DeviceRevoked, 8)
	if !isClosed(s, keyEdE, sealE) {
		t.Fatal("young absence control did not close")
	}
}

// Claim: a newer seen-unspent observation independently guards closure when the name head is absent.
func TestSeenObservationOrdersClosure(t *testing.T) {
	s := newScenario(t)
	wantAdmitted(t, s)
	state := s.journal.owners[journalOwner{"main", "id/alisa"}]
	state.hasHead = false
	tip := state.seenUnspent[sealE]
	tip.Height++
	state.seenUnspent[sealE] = tip
	s.next()
	s.chain = snapshotOf(t, s.list, sealE)
	wantRefused(t, s, DeviceRevoked, 8)
	if isClosed(s, keyEdE, sealE) {
		t.Fatal("older absence closed")
	}
	state.hasHead = false
	s.edit(t, func(d *SnapshotData) {
		d.Tip.Height = tip.Height
		d.Seals[2] = SealRecord{Outpoint: sealE, State: SealNotInUTXOSet}
	})
	wantRefused(t, s, DeviceRevoked, 8)
	if !isClosed(s, keyEdE, sealE) {
		t.Fatal("equal-tip absence control did not close")
	}
}

// Claim: an unspent seal observed through another device's admission closes on absence at its own first event.
func TestAllPinnedSealsAreObserved(t *testing.T) {
	s := threeDevices(t)
	seal2 := Outpoint{TxID: sealTxID, Vout: 3}
	wantAdmitted(t, s)
	if !s.journal.SeenUnspent("main", "id/alisa", seal2) {
		t.Fatal("other device seal not observed")
	}
	s.chain = snapshotOf(t, s.list, seal2)
	wantRefused(t, s.as(t, keyEd2), DeviceRevoked, 8)
	if !isClosed(s, keyEd2, seal2) {
		t.Fatal("first-event absence not closed")
	}
	control := threeDevices(t)
	control.chain = snapshotOf(t, control.list, seal2)
	wantRefused(t, control.as(t, keyEd2), DeviceRevoked, 8)
	if isClosed(control, keyEd2, seal2) {
		t.Fatal("never-seen control closed")
	}
	control.chain = snapshotOf(t, control.list)
	wantAdmitted(t, control)
}

// Claim: transient seal states refuse only the command without closing or setting seen-unspent, and later unspent admits.
func TestTransientSealNeverCloses(t *testing.T) {
	for _, state := range []SealState{SealUnconfirmed, SealSpendPending} {
		for _, primed := range []bool{false, true} {
			s := newScenario(t)
			if primed {
				wantAdmitted(t, s)
				s.next()
			}
			s.edit(t, func(d *SnapshotData) {
				d.Seals[2].State = state
				if state == SealUnconfirmed {
					d.Seals[2].Confirmations = 0
				}
			})
			r := wantRefused(t, s, DeviceRevoked, 8)
			if r.SealState != state || isClosed(s, keyEdE, sealE) || s.journal.SeenUnspent("main", "id/alisa", sealE) != primed {
				t.Fatalf("transient state journaled: %+v", r)
			}
			s.chain = snapshotOf(t, s.list)
			wantAdmitted(t, s)
		}
	}
}

func maxSnapshotPayload(t testing.TB) []byte {
	t.Helper()
	list := mustParse(t, []byte(exampleList))
	data := snapshotData(nameValue(pinOf(list)), list)
	data.Seals = append(data.Seals, SealRecord{Outpoint: Outpoint{TxID: sealTxID, Vout: 3}, State: SealUnspent, Confirmations: 1})
	for i := range data.Seals {
		data.Seals[i].ScriptPubKey = ""
	}
	needed := MaxSnapshotBytes - len(encodeSnapshotFile(data))
	if needed%2 != 0 {
		data.Names[0].Vout = 10
		needed--
	}
	for i := range data.Seals {
		n := needed
		if n > 20000 {
			n = 20000
		}
		data.Seals[i].ScriptPubKey += strings.Repeat("0", n)
		needed -= n
	}
	raw := encodeSnapshotFile(data)
	if needed != 0 || len(raw) != MaxSnapshotBytes {
		t.Fatalf("fixture size %d, remaining %d", len(raw), needed)
	}
	return raw
}

// Claim: present-state metadata, per-state confirmation counts and script bounds are validated beside each state's valid control.
func TestSealRecordSchema(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	base := snapshotData(nameValue(pinOf(list)), list)
	base.Seals = base.Seals[2:]
	for _, state := range []SealState{SealUnspent, SealUnconfirmed, SealSpendPending, SealNotInUTXOSet} {
		data := base
		data.Seals = append([]SealRecord(nil), base.Seals...)
		data.Seals[0].State = state
		if state == SealUnconfirmed || state == SealNotInUTXOSet {
			data.Seals[0].Confirmations = 0
		}
		if state == SealNotInUTXOSet {
			data.Seals[0].ScriptPubKey = ""
		}
		raw := string(encodeSnapshotFile(data))
		if _, err := parseSnapshotFile([]byte(raw)); err != nil {
			t.Fatal("state control:", state, err)
		}
		var bad []string
		if state == SealNotInUTXOSet {
			bad = []string{strings.Replace(raw, `"state":`, `"confirmations":0,"state":`, 1), strings.Replace(raw, `"state":`, `"script_pubkey":"","state":`, 1)}
		} else {
			count := `"confirmations":1`
			wrong := `"confirmations":0`
			if state == SealUnconfirmed {
				count, wrong = wrong, count
			}
			bad = []string{strings.Replace(raw, count+",", "", 1), strings.Replace(raw, count, wrong, 1), strings.Replace(raw, count, `"confirmations":812347`, 1), strings.Replace(raw, `"script_pubkey":"51",`, "", 1), strings.Replace(raw, `"script_pubkey":"51"`, `"script_pubkey":"AA"`, 1), strings.Replace(raw, `"script_pubkey":"51"`, `"script_pubkey":"a"`, 1), strings.Replace(raw, `"script_pubkey":"51"`, `"script_pubkey":"`+strings.Repeat("a", 20002)+`"`, 1)}
		}
		for _, b := range bad {
			if b == raw {
				t.Fatal("mutation did not change bytes")
			}
			if _, err := parseSnapshotFile([]byte(b)); err != ErrSnapshotInvalid {
				t.Fatal("invalid state metadata accepted:", state, err)
			}
		}
	}
	raw := maxSnapshotPayload(t)
	if _, err := parseSnapshotFile(raw); err != nil {
		t.Fatal("exact size control:", err)
	}
	if _, err := parseSnapshotFile(append(raw, ' ')); err != ErrCapacity {
		t.Fatal(err)
	}
}

// Claim: a tip exactly 7200 seconds ahead is allowed but one second further ahead is invalid, independently of file age.
func TestFutureTipTime(t *testing.T) {
	for _, ahead := range []uint64{7200, 7201} {
		s := newScenario(t)
		s.edit(t, func(d *SnapshotData) { d.WrittenAt = uint64(now.Unix()); d.Tip.Time = uint64(now.Unix()) + ahead })
		tip := testTip
		tip.Time = uint64(now.Unix()) + ahead
		s.at = Check{Now: now, ClockTrusted: true, LiveTip: &tip}
		want := LabelFresh
		if ahead > 7200 {
			want = LabelInvalid
		}
		if got := s.chain.Freshness(s.at); got != want {
			t.Fatalf("ahead %d: %s", ahead, got)
		}
	}
}

// Claim: a transient-closes source plant is detected by the ordinary transient-state test's closure assertions.
func TestTransientPlantControl(t *testing.T) {
	s := newScenario(t)
	wantAdmitted(t, s)
	s.next()
	s.edit(t, func(d *SnapshotData) { d.Seals[2].State = SealSpendPending })
	wantRefused(t, s, DeviceRevoked, 8)
	if isClosed(s, keyEdE, sealE) {
		t.Fatal("a transient state closed")
	}
	s.chain = snapshotOf(t, s.list)
	wantAdmitted(t, s)
}
