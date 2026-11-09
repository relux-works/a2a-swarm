package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func ownerHook(t *testing.T, p Paths, runtime, session, event string, exit bool, now time.Time) {
	t.Helper()
	if err := RecordMonitorOwnerHook(p, "a", time.Now().Add(time.Second), runtime, session, event, exit, now); err != nil {
		t.Fatal(err)
	}
}

// Proves local Start/refresh/replacement/end and exact TTL boundaries with
// stale/foreign controls. It does not qualify a runtime's hook installation.
func TestMonitorOwnerLifecycle(t *testing.T) {
	for _, runtime := range []string{"muse", "claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			p := testPaths(t)
			now := time.Now().UTC()
			ownerHook(t, p, runtime, "session-A", "SessionStart", false, now)
			s := monitorLoad(t, p)
			if s.MonitorConfig == nil || s.MonitorConfig.OwnerTTL != 12*time.Hour || s.MonitorOwner.Runtime != runtime || s.MonitorOwner.SessionIDHash != ownerHash("session-A") || !s.MonitorOwner.FirstSeen.Equal(now) || !validWriterSeal(s) {
				t.Fatalf("start: %+v", s)
			}
			if bytes.Contains(monitorDisk(t, p), []byte("session-A")) {
				t.Fatal("raw session retained in owner record")
			}
			ownerHook(t, p, runtime, "session-A", "UserPromptSubmit", false, now.Add(time.Hour))
			s = monitorLoad(t, p)
			if !s.MonitorOwner.FirstSeen.Equal(now) || !s.MonitorOwner.LastHookAt.Equal(now.Add(time.Hour)) {
				t.Fatal("refresh replaced first_seen")
			}
			ownerHook(t, p, runtime, "session-B", "SessionStart", false, now.Add(2*time.Hour))
			before := monitorDisk(t, p)
			for _, event := range []string{"UserPromptSubmit", "SessionEnd", "Stop"} {
				if err := RecordMonitorOwnerHook(p, "a", time.Now().Add(time.Second), runtime, "session-A", event, false, now.Add(3*time.Hour)); !errors.Is(err, ErrMonitorOwner) || !bytes.Equal(before, monitorDisk(t, p)) {
					t.Fatal("old owner changed replacement", event, err)
				}
			}
			ownerHook(t, p, runtime, "session-A", "SessionStart", false, now) // delayed Start
			if !bytes.Equal(before, monitorDisk(t, p)) {
				t.Fatal("delayed Start replaced newer owner")
			}
			s = monitorLoad(t, p)
			if !s.currentMonitorOwner(now.Add(14*time.Hour-time.Nanosecond)) || s.currentMonitorOwner(now.Add(14*time.Hour)) {
				t.Fatal("TTL boundary")
			}
			if err := RecordMonitorOwnerHook(p, "a", time.Now().Add(time.Second), runtime, "session-B", "UserPromptSubmit", false, now.Add(14*time.Hour)); !errors.Is(err, ErrMonitorOwner) {
				t.Fatal("expired owner re-admitted", err)
			}
			ownerHook(t, p, runtime, "session-C", "SessionStart", false, now.Add(15*time.Hour))
			ownerHook(t, p, runtime, "session-C", "Stop", false, now.Add(16*time.Hour))
			if monitorLoad(t, p).MonitorOwner == nil {
				t.Fatal("ordinary Stop ended association")
			}
			ownerHook(t, p, runtime, "session-C", "SessionEnd", false, now.Add(17*time.Hour))
			if monitorLoad(t, p).MonitorOwner != nil {
				t.Fatal("end retained owner")
			}
			ownerHook(t, p, runtime, "session-D", "SessionStart", false, now.Add(18*time.Hour))
			ownerHook(t, p, runtime, "session-D", "Stop", true, now.Add(19*time.Hour))
			if monitorLoad(t, p).MonitorOwner != nil {
				t.Fatal("exit retained owner")
			}
		})
	}
}

// Proves the hook writer's input allowlist and ordinary writer exclusions,
// each next to an admitted hook control. Host-account callers remain trusted.
func TestMonitorOwnerWriteBoundary(t *testing.T) {
	for _, session := range []string{"A", ".", "_", "-", "native._-session", "01234567-89ab-cdef-0123-456789abcdef", strings.Repeat("x", 128)} {
		p := testPaths(t)
		ownerHook(t, p, "muse", session, "SessionStart", false, time.Now())
		if monitorLoad(t, p).MonitorOwner.SessionIDHash != ownerHash(session) {
			t.Fatal("valid shape rejected", session)
		}
	}
	for _, session := range []string{"", "bad/shape", "bad\nshape", strings.Repeat("x", 129)} {
		p := testPaths(t)
		if err := RecordMonitorOwnerHook(p, "a", time.Now().Add(time.Second), "muse", session, "SessionStart", false, time.Now()); !errors.Is(err, ErrMonitorOwner) {
			t.Fatal("bad session admitted", err)
		}
		ownerHook(t, p, "muse", "valid._-session", "SessionStart", false, time.Now())
	}
	for _, bad := range []struct {
		runtime, event string
		exit           bool
	}{{"unknown", "SessionStart", false}, {"muse", "watch", false}, {"muse", "sweep", false}, {"muse", "sessionstart", false}, {"muse", "SessionStart", true}} {
		p := testPaths(t)
		if err := RecordMonitorOwnerHook(p, "a", time.Now().Add(time.Second), bad.runtime, "valid", bad.event, bad.exit, time.Now()); !errors.Is(err, ErrMonitorOwner) {
			t.Fatal("bad source/event admitted", err)
		}
		ownerHook(t, p, "muse", "valid", "SessionStart", false, time.Now())
	}
	p := monitorSeed(t)
	for _, remove := range []bool{false, true} {
		change := func(s *State) error {
			if remove {
				s.MonitorOwner = nil
			} else {
				s.MonitorOwner.LastHookAt = s.MonitorOwner.LastHookAt.Add(time.Hour)
			}
			return nil
		}
		before := monitorDisk(t, p)
		if _, err := UpdateState(p, "a", change); !errors.Is(err, ErrMonitorConflict) || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("ordinary update changed owner", err)
		}
		s := monitorLoad(t, p)
		_ = change(&s)
		if err := SaveState(p, "a", s); !errors.Is(err, ErrMonitorConflict) || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("ordinary save changed owner", err)
		}
		ownerHook(t, p, "muse", "fixture-session", "UserPromptSubmit", false, time.Now())
	}
}

// Proves real sealed renewal preserves holder/process/pending state, and
// refuses absent, expired, foreign-holder/host leases rather than acquiring.
func TestMonitorOwnerConditionalRenewal(t *testing.T) {
	for _, mode := range []string{"valid", "missing", "expired", "holder", "host"} {
		t.Run(mode, func(t *testing.T) {
			p := monitorSeed(t)
			now := time.Now()
			_, err := UpdateState(p, "a", func(s *State) error {
				s.Lease.Expires = now.Add(time.Minute)
				s.NextBatch = "pending-cursor"
				switch mode {
				case "missing":
					s.Lease = nil
				case "expired":
					s.Lease.Expires = now
				case "holder":
					s.Lease.Holder = "another"
				case "host":
					s.Lease.Host = "another"
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			record := monitorRecord()
			record.OwnerPID = 987 // deliberately unrelated to the lease PID
			record.OwnerBirth = "unrelated-process-metadata"
			err = WithMonitorGeneration(p, "a", time.Now().Add(time.Second), MonitorArm, record, func(g *MonitorGeneration) error {
				before := monitorLoad(t, p)
				err := g.RenewOwnerLease(monitorRecord().Generation)
				after := monitorLoad(t, p)
				if mode != "valid" {
					if !errors.Is(err, ErrMonitorLease) || !reflect.DeepEqual(before.Lease, after.Lease) {
						t.Fatal("lease acquired/taken over", err)
					}
					return nil
				}
				if err != nil || after.Lease.Expires.Before(now.Add(LeaseTTL)) || after.Lease.Expires.After(time.Now().Add(LeaseTTL)) {
					t.Fatal("renewal", err, after.Lease)
				}
				want := *before.Lease
				want.Expires = after.Lease.Expires
				if !reflect.DeepEqual(&want, after.Lease) || after.NextBatch != "pending-cursor" || !sameOwner(before.MonitorOwner, after.MonitorOwner) || !validWriterSeal(after) {
					t.Fatal("renewal changed unrelated state")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Proves TTL/end/replacement (including same session re-start) silence the
// sink and stop renewal. No mailbox receipt or live transport is exercised.
func TestMonitorOwnerDisarms(t *testing.T) {
	for _, mode := range []string{"ttl", "end", "replacement", "same-session", "missing", "missing-policy"} {
		t.Run(mode, func(t *testing.T) {
			p := monitorSeed(t)
			if mode == "ttl" {
				if _, err := UpdateState(p, "a", func(s *State) error { s.MonitorConfig = &MonitorConfig{OwnerTTL: time.Hour}; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing-policy" {
				if _, err := UpdateState(p, "a", func(s *State) error { s.MonitorConfig = nil; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "missing" {
				ownerHook(t, p, "muse", "fixture-session", "SessionEnd", false, time.Now())
			}
			err := monitorRun(p, MonitorArm, func(g *MonitorGeneration) error {
				if mode != "missing" && mode != "missing-policy" {
					if err := g.CheckRenewal(monitorRecord().Generation); err != nil {
						t.Fatal(err)
					}
				}
				before := monitorLoad(t, p).Lease
				switch mode {
				case "ttl":
					if _, err := UpdateState(p, "a", func(s *State) error { s.MonitorConfig = &MonitorConfig{OwnerTTL: time.Nanosecond}; return nil }); err != nil {
						t.Fatal(err)
					}
					if err := g.RenewOwnerLease(monitorRecord().Generation); !errors.Is(err, ErrMonitorOwner) {
						t.Fatal("TTL renewed", err)
					}
				case "end":
					ownerHook(t, p, "muse", "fixture-session", "SessionEnd", false, time.Now())
				case "replacement":
					ownerHook(t, p, "muse", "new-session", "SessionStart", false, time.Now())
				case "same-session":
					ownerHook(t, p, "muse", "fixture-session", "SessionStart", false, time.Now())
				}
				if mode != "ttl" {
					if err := g.RenewOwnerLease(monitorRecord().Generation); !errors.Is(err, ErrMonitorOwner) {
						t.Fatal("unknown renewed", err)
					}
				}
				if err := g.Announce(monitorRecord().Generation, func() error { t.Fatal("unknown emitted"); return nil }); !errors.Is(err, ErrMonitorGeneration) {
					t.Fatal(err)
				}
				if active, reason := g.Status(); active || reason != "owner_unknown" {
					t.Fatal("status", active, reason)
				}
				if !reflect.DeepEqual(before, monitorLoad(t, p).Lease) {
					t.Fatal("disarm changed lease")
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Proves renewal rechecks at its effect boundary while a real replacement
// holds control and waits for State. A prior successful check is insufficient.
func TestMonitorOwnerRenewalReplacementRace(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorArm, func(g *MonitorGeneration) error {
		if err := g.CheckRenewal(monitorRecord().Generation); err != nil {
			t.Fatal(err)
		}
		before := monitorLoad(t, p).Lease
		unlock, err := lockFile(p.StateFile("a")[:len(p.StateFile("a"))-5] + ".lock")
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if unlock != nil {
				unlock()
			}
		}()
		replaced := make(chan error, 1)
		go func() {
			replaced <- RecordMonitorOwnerHook(p, "a", time.Now().Add(3*time.Second), "muse", "replacement", "SessionStart", false, time.Now())
		}()
		// Wait until the hook owns control; State is intentionally held by the
		// fixture so the hook cannot complete before renewal starts.
		deadline := time.Now().Add(time.Second)
		for {
			err := WithRuntimeLock(p, "a", "control", time.Now().Add(10*time.Millisecond), func() error { return nil })
			if errors.Is(err, ErrLockTimeout) {
				break
			}
			if err != nil || time.Now().After(deadline) {
				t.Fatal("replacement did not enter control", err)
			}
		}
		renewed := make(chan error, 1)
		go func() { renewed <- g.RenewOwnerLease(monitorRecord().Generation) }()
		unlock()
		unlock = nil
		if err := <-replaced; err != nil {
			t.Fatal(err)
		}
		if err := <-renewed; !errors.Is(err, ErrMonitorOwner) {
			t.Fatal("old owner renewed", err)
		}
		after := monitorLoad(t, p)
		if !reflect.DeepEqual(before, after.Lease) || after.MonitorOwner.SessionIDHash != ownerHash("replacement") {
			t.Fatal("race changed lease or lost replacement")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Proves malformed/ambiguous owner JSON refuses alongside a valid record;
// seals and foreign-writer observations remain L02's responsibility.
func TestMonitorOwnerClosedSchema(t *testing.T) {
	p := monitorSeed(t)
	b, _ := json.Marshal(monitorLoad(t, p).MonitorOwner)
	for _, bad := range []string{`null`, `{}`, strings.Replace(string(b), `"runtime"`, `"Runtime"`, 1), strings.Replace(string(b), `"runtime":"muse"`, `"runtime":"muse","runtime":"muse"`, 1), strings.Replace(string(b), `"runtime":"muse"`, `"runtime":"muse","station":"ambient"`, 1)} {
		var owner MonitorOwner
		if json.Unmarshal([]byte(bad), &owner) == nil {
			t.Fatal("invalid owner accepted", bad)
		}
		if err := json.Unmarshal(b, &owner); err != nil {
			t.Fatal("valid neighbour", err)
		}
	}
	for _, bad := range []string{`null`, `{}`, `{"owner_ttl":0}`, `{"owner_ttl":-1}`, `{"Owner_TTL":1}`, `{"owner_ttl":1,"owner_ttl":2}`, `{"owner_ttl":1,"extra":2}`} {
		var c MonitorConfig
		if json.Unmarshal([]byte(bad), &c) == nil {
			t.Fatal("invalid policy accepted", bad)
		}
		if err := json.Unmarshal([]byte(`{"owner_ttl":43200000000000}`), &c); err != nil {
			t.Fatal("valid policy neighbour", err)
		}
	}
	for _, bad := range []string{`{"monitor_owner":null}`, `{"monitor_owner":` + string(b) + `,"MONITOR_OWNER":` + string(b) + `}`, `{"monitor_config":null}`, `{"monitor_config":{"owner_ttl":1},"MONITOR_CONFIG":{"owner_ttl":2}}`} {
		var s State
		if unmarshalState([]byte(bad), &s) == nil {
			t.Fatal("ambiguous State authority accepted", bad)
		}
		if err := unmarshalState(monitorDisk(t, p), &s); err != nil {
			t.Fatal("valid State neighbour", err)
		}
	}
}

// Proves the hook writer preserves L02's sticky foreign-writer fence, renewal
// does not repair invalid bytes, and a lock timeout has no partial owner write.
func TestMonitorOwnerFenceAndDeadline(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorArm, func(g *MonitorGeneration) error {
		monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"foreign"`) })
		before := monitorDisk(t, p)
		if err := g.RenewOwnerLease(monitorRecord().Generation); !errors.Is(err, ErrIncompatibleWriter) || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("renewal repaired foreign input", err)
		}
		ownerHook(t, p, "muse", "fixture-session", "UserPromptSubmit", false, time.Now())
		s := monitorLoad(t, p)
		if !validWriterSeal(s) || s.Writer.ForeignSeen.IsZero() {
			t.Fatal("hook erased foreign-writer evidence")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := monitorDisk(t, p)
	if err := WithRuntimeLock(p, "a", "control", time.Now().Add(time.Second), func() error {
		err := RecordMonitorOwnerHook(p, "a", time.Now().Add(20*time.Millisecond), "muse", "replacement", "SessionStart", false, time.Now())
		if !errors.Is(err, ErrLockTimeout) || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("timeout partially wrote owner", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ownerHook(t, p, "muse", "replacement", "SessionStart", false, time.Now())
	if monitorLoad(t, p).MonitorOwner.SessionIDHash != ownerHash("replacement") {
		t.Fatal("unlocked valid neighbour failed")
	}
}
