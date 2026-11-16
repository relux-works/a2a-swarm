package devicelist

import (
	"errors"
	"fmt"
	"testing"
)

// The five gates below are the ones a plant removes. Each has a named
// assertion that returns an error when the gate does not hold; the error
// starts with the name of the assertion. Each plant test first requires the
// assertion to pass on the real code, then removes the check and requires
// the assertion to fail, then restores the check. Removing a check from the
// source is checked separately with real failing test runs and controls.

// named prefixes a failure with the name of the assertion that found it.
func named(assertion string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", assertion, err)
}

func refusedWith(s *scenario, code Code, step int) error {
	result, err := s.admit()
	if err != nil {
		return fmt.Errorf("error instead of %s: %w", code, err)
	}
	if result.Admitted() {
		return fmt.Errorf("admitted as L1 instruct instead of %s", code)
	}
	if result.Code != code || result.Step != step {
		return fmt.Errorf("refused %s at step %d instead of %s at step %d", result.Code, result.Step, code, step)
	}
	return nil
}

// assertCeilingGateCodec: the reader refuses a matrix-ed25519 record whose
// ceiling is not exactly 1. It plants ceiling 0 only: for ceiling 2 the
// reader refuses earlier, by the highest level of the alg (O3.16), so that
// half of the rule is shown by assertCeilingGateAdmission.
func assertCeilingGateCodec() error {
	octets := listOctets(5, rec("Element on P", AlgMatrixEd25519, keyEdE, false, 0, 2))
	if list, err := Parse(octets); list != nil || !errors.Is(err, DeviceListInvalid) {
		return named("assertCeilingGateCodec", fmt.Errorf("the reader admitted a matrix-ed25519 record with ceiling 0: %v", err))
	}
	return nil
}

// assertCeilingGateAdmission: Admit refuses a matrix-ed25519 record whose
// ceiling is not exactly 1, for a list that did not pass the reader.
func assertCeilingGateAdmission(t testing.TB) error {
	for _, ceiling := range []int{0, 2} {
		s := newScenario(t)
		s.lists = fixedLists{tampered(s.list, func(r []Record) { r[2].Ceiling = ceiling })}
		if err := refusedWith(s, DeviceListInvalid, 9); err != nil {
			return named("assertCeilingGateAdmission", fmt.Errorf("ceiling %d: %w", ceiling, err))
		}
	}
	return nil
}

// assertHashToPinGateCache: the cache takes no octets whose hash is not the
// pinned one.
func assertHashToPinGateCache(t testing.TB) error {
	other := mustParse(t, replaced(t, "phone P", "phone Q"))
	cache := mustCache(t, 0)
	if _, err := cache.Add([]byte(exampleList), pinOf(other)); err != DeviceListMismatch || cache.Len() != 0 {
		return named("assertHashToPinGateCache", fmt.Errorf("the cache took a list under a pin with another hash: %v", err))
	}
	return nil
}

// assertHashToPinGateAdmission: Admit refuses a list whose octets do not hash
// to the pin, even when the lookup returns it under the pinned hash. The
// other list differs from the pinned one in a label only, so nothing but the
// hash tells them apart.
func assertHashToPinGateAdmission(t testing.TB) error {
	s := newScenario(t)
	s.lists = fixedLists{mustParse(t, replaced(t, "phone P", "phone Q"))}
	return named("assertHashToPinGateAdmission", refusedWith(s, DeviceListMismatch, 5))
}

// assertSealGate: Admit refuses a device whose seal the chain view shows
// absent, and journals the closure of a seal it saw unspent before.
func assertSealGate(t testing.TB) error {
	s := newScenario(t)
	if _, err := s.admit(); err != nil {
		return named("assertSealGate", err)
	}
	s.next()
	s.chain = snapshotOf(t, s.list, sealE)
	if err := refusedWith(s, DeviceRevoked, 8); err != nil {
		return named("assertSealGate", err)
	}
	if !s.journal.closedFor("main", "id/alisa", keyEdE, sealE) {
		return named("assertSealGate", errors.New("a seal seen unspent and then absent was refused without a journaled closure"))
	}
	return nil
}

// assertSeenUnspentGate: the absence of a seal that the journal never
// saw unspent refuses the command and journals no closure, so the device is
// admitted when a later view shows the seal unspent.
func assertSeenUnspentGate(t testing.TB) error {
	s := newScenario(t)
	unspent := s.chain
	s.chain = snapshotOf(t, s.list, sealE)
	if err := refusedWith(s, DeviceRevoked, 8); err != nil {
		return named("assertSeenUnspentGate", err)
	}
	if s.journal.closedFor("main", "id/alisa", keyEdE, sealE) {
		return named("assertSeenUnspentGate", errors.New("the absence of a never-seen seal was journaled as a permanent closure"))
	}
	s.chain = unspent
	result, err := s.admit()
	if err != nil || !result.Admitted() {
		return named("assertSeenUnspentGate", fmt.Errorf("the device was not admitted once its seal was unspent: %+v %v", result, err))
	}
	return nil
}

// assertConsistencyGate: the snapshot parser refuses a file whose
// consistency is not "bracketed", "witnessed" included, and no view is made
// of it. The file differs from the valid control in that one value.
func assertConsistencyGate(t testing.TB) error {
	list := mustParse(t, []byte(exampleList))
	data := snapshotData(nameValue(pinOf(list)), list)
	if _, err := newSnapshot(encodeSnapshotFile(data), configOf(data)); err != nil {
		return named("assertConsistencyGate", fmt.Errorf("the bracketed control was refused: %w", err))
	}
	for _, consistency := range []string{"witnessed", "atomic", ""} {
		data.Consistency = consistency
		raw := encodeSnapshotFile(data)
		if parsed, err := parseSnapshotFile(raw); err != ErrSnapshotInvalid || len(parsed.Names) != 0 {
			return named("assertConsistencyGate", fmt.Errorf("the parser took a snapshot of consistency %q: %v", consistency, err))
		}
		if view, err := newSnapshot(raw, configOf(data)); err != ErrSnapshotInvalid || view != nil {
			return named("assertConsistencyGate", fmt.Errorf("a view was made of a snapshot of consistency %q: %v", consistency, err))
		}
	}
	return nil
}

// Plant: the ceiling check removed. Named assertions that fail:
// assertCeilingGateCodec and assertCeilingGateAdmission.
// Claim: bypassing the ceiling gate kills both codec and admission assertions, which pass again after restoration.
func TestPlantCeilingCheckRemoved(t *testing.T) {
	if err := errors.Join(assertCeilingGateCodec(), assertCeilingGateAdmission(t)); err != nil {
		t.Fatal("the assertions must pass before the plant:", err)
	}
	original := messengerCeilingOK
	t.Cleanup(func() { messengerCeilingOK = original })
	messengerCeilingOK = func(int) bool { return true }
	codec, admission := assertCeilingGateCodec(), assertCeilingGateAdmission(t)
	messengerCeilingOK = original
	if codec == nil {
		t.Fatal("plant survived: assertCeilingGateCodec did not detect the removed ceiling check")
	}
	if admission == nil {
		t.Fatal("plant survived: assertCeilingGateAdmission did not detect the removed ceiling check")
	}
	if err := errors.Join(assertCeilingGateCodec(), assertCeilingGateAdmission(t)); err != nil {
		t.Fatal("the assertions must pass after the plant is removed:", err)
	}
}

// Plant: the hash-to-pin check removed. Named assertions that fail:
// assertHashToPinGateCache and assertHashToPinGateAdmission.
// Claim: bypassing hash-to-pin validation kills cache and admission assertions, which pass again after restoration.
func TestPlantHashToPinCheckRemoved(t *testing.T) {
	if err := errors.Join(assertHashToPinGateCache(t), assertHashToPinGateAdmission(t)); err != nil {
		t.Fatal("the assertions must pass before the plant:", err)
	}
	original := hashMatchesPin
	t.Cleanup(func() { hashMatchesPin = original })
	hashMatchesPin = func([]byte, string) bool { return true }
	cache, admission := assertHashToPinGateCache(t), assertHashToPinGateAdmission(t)
	hashMatchesPin = original
	if cache == nil {
		t.Fatal("plant survived: assertHashToPinGateCache did not detect the removed hash-to-pin check")
	}
	if admission == nil {
		t.Fatal("plant survived: assertHashToPinGateAdmission did not detect the removed hash-to-pin check")
	}
	if err := errors.Join(assertHashToPinGateCache(t), assertHashToPinGateAdmission(t)); err != nil {
		t.Fatal("the assertions must pass after the plant is removed:", err)
	}
}

// Plant: the seal check removed. Named assertion that fails: assertSealGate.
// Claim: bypassing the seal gate kills its named assertion, which passes before and after the plant.
func TestPlantSealCheckRemoved(t *testing.T) {
	if err := assertSealGate(t); err != nil {
		t.Fatal("the assertion must pass before the plant:", err)
	}
	original := sealState
	t.Cleanup(func() { sealState = original })
	sealState = func(ChainView, Outpoint) (SealState, error) { return SealUnspent, nil }
	planted := assertSealGate(t)
	sealState = original
	if planted == nil {
		t.Fatal("plant survived: assertSealGate did not detect the removed seal check")
	}
	if err := assertSealGate(t); err != nil {
		t.Fatal("the assertion must pass after the plant is removed:", err)
	}
}

// Plant: the seen-unspent condition of a permanent closure removed, so
// that every absence closes. Named assertion that fails:
// assertSeenUnspentGate.
// Claim: making every absence close kills the never-seen assertion, which passes before and after the plant.
func TestPlantSeenUnspentConditionRemoved(t *testing.T) {
	if err := assertSeenUnspentGate(t); err != nil {
		t.Fatal("the assertion must pass before the plant:", err)
	}
	original := seenUnspentBefore
	t.Cleanup(func() { seenUnspentBefore = original })
	seenUnspentBefore = func(*ownerJournal, Outpoint) bool { return true }
	planted := assertSeenUnspentGate(t)
	seenUnspentBefore = original
	if planted == nil {
		t.Fatal("plant survived: assertSeenUnspentGate did not detect the removed seen-unspent condition")
	}
	if err := assertSeenUnspentGate(t); err != nil {
		t.Fatal("the assertion must pass after the plant is removed:", err)
	}
}

// Plant: the consistency check of the snapshot parser removed. Named
// assertion that fails: assertConsistencyGate.
// Claim: bypassing consistency validation kills its named assertion, which passes before and after the plant.
func TestPlantConsistencyCheckRemoved(t *testing.T) {
	if err := assertConsistencyGate(t); err != nil {
		t.Fatal("the assertion must pass before the plant:", err)
	}
	original := consistencyAccepted
	t.Cleanup(func() { consistencyAccepted = original })
	consistencyAccepted = func(string) bool { return true }
	planted := assertConsistencyGate(t)
	consistencyAccepted = original
	if planted == nil {
		t.Fatal("plant survived: assertConsistencyGate did not detect the removed consistency check")
	}
	if err := assertConsistencyGate(t); err != nil {
		t.Fatal("the assertion must pass after the plant is removed:", err)
	}
}
