package devicelist

import (
	"testing"
	"time"
)

// Claim: the snapshot adapter reports what the host wrote and nothing newer.
// It gives the pin of the owner's name value, the network, the location of
// the name operation and the tip, the state of each seal it holds and
// ErrSealUnknown for one it does not hold, and it is never live, so a verdict
// on it is for observation only. Controls and negatives for the pin: a name
// value with a valid member gives the pin; one with the member twice gives
// none; and an EXPIRED name gives none although its value holds a valid
// member.
// Claim: snapshot views preserve name and seal facts but never establish a pin from an expired or duplicated name-value member.
func TestSnapshotView(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	view := snapshotOf(t, list, sealE)
	var chain ChainView = view
	if chain.Live() {
		t.Fatal("a snapshot claims to be live")
	}
	name, err := chain.Name()
	want := NameView{Network: "main", Owner: "id/alisa", HasPin: true, Pin: Pin{Seq: 5, SHA256: exampleHash}, Op: testOp, Tip: testTip}
	if err != nil || name != want {
		t.Fatalf("name view %+v %v", name, err)
	}
	if state, err := chain.Seal(Outpoint{TxID: sealTxID, Vout: 0}); err != nil || state != SealUnspent {
		t.Fatal("control: unspent seal", state, err)
	}
	if state, err := chain.Seal(sealE); err != nil || state != SealNotInUTXOSet {
		t.Fatal("absent seal", state, err)
	}
	if _, err := chain.Seal(Outpoint{TxID: sealTxID, Vout: 99}); err != ErrSealUnknown {
		t.Fatal("unknown seal", err)
	}
	twice := []byte(`{"swarma-devices":` + string(encodePin(pinOf(list))) + `,"swarma-devices":` + string(encodePin(pinOf(list))) + `}`)
	if name, _ := viewOf(t, snapshotData(twice, list)).Name(); name.HasPin || name.Pin != (Pin{}) || name.Op != testOp {
		t.Fatalf("a duplicated member gave a pin: %+v", name)
	}
	expired := snapshotData(nameValue(pinOf(list)), list)
	expired.Names[0].Expired = true
	if name, _ := viewOf(t, expired).Name(); name.HasPin || name.Pin != (Pin{}) || !name.Expired {
		t.Fatalf("an expired name gave a pin: %+v", name)
	}
}

// Claim: a snapshot counts only under the exact
// watch set and the network of the protected host configuration. Control: a
// file with two names and three seals is taken under the configuration that
// names exactly those, for either owner. Negatives, each ErrSnapshotInvalid
// and no view: another network; a name or a seal more, fewer or other than
// configured; and the plausible snapshot of a foreign owner. A malformed
// configuration is ErrInvalid.
// Claim: exact configured network and watch sets are required, with malformed configurations refused separately.
func TestSnapshotBindings(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	data := snapshotData(nameValue(pinOf(list)), list)
	data.Names = append(data.Names, NameRecord{Name: "id/bob", Value: []byte(`{}`), Op: testOp})
	raw := encodeSnapshotFile(data)
	for _, owner := range []string{"id/alisa", "id/bob"} {
		config := configOf(data)
		config.Owner = owner
		view, err := newSnapshot(raw, config)
		if err != nil {
			t.Fatal("control:", owner, err)
		}
		if name, _ := view.Name(); name.Owner != owner || name.HasPin != (owner == "id/alisa") {
			t.Fatalf("control: the view of %s: %+v", owner, name)
		}
	}
	other := Outpoint{TxID: sealTxID, Vout: 77}
	for name, change := range map[string]func(c *SnapshotConfig){
		"another network":        func(c *SnapshotConfig) { c.Network = "regtest" },
		"file names one more":    func(c *SnapshotConfig) { c.Names = []string{"id/alisa"} },
		"file names one fewer":   func(c *SnapshotConfig) { c.Names = append(c.Names, "id/carol") },
		"file names another":     func(c *SnapshotConfig) { c.Names = []string{"id/alisa", "id/carol"} },
		"file holds a seal more": func(c *SnapshotConfig) { c.Seals = c.Seals[:2] },
		"file holds a seal fewer": func(c *SnapshotConfig) {
			c.Seals = append(c.Seals, other)
		},
		"file holds another seal": func(c *SnapshotConfig) {
			c.Seals = []Outpoint{c.Seals[0], c.Seals[1], other}
		},
	} {
		config := configOf(data)
		change(&config)
		if view, err := newSnapshot(raw, config); err != ErrSnapshotInvalid || view != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// A foreign owner's snapshot: valid in itself, with the same seals, but
	// it names id/karl where the host watches id/alisa.
	foreign := snapshotFor("id/karl", nameValue(pinOf(list)), list)
	config := configOf(snapshotData(nameValue(pinOf(list)), list))
	if _, err := parseSnapshotFile(encodeSnapshotFile(foreign)); err != nil {
		t.Fatal("fixture: the foreign snapshot is not valid in itself:", err)
	}
	if view, err := newSnapshot(encodeSnapshotFile(foreign), config); err != ErrSnapshotInvalid || view != nil {
		t.Fatal("foreign owner's snapshot:", err)
	}
	for name, change := range map[string]func(c *SnapshotConfig){
		"owner outside the watch set": func(c *SnapshotConfig) { c.Owner = "id/carol" },
		"owner that is no id/ name":   func(c *SnapshotConfig) { c.Owner = "d/alisa"; c.Names = []string{"d/alisa", "id/bob"} },
		"relative path":               func(c *SnapshotConfig) { c.Path = "snapshot.json" },
		"path that is not clean":      func(c *SnapshotConfig) { c.Path = "/var/lib/../lib/snapshot.json" },
		"root as the path":            func(c *SnapshotConfig) { c.Path = "/" },
		"network":                     func(c *SnapshotConfig) { c.Network = "testnet" },
		"name twice":                  func(c *SnapshotConfig) { c.Names = []string{"id/alisa", "id/alisa"} },
		"seal twice":                  func(c *SnapshotConfig) { c.Seals = []Outpoint{c.Seals[0], c.Seals[0], c.Seals[1]} },
		"no seal":                     func(c *SnapshotConfig) { c.Seals = nil },
		"malformed seal":              func(c *SnapshotConfig) { c.Seals = []Outpoint{{TxID: "a1", Vout: 0}} },
	} {
		config := configOf(data)
		change(&config)
		if view, err := newSnapshot(raw, config); err != ErrInvalid || view != nil {
			t.Fatalf("configuration with %s: %v", name, err)
		}
	}
}

// Claim: snapshot labels use the consumer's strict time thresholds and independent tip, with contract precedence.
// The label of a snapshot is computed by
// the consumer from its own clock, its own constants (60 s of age, 7200 s of
// tip lag, zero future skew) and its own knowledge of the live tip, with
// the contract's precedence. Each threshold has the control at the bound and
// the negative one second past it. On a host without an independent live tip
// no row is fresh.
func TestSnapshotLabels(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	view := snapshotOf(t, list)
	live := testTip
	otherHash, otherHeight, otherTime := testTip, testTip, testTip
	otherHash.Hash = testOp.BlockHash
	otherHeight.Height++
	otherTime.Time++
	trusted := func(offset int64, tip *Tip) Check {
		return Check{Now: time.Unix(int64(writtenAt)+offset, 0), ClockTrusted: true, LiveTip: tip}
	}
	// The tip block is 600 s older than the write, so a check 6600 s after
	// the write sees a tip lag of exactly 7200 s.
	cases := []struct {
		name string
		at   Check
		want Freshness
	}{
		{"young, live tip equal", trusted(30, &live), LabelFresh},
		{"age 0", trusted(0, &live), LabelFresh},
		{"age exactly 60", trusted(60, &live), LabelFresh},
		{"age 61", trusted(61, &live), LabelStale},
		{"young, no live tip", trusted(30, nil), LabelUnknown},
		{"age 60, no live tip", trusted(60, nil), LabelUnknown},
		{"age 61, no live tip: stale before unknown", trusted(61, nil), LabelStale},
		{"live tip of another hash", trusted(30, &otherHash), LabelStale},
		{"live tip of another height", trusted(30, &otherHeight), LabelStale},
		{"live tip of another time", trusted(30, &otherTime), LabelStale},
		{"written at the check: no future skew", trusted(0, &live), LabelFresh},
		{"written one second ahead: invalid", trusted(-1, &live), LabelInvalid},
		{"written an hour ahead", trusted(-3600, nil), LabelInvalid},
		{"no trusted clock, young", Check{Now: time.Unix(int64(writtenAt)+30, 0), LiveTip: &live}, LabelUnknown},
		{"no trusted clock, old: unknown before stale", Check{Now: time.Unix(int64(writtenAt)+9999, 0), LiveTip: &live}, LabelUnknown},
		{"no trusted clock, future: unknown before invalid", Check{Now: time.Unix(int64(writtenAt)-9999, 0), LiveTip: &live}, LabelUnknown},
		{"zero check", Check{}, LabelUnknown},
	}
	for _, c := range cases {
		if got := view.Freshness(c.at); got != c.want {
			t.Fatalf("%s: label %q, want %q", c.name, got, c.want)
		}
	}
	// Tip lag, apart from age: the file is 0 s old at the check in every row.
	lag := func(tipAge uint64, tip bool) Freshness {
		data := snapshotData(nameValue(pinOf(list)), list)
		data.Tip.Time = writtenAt - tipAge
		c := Check{Now: time.Unix(int64(writtenAt), 0), ClockTrusted: true}
		if tip {
			c.LiveTip = &data.Tip
		}
		return viewOf(t, data).Freshness(c)
	}
	if got := lag(7200, true); got != LabelFresh {
		t.Fatalf("control: a tip lag of exactly 7200 s: %q", got)
	}
	if got := lag(7201, true); got != LabelStale {
		t.Fatalf("a tip lag of 7201 s: %q", got)
	}
	if got := lag(7200, false); got != LabelUnknown {
		t.Fatalf("control: a tip lag of 7200 s without a live tip: %q", got)
	}
	if got := lag(7201, false); got != LabelStale {
		t.Fatalf("a tip lag of 7201 s without a live tip: %q", got)
	}
	if SnapshotMaxAgeSeconds != 60 || SnapshotMaxTipLagSeconds != 7200 {
		t.Fatal("the consumer constants are not the accepted ones")
	}
}

// Claim: no freshness bound comes from the file. Two files that differ
// in nothing but their write time get their labels from the consumer's
// constants alone, and the parser has no member that could carry a bound: a
// file that adds one is refused. Control: the file without the member parses.
// Claim: added file-supplied freshness bounds refuse, while valid files are labelled solely by consumer constants.
func TestNoBoundIsReadFromTheFile(t *testing.T) {
	list := mustParse(t, []byte(exampleList))
	data := snapshotData(nameValue(pinOf(list)), list)
	raw := string(encodeSnapshotFile(data))
	if _, err := parseSnapshotFile([]byte(raw)); err != nil {
		t.Fatal("control:", err)
	}
	for _, member := range []string{`"max_age_seconds":9223372036854775807,`, `"max_age_seconds":600,`, `"fresh":true,`} {
		if _, err := parseSnapshotFile([]byte(`{` + member + raw[1:])); err != ErrSnapshotInvalid {
			t.Fatalf("a file with %s: %v", member, err)
		}
	}
	old := data
	old.WrittenAt = writtenAt - 600
	if got := viewOf(t, old).Freshness(at); got != LabelStale {
		t.Fatalf("a file written 630 s before the check: %q", got)
	}
	if got := viewOf(t, data).Freshness(at); got != LabelUnknown {
		t.Fatalf("control: a file written 30 s before the check: %q", got)
	}
}
