package devicelist

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

func wantAdmitted(t testing.TB, s *scenario) Result {
	t.Helper()
	r, err := s.admit()
	if err != nil || !r.Admitted() {
		t.Fatalf("valid control not admitted: %+v %v", r, err)
	}
	return r
}
func wantRefused(t testing.TB, s *scenario, code Code, step int) Result {
	t.Helper()
	r, err := s.admit()
	if err != nil || r.Admitted() || r.Outcome != OutcomeRefused || r.Code != code || r.Step != step || r.Level != 0 || r.Action != "" {
		t.Fatalf("want %s at step %d, got %+v %v", code, step, r, err)
	}
	return r
}
func isClosed(s *scenario, key string, seal Outpoint) bool {
	return s.journal.closedFor("main", s.list.Owner(), key, seal)
}

// Claim: a valid messenger event yields an L1 instruct observation naming its pin, tip and seal state.
func TestAdmitL1Instruct(t *testing.T) {
	s := newScenario(t)
	r := wantAdmitted(t, s)
	want := Result{Outcome: OutcomeInstruct, Level: 1, Action: "instruct", ObserveOnly: true, Label: LabelUnknown, Tip: testTip, NameHeight: testOp.Height, ListSeq: 5, ListSHA256: exampleHash, SealState: SealUnspent}
	if r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
	if (Result{}).Admitted() || (Result{Outcome: OutcomeInstruct}).Admitted() {
		t.Fatal("incomplete result admits")
	}
}

// Claim: admit, ordinary refusal, closure-recording refusal and ignored outcomes all remain ObserveOnly.
func TestAllOutcomesObserveOnly(t *testing.T) {
	s := newScenario(t)
	admitted := wantAdmitted(t, s)
	refused := wantRefused(t, s, Replay, 11)
	s.next()
	s.chain = snapshotOf(t, s.list, sealE)
	closed := wantRefused(t, s, DeviceRevoked, 8)
	if !isClosed(s, keyEdE, sealE) {
		t.Fatal("closure-recording case did not close")
	}
	s.event.Plaintext = true
	ignored, err := s.admit()
	if err != nil || ignored.Outcome != OutcomeIgnored {
		t.Fatalf("ignored: %+v %v", ignored, err)
	}
	for _, r := range []Result{admitted, refused, closed, ignored} {
		if !r.ObserveOnly {
			t.Fatalf("non-observe result %+v", r)
		}
	}
}

// Claim: each messenger gate refuses its code at its first relevant step beside an admitted control.
func TestAdmitRefusals(t *testing.T) {
	cases := []struct {
		name   string
		change func(*scenario)
		code   Code
		step   int
	}{
		{"forwarded session", func(s *scenario) { s.event.KeySource = "forwarded-room-key" }, MessengerUnauthenticated, 1},
		{"backup", func(s *scenario) { s.event.KeySource = "key-backup" }, MessengerUnauthenticated, 1},
		{"own outbound", func(s *scenario) { s.event.KeySource = "own-outbound-session" }, MessengerUnauthenticated, 1},
		{"no source", func(s *scenario) { s.event.KeySource = "" }, MessengerUnauthenticated, 1},
		{"other room", func(s *scenario) { s.event.Room = "!other:example.org" }, NotForMe, 2},
		{"no pin", func(s *scenario) { s.chain = viewOf(t, snapshotData([]byte(`{}`), s.list)) }, DevicePinInvalid, 4},
		{"expired", func(s *scenario) { s.edit(t, func(d *SnapshotData) { d.Names[0].Expired = true }) }, DevicePinInvalid, 4},
		{"cache miss", func(s *scenario) { s.lists = mustCache(t, 0) }, ListUnknown, 5},
		{"wrong hash", func(s *scenario) { s.lists = fixedLists{mustParse(t, replaced(t, "phone P", "phone Q"))} }, DeviceListMismatch, 5},
		{"wrong network", func(s *scenario) { s.edit(t, func(d *SnapshotData) { d.Network = "regtest" }) }, DeviceListMismatch, 5},
		{"wrong owner", func(s *scenario) {
			l := mustParse(t, replaced(t, `"owner":"id/alisa"`, `"owner":"id/example"`))
			s.lists = fixedLists{l}
			s.chain = viewOf(t, snapshotFor("id/alisa", nameValue(pinOf(l)), l))
		}, DeviceListMismatch, 5},
		{"wrong seq", func(s *scenario) {
			s.chain = viewOf(t, snapshotData(nameValue(Pin{Seq: 6, SHA256: exampleHash}), s.list))
		}, DeviceListMismatch, 5},
		{"unknown key", func(s *scenario) { s.event.OriginatorEd25519 = wireKey(t, keyEd2) }, DeviceUnknown, 6},
		{"wrong alg", func(s *scenario) { s.event.OriginatorEd25519 = wireKey(t, keySecpG) }, DeviceAlgMismatch, 6},
		{"absent seal", func(s *scenario) { s.chain = snapshotOf(t, s.list, sealE) }, DeviceRevoked, 8},
		{"closed key", func(s *scenario) {
			if err := s.journal.Close("main", "id/alisa", keyEdE, Outpoint{TxID: sealTxID, Vout: 9}); err != nil {
				t.Fatal(err)
			}
		}, DeviceRevoked, 8},
		{"closed seal", func(s *scenario) {
			if err := s.journal.Close("main", "id/alisa", keyEd3, sealE); err != nil {
				t.Fatal(err)
			}
		}, DeviceRevoked, 8},
		{"ceiling zero", func(s *scenario) { s.lists = fixedLists{tampered(s.list, func(r []Record) { r[2].Ceiling = 0 })} }, DeviceListInvalid, 9},
		{"ceiling two", func(s *scenario) { s.lists = fixedLists{tampered(s.list, func(r []Record) { r[2].Ceiling = 2 })} }, DeviceListInvalid, 9},
		{"presence", func(s *scenario) { s.lists = fixedLists{tampered(s.list, func(r []Record) { r[2].Presence = true })} }, DeviceListInvalid, 9},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			wantAdmitted(t, newScenario(t))
			s := newScenario(t)
			c.change(s)
			wantRefused(t, s, c.code, c.step)
		})
	}
	s := newScenario(t)
	s.lists = fixedLists{tampered(s.list, func([]Record) {})}
	wantAdmitted(t, s)
}

// Claim: plaintext is ignored without taking the encrypted event's replay key.
func TestPlaintextIgnored(t *testing.T) {
	s := newScenario(t)
	s.event.Plaintext = true
	r, err := s.admit()
	if err != nil || r.Outcome != OutcomeIgnored || r.Admitted() || r.Code != "" {
		t.Fatalf("%+v %v", r, err)
	}
	s.event.Plaintext = false
	wantAdmitted(t, s)
}

// Claim: replay keys distinguish session and index and a refused command consumes no replay key.
func TestReplay(t *testing.T) {
	s := newScenario(t)
	wantAdmitted(t, s)
	wantRefused(t, s, Replay, 11)
	s.next()
	wantAdmitted(t, s)
	s.event.SessionID = "another-session"
	wantAdmitted(t, s)
	s = newScenario(t)
	lists := s.lists
	s.lists = mustCache(t, 0)
	wantRefused(t, s, ListUnknown, 5)
	s.lists = lists
	wantAdmitted(t, s)
}

// Claim: reserved algorithms parse but refuse messenger admission while an adjacent Matrix device admits.
func TestReservedAlg(t *testing.T) {
	s := newScenario(t)
	s.use(t, listOctets(5, rec("matrix", AlgMatrixEd25519, keyEdE, false, 1, 2), rec("reserved", "ssh-ed25519", keyEd2, false, 1, 3)))
	wantAdmitted(t, s)
	s.as(t, keyEd2)
	wantRefused(t, s, DeviceAlgUnsupported, 6)
}

// Claim: an unknown algorithm is rejected by the parser and cache beside a valid Matrix list.
func TestUnknownAlgRefusesTheList(t *testing.T) {
	bad := listOctets(5, rec("device", "telegram", keyEdE, false, 1, 2))
	if _, err := Parse(bad); !errors.Is(err, DeviceListInvalid) {
		t.Fatal(err)
	}
	cache := mustCache(t, 0)
	if _, err := cache.Add(bad, Pin{Seq: 5, SHA256: HashOf(bad)}); !errors.Is(err, DeviceListInvalid) || cache.Len() != 0 {
		t.Fatal(err)
	}
	s := newScenario(t)
	s.use(t, listOctets(5, rec("device", AlgMatrixEd25519, keyEdE, false, 1, 2)))
	wantAdmitted(t, s)
}

// Claim: the list parser refuses Matrix ceilings zero and two beside ceiling one.
func TestCeilingExactlyOne(t *testing.T) {
	for _, n := range []int{0, 2} {
		if _, err := Parse(listOctets(5, rec("device", AlgMatrixEd25519, keyEdE, n == 2, n, 2))); !errors.Is(err, DeviceListInvalid) {
			t.Fatal(n, err)
		}
	}
	mustParse(t, listOctets(5, rec("device", AlgMatrixEd25519, keyEdE, false, 1, 2)))
}

type failingSeal struct{ *Snapshot }

func (failingSeal) Seal(Outpoint) (SealState, error) {
	return "", errors.New("upstream diagnostic sentinel")
}

type brokenChain struct{ *Snapshot }

func (brokenChain) Name() (NameView, error) {
	return NameView{}, errors.New("upstream diagnostic sentinel")
}

// Claim: a previously unspent then absent seal closes by key and outpoint, surviving later unspent views and list rotations.
func TestSeenUnspentThenAbsentClosesForEver(t *testing.T) {
	s := newScenario(t)
	wantAdmitted(t, s)
	if !s.journal.SeenUnspent("main", "id/alisa", sealE) {
		t.Fatal("not seen")
	}
	s.next()
	s.chain = snapshotOf(t, s.list, sealE)
	wantRefused(t, s, DeviceRevoked, 8)
	if !isClosed(s, keyEdE, sealE) {
		t.Fatal("not closed")
	}
	for _, c := range []struct {
		key string
		raw []byte
	}{{keyEdE, []byte(exampleList)}, {keyEdE, listOctets(6, rec("new seal", AlgMatrixEd25519, keyEdE, false, 1, 7))}, {keyEd2, listOctets(7, rec("new key", AlgMatrixEd25519, keyEd2, false, 1, 2))}} {
		control := newScenario(t)
		control.use(t, c.raw)
		control.as(t, c.key)
		wantAdmitted(t, control)
		control.journal = s.journal
		control.next()
		wantRefused(t, control, DeviceRevoked, 8)
	}
}

// Claim: never-seen absence is per-command only, whereas absence after an unspent observation closes permanently.
func TestNeverSeenAbsentIsPerCommand(t *testing.T) {
	s := newScenario(t)
	good := s.chain
	s.chain = snapshotOf(t, s.list, sealE)
	wantRefused(t, s, DeviceRevoked, 8)
	if isClosed(s, keyEdE, sealE) || s.journal.SeenUnspent("main", "id/alisa", sealE) {
		t.Fatal("never-seen absence journaled")
	}
	s.chain = good
	wantAdmitted(t, s)
	s.next()
	s.chain = snapshotOf(t, s.list, sealE)
	wantRefused(t, s, DeviceRevoked, 8)
	s.chain = good
	wantRefused(t, s, DeviceRevoked, 8)
	if !isClosed(s, keyEdE, sealE) {
		t.Fatal("control did not close")
	}
}

// Claim: name and seal errors return only a fixed error class without upstream text and cannot create closures.
func TestViewErrorsAreFixedClass(t *testing.T) {
	for _, name := range []bool{false, true} {
		s := newScenario(t)
		wantAdmitted(t, s)
		s.next()
		good := s.chain
		if name {
			s.chain = brokenChain{}
		} else {
			s.chain = failingSeal{snapshotOf(t, s.list)}
		}
		r, err := s.admit()
		if err != ErrChainView || strings.Contains(err.Error(), "sentinel") || r.Admitted() || !r.ObserveOnly || isClosed(s, keyEdE, sealE) {
			t.Fatalf("%+v %v", r, err)
		}
		s.chain = good
		wantAdmitted(t, s)
	}
}

// Claim: an unwatched signing seal is invalid/unavailable, not revoked, and its watched control admits on the same journal.
func TestUnknownSealIsInvalid(t *testing.T) {
	for _, stale := range []bool{false, true} {
		s := newScenario(t)
		good := s.chain
		s.edit(t, func(d *SnapshotData) { d.Seals = d.Seals[:2] })
		if stale {
			s.at.Now = now.Add(time.Minute)
		}
		r := wantRefused(t, s, ChainViewUnavailable, 8)
		if r.Label != LabelInvalid || isClosed(s, keyEdE, sealE) || s.journal.SeenUnspent("main", "id/alisa", sealE) {
			t.Fatalf("%+v", r)
		}
		s.chain = good
		wantAdmitted(t, s)
	}
}

func bobScenario(t testing.TB) *scenario {
	t.Helper()
	s := newScenario(t)
	s.use(t, replaced(t, `"owner":"id/alisa"`, `"owner":"id/bob"`))
	s.event.SessionID = "session-other-owner"
	return s
}
func threeDevices(t testing.TB) *scenario {
	t.Helper()
	s := newScenario(t)
	s.use(t, listOctets(5, rec("one", AlgMatrixEd25519, keyEdE, false, 1, 2), rec("two", AlgMatrixEd25519, keyEd2, false, 1, 3), rec("three", AlgMatrixEd25519, keyEd3, false, 1, 4)))
	return s
}
func (s *scenario) as(t testing.TB, key string) *scenario {
	t.Helper()
	s.event.OriginatorEd25519 = wireKey(t, key)
	s.next()
	return s
}

// Claim: height-based head changes retain the pin hash, observation tip and earliest equal-pin operation.
func TestHeadApproximation(t *testing.T) {
	six := replaced(t, `"seq":5`, `"seq":6`)
	sixPin := pinOf(mustParse(t, six))
	journaled := opAt(812310)
	other := journaled
	other.BlockHash = testTip.Hash
	for _, c := range []struct {
		name     string
		raw      []byte
		op       NameOp
		rollback bool
		wantSeq  uint64
		wantOp   NameOp
	}{
		{"lower seq higher operation", []byte(exampleList), opAt(812320), true, 6, journaled},
		{"lower seq lower operation", []byte(exampleList), testOp, false, 5, testOp},
		{"lower seq same height", []byte(exampleList), other, false, 5, other},
		{"same pin different block", six, other, false, 6, other},
		{"same pin higher operation", six, opAt(812320), false, 6, journaled},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newScenario(t)
			s.useAt(t, six, journaled)
			wantAdmitted(t, s)
			s.next()
			s.useAt(t, c.raw, c.op)
			if c.rollback {
				wantRefused(t, s, DeviceListRollback, 4)
			} else {
				wantAdmitted(t, s)
			}
			head, ok := s.journal.Head("main", "id/alisa")
			hash := exampleHash
			if c.wantSeq == 6 {
				hash = sixPin.SHA256
			}
			if !ok || head != (Head{Seq: c.wantSeq, SHA256: hash, Op: c.wantOp, ObservedAt: testTip}) {
				t.Fatalf("head %+v", head)
			}
			s.next()
			s.useAt(t, six, journaled)
			wantAdmitted(t, s)
		})
	}
}

// Claim: equal sequence with a different pin hash refuses rollback while equal sequence/hash at greater height keeps the earliest operation.
func TestEqualSequencePinConflict(t *testing.T) {
	s := newScenario(t)
	wantAdmitted(t, s)
	original, _ := s.journal.Head("main", "id/alisa")
	s.next()
	s.useAt(t, []byte(exampleList), opAt(812320))
	wantAdmitted(t, s)
	s.next()
	s.useAt(t, replaced(t, "phone P", "phone Q"), opAt(812320))
	wantRefused(t, s, DeviceListRollback, 4)
	if head, _ := s.journal.Head("main", "id/alisa"); head != original {
		t.Fatalf("head changed: %+v", head)
	}
}

// Claim: reorganising a name head does not lift an observe-grade closure, beside an unclosed control.
func TestReorganisationKeepsClosures(t *testing.T) {
	for _, closed := range []bool{false, true} {
		s := newScenario(t)
		s.useAt(t, replaced(t, `"seq":5`, `"seq":6`), opAt(812310))
		wantAdmitted(t, s)
		if closed {
			s.next()
			s.chain = snapshotOf(t, s.list, sealE)
			wantRefused(t, s, DeviceRevoked, 8)
		}
		s.next()
		s.useAt(t, []byte(exampleList), testOp)
		if closed {
			wantRefused(t, s, DeviceRevoked, 8)
		} else {
			wantAdmitted(t, s)
		}
		if head, _ := s.journal.Head("main", "id/alisa"); head.Seq != 5 {
			t.Fatalf("head %+v", head)
		}
	}
}

// Claim: another owner's head, closure and replay keys do not refuse this owner's otherwise valid event.
func TestJournalKeyedByOwner(t *testing.T) {
	s := newScenario(t)
	s.useAt(t, replaced(t, `"seq":5`, `"seq":6`), testOp)
	wantAdmitted(t, s)
	if err := s.journal.Close("main", "id/alisa", keyEdE, sealE); err != nil {
		t.Fatal(err)
	}
	b := bobScenario(t)
	b.event.SessionID = s.event.SessionID
	b.event.Index = s.event.Index
	b.journal = s.journal
	wantAdmitted(t, b)
	wantRefused(t, s, DeviceRevoked, 8)
	rolled := newScenario(t)
	rolled.journal = s.journal
	rolled.useAt(t, []byte(exampleList), opAt(812320))
	wantRefused(t, rolled, DeviceListRollback, 4)
}

// Claim: closure-capacity loss is sticky and fails closed, beside a control with sufficient closure capacity.
func TestClosureJournalFullFailsClosed(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		s := threeDevices(t)
		s.journal.maxClosures = capacity
		wantAdmitted(t, s)
		s.chain = snapshotOf(t, s.list, sealE, Outpoint{TxID: sealTxID, Vout: 3})
		wantRefused(t, s.as(t, keyEdE), DeviceRevoked, 8)
		wantRefused(t, s.as(t, keyEd2), DeviceRevoked, 8)
		s.chain = snapshotOf(t, s.list)
		wantRefused(t, s.as(t, keyEdE), DeviceRevoked, 8)
		if capacity == 1 {
			if !s.journal.Full("main", "id/alisa") {
				t.Fatal("not full")
			}
			wantRefused(t, s.as(t, keyEd2), JournalFull, 8)
			wantRefused(t, s.as(t, keyEd3), JournalFull, 8)
		} else {
			if s.journal.Full("main", "id/alisa") {
				t.Fatal("control full")
			}
			wantRefused(t, s.as(t, keyEd2), DeviceRevoked, 8)
			wantAdmitted(t, s.as(t, keyEd3))
		}
	}
}

// Claim: seen-unspent, owner and total replay bounds refuse without eviction beside sufficient-capacity controls.
func TestJournalBoundsFailClosed(t *testing.T) {
	t.Run("seen unspent", func(t *testing.T) {
		for _, capacity := range []int{2, 3} {
			s := threeDevices(t)
			s.journal.maxSeen = capacity
			if capacity == 2 {
				wantRefused(t, s, JournalFull, 8)
				if !s.journal.Full("main", "id/alisa") {
					t.Fatal("not full")
				}
				wantRefused(t, s.as(t, keyEd2), JournalFull, 8)
			} else {
				wantAdmitted(t, s)
				wantAdmitted(t, s.as(t, keyEd2))
			}
		}
	})
	t.Run("owners", func(t *testing.T) {
		for _, capacity := range []int{1, 2} {
			s := newScenario(t)
			s.journal.maxOwners = capacity
			wantAdmitted(t, s)
			b := bobScenario(t)
			b.journal = s.journal
			if capacity == 1 {
				wantRefused(t, b, JournalFull, 4)
			} else {
				wantAdmitted(t, b)
			}
			s.next()
			wantAdmitted(t, s)
		}
	})
	t.Run("total replay", func(t *testing.T) {
		for _, capacity := range []int{1, 2} {
			s := newScenario(t)
			s.journal.maxReplay = capacity
			wantAdmitted(t, s)
			s.next()
			if capacity == 1 {
				wantRefused(t, s, JournalFull, 11)
				s.event.Index = 0
				wantRefused(t, s, Replay, 11)
			} else {
				wantAdmitted(t, s)
			}
		}
	})
}

// Claim: one owner's replay exhaustion leaves another owner progressing until the separate total bound.
func TestReplayOwnerIsolation(t *testing.T) {
	s := newScenario(t)
	s.journal.maxOwnerReplay = 2
	s.journal.maxReplay = 3
	wantAdmitted(t, s)
	s.next()
	wantAdmitted(t, s)
	s.next()
	wantRefused(t, s, JournalFull, 11)
	b := bobScenario(t)
	b.journal = s.journal
	wantAdmitted(t, b)
	b.next()
	wantRefused(t, b, JournalFull, 11)
	b.event.Index = 0
	wantRefused(t, b, Replay, 11)
}

// Claim: external observe-closure imports validate spelling and fail closed at capacity beside valid imports.
func TestCloseValidatesItsArguments(t *testing.T) {
	j := NewObserveJournal()
	for _, c := range []struct {
		network, owner, key string
		seal                Outpoint
	}{{"testnet", "id/alisa", keyEdE, sealE}, {"main", "alisa", keyEdE, sealE}, {"main", "id/alisa", strings.ToUpper(keyEdE), sealE}, {"main", "id/alisa", keyEdE[2:], sealE}, {"main", "id/alisa", keyEdE, Outpoint{TxID: "A1" + sealTxID[2:]}}} {
		if err := j.Close(c.network, c.owner, c.key, c.seal); err != ErrInvalid {
			t.Fatal(err)
		}
	}
	j.maxClosures = 1
	if err := j.Close("main", "id/alisa", keyEdE, sealE); err != nil || !j.closedFor("main", "id/alisa", keyEdE, sealE) {
		t.Fatal(err)
	}
	if err := j.Close("main", "id/alisa", keyEd2, Outpoint{TxID: sealTxID, Vout: 3}); err != ErrJournalFull || !j.Full("main", "id/alisa") {
		t.Fatal(err)
	}
}

// Claim: competing causes stop at key source before room, device before seal, and closure before replay, beside admitted controls.
func TestStepOrder(t *testing.T) {
	t.Run("key source before room", func(t *testing.T) {
		s := newScenario(t)
		wantAdmitted(t, s)
		s.next()
		s.event.Room = "!other:example.org"
		wantRefused(t, s, NotForMe, 2)
		s.event.KeySource = "backup"
		wantRefused(t, s, MessengerUnauthenticated, 1)
	})
	t.Run("unknown device before absent seal", func(t *testing.T) {
		s := newScenario(t)
		wantAdmitted(t, s)
		s.next()
		s.chain = snapshotOf(t, s.list, sealE)
		wantRefused(t, s, DeviceRevoked, 8)
		s.event.OriginatorEd25519 = wireKey(t, keyEd2)
		wantRefused(t, s, DeviceUnknown, 6)
	})
	t.Run("closure before replay", func(t *testing.T) {
		s := newScenario(t)
		wantAdmitted(t, s)
		wantRefused(t, s, Replay, 11)
		if err := s.journal.Close("main", "id/alisa", keyEdE, sealE); err != nil {
			t.Fatal(err)
		}
		wantRefused(t, s, DeviceRevoked, 8)
	})
	t.Run("invalid view before pin", func(t *testing.T) {
		s := newScenario(t)
		wantAdmitted(t, s)
		s.next()
		s.chain = viewOf(t, snapshotData([]byte(`{}`), s.list))
		wantRefused(t, s, DevicePinInvalid, 4)
		s.at.Now = now.Add(-time.Hour)
		wantRefused(t, s, ChainViewUnavailable, 4)
	})
	t.Run("rollback before cache", func(t *testing.T) {
		s := newScenario(t)
		s.use(t, replaced(t, `"seq":5`, `"seq":6`))
		wantAdmitted(t, s)
		s.next()
		s.lists = mustCache(t, 0)
		wantRefused(t, s, ListUnknown, 5)
		s.useAt(t, []byte(exampleList), opAt(812320))
		s.lists = mustCache(t, 0)
		wantRefused(t, s, DeviceListRollback, 4)
	})
}

// Claim: a kept view is relabelled at every use, including strict future-time refusal and the 60-second age boundary.
func TestViewAgeIsCheckedAtUse(t *testing.T) {
	s := newScenario(t)
	for _, c := range []struct {
		offset int64
		label  Freshness
	}{{0, LabelUnknown}, {60, LabelUnknown}, {61, LabelStale}, {36000, LabelStale}, {-1, LabelInvalid}, {30, LabelUnknown}} {
		s.next()
		s.at = Check{Now: time.Unix(int64(writtenAt)+c.offset, 0), ClockTrusted: true}
		var r Result
		if c.label == LabelInvalid {
			r = wantRefused(t, s, ChainViewUnavailable, 4)
		} else {
			r = wantAdmitted(t, s)
		}
		if r.Label != c.label {
			t.Fatalf("%+v", r)
		}
	}
}

// Claim: unavailable snapshots yield a Result for each valid event, with unknown for absence and invalid for a refused file.
func TestUnavailableView(t *testing.T) {
	for _, absent := range []bool{true, false} {
		s := newScenario(t)
		wantAdmitted(t, s)
		s.next()
		s.chain = UnavailableView{Absent: absent}
		for i := 0; i < 2; i++ {
			r := wantRefused(t, s, ChainViewUnavailable, 4)
			label := LabelInvalid
			if absent {
				label = LabelUnknown
			}
			if r.Label != label || !r.ObserveOnly {
				t.Fatalf("%+v", r)
			}
		}
		s.chain = snapshotOf(t, s.list)
		wantAdmitted(t, s)
	}
}

// Claim: malformed event facts and missing arguments error without admission, while the restored scenario admits.
func TestOperationalFailures(t *testing.T) {
	for _, change := range []func(*Event){func(e *Event) { e.OriginatorEd25519 += "=" }, func(e *Event) { e.OriginatorEd25519 = "short" }, func(e *Event) { e.SessionID = "" }, func(e *Event) { e.Room = "" }} {
		s := newScenario(t)
		good := s.event
		change(&s.event)
		r, err := s.admit()
		if err != ErrInvalid || r.Admitted() || !r.ObserveOnly {
			t.Fatalf("%+v %v", r, err)
		}
		s.event = good
		wantAdmitted(t, s)
	}
	s := newScenario(t)
	for _, call := range []func() (Result, error){func() (Result, error) { return Admit(s.event, "", s.at, s.lists, s.chain, s.journal) }, func() (Result, error) { return Admit(s.event, s.room, s.at, nil, s.chain, s.journal) }, func() (Result, error) { return Admit(s.event, s.room, s.at, s.lists, nil, s.journal) }, func() (Result, error) { return Admit(s.event, s.room, s.at, s.lists, s.chain, nil) }} {
		r, err := call()
		if err != ErrInvalid || r.Admitted() || !r.ObserveOnly {
			t.Fatalf("%+v %v", r, err)
		}
	}
	wantAdmitted(t, s)
}

// Claim: under concurrent cache traffic one repeated pair admits once and 32 distinct pairs all admit on the same journal.
func TestConcurrentAdmit(t *testing.T) {
	s := newScenario(t)
	cache := s.lists.(*Cache)
	for _, distinct := range []bool{false, true} {
		var wg sync.WaitGroup
		results := make([]Result, 32)
		errs := make([]error, 32)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				event := s.event
				event.Index = 7
				if distinct {
					event.Index = 100 + uint64(i)
				}
				_, errs[i] = cache.Add([]byte(exampleList), Pin{Seq: 5, SHA256: exampleHash})
				if errs[i] == nil {
					results[i], errs[i] = Admit(event, s.room, s.at, cache, s.chain, s.journal)
				}
				cache.Get(exampleHash)
			}(i)
		}
		wg.Wait()
		admitted, replayed := 0, 0
		for i, r := range results {
			if errs[i] != nil {
				t.Fatal(errs[i])
			}
			if r.Admitted() {
				admitted++
			} else if r.Code == Replay {
				replayed++
			} else {
				t.Fatalf("%+v", r)
			}
		}
		if distinct && admitted != 32 || !distinct && (admitted != 1 || replayed != 31) {
			t.Fatalf("%d admitted, %d replayed", admitted, replayed)
		}
	}
}
