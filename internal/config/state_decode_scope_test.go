package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// memoFixture returns a plain Paths, the same Paths carrying an open scope for
// alice, and the scope.
func memoFixture(t *testing.T) (Paths, Paths, *StateDecodeScope) {
	t.Helper()
	plain := Paths{Base: t.TempDir()}
	sc := NewStateDecodeScope("alice")
	sc.Open()
	t.Cleanup(sc.Close)
	return plain, plain.WithStateDecodeScope(sc), sc
}

func writeStateBytes(t *testing.T, p Paths, name string, data []byte) {
	t.Helper()
	path := p.StateFile(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".fixture"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// richState persists a realistic State with nested terminal, alert content,
// wake ledger, subscriptions, hook sessions, thread events and opaque members.
func richState(t *testing.T, p Paths) []byte {
	t.Helper()
	orphaned := true
	_, err := UpdateState(p, "alice", func(s *State) error {
		s.NextBatch = "s1"
		s.Threads = []string{"$r1", "$r2"}
		s.Muted = []string{"$m1"}
		s.LegacyAcked = []string{"$a1"}
		s.LegacySubscriptions = map[string][]string{"!room": {"msg", "decision"}}
		s.HookSessions = []HookSession{{ID: "h1", Blocks: 1, Rung: []string{"$e1", "$e2"}}}
		for i := 0; i < 3; i++ {
			s.RememberThreadEvent(ThreadEvent{Room: "!room", EventID: fmt.Sprintf("$e%d", i), Root: "$r1"})
		}
		s.AgentDMContexts = json.RawMessage(`{"!dm":{"kind":"agent","n":1.000000000000000000001}}`)
		s.Lease = &Lease{Holder: "sess", Host: "h", PID: 7, Expires: time.Unix(2000000000, 0).UTC()}
		s.Terminal = &TerminalBinding{Generation: "g1", SessionID: "sess", Host: "h", Rung: []string{"$e1"}, AttemptIDs: []string{"$e1"},
			Reservations: []time.Time{time.Unix(1900000000, 0).UTC()}, OwnNotice: OwnNoticeObservation{PossiblyOrphaned: &orphaned, Reason: "r"},
			NoticeReport: &NoticeReport{Since: "x", Message: LivenessAlertMessage{Txn: "t", Room: "!room", Content: map[string]any{"body": "b", "nested": map[string]any{"list": []any{"a", 1.5, true, nil}}}}},
			Idle:         &TerminalIdle{Generation: "g1", Target: tmuxwake.Target{Socket: "/s", Pane: "%0"}}, NoticePosition: &tmuxwake.ComposerPosition{Row: 1, Column: 2},
			Readiness: &TerminalReadiness{Version: "v"}, Recovery: &TerminalRecovery{Reason: TerminalRecovered, At: time.Unix(1800000000, 0).UTC()}}
		s.Wake = codexwake.Ledger{Attempt: &codexwake.Attempt{ID: "w1", IDs: []string{"$e1"}}, Rung: []string{"$e2"}, LastMailIDs: []string{"$e3"}, Reservations: []time.Time{time.Unix(1700000000, 0).UTC()}}
		s.Liveness = &AgentStatus{Name: "alice", State: "idle", OwnNotice: OwnNoticeObservation{PossiblyOrphaned: &orphaned}}
		s.LivenessAlerts = map[string]LivenessAlert{"bob": {Value: "stale", Pending: &LivenessAlertMessage{Txn: "t2", Room: "!r", Content: map[string]any{"deep": map[string]any{"k": []any{map[string]any{"x": "y"}}}}}}}
		s.MailAlarm.Jobs = []MailAlarmJob{{Kind: "k", ID: "j1"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	// Add an unknown top-level member with exact numeric precision.
	data = bytes.Replace(data, []byte("{\n"), []byte("{\n  \"zz_opaque\": {\"n\": 1.000000000000000000001, \"l\": [1e0, -0]},\n"), 1)
	writeStateBytes(t, p, "alice", data)
	return data
}

func ordinaryState(t *testing.T, p Paths) State {
	t.Helper()
	plain := p
	plain.stateDecode = nil
	s, err := LoadState(plain, "alice")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// Claim: two same-input LoadStates under one open epoch perform two real file
// reads but one pure typed parse, and return equal, independently owned results.
func TestStateMemoTwoReadsOneParse(t *testing.T) {
	plain, scoped, sc := memoFixture(t)
	richState(t, plain)
	want := ordinaryState(t, plain)
	before := ReadStateDecodeStats()
	a, err := LoadState(scoped, "alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadState(scoped, "alice")
	if err != nil {
		t.Fatal(err)
	}
	after := ReadStateDecodeStats()
	if got := after.StateReadAttempts - before.StateReadAttempts; got != 2 {
		t.Fatalf("read attempts=%d want 2", got)
	}
	if got := after.UnmarshalStatePasses - before.UnmarshalStatePasses; got != 1 {
		t.Fatalf("typed parses=%d want 1", got)
	}
	st := sc.Stats()
	if st.Parses != 1 || st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("scope stats=%+v", st)
	}
	if !reflect.DeepEqual(a, want) || !reflect.DeepEqual(b, want) {
		t.Fatal("memo results differ from the ordinary decode")
	}
	a.Terminal.Rung[0] = "MUTATED"
	if b.Terminal.Rung[0] == "MUTATED" {
		t.Fatal("two LoadState results share memory")
	}
}

// Claim: without an open epoch (nil scope, closed scope, other identity) the
// decoder runs once per read, exactly as before; a valid neighbour hits.
func TestStateMemoOffEpochIsOrdinary(t *testing.T) {
	plain, scoped, sc := memoFixture(t)
	richState(t, plain)
	other := plain.WithStateDecodeScope(NewStateDecodeScope("bob")) // never opened, wrong identity
	for _, tc := range []struct {
		name string
		p    Paths
		prep func()
	}{
		{"nil scope", plain, func() {}},
		{"wrong identity", other, func() {}},
		{"closed epoch", scoped, sc.Close},
	} {
		tc.prep()
		before := ReadStateDecodeStats()
		for i := 0; i < 2; i++ {
			if _, err := LoadState(tc.p, "alice"); err != nil {
				t.Fatal(err)
			}
		}
		if got := ReadStateDecodeStats().UnmarshalStatePasses - before.UnmarshalStatePasses; got != 2 {
			t.Fatalf("%s: parses=%d want 2", tc.name, got)
		}
	}
	sc.Open()
	before := ReadStateDecodeStats()
	for i := 0; i < 2; i++ {
		if _, err := LoadState(scoped, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	if got := ReadStateDecodeStats().UnmarshalStatePasses - before.UnmarshalStatePasses; got != 1 {
		t.Fatalf("reopened epoch parses=%d want 1", got)
	}
}

// Claim: A->B->A with A resident parses two versions and every read returns
// its own current bytes; an epoch reopen starts empty.
func TestStateMemoABA(t *testing.T) {
	plain, scoped, sc := memoFixture(t)
	a := richState(t, plain)
	b := bytes.Replace(a, []byte(`"next_batch": "s1"`), []byte(`"next_batch": "s2"`), 1)
	if bytes.Equal(a, b) {
		t.Fatal("fixture")
	}
	for i, data := range [][]byte{a, b, a, b} {
		writeStateBytes(t, plain, "alice", data)
		s, err := LoadState(scoped, "alice")
		if err != nil {
			t.Fatal(err)
		}
		want := map[int]string{0: "s1", 1: "s2", 2: "s1", 3: "s2"}[i]
		if s.NextBatch != want || !bytes.Equal(s.rawState, data) {
			t.Fatalf("read %d: next_batch=%q", i, s.NextBatch)
		}
	}
	if st := sc.Stats(); st.Parses != 2 || st.Hits != 2 {
		t.Fatalf("stats=%+v want 2 parses, 2 hits", st)
	}
	sc.Close()
	sc.Open()
	if _, err := LoadState(scoped, "alice"); err != nil {
		t.Fatal(err)
	}
	if st := sc.Stats(); st.Parses != 3 {
		t.Fatalf("new epoch reused an old frame: %+v", st)
	}
}

// SAME_LENGTH_MIGRATION_OR_BULK_CHANGE. Claim: an input with identical length
// and restored mtime but different bytes misses and yields the exact ordinary
// value or refusal (migration, authority alias, bulk thread, opaque, host
// null). Kills M1 (a size/mtime-only hit). Positive control: nil migration.
func TestStateMemoSameLengthChangeMisses(t *testing.T) {
	cases := []struct {
		name    string
		a, b    string
		wantErr error
		check   func(State) bool
	}{
		{"migration", `{"zzzzzzzzz":{"phase":"started","daemon_base":"/d","at":"2026-01-01T00:00:00Z"},"next_batch":"s1"}`,
			`{"migration":{"phase":"started","daemon_base":"/d","at":"2026-01-01T00:00:00Z"},"next_batch":"s1"}`, nil,
			func(s State) bool { return s.Migration != nil && s.Migration.Phase == "started" }},
		{"authority alias", `{"writer":{"protocol":2,"seal":""},"Writex":{"protocol":2,"seal":""}}`,
			`{"writer":{"protocol":2,"seal":""},"Writer":{"protocol":2,"seal":""}}`, ErrMonitorState, nil},
		{"bulk thread", `{"threads":["$aaaa","$bbbb","$cccc"]}`, `{"threads":["$aaaa","$bxbb","$cccc"]}`, nil,
			func(s State) bool { return len(s.Threads) == 3 && s.Threads[1] == "$bxbb" }},
		{"opaque", `{"zz":1.000000000000000000001,"next_batch":"s"}`, `{"zz":1.000000000000000000002,"next_batch":"s"}`, nil,
			func(s State) bool { return string(s.opaque["zz"]) == "1.000000000000000000002" }},
		{"host null", `{"host_enrollmenx":null}`, `{"host_enrollment":null}`, ErrHostEnrollment, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plain, scoped, sc := memoFixture(t)
			if len(tc.a) != len(tc.b) {
				t.Fatal("fixture lengths differ")
			}
			writeStateBytes(t, plain, "alice", []byte(tc.a))
			first, err := LoadState(scoped, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if first.Migration != nil {
				t.Fatal("positive control: nil migration expected")
			}
			info, _ := os.Stat(plain.StateFile("alice"))
			writeStateBytes(t, plain, "alice", []byte(tc.b))
			os.Chtimes(plain.StateFile("alice"), info.ModTime(), info.ModTime())
			got, err := LoadState(scoped, "alice")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err=%v want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || !tc.check(got) || !reflect.DeepEqual(got, ordinaryState(t, plain)) {
				t.Fatalf("stale result: %+v err=%v", got, err)
			}
			if st := sc.Stats(); st.Hits != 0 || st.Parses != 2 {
				t.Fatalf("changed bytes hit a frame: %+v", st)
			}
			if tc.name == "migration" {
				if err := CheckIdentityMigration(scoped, "alice"); !errors.Is(err, ErrIdentityMigrated) {
					t.Fatalf("migration guard=%v", err)
				}
				if _, err := UpdateState(scoped, "alice", func(*State) error { return nil }); !errors.Is(err, ErrIdentityMigrated) {
					t.Fatalf("writer migration refusal=%v", err)
				}
				if data, _ := os.ReadFile(plain.StateFile("alice")); string(data) != tc.b {
					t.Fatal("refused writer changed the file")
				}
			}
		})
	}
}

// DEEP_ALIAS_ROLLBACK. Claim: mutating every leaf of a hit result, of a writer
// working copy (with rollback), or of the caller's input buffer never changes
// the frame, another reader or the writer's old snapshot; a real update beside
// them persists exactly once. Kills M2 (shared frame or working copy).
func TestStateMemoDeepAliasRollback(t *testing.T) {
	plain, scoped, _ := memoFixture(t)
	data := richState(t, plain)
	want := ordinaryState(t, plain)
	first, err := LoadState(scoped, "alice")
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadState(scoped, "alice") // hit
	if err != nil {
		t.Fatal(err)
	}
	mutateEverything(t, &first)
	mutateEverything(t, &second)
	third, err := LoadState(scoped, "alice") // hit after both results were mutated
	if err != nil || !reflect.DeepEqual(third, want) {
		t.Fatalf("frame changed by a mutated result: %v", err)
	}
	// Rollback: a callback mutates nested state and fails.
	sentinel := errors.New("rollback")
	old, err := UpdateState(scoped, "alice", func(s *State) error {
		mutateEverything(t, s)
		return sentinel
	})
	if !errors.Is(err, sentinel) || !reflect.DeepEqual(old, want) {
		t.Fatalf("writer old snapshot changed by its callback: %v", err)
	}
	if disk, _ := os.ReadFile(plain.StateFile("alice")); !bytes.Equal(disk, data) {
		t.Fatal("failed callback wrote state")
	}
	if again, _ := LoadState(scoped, "alice"); !reflect.DeepEqual(again, want) {
		t.Fatal("frame changed by a writer callback")
	}
	// The caller's input buffer is not retained by a frame.
	buf := append([]byte(nil), data...)
	if _, err := decodeFreshState(scoped, "alice", plain.StateFile("alice"), buf); err != nil {
		t.Fatal(err)
	}
	for i := range buf {
		buf[i] = ' '
	}
	if again, _ := LoadState(scoped, "alice"); !reflect.DeepEqual(again, want) {
		t.Fatal("frame aliases the caller's input buffer")
	}
	// Positive: a real nested update persists exactly once with a fresh seal.
	saved, err := UpdateState(scoped, "alice", func(s *State) error {
		s.Terminal.Rung[0] = "$changed"
		s.LivenessAlerts["bob"].Pending.Content["deep"].(map[string]any)["k"] = "v2"
		return nil
	})
	if err != nil || saved.Terminal.Rung[0] != "$changed" {
		t.Fatalf("update: %v", err)
	}
	disk := ordinaryState(t, plain)
	if disk.Terminal.Rung[0] != "$changed" || disk.LivenessAlerts["bob"].Pending.Content["deep"].(map[string]any)["k"] != "v2" || !validWriterSeal(disk) {
		t.Fatal("update not persisted with a valid seal")
	}
	var compact bytes.Buffer
	if json.Compact(&compact, disk.opaque["zz_opaque"]) != nil || compact.String() != `{"n":1.000000000000000000001,"l":[1e0,-0]}` {
		t.Fatalf("opaque precision lost: %s", disk.opaque["zz_opaque"])
	}
	if !reflect.DeepEqual(saved.Terminal, disk.Terminal) || bytes.HasSuffix(saved.rawState, []byte("\n")) || !bytes.HasSuffix(disk.rawState, []byte("\n")) {
		t.Fatal("saved representation differs from the persisted bytes")
	}
}

// CHANGED_WRITER_INPUT_FENCE. Claim: each writer verifies the seal of its own
// current input under its own lock: after a sealed transaction in the same
// epoch, an unsealed foreign replacement is fenced and never elided. Kills M4
// (seal/fence reused across locks). Positive control: sealed no-op elides.
func TestStateMemoChangedWriterInputFence(t *testing.T) {
	plain, scoped, _ := memoFixture(t)
	if _, err := UpdateState(plain, "alice", func(s *State) error { s.Threads = []string{"$r"}; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(scoped, "alice", func(s *State) error { s.NextBatch = "s1"; return nil }); err != nil {
		t.Fatal(err)
	}
	sealed, _ := os.ReadFile(plain.StateFile("alice"))
	before := ReadStateDecodeStats()
	if _, err := UpdateState(scoped, "alice", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if after, _ := os.ReadFile(plain.StateFile("alice")); !bytes.Equal(after, sealed) {
		t.Fatal("positive control: sealed no-op was rewritten")
	}
	if ReadStateDecodeStats().WriterSealChecks-before.WriterSealChecks < 1 {
		t.Fatal("writer did not verify its input seal")
	}
	if ordinaryState(t, plain).Writer.ForeignSeen.IsZero() == false {
		t.Fatal("positive control fenced")
	}
	// Foreign writer: same content, seal removed.
	var m map[string]json.RawMessage
	json.Unmarshal(sealed, &m)
	m["writer"] = json.RawMessage(`{"protocol":1,"seal":""}`)
	foreign, _ := json.MarshalIndent(m, "", "  ")
	writeStateBytes(t, plain, "alice", append(foreign, '\n'))
	before = ReadStateDecodeStats()
	if _, err := UpdateState(scoped, "alice", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if ReadStateDecodeStats().WriterSealChecks-before.WriterSealChecks < 1 {
		t.Fatal("no fresh seal check for the changed input")
	}
	got := ordinaryState(t, plain)
	if got.Writer.ForeignSeen.IsZero() || !validWriterSeal(got) {
		t.Fatalf("foreign input not fenced: %+v", got.Writer)
	}
}

// Claim: sticky invalid/ambiguous writer metadata survives a frame hit and the
// writer clone, so the next write fences instead of laundering it.
func TestStateMemoAmbiguousWriterNotLaundered(t *testing.T) {
	plain, scoped, _ := memoFixture(t)
	writeStateBytes(t, plain, "alice", []byte(`{"writer":{"protocol":2,"seal":"x","Seal":"y"},"next_batch":"s"}`))
	for i := 0; i < 2; i++ {
		s, err := LoadState(scoped, "alice")
		if err != nil || s.Writer == nil || !s.Writer.ambiguous {
			t.Fatalf("read %d lost ambiguous writer metadata: %v", i, err)
		}
	}
	if _, err := UpdateState(scoped, "alice", func(s *State) error { s.LastInbox = "i"; return nil }); err != nil {
		t.Fatal(err)
	}
	if ordinaryState(t, plain).Writer.ForeignSeen.IsZero() {
		t.Fatal("ambiguous writer input was not fenced")
	}
}

// Claim: unsafe or changed file system objects take the ordinary path and
// error after a primed frame: wrong mode, symlink, FIFO, unreadable, missing,
// recreated. No stale success.
func TestStateMemoUnsafeFileSystem(t *testing.T) {
	plain, scoped, _ := memoFixture(t)
	data := richState(t, plain)
	path := plain.StateFile("alice")
	prime := func() {
		writeStateBytes(t, plain, "alice", data)
		if _, err := LoadState(scoped, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	prime()
	os.Chmod(path, 0o644)
	if _, err := LoadState(scoped, "alice"); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("wrong mode: %v", err)
	}
	prime()
	target := filepath.Join(t.TempDir(), "target.json")
	os.WriteFile(target, data, 0o600)
	os.Remove(path)
	os.Symlink(target, path)
	if _, err := LoadState(scoped, "alice"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink: %v", err)
	}
	os.Remove(path)
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(scoped, "alice"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("fifo: %v", err)
	}
	os.Remove(path)
	if s, err := LoadState(scoped, "alice"); err != nil || !reflect.DeepEqual(s, State{}) {
		t.Fatalf("missing: %+v %v", s, err)
	}
	prime()
	os.Chmod(path, 0) // unreadable: refused at the mode check before any read
	if _, err := LoadState(scoped, "alice"); err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("unreadable: %v", err)
	}
	os.Chmod(path, 0o600)
	recreated := bytes.Replace(data, []byte(`"next_batch": "s1"`), []byte(`"next_batch": "s9"`), 1)
	os.Remove(path)
	writeStateBytes(t, plain, "alice", recreated)
	if s, err := LoadState(scoped, "alice"); err != nil || s.NextBatch != "s9" {
		t.Fatalf("recreated: %v", err)
	}
}

// Claim: split roots fence independently: a migration in either root refuses
// even when the other root's identical-looking input is resident.
func TestStateMemoSplitRootFences(t *testing.T) {
	for _, migratedRoot := range []string{"base", "agent"} {
		t.Run(migratedRoot, func(t *testing.T) {
			base, agentBase := t.TempDir(), t.TempDir()
			sc := NewStateDecodeScope("alice")
			sc.Open()
			defer sc.Close()
			p := Paths{Base: base, AgentStateBase: agentBase}.WithStateDecodeScope(sc)
			clean := []byte(`{"next_batch":"s1"}`)
			writeStateBytes(t, Paths{Base: base}, "alice", clean)
			writeStateBytes(t, Paths{Base: agentBase}, "alice", clean)
			if err := CheckIdentityMigration(p, "alice"); err != nil {
				t.Fatalf("positive control: %v", err)
			}
			migrated := []byte(`{"migration":{"phase":"started","daemon_base":"/d","at":"2026-01-01T00:00:00Z"},"next_batch":"s1"}`)
			root := base
			if migratedRoot == "agent" {
				root = agentBase
			}
			writeStateBytes(t, Paths{Base: root}, "alice", migrated)
			if err := CheckIdentityMigration(p, "alice"); !errors.Is(err, ErrIdentityMigrated) {
				t.Fatalf("migration in %s root not refused: %v", migratedRoot, err)
			}
		})
	}
}

// Claim: each frozen bound is enforced independently; inputs beyond a bound
// (including a real >1 MiB State) decode ordinarily with the correct result,
// no new error, and bounded retained data; LRU evicts and reparses honestly.
func TestStateMemoBounds(t *testing.T) {
	versions := func(n int) [][]byte {
		var out [][]byte
		for i := 0; i < n; i++ {
			out = append(out, []byte(fmt.Sprintf(`{"next_batch":"s%03d","threads":["$a","$b"]}`, i)))
		}
		return out
	}
	one := func() State {
		var s State
		unmarshalState(versions(1)[0], &s)
		return s
	}()
	c := parsedStateCharge(&one)
	raw := int64(len(versions(1)[0]))
	for _, tc := range []struct {
		name   string
		limits stateMemoLimits
	}{
		{"entries", stateMemoLimits{entries: 2, input: 1 << 20, raw: 1 << 20, charge: 1 << 20, nod: 1 << 20}},
		{"raw", stateMemoLimits{entries: 32, input: 1 << 20, raw: 2*raw + 1, charge: 1 << 20, nod: 1 << 20}},
		{"charge", stateMemoLimits{entries: 32, input: 1 << 20, raw: 1 << 20, charge: 2*c.bytes + 1, nod: 1 << 20}},
		{"nodes", stateMemoLimits{entries: 32, input: 1 << 20, raw: 1 << 20, charge: 1 << 20, nod: 2*c.nodes + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plain, scoped, sc := memoFixture(t)
			sc.limits = tc.limits
			vs := versions(3)
			for round := 0; round < 2; round++ {
				for i, v := range vs {
					writeStateBytes(t, plain, "alice", v)
					s, err := LoadState(scoped, "alice")
					if err != nil || s.NextBatch != fmt.Sprintf("s%03d", i) {
						t.Fatalf("round %d version %d: %v", round, i, err)
					}
				}
			}
			st := sc.Stats()
			if st.PeakEntries > 2 || st.Evictions == 0 || st.Parses != 6 || st.Hits != 0 {
				t.Fatalf("bound not enforced: %+v", st)
			}
			if sc.raw > tc.limits.raw || sc.charge > tc.limits.charge || sc.nodes > tc.limits.nod {
				t.Fatalf("retained beyond bound: raw=%d charge=%d nodes=%d", sc.raw, sc.charge, sc.nodes)
			}
		})
	}
	t.Run("oversize", func(t *testing.T) {
		plain, scoped, sc := memoFixture(t)
		var threads []string
		for i := 0; len(strings.Join(threads, ",")) < 1<<20; i++ {
			threads = append(threads, fmt.Sprintf("$%060d", i))
		}
		big, _ := json.Marshal(map[string]any{"threads": threads, "next_batch": "big"})
		writeStateBytes(t, plain, "alice", big)
		for i := 0; i < 2; i++ {
			s, err := LoadState(scoped, "alice")
			if err != nil || s.NextBatch != "big" || len(s.Threads) != len(threads) {
				t.Fatalf("oversize valid input: %v", err)
			}
		}
		if st := sc.Stats(); st.FallbackOversize != 2 || st.Admits != 0 || sc.raw != 0 {
			t.Fatalf("oversize retained or not counted: %+v", st)
		}
	})
	t.Run("graph larger than every bound", func(t *testing.T) {
		plain, scoped, sc := memoFixture(t)
		sc.limits.nod = 1
		writeStateBytes(t, plain, "alice", versions(1)[0])
		for i := 0; i < 2; i++ {
			if s, err := LoadState(scoped, "alice"); err != nil || s.NextBatch != "s000" {
				t.Fatal(err)
			}
		}
		if st := sc.Stats(); st.FallbackNotAdmitted != 2 || st.Admits != 0 {
			t.Fatalf("%+v", st)
		}
	})
}

// Claim: an invalid input is never a frame: every read returns the ordinary
// error, and a later valid input in the same epoch decodes normally.
func TestStateMemoParseFailureNotCached(t *testing.T) {
	plain, scoped, sc := memoFixture(t)
	writeStateBytes(t, plain, "alice", []byte(`{"next_batch":`))
	_, want := LoadState(plain, "alice")
	for i := 0; i < 2; i++ {
		if _, err := LoadState(scoped, "alice"); err == nil || err.Error() != want.Error() {
			t.Fatalf("err=%v want %v", err, want)
		}
	}
	if st := sc.Stats(); st.Admits != 0 || st.Parses != 2 {
		t.Fatalf("%+v", st)
	}
	writeStateBytes(t, plain, "alice", []byte(`{"next_batch":"ok"}`))
	if s, err := LoadState(scoped, "alice"); err != nil || s.NextBatch != "ok" {
		t.Fatal(err)
	}
}

// Claim: concurrent readers of one input share a single parse (singleflight)
// and each gets an independent copy; a close during the parse publishes
// nothing and strands no waiter; a parser failure releases its waiters.
func TestStateMemoConcurrency(t *testing.T) {
	t.Run("singleflight", func(t *testing.T) {
		plain, scoped, sc := memoFixture(t)
		richState(t, plain)
		want := ordinaryState(t, plain)
		release := make(chan struct{})
		var once sync.Once
		stateScopeParseHook = func() { once.Do(func() { <-release }) }
		defer func() { stateScopeParseHook = nil }()
		var wg sync.WaitGroup
		results := make([]State, 8)
		for i := range results {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				s, err := LoadState(scoped, "alice")
				if err != nil {
					t.Error(err)
				}
				results[i] = s
			}(i)
		}
		for sc.Stats().Waits < 7 {
			time.Sleep(time.Millisecond)
		}
		close(release)
		wg.Wait()
		if st := sc.Stats(); st.Parses != 1 || st.Hits != 7 {
			t.Fatalf("%+v", st)
		}
		for i := range results {
			if !reflect.DeepEqual(results[i], want) {
				t.Fatal("concurrent result differs")
			}
			results[i].Terminal.Rung[0] = fmt.Sprint(i)
		}
		for i := range results {
			if results[i].Terminal.Rung[0] != fmt.Sprint(i) {
				t.Fatal("concurrent results alias")
			}
		}
	})
	t.Run("close during parse", func(t *testing.T) {
		plain, scoped, sc := memoFixture(t)
		richState(t, plain)
		want := ordinaryState(t, plain)
		release := make(chan struct{})
		var once sync.Once
		stateScopeParseHook = func() { once.Do(func() { <-release }) }
		defer func() { stateScopeParseHook = nil }()
		done := make(chan State, 2)
		for i := 0; i < 2; i++ {
			go func() { s, _ := LoadState(scoped, "alice"); done <- s }()
		}
		for sc.Stats().Waits < 1 {
			time.Sleep(time.Millisecond)
		}
		sc.Close()
		sc.Open()
		close(release)
		for i := 0; i < 2; i++ {
			if s := <-done; !reflect.DeepEqual(s, want) {
				t.Fatal("result across close differs")
			}
		}
		if st := sc.Stats(); st.Admits != 0 || st.FallbackClosed != 2 {
			t.Fatalf("late parse populated a new epoch: %+v", st)
		}
		if _, err := LoadState(scoped, "alice"); err != nil || sc.Stats().Parses != 2 {
			t.Fatalf("new epoch: %v %+v", err, sc.Stats())
		}
	})
	t.Run("parser failure releases waiters", func(t *testing.T) {
		plain, scoped, sc := memoFixture(t)
		writeStateBytes(t, plain, "alice", []byte(`{"next_batch":1}`))
		release := make(chan struct{})
		var once sync.Once
		stateScopeParseHook = func() { once.Do(func() { <-release }) }
		defer func() { stateScopeParseHook = nil }()
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			go func() { _, err := LoadState(scoped, "alice"); errs <- err }()
		}
		for sc.Stats().Waits < 1 {
			time.Sleep(time.Millisecond)
		}
		close(release)
		for i := 0; i < 2; i++ {
			if err := <-errs; err == nil {
				t.Fatal("invalid input succeeded")
			}
		}
	})
	t.Run("readers and writer race", func(t *testing.T) {
		plain, scoped, _ := memoFixture(t)
		richState(t, plain)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for j := 0; j < 10; j++ {
					s, err := LoadState(scoped, "alice")
					if err != nil {
						t.Error(err)
						return
					}
					s.Terminal.Rung = append(s.Terminal.Rung, "x")
					s.LivenessAlerts["bob"].Pending.Content["deep"] = j
				}
			}()
			go func(i int) {
				defer wg.Done()
				for j := 0; j < 5; j++ {
					if _, err := UpdateState(scoped, "alice", func(s *State) error { s.LastInbox = fmt.Sprint(i, j); return nil }); err != nil {
						t.Error(err)
					}
				}
			}(i)
		}
		wg.Wait()
		if s := ordinaryState(t, plain); !validWriterSeal(s) || len(s.Terminal.Rung) != 1 {
			t.Fatal("concurrent readers changed persisted state")
		}
	})
}

// Claim: the writer working copy is a clone only inside an open epoch for the
// scope identity; a failed save returns old with no frame for unsaved bytes.
func TestStateMemoWriterPaths(t *testing.T) {
	plain, scoped, sc := memoFixture(t)
	richState(t, plain)
	if _, err := UpdateState(scoped, "alice", func(s *State) error { s.LastInbox = "a"; return nil }); err != nil {
		t.Fatal(err)
	}
	if st := sc.Stats(); st.WriterClones != 1 {
		t.Fatalf("writer clone not used: %+v", st)
	}
	sc.Close()
	if _, err := UpdateState(scoped, "alice", func(s *State) error { s.LastInbox = "b"; return nil }); err != nil {
		t.Fatal(err)
	}
	if st := sc.Stats(); st.WriterClones != 1 {
		t.Fatalf("closed epoch cloned: %+v", st)
	}
	sc.Open()
	dir := filepath.Dir(plain.StateFile("alice"))
	before, _ := os.ReadFile(plain.StateFile("alice"))
	if os.Geteuid() != 0 {
		os.Chmod(dir, 0o500)
		old, err := UpdateState(scoped, "alice", func(s *State) error { s.LastInbox = "c"; return nil })
		os.Chmod(dir, 0o700)
		if err == nil || old.LastInbox != "b" {
			t.Fatalf("failed save: %v %q", err, old.LastInbox)
		}
		if after, _ := os.ReadFile(plain.StateFile("alice")); !bytes.Equal(after, before) {
			t.Fatal("failed save changed the file")
		}
	}
}

// --- clone coverage over the whole reachable State type graph ---

// fillValue sets every settable leaf reachable from v to a deterministic,
// path-specific non-zero value; slices and maps get two elements.
func fillValue(v reflect.Value, path string, seed int) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(fmt.Sprintf("%s#%d", path, seed))
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(int64(len(path) + seed))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(uint64(len(path)%200 + seed))
	case reflect.Float32, reflect.Float64:
		v.SetFloat(float64(len(path)) + 0.5)
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Struct:
		if v.Type() == timeType {
			v.Set(reflect.ValueOf(time.Unix(int64(1700000000+len(path)+seed), 0).UTC()))
			return
		}
		for i := 0; i < v.NumField(); i++ {
			if v.Field(i).CanSet() {
				fillValue(v.Field(i), path+"."+v.Type().Field(i).Name, seed)
			}
		}
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
		fillValue(v.Elem(), path, seed)
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 2, 2))
		for i := 0; i < 2; i++ {
			fillValue(v.Index(i), fmt.Sprintf("%s[%d]", path, i), seed)
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
		for i := 0; i < 2; i++ {
			k := reflect.New(v.Type().Key()).Elem()
			fillValue(k, fmt.Sprintf("%s{k%d}", path, i), seed)
			e := reflect.New(v.Type().Elem()).Elem()
			fillValue(e, fmt.Sprintf("%s{v%d}", path, i), seed)
			v.SetMapIndex(k, e)
		}
	case reflect.Interface:
		v.Set(reflect.ValueOf(map[string]any{"s": path, "n": float64(seed), "l": []any{"x", map[string]any{"b": true, "z": nil}}}))
	default:
		panic("unfilled kind " + v.Kind().String() + " at " + path)
	}
}

func populatedState(seed int) State {
	var s State
	fillValue(reflect.ValueOf(&s).Elem(), "State", seed)
	s.opaque = map[string]json.RawMessage{"zz": json.RawMessage(fmt.Sprintf(`{"n":1.000000000000000000001,"seed":%d}`, seed))}
	s.rawState = []byte(fmt.Sprintf(`{"raw":%d}`, seed))
	s.Writer.ambiguous = true
	return s
}

// mutateEverything changes every leaf reachable from s in place, including the
// unexported opaque/raw buffers, and adds a key to every map.
func mutateEverything(t *testing.T, s *State) int {
	t.Helper()
	n := mutateValue(reflect.ValueOf(s).Elem())
	for k, v := range s.opaque {
		for i := range v {
			v[i] = '!'
		}
		s.opaque[k] = v
		n++
	}
	for i := range s.rawState {
		s.rawState[i] = '!'
	}
	if s.Writer != nil {
		s.Writer.ambiguous = !s.Writer.ambiguous
	}
	return n + 2
}

func mutateValue(v reflect.Value) int {
	switch v.Kind() {
	case reflect.String:
		if v.CanSet() {
			v.SetString(v.String() + "!")
			return 1
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.CanSet() {
			v.SetInt(v.Int() + 1)
			return 1
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if v.CanSet() {
			v.SetUint(v.Uint() + 1)
			return 1
		}
	case reflect.Float32, reflect.Float64:
		if v.CanSet() {
			v.SetFloat(v.Float() + 1)
			return 1
		}
	case reflect.Bool:
		if v.CanSet() {
			v.SetBool(!v.Bool())
			return 1
		}
	case reflect.Struct:
		if v.Type() == timeType {
			if v.CanSet() {
				v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
				return 1
			}
			return 0
		}
		n := 0
		for i := 0; i < v.NumField(); i++ {
			n += mutateValue(v.Field(i))
		}
		return n
	case reflect.Pointer:
		if !v.IsNil() {
			return mutateValue(v.Elem())
		}
	case reflect.Slice:
		n := 0
		for i := 0; i < v.Len(); i++ {
			n += mutateValue(v.Index(i))
		}
		return n
	case reflect.Map:
		if v.IsNil() || !v.CanSet() && v.CanAddr() {
			return 0
		}
		n := 0
		for _, k := range v.MapKeys() {
			e := reflect.New(v.Type().Elem()).Elem()
			e.Set(v.MapIndex(k))
			n += mutateValue(e)
			v.SetMapIndex(k, e)
		}
		k := reflect.New(v.Type().Key()).Elem()
		if k.Kind() == reflect.String {
			k.SetString("added!")
			v.SetMapIndex(k, reflect.New(v.Type().Elem()).Elem())
			n++
		}
		return n
	case reflect.Interface:
		if v.IsNil() {
			return 0
		}
		e := v.Elem()
		switch e.Kind() {
		case reflect.Map, reflect.Slice:
			return mutateValue(e)
		default:
			if v.CanSet() {
				v.Set(reflect.ValueOf("mutated!"))
				return 1
			}
		}
	}
	return 0
}

// sharedMutable reports the first pointer, map or slice backing reachable
// from both a and b at the same address (strings and time zones excluded).
func sharedMutable(a, b reflect.Value, path string) string {
	switch a.Kind() {
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		return sharedMutable(a.Elem(), b.Elem(), path)
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return ""
		}
		if a.Pointer() == b.Pointer() {
			return path
		}
		it := a.MapRange()
		for it.Next() {
			if r := sharedMutable(it.Value(), b.MapIndex(it.Key()), path+"{}"); r != "" {
				return r
			}
		}
	case reflect.Slice:
		if a.Cap() > 0 && b.Cap() > 0 && a.Pointer() == b.Pointer() {
			return path
		}
		for i := 0; i < a.Len() && i < b.Len(); i++ {
			if r := sharedMutable(a.Index(i), b.Index(i), path+"[]"); r != "" {
				return r
			}
		}
	case reflect.Struct:
		if a.Type() == timeType {
			return ""
		}
		for i := 0; i < a.NumField(); i++ {
			if r := sharedMutable(a.Field(i), b.Field(i), path+"."+a.Type().Field(i).Name); r != "" {
				return r
			}
		}
	case reflect.Interface:
		if a.IsNil() || b.IsNil() || a.Elem().Type() != b.Elem().Type() {
			return ""
		}
		return sharedMutable(a.Elem(), b.Elem(), path)
	}
	return ""
}

// Claim: over the entire reachable State type graph (every field the reflect
// filler can reach, plus unexported opaque/raw/ambiguous), the clone is equal,
// shares no mutable memory, and mutation of either side never reaches the
// other. The expected values come from an independent deterministic filler,
// not from the clone helper's own inventory; a new mutable field copied by
// value fails here.
func TestStateCloneCoversWholeGraph(t *testing.T) {
	src := populatedState(1)
	clone, ok := cloneState(&src)
	if !ok || !reflect.DeepEqual(clone, src) {
		t.Fatal("clone differs from source")
	}
	if p := sharedMutable(reflect.ValueOf(clone), reflect.ValueOf(src), "State"); p != "" {
		t.Fatalf("clone shares mutable memory at %s", p)
	}
	if n := mutateEverything(t, &clone); n < 300 {
		t.Fatalf("mutator reached only %d leaves", n)
	}
	if !reflect.DeepEqual(src, populatedState(1)) {
		t.Fatal("mutating the clone changed the source")
	}
	clone2, _ := cloneState(&src)
	mutateEverything(t, &src)
	if !reflect.DeepEqual(clone2, populatedState(1)) {
		t.Fatal("mutating the source changed the clone")
	}
}

// Claim: nil versus non-nil empty containers survive cloning exactly.
func TestStateClonePreservesNilAndEmpty(t *testing.T) {
	var s State
	if c, ok := cloneState(&s); !ok || !reflect.DeepEqual(c, s) {
		t.Fatal("zero state")
	}
	e := populatedState(2)
	e.Threads, e.ThreadEvents, e.opaque, e.LivenessAlerts = []string{}, []ThreadEvent{}, map[string]json.RawMessage{}, map[string]LivenessAlert{}
	e.Terminal.Rung, e.rawState, e.AgentDMContexts = []string{}, []byte{}, json.RawMessage{}
	c, _ := cloneState(&e)
	if !reflect.DeepEqual(c, e) || c.Threads == nil || c.opaque == nil || c.Terminal.Rung == nil || c.rawState == nil || c.AgentDMContexts == nil {
		t.Fatal("empty containers became nil")
	}
}

// Claim: an unknown dynamic content type disables the clone (ordinary decode
// is used) instead of a shallow or partial copy.
func TestStateCloneUnsupportedDynamicValue(t *testing.T) {
	s := populatedState(3)
	s.LivenessAlerts["x"] = LivenessAlert{Pending: &LivenessAlertMessage{Content: map[string]any{"bad": []int{1}}}}
	if _, ok := cloneState(&s); ok {
		t.Fatal("unsupported dynamic value cloned")
	}
	plain, scoped, sc := memoFixture(t)
	richState(t, plain)
	old, _ := LoadState(plain, "alice")
	old.Terminal.NoticeReport.Message.Content["bad"] = []int{1} // not a JSON-decoded shape
	got, err := writerWorkingCopy(scoped, "alice", &old)
	if err != nil || sc.Stats().FallbackWriterClone != 1 || !reflect.DeepEqual(got, ordinaryState(t, plain)) {
		t.Fatalf("writer fallback: %v %+v", err, sc.Stats())
	}
}

// Claim: the clone is pinned to the reviewed schema; any field change in the
// reachable graph changes the signature and disables reuse.
func TestStateCloneSchemaPinned(t *testing.T) {
	if !stateCloneSchemaOK {
		t.Fatalf("State schema changed (%s): review cloneState and its coverage, then update stateCloneSchemaPin", stateSchemaSignature())
	}
	type extended struct {
		State
		Extra []string
	}
	if stateSchemaSignatureOf(reflect.TypeFor[extended]()) == stateSchemaSignature() {
		t.Fatal("signature does not detect a new field")
	}
}

// Claim: a populated State read from disk through a hit equals the ordinary
// decode for every field, including opaque precision and markers.
func TestStateMemoHitMatchesOrdinary(t *testing.T) {
	plain, scoped, _ := memoFixture(t)
	richState(t, plain)
	want := ordinaryState(t, plain)
	for i := 0; i < 3; i++ {
		got, err := LoadState(scoped, "alice")
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("read %d differs", i)
		}
	}
	if string(want.opaque["zz_opaque"]) != `{"n": 1.000000000000000000001, "l": [1e0, -0]}` {
		t.Fatalf("fixture opaque=%s", want.opaque["zz_opaque"])
	}
}
