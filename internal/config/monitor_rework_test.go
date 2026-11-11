package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Proves: canonical bytes and SHA-256 agree with a fixed independent vector,
// including writer authority metadata. This is consistency, not authentication.
func TestMonitorSealOracle(t *testing.T) {
	const raw = `{"writer":{"seal":"2d95e054ed694b8cefb5c27746672961404b923cd3486394f230d27c988adbd3","protocol":2,"foreign_seen":"2026-01-01T00:00:00Z"},"next_batch":"x","lease":{"holder":"fixture-session","host":"fixture-host","pid":123,"expires":"2030-01-01T00:00:00Z"},"future":{"z":9007199254740993,"a":[1.0,1e0,-0,"<>&"]}}`
	const canonical = `{"future":{"a":[1.0,1e0,-0,"\u003c\u003e\u0026"],"z":9007199254740993},"lease":{"expires":"2030-01-01T00:00:00Z","holder":"fixture-session","host":"fixture-host","pid":123},"next_batch":"x","writer":{"foreign_seen":"2026-01-01T00:00:00Z","protocol":2}}`
	const digest = "2d95e054ed694b8cefb5c27746672961404b923cd3486394f230d27c988adbd3"
	got, err := canonicalState([]byte(raw))
	if err != nil || string(got) != canonical {
		t.Errorf("canonical bytes = %s, %v; want literal vector", got, err)
	}
	hash, err := stateDigest([]byte(raw))
	if err != nil || hash != digest {
		t.Errorf("digest = %s, %v; want %s", hash, err, digest)
	}
	var s State
	if err := unmarshalState([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if !validWriterSeal(s) {
		t.Fatal("untouched golden State must verify")
	}
	for _, edit := range []string{"delete-foreign-seen", "zero-foreign-seen", "protocol"} {
		t.Run(edit, func(t *testing.T) {
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(raw), &fields); err != nil {
				t.Fatal(err)
			}
			var writer map[string]json.RawMessage
			if err := json.Unmarshal(fields["writer"], &writer); err != nil {
				t.Fatal(err)
			}
			switch edit {
			case "delete-foreign-seen":
				delete(writer, "foreign_seen")
			case "zero-foreign-seen":
				writer["foreign_seen"] = json.RawMessage(`"0001-01-01T00:00:00Z"`)
			case "protocol":
				writer["protocol"] = json.RawMessage(`3`)
			}
			fields["writer"], err = json.Marshal(writer)
			if err != nil {
				t.Fatal(err)
			}
			b, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			var changed State
			if err := unmarshalState(b, &changed); err != nil {
				t.Fatal(err)
			}
			if validWriterSeal(changed) {
				t.Fatal("writer edit retained seal")
			}
		})
	}
}

// Proves: changing only each current top-level member invalidates an otherwise
// valid disk seal, including nested opaque bytes. Schema inventory prevents
// new typed fields from silently escaping this test. No old executable is run.
func TestMonitorSealEveryStateField(t *testing.T) {
	edits := map[string]string{
		"migration":         `{"phase":"in_progress","daemon_base":"/fixture","at":"2026-01-02T03:04:05Z"}`,
		"monitor_owner":     `{"runtime":"muse","session_id_hash":"` + ownerHash("replacement-session") + `","first_seen":"2026-11-09T00:00:00Z","last_hook_at":"2026-11-09T00:01:00Z"}`,
		"monitor_config":    `{"owner_ttl":3600000000000}`,
		"mail_alarm_config": `{"minutes":20,"owner":"auto"}`,
		"mail_alarm":        `{"active":true}`, "notification": `{"version":1}`,
		"next_batch": `"old-cursor"`, "last_inbox": `"old-frame"`,
		"agent_dm_contexts": `{"contract":"a2a-agent-dm/v1","rooms":["!retained"]}`,
		"pending":           `[{"old":true}]`, "acked": `["old-id"]`, "threads": `["old-thread"]`,
		"thread_events": `[{"room":"r","event_id":"e","root":"t"}]`,
		"muted":         `["old-muted"]`, "subscriptions": `{"r":["message"]}`,
		"hook_sessions": `[{"id":"old-session","blocks":1}]`, "hooks_paused": `true`,
		"lease":   `{"holder":"old-owner","pid":12,"expires":"2030-01-01T00:00:00Z"}`,
		"runtime": `{}`, "terminal": `{}`, "wake": `{"rung":["old-wake"]}`, "liveness": `{}`,
		"runtime_profile_pending": `{}`, "notice_status": `{}`, "notice_write_uncertain": `true`,
		"notice_retry": `{}`, "own_status_write": `{}`, "liveness_source": `"old-source"`,
		"liveness_alerts": `{"old":{}}`, "future": `{"z":9007199254740994,"a":[1.0,1e0,-0]}`,
	}
	m := monitorRecord()
	m.Phase = "paused"
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	edits["monitor"] = string(b)
	scope, attachment, now := hostFixture()
	s := State{Lease: &attachment.Lease}
	if err := s.EnrollHost(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	b, err = json.Marshal(s.HostEnrollment)
	if err != nil {
		t.Fatal(err)
	}
	edits["host_enrollment"] = string(b)
	// Writer has dedicated protocol/foreign_seen deletion and zeroing controls
	// above; deleting the writer itself must also break verification.
	edits["writer"] = `null`
	for _, field := range stateJSONFields {
		if _, ok := edits[field]; !ok {
			t.Errorf("missing mutation for %s", field)
		}
	}
	for field, edit := range edits {
		t.Run(field, func(t *testing.T) {
			p := monitorSeed(t)
			if !validWriterSeal(monitorLoad(t, p)) {
				t.Fatal("untouched neighbour")
			}
			monitorOldWrite(t, p, func(fields map[string]json.RawMessage) { fields[field] = json.RawMessage(edit) })
			// Decode directly: Host custody companions are intentionally not changed
			// along with its one field, since this test isolates checksum coverage.
			var changed State
			if err := unmarshalState(monitorDisk(t, p), &changed); err != nil {
				t.Fatal("mutation must remain typed-decodable", err)
			}
			if validWriterSeal(changed) {
				t.Fatal("single-field old write retained seal")
			}
		})
	}
}

// Proves: every typed bound has an accepted edge and a rejected adjacent value,
// through direct validation AND the JSON decoder. It does not prove admission.
func TestMonitorTypedBounds(t *testing.T) {
	type bound struct {
		name string
		set  func(*Monitor, bool)
	}
	cases := []bound{
		{"generation-short", func(m *Monitor, bad bool) {
			n := 32
			if bad {
				n--
			}
			m.Generation = strings.Repeat("a", n)
		}},
		{"generation-long", func(m *Monitor, bad bool) {
			n := 32
			if bad {
				n++
			}
			m.Generation = strings.Repeat("a", n)
		}},
		{"generation-hex", func(m *Monitor, bad bool) {
			if bad {
				m.Generation = "g" + strings.Repeat("a", 31)
			} else {
				m.Generation = "f" + strings.Repeat("a", 31)
			}
		}},
		{"owner-pid", func(m *Monitor, bad bool) {
			m.OwnerPID = 2
			if bad {
				m.OwnerPID = 1
			}
		}},
		{"consumer-pid", func(m *Monitor, bad bool) {
			m.ConsumerPID = 2
			if bad {
				m.ConsumerPID = 1
			}
		}},
		{"heartbeat-min", func(m *Monitor, bad bool) {
			m.HeartbeatEvery = 1
			if bad {
				m.HeartbeatEvery = 0
			}
		}},
		{"heartbeat-max", func(m *Monitor, bad bool) {
			m.HeartbeatEvery = LeaseTTL - 1
			if bad {
				m.HeartbeatEvery = LeaseTTL
			}
		}},
		{"heartbeat-at", func(m *Monitor, bad bool) {
			m.HeartbeatAt = time.Time{}.Add(1)
			if bad {
				m.HeartbeatAt = time.Time{}
			}
		}},
		{"rung-count", func(m *Monitor, bad bool) {
			n := HookRungMax
			if bad {
				n++
			}
			m.Rung = make([]string, n)
			for i := range m.Rung {
				m.Rung[i] = "x"
			}
		}},
		{"stdout-endpoint", func(m *Monitor, bad bool) {
			m.Transport = "stdout"
			m.EndpointRef = ""
			if bad {
				m.EndpointRef = "x"
			}
		}},
		{"transport", func(m *Monitor, bad bool) {
			m.Transport = "ws"
			m.EndpointRef = "endpoint"
			if bad {
				m.Transport = "wss"
			}
		}},
	}
	fields := []struct {
		name     string
		limit    int
		required bool
		set      func(*Monitor, string)
	}{
		{"session", 200, true, func(m *Monitor, s string) { m.SessionID = s }},
		{"host", 128, true, func(m *Monitor, s string) { m.Host = s }},
		{"owner-birth", 128, true, func(m *Monitor, s string) { m.OwnerBirth = s }},
		{"consumer-birth", 128, true, func(m *Monitor, s string) { m.ConsumerBirth = s }},
		{"rung-token", 512, true, func(m *Monitor, s string) { m.Rung = []string{s} }},
		{"notification-token", 128, false, func(m *Monitor, s string) { m.NotificationToken = s }},
		{"ws-endpoint", 160, true, func(m *Monitor, s string) { m.Transport = "ws"; m.EndpointRef = s }},
	}
	for _, f := range fields {
		cases = append(cases, bound{f.name + "-max", func(m *Monitor, bad bool) {
			n := f.limit
			if bad {
				n++
			}
			f.set(m, strings.Repeat("x", n))
		}})
		if f.required {
			cases = append(cases, bound{f.name + "-min", func(m *Monitor, bad bool) {
				v := "x"
				if bad {
					v = ""
				}
				f.set(m, v)
			}})
		}
		for _, invalid := range []string{"\x00", "\n", string([]byte{0xff})} {
			cases = append(cases, bound{f.name + "-encoding-" + fmt.Sprintf("%x", []byte(invalid)), func(m *Monitor, bad bool) {
				v := "x"
				if bad {
					v = invalid
				}
				f.set(m, v)
			}})
		}
	}
	for _, endpoint := range []string{".", "..", "a/b", `a\b`, "a:b"} {
		cases = append(cases, bound{"ws-" + endpoint, func(m *Monitor, bad bool) {
			m.Transport = "ws"
			m.EndpointRef = "endpoint"
			if bad {
				m.EndpointRef = endpoint
			}
		}})
	}
	for _, field := range []string{"version", "kind", "runtime", "phase", "error-class"} {
		cases = append(cases, bound{field, func(m *Monitor, bad bool) {
			switch field {
			case "version":
				m.Version = 1
				if bad {
					m.Version = 2
				}
			case "kind":
				m.Kind = "monitor"
				if bad {
					m.Kind = "other"
				}
			case "runtime":
				m.Runtime = "muse"
				if bad {
					m.Runtime = "other"
				}
			case "phase":
				m.Phase = "stopped"
				if bad {
					m.Phase = "stop"
				}
			case "error-class":
				m.LastErrorClass = "output"
				if bad {
					m.LastErrorClass = "other"
				}
			}
		}})
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, bad := range []bool{false, true} {
				m := monitorRecord()
				c.set(&m, bad)
				err := m.Validate()
				if bad && !errors.Is(err, ErrMonitorState) || !bad && err != nil {
					t.Fatalf("bad=%v validation=%v", bad, err)
				}
				// Invalid UTF-8 is repaired by encoding/json on marshal, so only the
				// direct typed call can observe that representation.
				b, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(b, []byte(`\ufffd`)) {
					continue
				}
				var decoded Monitor
				err = json.Unmarshal(b, &decoded)
				if bad && !errors.Is(err, ErrMonitorState) || !bad && err != nil {
					t.Fatalf("bad=%v decode=%v", bad, err)
				}
			}
		})
	}
}

// Proves: new files receive clean seals through each ordinary API; existing
// unsealed files receive foreign_seen and require explicit arm after reseal.
// Clean admission also requires a hook-admitted owner; no native hook is run.
func TestMonitorFirstSealIdentityHistory(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, route := range []string{"save", "update", "by", "until"} {
			t.Run(route+map[bool]string{false: "-new", true: "-existing"}[existing], func(t *testing.T) {
				p := monitorPaths(t)
				if existing {
					seedStateJSON(t, p, `{"next_batch":"legacy"}`)
				}
				change := func(s *State) error { s.NextBatch = "ordinary"; s.Writer = &StateWriter{Protocol: 99}; return nil }
				var err error
				switch route {
				case "save":
					s := State{}
					_ = change(&s)
					err = SaveState(p, "a", s)
				case "update":
					_, err = UpdateState(p, "a", change)
				case "by":
					_, err = UpdateStateBy(p, "a", time.Now().Add(time.Second), change)
				case "until":
					_, err = UpdateStateUntil(p, "a", time.Now().Add(time.Second), change)
				}
				if err != nil {
					t.Fatal(err)
				}
				s := monitorLoad(t, p)
				if s.Writer.Protocol != 2 || !validWriterSeal(s) || s.Writer.ForeignSeen.IsZero() == existing {
					t.Fatal("wrong first seal/history")
				}
				if !existing {
					before := monitorDisk(t, p)
					if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error {
						t.Fatal("ownerless identity admitted")
						return nil
					}); !errors.Is(err, ErrMonitorOwner) || !bytes.Equal(before, monitorDisk(t, p)) {
						t.Fatal("clean no-owner neighbour", err)
					}
					ownerHook(t, p, "muse", monitorRecord().SessionID, "SessionStart", false, time.Now())
				}
				before := monitorDisk(t, p)
				err = monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error { return g.CheckRenewal(monitorRecord().Generation) })
				if existing {
					if !errors.Is(err, ErrIncompatibleWriter) || !bytes.Equal(before, monitorDisk(t, p)) {
						t.Fatal("implicit admission cleared legacy history", err)
					}
				} else if err != nil {
					t.Fatal("new clean identity refused", err)
				}
				if existing {
					ownerHook(t, p, "muse", monitorRecord().SessionID, "SessionStart", false, time.Now())
				}
				// Explicit arm is usable in both cases, with a fresh generation after
				// the new identity's successful automatic generation has cleanly stopped.
				m := monitorRecord()
				m.Generation = strings.Repeat("b", 32)
				if err := WithMonitorGeneration(p, "a", time.Now().Add(time.Second), MonitorArm, m, func(g *MonitorGeneration) error {
					s := monitorLoad(t, p)
					if s.Writer.Protocol != 2 || !s.Writer.ForeignSeen.IsZero() || !validWriterSeal(s) {
						t.Fatal("arm seal")
					}
					return g.CheckRenewal(m.Generation)
				}); err != nil {
					t.Fatal(err)
				}
				s = monitorLoad(t, p)
				if s.Writer.Protocol != 2 || !validWriterSeal(s) {
					t.Fatal("close seal")
				}
			})
		}
	}
}

// Proves: malformed writer alone is ignored, detected with a closed refusal,
// then re-sealed by ordinary work. Bad non-writer types remain load failures.
func TestMonitorMalformedWriter(t *testing.T) {
	for _, writer := range []string{`"old"`, `42`, `true`, `[]`, `{"protocol":"two"}`, `{"protocol":2,"seal":42}`, `{"foreign_seen":"bad-time"}`, `null`} {
		t.Run(writer, func(t *testing.T) {
			p := monitorSeed(t)
			monitorOldWrite(t, p, func(fields map[string]json.RawMessage) { fields["writer"] = json.RawMessage(writer) })
			before := monitorDisk(t, p)
			s, err := LoadState(p, "a")
			if err != nil || s.Writer != nil || s.NextBatch != "before" || s.Lease == nil || s.Lease.Holder != "fixture-session" {
				t.Fatal("malformed writer bricked or changed State", err)
			}
			if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { t.Fatal("malformed writer admitted"); return nil }); !errors.Is(err, ErrIncompatibleWriter) || !bytes.Equal(before, monitorDisk(t, p)) {
				t.Fatal("wrong closed refusal", err)
			}
			if _, err := UpdateState(p, "a", func(s *State) error { s.LastInbox = "compatible"; return nil }); err != nil {
				t.Fatal(err)
			}
			s = monitorLoad(t, p)
			if s.Writer.Protocol != 2 || s.Writer.ForeignSeen.IsZero() || !validWriterSeal(s) || s.NextBatch != "before" || s.LastInbox != "compatible" {
				t.Fatal("ordinary repair lost history")
			}
		})
	}
	for _, raw := range []string{`{"next_batch":42}`, `{"lease":[]}`, `{"lease":{"pid":"bad"}}`, `{"monitor":{}}`, `{"writer":"bad",`} {
		p := monitorPaths(t)
		seedStateJSON(t, p, raw)
		if _, err := LoadState(p, "a"); err == nil {
			t.Fatal("non-writer State became tolerant", raw)
		}
	}
	p := monitorSeed(t)
	if !validWriterSeal(monitorLoad(t, p)) {
		t.Fatal("valid writer neighbour")
	}
}

// Proves: malformed metadata during a live generation closes as incompatible,
// leaves disk untouched, and never emits. Admission tests alone cannot prove it.
func TestMonitorMalformedWriterDuringGeneration(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		monitorOldWrite(t, p, func(fields map[string]json.RawMessage) { fields["writer"] = json.RawMessage(`[]`) })
		before := monitorDisk(t, p)
		err := g.Announce(monitorRecord().Generation, func() error { t.Fatal("malformed writer emitted"); return nil })
		active, reason := g.Status()
		if !errors.Is(err, ErrIncompatibleWriter) || active || reason != "incompatible_writer" || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("wrong live refusal", err, reason)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Proves: a valid seal does not authorize announcements after foreign_seen,
// and stopped generation IDs cannot be reused. It does not close the later
// post-check output window, which belongs to the announcement transaction.
func TestMonitorStickyAnnouncementAndGenerationReuse(t *testing.T) {
	p := monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(g *MonitorGeneration) error {
		if err := g.Announce(monitorRecord().Generation, func() error { return nil }); err != nil {
			t.Fatal("valid announcement neighbour", err)
		}
		monitorOldWrite(t, p, func(fields map[string]json.RawMessage) { fields["next_batch"] = json.RawMessage(`"old"`) })
		if _, err := UpdateState(p, "a", func(*State) error { return nil }); err != nil {
			t.Fatal(err)
		}
		s := monitorLoad(t, p)
		if !validWriterSeal(s) || s.Writer.ForeignSeen.IsZero() {
			t.Fatal("sticky valid-seal fixture")
		}
		before := monitorDisk(t, p)
		if err := g.Announce(monitorRecord().Generation, func() error { t.Fatal("sticky seal emitted"); return nil }); !errors.Is(err, ErrIncompatibleWriter) || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal(err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	p = monitorSeed(t)
	if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { return nil }); err != nil {
		t.Fatal(err)
	}
	before := monitorDisk(t, p)
	for _, entry := range []MonitorEntry{MonitorArm, MonitorAutomatic, MonitorRecovery} {
		if err := monitorRun(p, entry, func(*MonitorGeneration) error { t.Fatal("reused generation admitted"); return nil }); !errors.Is(err, ErrMonitorGeneration) || !bytes.Equal(before, monitorDisk(t, p)) {
			t.Fatal("reused generation refusal", err)
		}
	}
	m := monitorRecord()
	m.Generation = strings.Repeat("b", 32)
	if err := WithMonitorGeneration(p, "a", time.Now().Add(time.Second), MonitorAutomatic, m, func(*MonitorGeneration) error { return nil }); err != nil {
		t.Fatal("new generation neighbour", err)
	}
}

// Proves: Host v2's distinct persistence path also forces protocol 2 on every
// ordinary save API, even when the caller supplies another protocol. It does
// not qualify Host activation or external notification delivery.
func TestMonitorHostSavesProtocol2(t *testing.T) {
	p := monitorPaths(t)
	scope, attachment, now := hostFixture()
	scope.Agent = "a"
	attachment.Scope = scope
	s := State{Lease: &attachment.Lease}
	if err := s.EnrollHost(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHostNotifications(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	s.Writer = &StateWriter{Protocol: 99}
	if err := SaveState(p, "a", s); err != nil {
		t.Fatal(err)
	}
	assert := func(t *testing.T) {
		t.Helper()
		saved := monitorLoad(t, p)
		if saved.HostEnrollment.Version != 2 || saved.Writer.Protocol != 2 || !validWriterSeal(saved) {
			t.Fatal("Host save did not store protocol 2")
		}
	}
	assert(t)
	for _, route := range []string{"update", "by", "until"} {
		t.Run(route, func(t *testing.T) {
			change := func(s *State) error { s.Writer = &StateWriter{Protocol: 99}; s.LastInbox = route; return nil }
			var err error
			switch route {
			case "update":
				_, err = UpdateState(p, "a", change)
			case "by":
				_, err = UpdateStateBy(p, "a", time.Now().Add(time.Second), change)
			case "until":
				_, err = UpdateStateUntil(p, "a", time.Now().Add(time.Second), change)
			}
			if err != nil {
				t.Fatal(err)
			}
			assert(t)
		})
	}
}
