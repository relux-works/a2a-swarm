package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// foreignStateWrite changes the busy fixture in place under the state lock
// without resealing it, like a writer that predates the compatible seal.
func foreignStateWrite(t *testing.T, p Paths, change func(map[string]json.RawMessage)) {
	t.Helper()
	path := p.StateFile(busyStateName)
	unlock, err := lockFile(filepath.Join(filepath.Dir(path), busyStateName+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	change(m)
	if data, err = json.Marshal(m); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// staleFormattedWrite edits the loaded state in place and re-emits it in the
// writer's own format, keeping the old writer member and seal unchanged. The
// bytes then differ from the sealed input only in content, which is exactly
// the input an elision that skipped seal verification would wrongly accept.
func staleFormattedWrite(t *testing.T, p Paths, change func(*State)) {
	t.Helper()
	path := p.StateFile(busyStateName)
	unlock, err := lockFile(filepath.Join(filepath.Dir(path), busyStateName+".lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	st, err := LoadState(p, busyStateName)
	if err != nil {
		t.Fatal(err)
	}
	change(&st)
	data, err := marshalState(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func busyDisk(t *testing.T, p Paths) []byte {
	t.Helper()
	data, err := os.ReadFile(p.StateFile(busyStateName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func busyLoad(t *testing.T, p Paths) State {
	t.Helper()
	st, err := LoadState(p, busyStateName)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func busyNoop(p Paths) func() error {
	return func() error {
		_, err := UpdateState(p, busyStateName, func(st *State) error { st.Terminal.Idle = nil; st.Terminal.Outcome = syntheticBusyOutcome; return nil })
		return err
	}
}

// Proves elision never skips compatible-writer fencing: every foreign or
// stale input, including one that already carries a sticky fence, is still
// rewritten by a no-op transaction with a valid seal, a fence and the foreign
// value kept; only the next no-op on that resealed input is elided and the
// sticky timestamp survives it.
func TestUpdateStateUnchangedForeignInputStillFenced(t *testing.T) {
	stale := func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"s-foreign-batch"`) }
	cases := map[string]func(*testing.T, Paths){
		"unsealed": func(t *testing.T, p Paths) {
			foreignStateWrite(t, p, func(m map[string]json.RawMessage) { delete(m, "writer"); stale(m) })
		},
		"tampered seal": func(t *testing.T, p Paths) {
			foreignStateWrite(t, p, func(m map[string]json.RawMessage) {
				m["writer"] = json.RawMessage(`{"protocol":2,"seal":"` + strings.Repeat("f", 64) + `"}`)
				stale(m)
			})
		},
		"stale seal": func(t *testing.T, p Paths) { foreignStateWrite(t, p, stale) },
		"old protocol with matching digest": func(t *testing.T, p Paths) {
			foreignStateWrite(t, p, func(m map[string]json.RawMessage) {
				stale(m)
				m["writer"] = json.RawMessage(`{"protocol":1,"seal":""}`)
				data, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				digest, err := stateDigest(data)
				if err != nil {
					t.Fatal(err)
				}
				m["writer"] = json.RawMessage(`{"protocol":1,"seal":"` + digest + `"}`)
			})
		},
		"already fenced then stale": func(t *testing.T, p Paths) {
			foreignStateWrite(t, p, func(m map[string]json.RawMessage) { delete(m, "writer") })
			if err := busyNoop(p)(); err != nil {
				t.Fatal(err)
			}
			if st := busyLoad(t, p); !validWriterSeal(st) || st.Writer.ForeignSeen.IsZero() {
				t.Fatal("setup did not produce a sealed fenced input")
			}
			foreignStateWrite(t, p, stale)
		},
		// The decisive case for the seal precondition: same format, same
		// writer member and sticky fence, stale seal over changed content.
		"already fenced then stale in writer format": func(t *testing.T, p Paths) {
			foreignStateWrite(t, p, func(m map[string]json.RawMessage) { delete(m, "writer") })
			if err := busyNoop(p)(); err != nil {
				t.Fatal(err)
			}
			staleFormattedWrite(t, p, func(st *State) { st.NextBatch = "s-foreign-batch" })
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := seedSyntheticBusyState(t)
			prepare(t, p)
			if validWriterSeal(busyLoad(t, p)) {
				t.Fatal("negative control: prepared input still carries a valid seal")
			}
			if n := countStateRewrites(t, p, busyStateName, busyNoop(p)); n != 1 {
				t.Fatalf("foreign input rewritten %d times, want 1", n)
			}
			fenced := busyLoad(t, p)
			first := fenced.Writer.ForeignSeen
			if !validWriterSeal(fenced) || first.IsZero() || fenced.NextBatch != "s-foreign-batch" || len(fenced.ThreadEvents) != syntheticThreadRecords {
				t.Fatal("foreign input was not resealed with a fence and its value")
			}
			if n := countStateRewrites(t, p, busyStateName, busyNoop(p), busyNoop(p)); n != 0 {
				t.Fatalf("resealed fenced input rewritten %d times, want 0", n)
			}
			if again := busyLoad(t, p); !again.Writer.ForeignSeen.Equal(first) || !validWriterSeal(again) {
				t.Fatal("sticky fence changed across elided transactions")
			}
		})
	}
}

// Proves malformed or duplicate authority members still refuse the whole
// transaction before the callback, with no write: the elision is never
// reached for input that LoadState rejects.
func TestUpdateStateUnchangedMalformedAuthorityRefused(t *testing.T) {
	cases := map[string]func(map[string]json.RawMessage){
		"duplicate writer alias": func(m map[string]json.RawMessage) { m["Writer"] = m["writer"] },
		"null monitor owner":     func(m map[string]json.RawMessage) { m["monitor_owner"] = json.RawMessage(`null`) },
		"null migration":         func(m map[string]json.RawMessage) { m["migration"] = json.RawMessage(`null`) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := seedSyntheticBusyState(t)
			foreignStateWrite(t, p, change)
			before := busyDisk(t, p)
			called := false
			replaced, err := stateReplaced(t, p, busyStateName, func() error {
				_, err := UpdateState(p, busyStateName, func(*State) error { called = true; return nil })
				return err
			})
			if !errors.Is(err, ErrMonitorState) || called || replaced || !bytes.Equal(before, busyDisk(t, p)) {
				t.Fatalf("malformed authority: err=%v called=%v replaced=%v", err, called, replaced)
			}
		})
	}
}

// Proves unknown top-level members survive both an elided no-op and a later
// changed write with their exact number lexemes; elision does not route
// around opaque preservation.
func TestUpdateStateUnchangedPreservesOpaque(t *testing.T) {
	p, _ := seedSyntheticBusyState(t)
	const future = `{"z":9007199254740993,"a":[1.0,1e0,-0]}`
	foreignStateWrite(t, p, func(m map[string]json.RawMessage) { m["synthetic_future"] = json.RawMessage(future) })
	if err := busyNoop(p)(); err != nil {
		t.Fatal(err)
	}
	sealed := busyDisk(t, p)
	if n := countStateRewrites(t, p, busyStateName, busyNoop(p), busyNoop(p)); n != 0 || !bytes.Equal(sealed, busyDisk(t, p)) {
		t.Fatalf("opaque input rewritten %d times or changed", n)
	}
	if _, err := UpdateState(p, busyStateName, func(st *State) error { st.Terminal.Outcome = syntheticIdleOutcome; return nil }); err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(busyDisk(t, p), &m); err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, m["synthetic_future"]); err != nil || compact.String() != future {
		t.Fatalf("opaque member = %s, %v", compact.String(), err)
	}
	if st := busyLoad(t, p); !validWriterSeal(st) || st.Terminal.Outcome != syntheticIdleOutcome {
		t.Fatal("changed write after elision was not sealed")
	}
}

// Proves a no-op transaction falls back to the pre-existing replacement
// whenever the path no longer holds the verified input at decision time:
// a non-cooperating replacement, a widened mode or a removed file. The
// returned state and the final file are the transaction's own representation,
// never a success claim over bytes it did not verify.
func TestUpdateStateUnchangedInterveningChangeFallsBack(t *testing.T) {
	cases := map[string]func(*testing.T, Paths){
		"replaced": func(t *testing.T, p Paths) {
			path := p.StateFile(busyStateName)
			tmp := path + ".synthetic-foreign"
			s := syntheticBusyState()
			s.NextBatch = "s-intervening-batch"
			s.Writer = &StateWriter{Protocol: stateProtocol}
			data, err := sealedState(s)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(tmp, path); err != nil {
				t.Fatal(err)
			}
		},
		"mode widened": func(t *testing.T, p Paths) {
			if err := os.Chmod(p.StateFile(busyStateName), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"removed": func(t *testing.T, p Paths) {
			if err := os.Remove(p.StateFile(busyStateName)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, intervene := range cases {
		t.Run(name, func(t *testing.T) {
			p, raw := seedSyntheticBusyState(t)
			got, err := UpdateState(p, busyStateName, func(st *State) error {
				intervene(t, p)
				st.Terminal.Outcome = syntheticBusyOutcome
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(p.StateFile(busyStateName))
			if err != nil || info.Mode().Perm() != 0o600 {
				t.Fatal("fallback did not restore a 0600 state file", err)
			}
			if !bytes.Equal(busyDisk(t, p), raw) || got.NextBatch != "s-synthetic-batch" || !validWriterSeal(got) {
				t.Fatal("no-op claimed success without persisting its own representation")
			}
		})
	}
}

// Proves existing refusals and failure modes still win before elision, with
// no write: a callback error and a callback that sets a migration fence (the
// callback ran), and a migration-fenced input, a widened file mode,
// cancellation and a lock timeout (the callback never ran).
func TestUpdateStateUnchangedRefusalsWithoutWrite(t *testing.T) {
	sentinel := errors.New("synthetic callback refusal")
	cases := map[string]struct {
		prepare func(*testing.T, Paths) func()
		run     func(Paths, func(*State) error) error
		want    func(error) bool
		calls   bool
	}{
		"callback error": {
			run: func(p Paths, fn func(*State) error) error {
				_, e := UpdateState(p, busyStateName, func(st *State) error { fn(st); return sentinel })
				return e
			},
			want:  func(e error) bool { return errors.Is(e, sentinel) },
			calls: true,
		},
		"callback sets migration": {
			run: func(p Paths, fn func(*State) error) error {
				_, e := UpdateState(p, busyStateName, func(st *State) error {
					fn(st)
					st.Migration = &IdentityMigration{Phase: "in_progress", DaemonBase: "/synthetic", At: time.Unix(1, 0).UTC()}
					return nil
				})
				return e
			},
			want:  func(e error) bool { return errors.Is(e, ErrIdentityMigrated) },
			calls: true,
		},
		"migration fenced input": {
			prepare: func(t *testing.T, p Paths) func() {
				s := syntheticBusyState()
				s.Migration = &IdentityMigration{Phase: "in_progress", DaemonBase: "/synthetic", At: time.Unix(1, 0).UTC()}
				s.Writer = &StateWriter{Protocol: stateProtocol}
				data, err := sealedState(s)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p.StateFile(busyStateName), append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			run:  func(p Paths, fn func(*State) error) error { _, e := UpdateState(p, busyStateName, fn); return e },
			want: func(e error) bool { return errors.Is(e, ErrIdentityMigrated) },
		},
		"widened mode refused": {
			prepare: func(t *testing.T, p Paths) func() {
				if err := os.Chmod(p.StateFile(busyStateName), 0o644); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			run:  func(p Paths, fn func(*State) error) error { _, e := UpdateState(p, busyStateName, fn); return e },
			want: func(e error) bool { return e != nil && strings.Contains(e.Error(), "want 0600") },
		},
		"cancelled": {
			run: func(p Paths, fn func(*State) error) error {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, e := UpdateStateContext(ctx, p, busyStateName, time.Time{}, fn)
				return e
			},
			want: func(e error) bool { return errors.Is(e, context.Canceled) },
		},
		"lock timeout": {
			prepare: func(t *testing.T, p Paths) func() {
				unlock, err := lockFile(filepath.Join(filepath.Dir(p.StateFile(busyStateName)), busyStateName+".lock"))
				if err != nil {
					t.Fatal(err)
				}
				return unlock
			},
			run: func(p Paths, fn func(*State) error) error {
				_, e := UpdateStateBy(p, busyStateName, time.Now().Add(50*time.Millisecond), fn)
				return e
			},
			want: func(e error) bool { return errors.Is(e, ErrLockTimeout) },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := seedSyntheticBusyState(t)
			if c.prepare != nil {
				if release := c.prepare(t, p); release != nil {
					defer release()
				}
			}
			before := busyDisk(t, p)
			called := false
			replaced, err := stateReplaced(t, p, busyStateName, func() error {
				return c.run(p, func(*State) error { called = true; return nil })
			})
			if !c.want(err) || called != c.calls || replaced || !bytes.Equal(before, busyDisk(t, p)) {
				t.Fatalf("err=%v called=%v replaced=%v", err, called, replaced)
			}
		})
	}
}

// Proves every effective change is still persisted exactly once, including
// the authority and arrival changes a busy cycle can observe (lease renewal,
// generation replacement, cursor move, new thread record, idle qualification),
// and that repeating the same change is then elided.
func TestUpdateStateUnchangedChangesStillPersist(t *testing.T) {
	expires := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	idle := &TerminalIdle{At: time.Date(2030, 1, 1, 0, 0, 1, 0, time.UTC), Generation: strings.Repeat("0", 32), SessionID: "synthetic-session"}
	extra := ThreadEvent{Room: syntheticID("!", "room", 99), EventID: syntheticID("$", "event", 99999), Root: syntheticID("$", "root", 99)}
	cases := map[string]struct {
		change func(*State)
		check  func(State) bool
	}{
		"lease renewal": {
			change: func(st *State) {
				st.Lease = &Lease{Holder: "synthetic-session", Host: "synthetic-host", PID: 4, Expires: expires}
			},
			check: func(st State) bool { return st.Lease != nil && st.Lease.Expires.Equal(expires) },
		},
		"generation replaced": {
			change: func(st *State) { st.Terminal.Generation = strings.Repeat("1", 32) },
			check:  func(st State) bool { return st.Terminal.Generation == strings.Repeat("1", 32) },
		},
		"cursor moved": {
			change: func(st *State) { st.NextBatch = "s-synthetic-next" },
			check:  func(st State) bool { return st.NextBatch == "s-synthetic-next" },
		},
		"thread record learned": {
			change: func(st *State) { st.RememberThreadEvent(extra) },
			check: func(st State) bool {
				return len(st.ThreadEvents) == syntheticThreadRecords && st.ThreadEventRoot(extra.Room, extra.EventID) == extra.Root
			},
		},
		"idle qualification": {
			change: func(st *State) { st.Terminal.Idle = idle; st.Terminal.Outcome = "armed: qualifying idle" },
			check:  func(st State) bool { return st.Terminal.Idle != nil && st.Terminal.Idle.At.Equal(idle.At) },
		},
		"outcome busy to idle": {
			change: func(st *State) { st.Terminal.Outcome = syntheticIdleOutcome },
			check:  func(st State) bool { return st.Terminal.Outcome == syntheticIdleOutcome },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, _ := seedSyntheticBusyState(t)
			op := func() error {
				_, err := UpdateState(p, busyStateName, func(st *State) error { c.change(st); return nil })
				return err
			}
			if n := countStateRewrites(t, p, busyStateName, op); n != 1 {
				t.Fatalf("change rewritten %d times, want 1", n)
			}
			if st := busyLoad(t, p); !c.check(st) || !validWriterSeal(st) {
				t.Fatal("change not persisted as a sealed state")
			}
			if n := countStateRewrites(t, p, busyStateName, op); n != 0 {
				t.Fatalf("repeated identical change rewritten %d times, want 0", n)
			}
		})
	}
}

// Proves the elided path keeps old/new snapshots independent: the returned
// state is not the callback's working copy, and mutating either afterwards
// changes neither the other nor the disk input of the next transaction.
func TestUpdateStateUnchangedSnapshotsIndependent(t *testing.T) {
	p, raw := seedSyntheticBusyState(t)
	var working *TerminalBinding
	var workingEvents []ThreadEvent
	var got State
	replaced, err := stateReplaced(t, p, busyStateName, func() error {
		var err error
		got, err = UpdateState(p, busyStateName, func(st *State) error {
			working, workingEvents = st.Terminal, st.ThreadEvents
			return nil
		})
		return err
	})
	if err != nil || replaced {
		t.Fatalf("setup: err=%v replaced=%v", err, replaced)
	}
	if got.Terminal == working || &got.ThreadEvents[0] == &workingEvents[0] || !validWriterSeal(got) {
		t.Fatal("returned state aliases the callback working copy")
	}
	working.Outcome = "mutated-working"
	workingEvents[0].Room = "mutated-working"
	if got.Terminal.Outcome != syntheticBusyOutcome || got.ThreadEvents[0].Room != syntheticID("!", "room", 0) {
		t.Fatal("working copy mutation reached the returned state")
	}
	got.Terminal.Outcome = "mutated-returned"
	got.ThreadEvents[0].Room = "mutated-returned"
	if _, err := UpdateState(p, busyStateName, func(st *State) error {
		if st.Terminal.Outcome != syntheticBusyOutcome || st.ThreadEvents[0].Room != syntheticID("!", "room", 0) {
			return errors.New("returned state mutation reached the next working copy")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, busyDisk(t, p)) {
		t.Fatal("snapshot mutations reached the disk")
	}
}

// Proves the elided path keeps the replacement's durability taxonomy on a
// real filesystem: an unreadable base directory (parent sync) fails the no-op
// with no write, an unreadable state directory yields ErrStateWriteUncertain
// with no write, and restoring access succeeds. Skipped for root, which
// bypasses directory read permission.
func TestUpdateStateUnchangedDirectorySyncFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	cases := map[string]struct {
		dir  func(Paths) string
		want func(error) bool
	}{
		"parent sync": {
			dir:  func(p Paths) string { return p.Base },
			want: func(e error) bool { return e != nil && !errors.Is(e, ErrStateWriteUncertain) },
		},
		"state directory sync": {
			dir:  func(p Paths) string { return filepath.Dir(p.StateFile(busyStateName)) },
			want: func(e error) bool { return errors.Is(e, ErrStateWriteUncertain) },
		},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p, raw := seedSyntheticBusyState(t)
			dir := c.dir(p)
			if err := os.Chmod(dir, 0o300); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(dir, 0o700) })
			replaced, err := stateReplaced(t, p, busyStateName, busyNoop(p))
			if !c.want(err) || replaced {
				t.Fatalf("err=%v replaced=%v", err, replaced)
			}
			if err := os.Chmod(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if replaced, err := stateReplaced(t, p, busyStateName, busyNoop(p)); err != nil || replaced || !bytes.Equal(raw, busyDisk(t, p)) {
				t.Fatalf("positive control: err=%v replaced=%v", err, replaced)
			}
		})
	}
}

// Proves the helper repeats writeOrdinaryStateAtomic's directory sync order
// and error taxonomy exactly: parent then state directory; a parent failure is
// returned as is and a state-directory failure is ErrStateWriteUncertain.
func TestConfirmOrdinaryStateUnchangedMatchesWriter(t *testing.T) {
	sentinel := errors.New("synthetic sync failure")
	for _, failAt := range []int{-1, 0, 1} {
		path := filepath.Join(t.TempDir(), "state", "synthetic.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		results := make([]error, 2)
		calls := make([][]string, 2)
		for i, run := range []func(func(string) error) error{
			func(sync func(string) error) error { return confirmOrdinaryStateUnchanged(path, sync) },
			func(sync func(string) error) error {
				return writeOrdinaryStateAtomic(path, "synthetic", []byte("{}\n"), sync)
			},
		} {
			results[i] = run(func(dir string) error {
				calls[i] = append(calls[i], dir)
				if len(calls[i])-1 == failAt {
					return sentinel
				}
				return nil
			})
		}
		want := []string{filepath.Dir(filepath.Dir(path)), filepath.Dir(path)}
		if failAt == 0 {
			want = want[:1]
		}
		for i := range results {
			if strings.Join(calls[i], "|") != strings.Join(want, "|") {
				t.Fatalf("failAt=%d route %d synced %v, want %v", failAt, i, calls[i], want)
			}
		}
		switch failAt {
		case -1:
			if results[0] != nil || results[1] != nil {
				t.Fatalf("success: %v, %v", results[0], results[1])
			}
		case 0:
			if !errors.Is(results[0], sentinel) || !errors.Is(results[1], sentinel) || errors.Is(results[0], ErrStateWriteUncertain) {
				t.Fatalf("parent failure: %v, %v", results[0], results[1])
			}
		case 1:
			if !errors.Is(results[0], ErrStateWriteUncertain) || !errors.Is(results[1], ErrStateWriteUncertain) {
				t.Fatalf("state directory failure: %v, %v", results[0], results[1])
			}
		}
	}
}

// Proves the unchanged decision requires byte identity with the verified
// input plus exactly one trailing newline: extra trailing whitespace, compact
// formatting and a changed member all report changed (and a no-op on such a
// file normalizes it once), while an exact input reports unchanged.
func TestUnchangedSealedStateRequiresExactBytes(t *testing.T) {
	p, raw := seedSyntheticBusyState(t)
	sealedDecode := func(data []byte) (State, State) {
		var old, s State
		if err := unmarshalState(data, &old); err != nil {
			t.Fatal(err)
		}
		if err := unmarshalState(data, &s); err != nil {
			t.Fatal(err)
		}
		fence, sealed := ordinaryWriterFenceSealed(old, time.Now())
		if !sealed {
			t.Fatal("variant lost its valid seal")
		}
		s.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
		return old, s
	}
	old, s := sealedDecode(raw)
	if !unchangedSealedState(old, s) {
		t.Fatal("exact sealed input reported changed")
	}
	s.NextBatch = "s-changed"
	if unchangedSealedState(old, s) {
		t.Fatal("changed member reported unchanged")
	}
	if unchangedSealedState(State{rawState: old.rawState}, s) {
		t.Fatal("input without writer metadata reported unchanged")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		t.Fatal(err)
	}
	for name, variant := range map[string][]byte{
		"extra trailing newlines": append(bytes.Clone(raw), '\n', '\n'),
		"compact":                 compact.Bytes(),
	} {
		if old, s := sealedDecode(variant); unchangedSealedState(old, s) {
			t.Fatalf("%s reported unchanged", name)
		}
		if err := os.WriteFile(p.StateFile(busyStateName), variant, 0o600); err != nil {
			t.Fatal(err)
		}
		if n := countStateRewrites(t, p, busyStateName, busyNoop(p)); n != 1 || !bytes.Equal(raw, busyDisk(t, p)) {
			t.Fatalf("%s: rewritten %d times or not normalized", name, n)
		}
	}
}

// Proves the split fence helper keeps ordinaryWriterFence's decision for every
// input class and reports sealed only for a non-empty input with a valid seal.
func TestOrdinaryWriterFenceSealedMatchesFence(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	p, _ := seedSyntheticBusyState(t)
	sealedInput := busyLoad(t, p)
	foreignStateWrite(t, p, func(m map[string]json.RawMessage) { delete(m, "writer") })
	unsealedInput := busyLoad(t, p)
	if err := busyNoop(p)(); err != nil {
		t.Fatal(err)
	}
	fencedInput := busyLoad(t, p)
	foreignStateWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"s-stale"`) })
	staleFencedInput := busyLoad(t, p)
	sticky := fencedInput.Writer.ForeignSeen
	if sticky.IsZero() || !staleFencedInput.Writer.ForeignSeen.Equal(sticky) {
		t.Fatal("setup did not produce a sticky fence")
	}
	// Expected fences restate the pre-existing contract independently: no
	// input or a valid unfenced seal gives none, an unsealed input is fenced
	// now, and a sticky fence is kept whether or not its seal still verifies.
	for name, c := range map[string]struct {
		in     State
		fence  time.Time
		sealed bool
	}{
		"missing":      {State{}, time.Time{}, false},
		"sealed":       {sealedInput, time.Time{}, true},
		"unsealed":     {unsealedInput, now, false},
		"fenced":       {fencedInput, sticky, true},
		"stale fenced": {staleFencedInput, sticky, false},
	} {
		fence, sealed := ordinaryWriterFenceSealed(c.in, now)
		if !fence.Equal(c.fence) || !ordinaryWriterFence(c.in, now).Equal(c.fence) || sealed != c.sealed {
			t.Fatalf("%s: fence=%v sealed=%v", name, fence, sealed)
		}
	}
}
