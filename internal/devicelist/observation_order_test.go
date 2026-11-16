package devicelist

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// Claim: equal-height competing hashes change neither head nor seen tips and cannot close, beside identical-hash controls.
func TestEqualHeightObservationOrdering(t *testing.T) {
	for _, different := range []bool{false, true} {
		t.Run(map[bool]string{false: "same hash control", true: "competing hash"}[different], func(t *testing.T) {
			s := newScenario(t)
			s.useAt(t, replaced(t, `"seq":5`, `"seq":6`), opAt(812310))
			wantObservedInstruct(t, s)
			head, _ := s.journal.Head("main", s.list.Owner())
			state := s.journal.owners[journalOwner{"main", s.list.Owner()}]
			seen := state.seenUnspent[sealE]
			s.next()
			s.use(t, []byte(exampleList))
			if different {
				s.edit(t, func(d *snapshotDataValue) { d.Tip.Hash = strings.Repeat("a", 64) })
			}
			wantObservedInstruct(t, s)
			got, _ := s.journal.Head("main", s.list.Owner())
			if different {
				if got != head || state.seenUnspent[sealE] != seen {
					t.Fatal("unordered view overwrote head or seen tip")
				}
			} else if got.Seq != 5 || got.ObservedAt != testTip {
				t.Fatalf("same-hash control did not take head: %+v", got)
			}
			s.next()
			s.edit(t, func(d *snapshotDataValue) {
				if different {
					d.Tip.Hash = strings.Repeat("a", 64)
				}
				d.Seals[2] = snapshotSealRecord{Outpoint: sealE, State: SealNotInUTXOSet}
			})
			wantRefused(t, s, DeviceRevoked, 8)
			if isClosed(s, keyEdE, sealE) == different {
				t.Fatal("equal-height closure ordering failed")
			}
		})
	}
}

// Claim: a competing hash cannot overwrite a seen tip or close even without a head; the identical hash can close.
func TestSeenTipHashOrdering(t *testing.T) {
	for _, different := range []bool{false, true} {
		s := newScenario(t)
		wantObservedInstruct(t, s)
		state := s.journal.owners[journalOwner{"main", s.list.Owner()}]
		seen := state.seenUnspent[sealE]
		state.hasHead = false
		if different {
			s.edit(t, func(d *snapshotDataValue) { d.Tip.Hash = strings.Repeat("a", 64) })
		}
		s.next()
		wantObservedInstruct(t, s)
		if state.seenUnspent[sealE] != seen {
			t.Fatal("unordered unspent overwrote seen tip")
		}
		state.hasHead = false
		s.next()
		s.edit(t, func(d *snapshotDataValue) {
			if different {
				d.Tip.Hash = strings.Repeat("a", 64)
			}
			d.Seals[2] = snapshotSealRecord{Outpoint: sealE, State: SealNotInUTXOSet}
		})
		wantRefused(t, s, DeviceRevoked, 8)
		if isClosed(s, keyEdE, sealE) == different {
			t.Fatal("seen-tip closure ordering failed")
		}
	}
}

// Claim: replay pairs are retained in both stale-to-young and young-to-stale directions, and a new pair still observes instruct.
func TestReplayAcrossViewAges(t *testing.T) {
	for _, staleFirst := range []bool{true, false} {
		s := newScenario(t)
		if staleFirst {
			s.at.Now = now.Add(time.Minute)
		}
		wantObservedInstruct(t, s)
		wantRefused(t, s, Replay, 11)
		if staleFirst {
			s.at = at
		} else {
			s.at.Now = now.Add(time.Minute)
		}
		wantRefused(t, s, Replay, 11)
		s.next()
		wantObservedInstruct(t, s)
		if s.journal.replayCount != 2 {
			t.Fatal("replay count did not include both instruct observations")
		}
	}
}

// Claim: insufficient whole-list capacity leaves every seen entry and tip unchanged, beside sufficient-capacity controls.
func TestSeenUnspentSetAtomic(t *testing.T) {
	for _, primed := range []bool{false, true} {
		for _, capacity := range []int{2, 3} {
			s := threeDevices(t)
			if primed {
				s.edit(t, func(d *snapshotDataValue) { d.Seals = d.Seals[:1] })
				wantObservedInstruct(t, s)
				s.next()
				s.chain = snapshotOf(t, s.list)
			}
			s.journal.maxSeen = capacity
			before := map[Outpoint]Tip{}
			if state := s.journal.owners[journalOwner{"main", s.list.Owner()}]; state != nil {
				for seal, tip := range state.seenUnspent {
					before[seal] = tip
				}
			}
			s.edit(t, func(d *snapshotDataValue) { d.Tip.Height++ })
			if capacity == 2 {
				wantRefused(t, s, JournalFull, 8)
				state := s.journal.owners[journalOwner{"main", s.list.Owner()}]
				if !state.full || !reflect.DeepEqual(state.seenUnspent, before) {
					t.Fatalf("partial seen-unspent writes: %+v, before %+v", state.seenUnspent, before)
				}
			} else {
				wantObservedInstruct(t, s)
				state := s.journal.owners[journalOwner{"main", s.list.Owner()}]
				if state.full || len(state.seenUnspent) != 3 {
					t.Fatal("sufficient-capacity control failed")
				}
				for _, tip := range state.seenUnspent {
					if tip.Height != testTip.Height+1 {
						t.Fatal("control did not update every seen tip")
					}
				}
			}
		}
	}
}

// Claim: ceiling and replay refusals still journal another list seal's unspent view, which can precede its later observe closure.
func TestRefusedEventStillObservesList(t *testing.T) {
	for _, replay := range []bool{false, true} {
		s := threeDevices(t)
		seal2 := Outpoint{TxID: sealTxID, Vout: 3}
		if replay {
			s.edit(t, func(d *snapshotDataValue) { d.Seals = d.Seals[:1] })
			wantObservedInstruct(t, s)
			if s.journal.SeenUnspent("main", s.list.Owner(), seal2) {
				t.Fatal("partial-view control unexpectedly observed the other seal")
			}
			s.chain = snapshotOf(t, s.list)
			wantRefused(t, s, Replay, 11)
		} else {
			wantObservedInstruct(t, threeDevices(t))
			s.lists = fixedLists{tampered(s.list, func(records []Record) {
				for i := range records {
					if records[i].Key == keyEdE {
						records[i].Ceiling = 2
					}
				}
			})}
			wantRefused(t, s, DeviceListInvalid, 9)
			s.lists = fixedLists{s.list}
		}
		if !s.journal.SeenUnspent("main", s.list.Owner(), seal2) {
			t.Fatal("later event refusal lost view evidence")
		}
		s.chain = snapshotOf(t, s.list, seal2)
		wantRefused(t, s.as(t, keyEd2), DeviceRevoked, 8)
		if !isClosed(s, keyEd2, seal2) {
			t.Fatal("refused-event observation did not precede closure")
		}
		control := threeDevices(t)
		control.chain = snapshotOf(t, control.list, seal2)
		wantRefused(t, control.as(t, keyEd2), DeviceRevoked, 8)
		if isClosed(control, keyEd2, seal2) {
			t.Fatal("never-seen absence control closed")
		}
		control.chain = snapshotOf(t, control.list)
		wantObservedInstruct(t, control)
	}
}
