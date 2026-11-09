package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
)

func monitorPaths(t *testing.T) Paths {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return Paths{Base: base}
}

func monitorRecord() Monitor {
	return Monitor{Version: 1, Kind: "monitor", Runtime: "muse", Generation: strings.Repeat("a", 32), SessionID: "fixture-session", Host: "fixture-host", OwnerPID: 123, OwnerBirth: "fixture-owner-birth", ConsumerPID: os.Getpid(), ConsumerBirth: "fixture-consumer-birth", Transport: "stdout", Phase: "streaming", HeartbeatEvery: time.Second, HeartbeatAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func monitorDisk(t *testing.T, p Paths) []byte {
	t.Helper()
	b, err := os.ReadFile(p.StateFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func monitorLoad(t *testing.T, p Paths) State {
	t.Helper()
	s, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func monitorSeed(t *testing.T) Paths {
	t.Helper()
	p := monitorPaths(t)
	seedStateJSON(t, p, `{"next_batch":"before","future":{"z":9007199254740993,"a":[1.0,1e0,-0]},"lease":{"holder":"fixture-session","host":"fixture-host","pid":123,"expires":"2030-01-01T00:00:00Z"}}`)
	s := monitorLoad(t, p)
	s.Writer = &StateWriter{Protocol: 2}
	b, err := sealedState(s)
	if err != nil {
		t.Fatal(err)
	}
	seedStateJSON(t, p, string(b))
	return p
}
func monitorRun(p Paths, entry MonitorEntry, fn func(*MonitorGeneration) error) error {
	return WithMonitorGeneration(p, "a", time.Now().Add(time.Second), entry, monitorRecord(), fn)
}

// Simulate the pre-L02 serializer: it takes the existing state lock, changes
// covered values and preserves the opaque writer. This is an old-writer
// history, NOT a call to the candidate's now-compatible UpdateState method.
func monitorOldWrite(t *testing.T, p Paths, change func(map[string]json.RawMessage)) {
	t.Helper()
	unlock, err := lockFile(p.StateFile("a")[:len(p.StateFile("a"))-5] + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var m map[string]json.RawMessage
	if json.Unmarshal(monitorDisk(t, p), &m) != nil {
		t.Fatal("decode fixture")
	}
	change(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if os.WriteFile(p.StateFile("a"), b, 0600) != nil {
		t.Fatal("old write")
	}
}

// Proves: Canonicalization preserves number lexemes, ignores formatting, and accepts sealed future protocols; it does not authenticate writers.
func TestMonitorCanonicalSeal(t *testing.T) {
	a := []byte(`{"future":{"z":9007199254740993,"a":[1.0,1e0,-0,"<>&"]},"next_batch":"x"}`)
	b := []byte(`{ "next_batch":"x", "future": { "a":[1.0,1e0,-0,"\u003c\u003e\u0026"], "z":9007199254740993 } }`)
	x, e := stateDigest(a)
	y, f := stateDigest(b)
	if e != nil || f != nil || x != y {
		t.Fatal("key/whitespace/string canonicalization")
	}
	for _, changed := range []string{`{"n":1}`, `{"n":1.0}`, `{"n":1e0}`, `{"n":-0}`, `{"n":0}`, `{"n":9007199254740993}`, `{"n":9007199254740992}`} {
		for _, other := range []string{`{"n":42}`, `{"n":9007199254740994}`} {
			u, _ := stateDigest([]byte(changed))
			v, _ := stateDigest([]byte(other))
			if u == v {
				t.Fatal("numeric precision lost")
			}
		}
	}
	seen := map[string]bool{}
	for _, raw := range []string{`{"n":1}`, `{"n":1.0}`, `{"n":1e0}`, `{"n":-0}`, `{"n":0}`} {
		h, _ := stateDigest([]byte(raw))
		if seen[h] {
			t.Fatal("number lexemes merged")
		}
		seen[h] = true
	}
	for _, bad := range []string{`{"future":{"x":1,"x":2}}`, `{"x":1,"x":1}`, `{"writer":{},"WRITER":{}}`, `[]`, `null`, `{} {}`} {
		if _, err := stateDigest([]byte(bad)); err == nil {
			t.Fatal("ambiguous canonical input accepted")
		}
	}
	p := monitorSeed(t)
	s := monitorLoad(t, p)
	if !validWriterSeal(s) {
		t.Fatal("valid neighbor")
	}
	s.Writer.Protocol = 3
	b3, err := sealedState(s)
	if err != nil {
		t.Fatal(err)
	}
	seedStateJSON(t, p, string(b3))
	if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { return nil }); err != nil {
		t.Fatal("protocol >= 2 must remain accepted", err)
	}
	if saved := monitorLoad(t, p); saved.Writer.Protocol != 2 || !validWriterSeal(saved) {
		t.Fatal("monitor arm/close must save protocol 2 even after reading a future protocol")
	}
}

// Proves: Every ordinary save route records protocol 2 and retains foreign history until explicit arm; it does not prove external effects durable.
func TestMonitorStickyOrdinaryWriters(t *testing.T) {
	routes := map[string]func(Paths, func(*State) error) error{
		"SaveState": func(p Paths, f func(*State) error) error {
			s, err := LoadState(p, "a")
			if err != nil {
				return err
			}
			if err = f(&s); err != nil {
				return err
			}
			return SaveState(p, "a", s)
		},
		"UpdateState": func(p Paths, f func(*State) error) error { _, e := UpdateState(p, "a", f); return e },
		"UpdateStateBy": func(p Paths, f func(*State) error) error {
			_, e := UpdateStateBy(p, "a", time.Now().Add(time.Second), f)
			return e
		},
		"UpdateStateUntil": func(p Paths, f func(*State) error) error {
			_, e := UpdateStateUntil(p, "a", time.Now().Add(time.Second), f)
			return e
		},
	}
	for route, write := range routes {
		t.Run(route, func(t *testing.T) {
			p := monitorSeed(t)
			monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"old-mutated"`) })
			if validWriterSeal(monitorLoad(t, p)) {
				t.Fatal("old mutation negative control")
			}
			if err := write(p, func(s *State) error { s.PutHookSession(HookSession{ID: "fixture-hook", Blocks: 1}); return nil }); err != nil {
				t.Fatal(err)
			}
			s := monitorLoad(t, p)
			first := s.Writer.ForeignSeen
			if !validWriterSeal(s) || s.Writer.Protocol != 2 || first.IsZero() || first.Location() != time.UTC || s.NextBatch != "old-mutated" || s.HookBlocks("fixture-hook") != 1 {
				t.Fatal("ordinary write did not retain fenced history")
			}
			for i := 0; i < 3; i++ {
				if _, err := UpdateState(p, "a", func(s *State) error { s.Writer = nil; s.Threads = append(s.Threads, "ordinary"); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if saved := monitorLoad(t, p); saved.Writer.Protocol != 2 || !saved.Writer.ForeignSeen.Equal(first) {
				t.Fatal("compatible writer erased first timestamp")
			}
			before := monitorDisk(t, p)
			for _, entry := range []MonitorEntry{MonitorAutomatic, MonitorRecovery} {
				called := false
				err := monitorRun(p, entry, func(*MonitorGeneration) error { called = true; return nil })
				if !errors.Is(err, ErrIncompatibleWriter) || called || !bytes.Equal(before, monitorDisk(t, p)) {
					t.Fatal("auto/recovery cleared fence or had effects", err)
				}
			}
			if err := monitorRun(p, MonitorArm, func(g *MonitorGeneration) error {
				if !monitorLoad(t, p).Writer.ForeignSeen.IsZero() {
					t.Fatal("explicit arm did not clear")
				}
				return g.CheckRenewal(monitorRecord().Generation)
			}); err != nil {
				t.Fatal(err)
			}
			after := monitorLoad(t, p)
			if !after.Writer.ForeignSeen.IsZero() || !validWriterSeal(after) || after.Monitor.Phase != "stopped" {
				t.Fatal("clean stop/fresh seal")
			}
		})
	}
}

// Proves: SaveState checks current disk input even when supplied a stale caller copy.
func TestMonitorSaveChecksDiskNotCaller(t *testing.T) {
	p := monitorSeed(t)
	stale := monitorLoad(t, p)
	monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"concurrent-old"`) })
	stale.NextBatch = "compatible-save"
	if err := SaveState(p, "a", stale); err != nil {
		t.Fatal(err)
	}
	s := monitorLoad(t, p)
	if s.Writer.ForeignSeen.IsZero() || !validWriterSeal(s) {
		t.Fatal("SaveState checked caller value instead of disk input")
	}
}

// Proves: The fence precedes callbacks and lease refresh, while aborts and failed serialization leave disk unchanged.
func TestMonitorOrdinaryPreCallbackAndFailure(t *testing.T) {
	p := monitorSeed(t)
	monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"old"`) })
	before := monitorDisk(t, p)
	effect := false
	_, err := UpdateState(p, "a", func(s *State) error {
		effect = true
		if s.Writer.ForeignSeen.IsZero() || validWriterSeal(monitorLoad(t, p)) {
			t.Fatal("check happened after callback")
		}
		s.Writer.ForeignSeen = time.Time{}
		s.LegacyPending = json.RawMessage(`{`)
		return nil
	})
	if err == nil || !effect || !bytes.Equal(before, monitorDisk(t, p)) {
		t.Fatal("failure ordering: callback can run but failed save must not claim durable fence")
	}
	if monitorLoad(t, p).Writer.ForeignSeen.IsZero() == false {
		t.Fatal("falsely claimed durable flag after failed save")
	}
	abort := errors.New("fixture abort")
	_, err = UpdateState(p, "a", func(*State) error { return abort })
	if !errors.Is(err, abort) || !bytes.Equal(before, monitorDisk(t, p)) {
		t.Fatal("callback abort wrote state")
	}
	_, err = UpdateState(p, "a", func(s *State) error {
		return s.Acquire("a", "fixture-session", "fixture-host", 123, time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC), LeaseTTL, nil)
	})
	if err != nil || monitorLoad(t, p).Writer.ForeignSeen.IsZero() {
		t.Fatal("ordinary lease refresh lost fence", err)
	}
}

// Proves: Implicit admission refuses missing/invalid seals and retained history; explicit first arm has a narrow bootstrap exception.
func TestMonitorMissingInvalidAndHistory(t *testing.T) {
	cases := map[string]func(*testing.T, Paths){
		"missing": func(t *testing.T, p Paths) { seedStateJSON(t, p, `{"next_batch":"legacy"}`) },
		"pre-preservation": func(t *testing.T, p Paths) {
			seedStateJSON(t, p, `{"next_batch":"legacy","lease":{"holder":"owner","pid":123,"expires":"2030-01-01T00:00:00Z"}}`)
		},
		"protocol1": func(t *testing.T, p Paths) {
			s := monitorLoad(t, p)
			s.Writer.Protocol = 1
			b, e := sealedState(s)
			if e != nil {
				t.Fatal(e)
			}
			seedStateJSON(t, p, string(b))
		},
		"mismatch": func(t *testing.T, p Paths) {
			monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"changed"`) })
		},
		"missing-after-history": func(t *testing.T, p Paths) {
			if e := monitorRun(p, MonitorArm, func(*MonitorGeneration) error { return nil }); e != nil {
				t.Fatal(e)
			}
			monitorOldWrite(t, p, func(m map[string]json.RawMessage) { delete(m, "writer") })
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p := monitorSeed(t)
			change(t, p)
			before := monitorDisk(t, p)
			for _, entry := range []MonitorEntry{MonitorAutomatic, MonitorRecovery} {
				err := monitorRun(p, entry, func(*MonitorGeneration) error { t.Fatal("refused callback ran"); return nil })
				if !errors.Is(err, ErrIncompatibleWriter) || !bytes.Equal(before, monitorDisk(t, p)) {
					t.Fatal("refusal changed state", err)
				}
			}
			err := monitorRun(p, MonitorArm, func(*MonitorGeneration) error { return nil })
			first := name == "missing" || name == "pre-preservation"
			if first && err != nil {
				t.Fatal("explicit first arm refused", err)
			}
			if !first && (!errors.Is(err, ErrIncompatibleWriter) || !bytes.Equal(before, monitorDisk(t, p))) {
				t.Fatal("arm repaired invalid existing seal/history", err)
			}
			if e := WithRuntimeLock(p, "a", "consumer", time.Now(), func() error { return nil }); e != nil {
				t.Fatal("refused admission retained consumer lock", e)
			}
		})
	}
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { return nil }); err != nil {
		t.Fatal("valid neighbor refused", err)
	}
}

// Proves: Renewal, announcement and close detect foreign writes without changing State or mailbox and release authority.
func TestMonitorDetectionIsByteIdenticalAndStops(t *testing.T) {
	for _, action := range []string{"renewal", "announcement", "close"} {
		t.Run(action, func(t *testing.T) {
			p := monitorSeed(t)
			mb, err := OpenMailbox(p, "a")
			if err != nil {
				t.Fatal(err)
			}
			_, err = mb.Append([]MailEntry{{ID: "fixture-unread", Kind: MailEvent, Room: "fixture-room", Event: json.RawMessage(`{"body":"BODY_TOPIC_DISPLAY_RAW_KIND_ERROR_SENTINEL"}`)}})
			if err != nil {
				t.Fatal(err)
			}
			mailBefore, err := os.ReadFile(p.MailboxFile("a"))
			if err != nil {
				t.Fatal(err)
			}
			var handle *MonitorGeneration
			err = monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
				handle = g
				monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"old"`) })
				before := monitorDisk(t, p)
				emitted := false
				var e error
				switch action {
				case "renewal":
					e = g.CheckRenewal(monitorRecord().Generation)
				case "announcement":
					e = g.Announce(monitorRecord().Generation, func() error { emitted = true; return nil })
				case "close":
					e = g.Close()
				}
				if !errors.Is(e, ErrIncompatibleWriter) || emitted || !bytes.Equal(before, monitorDisk(t, p)) {
					t.Fatal("detection wrote state or emitted", e)
				}
				active, reason := g.Status()
				if active || reason != "incompatible_writer" {
					t.Fatal("wrong local status")
				}
				if e := WithRuntimeLock(p, "a", "consumer", time.Now(), func() error { return nil }); e != nil {
					t.Fatal("authority lock retained", e)
				}
				for _, entry := range []MonitorEntry{MonitorAutomatic, MonitorRecovery, MonitorArm} {
					if e := monitorRun(p, entry, func(*MonitorGeneration) error { t.Fatal("restart admitted"); return nil }); !errors.Is(e, ErrIncompatibleWriter) {
						t.Fatal(e)
					}
				}
				if !bytes.Equal(before, monitorDisk(t, p)) {
					t.Fatal("restart changed invalid bytes")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if e := handle.Announce(monitorRecord().Generation, func() error { t.Fatal("stopped handle emitted"); return nil }); !errors.Is(e, ErrMonitorGeneration) {
				t.Fatal(e)
			}
			after, _ := os.ReadFile(p.MailboxFile("a"))
			mb, err = OpenMailbox(p, "a")
			if err != nil || !bytes.Equal(mailBefore, after) || len(mb.Pending()) != 1 || mb.Received("fixture-unread") {
				t.Fatal("monitor touched mailbox")
			}
		})
	}
}

// Proves: A concurrent renewal detects a completed old-writer edit without persisting repair; this is one ordered interleaving.
func TestMonitorConcurrentRenewalMismatch(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		start := make(chan struct{})
		done := make(chan error, 1)
		cancel := make(chan struct{})
		defer close(cancel)
		go func() {
			select {
			case <-start:
				done <- g.CheckRenewal(monitorRecord().Generation)
			case <-cancel:
			}
		}()
		monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"old"`) })
		before := monitorDisk(t, p)
		close(start)
		select {
		case e := <-done:
			if !errors.Is(e, ErrIncompatibleWriter) {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("renewal did not finish")
		}
		if !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("concurrent detection wrote state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Proves: Value-preserving old writes are undetectable, while compatible reseals retain detected history.
func TestMonitorNoOpOldHistoryHasNoNumericBound(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		for i := 0; i < 4; i++ {
			// The old same-host pid Acquire history leaves the known-session lease
			// unchanged. Re-saving identical values provides no evidence to a seal.
			monitorOldWrite(t, p, func(map[string]json.RawMessage) {})
			if err := g.CheckRenewal(monitorRecord().Generation); err != nil {
				t.Fatal("value-preserving history became detectable", err)
			}
		}
		for i := 0; i < 3; i++ {
			monitorOldWrite(t, p, func(m map[string]json.RawMessage) {
				m["next_batch"] = json.RawMessage(`"old-mutation` + strings.Repeat("-next", i) + `"`)
			})
			if _, err := UpdateState(p, "a", func(s *State) error { s.PutHookSession(HookSession{ID: "fixture-hook", Blocks: i + 1}); return nil }); err != nil {
				t.Fatal(err)
			}
			if !validWriterSeal(monitorLoad(t, p)) || monitorLoad(t, p).Writer.ForeignSeen.IsZero() {
				t.Fatal("compatible transform hid old mutation")
			}
		}
		if e := g.CheckRenewal(monitorRecord().Generation); !errors.Is(e, ErrIncompatibleWriter) {
			t.Fatal("sticky flag did not close repeated history", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Proves: Conflicting custody returns its specific closed error; a valid sink runs without State/control locks.
func TestMonitorExclusionAndSink(t *testing.T) {
	for _, kind := range []string{"terminal", "native", "host", "consumer-lock"} {
		t.Run(kind, func(t *testing.T) {
			p := monitorSeed(t)
			var release func()
			switch kind {
			case "terminal":
				_, _ = UpdateState(p, "a", func(s *State) error { s.Terminal = &TerminalBinding{}; return nil })
			case "native":
				_, _ = UpdateState(p, "a", func(s *State) error { s.Runtime = &codexwake.Binding{Gate: "unknown"}; return nil })
			case "host":
				scope, a, now := hostFixture()
				scope.Agent = "a"
				a.Scope = scope
				_, e := UpdateState(p, "a", func(s *State) error {
					s.Lease = &a.Lease
					return s.EnrollHost(a, scope, now)
				})
				if e != nil {
					t.Fatal(e)
				}
			case "consumer-lock":
				var e error
				release, e = AcquireRawConsumerLock(p, "a", time.Now())
				if e != nil {
					t.Fatal(e)
				}
				defer release()
			}
			before := monitorDisk(t, p)
			err := monitorRun(p, MonitorArm, func(*MonitorGeneration) error { t.Fatal("conflicting generation ran"); return nil })
			want := ErrMonitorConflict
			if kind == "consumer-lock" {
				want = ErrLockTimeout
			}
			if !errors.Is(err, want) || !bytes.Equal(before, monitorDisk(t, p)) {
				t.Fatal("conflicting admission changed state", err)
			}
		})
	}
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		writes := 0
		if err := g.Announce(monitorRecord().Generation, func() error {
			writes++
			// Output must not hold state/control locks: inbox may run immediately.
			if err := WithRuntimeLock(p, "a", "control", time.Now(), func() error {
				_, err := UpdateState(p, "a", func(s *State) error { s.LastInbox = "fixture-frame"; return nil })
				return err
			}); err != nil {
				t.Fatal("sink held prompt locks", err)
			}
			return nil
		}); err != nil || writes != 1 {
			t.Fatal("valid sink refused", err)
		}
		for _, foreign := range []*MonitorGeneration{nil, {}, &MonitorGeneration{record: monitorRecord()}} {
			if e := foreign.Announce(monitorRecord().Generation, func() error { t.Fatal("foreign consumer emitted"); return nil }); !errors.Is(e, ErrMonitorGeneration) {
				t.Fatal(e)
			}
		}
		if e := g.Announce(strings.Repeat("b", 32), func() error { t.Fatal("second generation emitted"); return nil }); !errors.Is(e, ErrMonitorGeneration) {
			t.Fatal(e)
		}
		if e := WithMonitorGeneration(p, "a", time.Now(), MonitorAutomatic, monitorRecord(), func(*MonitorGeneration) error { t.Fatal("second monitor"); return nil }); !errors.Is(e, ErrLockTimeout) {
			t.Fatal(e)
		}
		s := monitorLoad(t, p)
		deadCalled := false
		if e := s.Acquire("a", "other", "fixture-host", 55, time.Now(), LeaseTTL, func(string) bool { deadCalled = true; return true }); !errors.Is(e, ErrMonitorConflict) || deadCalled {
			t.Fatal("competing Acquire probed/mutated", e)
		}
		lease := *s.Lease
		if e := s.Acquire("a", lease.Holder, lease.Host, 999, time.Now(), LeaseTTL, nil); e != nil || !reflect.DeepEqual(lease, *s.Lease) {
			t.Fatal("same owner not borrowed without renewal", e)
		}
		if _, e := s.Release("a", lease.Holder, true, time.Now(), nil); !errors.Is(e, ErrMonitorConflict) {
			t.Fatal("force release bypass", e)
		}
		before := monitorDisk(t, p)
		for _, change := range []func(*State){func(s *State) { s.Monitor = nil }, func(s *State) { s.Monitor.Phase = "stopped" }, func(s *State) { s.Runtime = &codexwake.Binding{} }} {
			if _, e := UpdateState(p, "a", func(s *State) error { change(s); return nil }); !errors.Is(e, ErrMonitorConflict) {
				t.Fatal("ordinary writer changed monitor authority", e)
			}
		}
		if !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("refusal changed state")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Proves: Closed monitor decoding rejects malformed authority with valid phases as neighbours.
func TestMonitorClosedSchemaAndPhases(t *testing.T) {
	good, _ := json.Marshal(monitorRecord())
	for _, bad := range []string{
		strings.Replace(string(good), `"version":1`, `"version":2`, 1),
		strings.Replace(string(good), `"kind":"monitor"`, `"kind":"RAW_KIND_SENTINEL"`, 1),
		strings.Replace(string(good), `"phase":"streaming"`, `"phase":"ERROR_SENTINEL"`, 1),
		strings.TrimSuffix(string(good), "}") + `,"future_authority":true}`,
		strings.TrimSuffix(string(good), "}") + `,"PHASE":"stopped"}`,
		strings.TrimSuffix(string(good), "}") + `,"phase":"stopped"}`,
		`null`, `[]`, `"BODY_SENTINEL"`,
	} {
		p := monitorPaths(t)
		seedStateJSON(t, p, `{"monitor":`+bad+`}`)
		before := monitorDisk(t, p)
		if _, e := UpdateState(p, "a", func(*State) error { t.Fatal("invalid authority callback"); return nil }); !errors.Is(e, ErrMonitorState) || strings.Contains(e.Error(), "SENTINEL") {
			t.Fatal("wrong closed error", e)
		}
		if !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("malformed authority overwritten")
		}
	}
	for _, alias := range []string{`"MONITOR":`, `"monitor":`, `"monITOR":`} {
		p := monitorPaths(t)
		seedStateJSON(t, p, `{"monitor":`+string(good)+`,`+alias+string(good)+`}`)
		if _, e := LoadState(p, "a"); !errors.Is(e, ErrMonitorState) {
			t.Fatal("duplicate top-level authority admitted", e)
		}
	}
	for _, phase := range []string{"starting", "waiting-client", "streaming", "paused", "stopping", "error", "unknown", "stopped"} {
		m := monitorRecord()
		m.Phase = phase
		s := State{Monitor: &m}
		if m.Validate() != nil || (s.RefuseMonitorConsumer() != nil) != (phase != "stopped") {
			t.Fatal("phase gate", phase)
		}
	}
}

// Proves: Concurrent compatible writers preserve all updates and the sticky fence under the State lock.
func TestMonitorConcurrentOrdinaryFence(t *testing.T) {
	p := monitorSeed(t)
	monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"old"`) })
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := UpdateState(p, "a", func(s *State) error { s.Threads = append(s.Threads, "ordinary"); return nil })
			errs <- e
		}()
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary writers did not finish")
	}
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	s := monitorLoad(t, p)
	if len(s.Threads) != 8 || s.Writer.ForeignSeen.IsZero() || !validWriterSeal(s) {
		t.Fatal("concurrent fence/update lost")
	}
}

// Payload comparisons retain all previous custody/ledger assertions while
// separately checking the intentionally additive writer seal and disk input.
func sameStatePayload(a, b State) bool {
	a.Writer, b.Writer = nil, nil
	a.rawState, b.rawState = nil, nil
	return reflect.DeepEqual(a, b)
}

// Proves: Directory-sync faults distinguish unchanged pre-rename input from uncertain post-rename output.
func TestMonitorOrdinaryPersistenceFailureBoundaries(t *testing.T) {
	for _, failAt := range []int{0, 1, 2} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			p := monitorSeed(t)
			old := monitorDisk(t, p)
			next := []byte(`{"next_batch":"replacement-fixture"}`)
			calls := 0
			err := writeOrdinaryStateAtomic(p.StateFile("a"), "a", next, func(string) error {
				calls++
				if calls == failAt {
					return errors.New("fixture directory sync failure")
				}
				return nil
			})
			want := next
			if failAt == 1 {
				want = old
				if err == nil || errors.Is(err, ErrStateWriteUncertain) {
					t.Fatal("pre-rename error misreported", err)
				}
			}
			if failAt == 2 && !errors.Is(err, ErrStateWriteUncertain) {
				t.Fatal("post-rename outcome claimed durable", err)
			}
			if failAt == 0 && err != nil {
				t.Fatal("valid persistence neighbor", err)
			}
			if !bytes.Equal(want, monitorDisk(t, p)) {
				t.Fatal("failure ordering bytes")
			}
		})
	}
}

// Proves: Only explicit same-scope arm clears a valid sticky fence and replaces retained generation history.
func TestMonitorExplicitRearmRetainedFence(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"old"`) })
		if e := g.CheckRenewal(monitorRecord().Generation); !errors.Is(e, ErrIncompatibleWriter) {
			t.Fatal(e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The ordinary writer seals and keeps the row, never persisting stopped or
	// last_error on the monitor's detection path.
	if _, err := UpdateState(p, "a", func(s *State) error { s.SetHookBlocks("ordinary", 1); return nil }); err != nil {
		t.Fatal(err)
	}
	s := monitorLoad(t, p)
	if s.Monitor.Phase != "streaming" || s.Writer.ForeignSeen.IsZero() {
		t.Fatal("lost retained history/fence")
	}
	next := monitorRecord()
	next.Generation = strings.Repeat("b", 32)
	before := monitorDisk(t, p)
	for _, entry := range []MonitorEntry{MonitorAutomatic, MonitorRecovery} {
		if e := WithMonitorGeneration(p, "a", time.Now().Add(time.Second), entry, next, func(*MonitorGeneration) error { t.Fatal("implicit rearm"); return nil }); !errors.Is(e, ErrIncompatibleWriter) {
			t.Fatal(e)
		}
	}
	for _, foreign := range []string{"host", "session"} {
		m := next
		if foreign == "host" {
			m.Host = "foreign"
		} else {
			m.SessionID = "foreign"
		}
		if e := WithMonitorGeneration(p, "a", time.Now().Add(time.Second), MonitorArm, m, func(*MonitorGeneration) error { t.Fatal("foreign rearm"); return nil }); !errors.Is(e, ErrMonitorConflict) {
			t.Fatal(e)
		}
	}
	if !bytes.Equal(before, monitorDisk(t, p)) {
		t.Fatal("refusal cleared fence")
	}
	if e := WithMonitorGeneration(p, "a", time.Now().Add(time.Second), MonitorArm, next, func(g *MonitorGeneration) error {
		if !monitorLoad(t, p).Writer.ForeignSeen.IsZero() {
			t.Fatal("explicit same-scope rearm did not clear")
		}
		return g.CheckRenewal(next.Generation)
	}); e != nil {
		t.Fatal("explicit rearm retained row must be usable", e)
	}
}

// Proves: Output/decode failures use closed errors, and ambiguous writer metadata is fenced by ordinary work.
func TestMonitorClosedErrorsAndAmbiguousWriter(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		if e := g.Announce(monitorRecord().Generation, func() error { return errors.New("RAW_ERROR_BODY_TOPIC_DISPLAY_KIND_SENTINEL") }); !errors.Is(e, ErrMonitorOutput) || strings.Contains(e.Error(), "SENTINEL") {
			t.Fatal("sink error escaped closed vocabulary", e)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p = monitorSeed(t)
	raw := string(monitorDisk(t, p))
	raw = strings.Replace(raw, `"protocol": 2`, `"protocol": 1,"PROTOCOL": 2`, 1)
	seedStateJSON(t, p, raw)
	if e := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { t.Fatal("ambiguous writer admitted"); return nil }); !errors.Is(e, ErrIncompatibleWriter) {
		t.Fatal(e)
	}
	// Ordinary compatibility still works and durably flags the ambiguous input.
	if _, e := UpdateState(p, "a", func(s *State) error { s.NextBatch = "ordinary"; return nil }); e != nil || monitorLoad(t, p).Writer.ForeignSeen.IsZero() {
		t.Fatal("ordinary ambiguity handling", e)
	}
	p = monitorSeed(t)
	seedStateJSON(t, p, `{"lease":{"expires":"RAW_ERROR_SENTINEL"}}`)
	if e := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { t.Fatal("malformed state admitted"); return nil }); !errors.Is(e, ErrMonitorUnavailable) || strings.Contains(e.Error(), "SENTINEL") {
		t.Fatal("raw decode error escaped", e)
	}
}
