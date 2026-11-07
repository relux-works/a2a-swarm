//go:build unix

package association

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// All state, leases and descriptor bytes are synthetic and private to each test.
// These tests do not inspect any installed identity, process or native runtime.
func object(t *testing.T, b []byte) map[string]any {
	t.Helper()
	m, e := parse(b, 32, 65536)
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func marshal(t *testing.T, m any) []byte {
	t.Helper()
	b, e := json.Marshal(m)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func fixture(t *testing.T) (Context, []byte, map[string]any, time.Time) {
	t.Helper()
	base, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(base, 0700); e != nil {
		t.Fatal(e)
	}
	c := Context{Name: "synthetic-own", Connection: "legacy", Base: base, Root: base, MachineLabel: "Synthetic-Workstation.local"}
	if e := os.Mkdir(filepath.Join(base, "state"), 0700); e != nil {
		t.Fatal(e)
	}
	for path, body := range map[string]string{"host": "synthetic-host\n", "state/synthetic-own.lock": ""} {
		if e := os.WriteFile(filepath.Join(base, path), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	now := time.UnixMilli(1800000000000)
	expected := map[string]any{"generation": strings.Repeat("a", 32), "consumer_pid": 41001, "consumer_birth": "SYNTHETIC_CONSUMER_BIRTH", "owner_pid": 42001, "owner_birth": "SYNTHETIC_OWNER_BIRTH", "runtime": "codex", "session_id": "SYNTHETIC_PRIVATE_SESSION", "target": map[string]any{"socket": "/synthetic-private/own/socket", "socket_id": "SYNTHETIC_SOCKET_ID", "pane": "%817263", "pane_pid": 43001, "pane_birth": "SYNTHETIC_PANE_BIRTH", "owner_pid": 42001, "owner_birth": "SYNTHETIC_OWNER_BIRTH", "runtime": "codex"}}
	request := map[string]any{"schema": Schema, "source_identity": c.Name, "source_connection": c.Connection, "launch_intent_sha256": hash("complete synthetic intent including launch options"), "issued_at_ms": now.UnixMilli() - 1, "expires_at_ms": now.UnixMilli() + 29999, "expected": expected}
	binding := object(t, marshal(t, expected))
	binding["daemon_pid"] = binding["consumer_pid"]
	delete(binding, "consumer_pid")
	binding["daemon_birth"] = binding["consumer_birth"]
	delete(binding, "consumer_birth")
	delete(binding, "runtime")
	binding["host"] = c.MachineLabel
	binding["stopped"] = false
	state := map[string]any{"terminal": binding, "lease": map[string]any{"holder": "SYNTHETIC_PRIVATE_SESSION", "expires": now.Add(time.Minute).Format(time.RFC3339Nano)}, "notification": map[string]any{"synthetic272": true}, "new_opaque_272_field": []any{true, "retained"}, "host_enrollment": map[string]any{"uninterpreted": true}, "hooks_paused": true, "runtime": nil, "wake": map[string]any{"synthetic-unknown": "retained"}}
	raw := marshal(t, request)
	if e := loadTestSchema(t, "private-input").validate(raw); e != nil {
		t.Fatal(e)
	}
	return c, raw, state, now
}
func execute(t *testing.T, c Context, req []byte, s map[string]any, now time.Time, change func(*Options)) (output, int, string) {
	t.Helper()
	path := filepath.Join(c.Base, "state", c.Name+".json")
	before := marshal(t, s)
	if e := os.WriteFile(path, before, 0600); e != nil {
		t.Fatal(e)
	}
	var buf bytes.Buffer
	o := Options{Input: frame(t, req), Output: &buf, Now: func() time.Time { return now }, Resolve: func() (Context, error) { return c, nil }}
	if change != nil {
		change(&o)
	}
	before, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	code := Run(o)
	checkPublicSchema(t, buf.Bytes(), code)
	after, e := os.ReadFile(path)
	if e != nil || !bytes.Equal(before, after) {
		t.Fatal("state effect")
	}
	var out output
	if e = json.Unmarshal(buf.Bytes(), &out); e != nil {
		t.Fatal(e)
	}
	if len(buf.Bytes()) > 2048 || bytes.Count(buf.Bytes(), []byte{'\n'}) != 1 {
		t.Fatal("output framing")
	}
	if out.Native != "not-observed-by-this-interface" || out.Scope != "stored-own-binding" {
		t.Fatal("native claim")
	}
	for _, sentinel := range []string{"SYNTHETIC_PRIVATE_SESSION", "SYNTHETIC_CONSUMER_BIRTH", "SYNTHETIC_OWNER_BIRTH", "SYNTHETIC_PANE_BIRTH", "SYNTHETIC_SOCKET_ID", "/synthetic-private/own/socket", "%817263"} {
		if strings.Contains(buf.String(), sentinel) {
			t.Fatal("private disclosure")
		}
	}
	return out, code, buf.String()
}
func TestAssociationSyntheticPositiveAndRefusals(t *testing.T) {
	cases := []struct {
		name, reason string
		code         int
		state        func(map[string]any)
		request      func(map[string]any)
	}{
		{name: "valid", reason: "stored_tuple_equal", code: 0},
		{name: "new272opaque-state-compatible", reason: "stored_tuple_equal", code: 0, state: func(s map[string]any) { s["principal_coverage"] = map[string]any{"unrelated": "retained"} }},
		{name: "missing-native-runtime-does-not-mean-absence", reason: "stored_tuple_equal", code: 0, state: func(s map[string]any) { delete(s, "runtime") }},
		{name: "generation", reason: "generation_mismatch", code: 1, state: func(s map[string]any) { s["terminal"].(map[string]any)["generation"] = strings.Repeat("b", 32) }},
		{name: "session", reason: "tuple_mismatch", code: 1, state: func(s map[string]any) {
			s["terminal"].(map[string]any)["session_id"] = "SYNTHETIC_REPLACED_SESSION"
			s["lease"].(map[string]any)["holder"] = "SYNTHETIC_REPLACED_SESSION"
		}},
		{name: "stopped-ambiguous", reason: "binding_stopped", code: 2, state: func(s map[string]any) {
			s["terminal"].(map[string]any)["stopped"] = true
			s["terminal"].(map[string]any)["outcome"] = "target exited or replaced"
		}},
		{name: "host", reason: "stale_binding", code: 2, state: func(s map[string]any) { s["terminal"].(map[string]any)["host"] = "synthetic-other-host" }},
		{name: "missing-host", reason: "stale_binding", code: 2, state: func(s map[string]any) { delete(s["terminal"].(map[string]any), "host") }},
		{name: "pid-birth-holder", reason: "stale_binding", code: 2, state: func(s map[string]any) {
			s["lease"].(map[string]any)["holder"] = "pid:41001:synthetic-birth@synthetic-host"
		}},
		{name: "pid-holder-spoofed-session", reason: "stale_binding", code: 2, state: func(s map[string]any) {
			s["lease"].(map[string]any)["holder"] = "pid:41001:synthetic-birth@synthetic-host"
			s["terminal"].(map[string]any)["session_id"] = "pid:41001:synthetic-birth@synthetic-host"
		}, request: func(r map[string]any) {
			r["expected"].(map[string]any)["session_id"] = "pid:41001:synthetic-birth@synthetic-host"
		}},
		{name: "expired-lease", reason: "stale_binding", code: 2, state: func(s map[string]any) {
			s["lease"].(map[string]any)["expires"] = time.UnixMilli(1800000000000).Format(time.RFC3339Nano)
		}},
		{name: "missing-lease", reason: "stale_binding", code: 2, state: func(s map[string]any) { delete(s, "lease") }},
		{name: "missing", reason: "binding_missing", code: 2, state: func(s map[string]any) { delete(s, "terminal") }},
		{name: "partial", reason: "binding_invalid", code: 2, state: func(s map[string]any) { delete(s["terminal"].(map[string]any), "daemon_birth") }},
		{name: "partial-stopped", reason: "binding_invalid", code: 2, state: func(s map[string]any) { delete(s["terminal"].(map[string]any), "stopped") }},
		{name: "context", reason: "context_unavailable", code: 2, request: func(r map[string]any) { r["source_identity"] = "synthetic-foreign" }},
		{name: "connection", reason: "context_unavailable", code: 2, request: func(r map[string]any) { r["source_connection"] = "synthetic-foreign" }},
		{name: "future", reason: "input_invalid", code: 2, request: func(r map[string]any) { r["issued_at_ms"] = 1800000000001; r["expires_at_ms"] = 1800000000010 }},
		{name: "expired", reason: "input_expired", code: 2, request: func(r map[string]any) { r["issued_at_ms"] = 1799999999990; r["expires_at_ms"] = 1800000000000 }},
		{name: "ttl", reason: "input_invalid", code: 2, request: func(r map[string]any) { r["expires_at_ms"] = 1800000040000 }},
		{name: "unknown", reason: "input_invalid", code: 2, request: func(r map[string]any) { r["path"] = "synthetic-disallowed" }},
		{name: "null", reason: "input_invalid", code: 2, request: func(r map[string]any) { r["expected"] = nil }},
		{name: "privacy-observation-time", reason: "privacy_collision", code: 2, request: func(r map[string]any) { r["expected"].(map[string]any)["session_id"] = "1800" }},
		{name: "privacy-pane-numeric", reason: "privacy_collision", code: 2, request: func(r map[string]any) { r["expected"].(map[string]any)["target"].(map[string]any)["pane_pid"] = 1800 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, b, s, now := fixture(t)
			r := object(t, b)
			if tc.state != nil {
				tc.state(s)
			}
			if tc.request != nil {
				tc.request(r)
			}
			out, code, _ := execute(t, c, marshal(t, r), s, now, nil)
			if code != tc.code || out.Reason != tc.reason {
				t.Fatalf("got %d/%s want %d/%s", code, out.Reason, tc.code, tc.reason)
			}
			if tc.reason == "privacy_collision" && (out.Observed != nil || out.Digest != nil || out.Stored != nil) {
				t.Fatal("collision not suppressed")
			}
			if tc.reason == "binding_stopped" && (out.Stored == nil || out.Stored.StoppedReason == nil || *out.Stored.StoppedReason != "recorded-stop-cause-ambiguous") {
				t.Fatal("stop claim")
			}
		})
	}
}
func TestAssociationEveryTargetField(t *testing.T) {
	for _, field := range []string{"socket", "socket_id", "pane", "pane_pid", "pane_birth", "owner_pid", "owner_birth", "runtime"} {
		t.Run(field, func(t *testing.T) {
			c, b, s, now := fixture(t)
			r := object(t, b)
			x := r["expected"].(map[string]any)
			target := x["target"].(map[string]any)
			switch field {
			case "pane_pid":
				target[field] = 43002
			case "owner_pid":
				target[field] = 42002
				x[field] = 42002
			case "owner_birth":
				target[field] = "SYNTHETIC_OTHER_BIRTH"
				x[field] = "SYNTHETIC_OTHER_BIRTH"
			case "runtime":
				target[field] = "claude"
				x[field] = "claude"
			case "pane":
				target[field] = "%817264"
			default:
				target[field] = "SYNTHETIC_OTHER_VALUE"
			}
			out, code, _ := execute(t, c, marshal(t, r), s, now, nil)
			if code != 1 || out.Reason != "tuple_mismatch" {
				t.Fatalf("%d/%s", code, out.Reason)
			}
		})
	}
}
func TestAssociationMalformedJSON(t *testing.T) {
	for _, kind := range []string{"duplicate", "nested-duplicate", "fraction", "exponent", "negative", "suffix", "utf8", "bom", "unknown-target", "null-target"} {
		t.Run(kind, func(t *testing.T) {
			c, b, s, now := fixture(t)
			switch kind {
			case "duplicate":
				b = append([]byte(`{"schema":"other",`), b[1:]...)
			case "nested-duplicate":
				b = bytes.Replace(b, []byte(`"pane_pid":43001`), []byte(`"pane_pid":43001,"pane_pid":43001`), 1)
			case "fraction":
				b = bytes.Replace(b, []byte(`"pane_pid":43001`), []byte(`"pane_pid":43001.0`), 1)
			case "exponent":
				b = bytes.Replace(b, []byte(`"pane_pid":43001`), []byte(`"pane_pid":4.3001e4`), 1)
			case "negative":
				b = bytes.Replace(b, []byte(`"pane_pid":43001`), []byte(`"pane_pid":-1`), 1)
			case "suffix":
				b = append(b, '0')
			case "utf8":
				b = append(b, 255)
			case "bom":
				b = append([]byte{239, 187, 191}, b...)
			case "unknown-target":
				b = bytes.Replace(b, []byte(`"target":{`), []byte(`"target":{"foreign":true,`), 1)
			case "null-target":
				r := object(t, b)
				r["expected"].(map[string]any)["target"] = nil
				b = marshal(t, r)
			}
			out, code, _ := execute(t, c, b, s, now, nil)
			if code != 2 || out.Reason != "input_invalid" {
				t.Fatalf("%d/%s", code, out.Reason)
			}
		})
	}
}
func TestAssociationNoCreateAndLockContention(t *testing.T) {
	for _, kind := range []string{"missing-lock", "busy-lock", "symlink-state", "oversize-state", "duplicate-state", "bad-state-mode"} {
		t.Run(kind, func(t *testing.T) {
			c, b, s, now := fixture(t)
			path := filepath.Join(c.Base, "state", c.Name+".lock")
			var held *os.File
			change := func(o *Options) {
				switch kind {
				case "missing-lock":
					if e := os.Remove(path); e != nil {
						t.Fatal(e)
					}
				case "busy-lock":
					var e error
					held, e = os.Open(path)
					if e != nil {
						t.Fatal(e)
					}
					if e = syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
						t.Fatal(e)
					}
				case "symlink-state":
					p := filepath.Join(c.Base, "state", c.Name+".json")
					raw, e := os.ReadFile(p)
					if e != nil {
						t.Fatal(e)
					}
					os.WriteFile(filepath.Join(c.Base, "synthetic-target"), raw, 0600)
					os.Remove(p)
					os.Symlink(filepath.Join(c.Base, "synthetic-target"), p)
				case "oversize-state":
					os.WriteFile(filepath.Join(c.Base, "state", c.Name+".json"), bytes.Repeat([]byte(" "), 1048577), 0600)
				case "duplicate-state":
					os.WriteFile(filepath.Join(c.Base, "state", c.Name+".json"), []byte(`{"terminal":null,"terminal":null}`), 0600)
				case "bad-state-mode":
					os.Chmod(filepath.Join(c.Base, "state", c.Name+".json"), 0644)
				}
			}
			// Snapshot effects are checked against the post-control state here: the
			// ordinary execute helper expects its original bytes to remain unchanged.
			statePath := filepath.Join(c.Base, "state", c.Name+".json")
			os.WriteFile(statePath, marshal(t, s), 0600)
			var buf bytes.Buffer
			o := Options{Input: frame(t, b), Output: &buf, Now: func() time.Time { return now }, Resolve: func() (Context, error) { return c, nil }}
			change(&o)
			before, _ := os.ReadFile(statePath)
			start := time.Now()
			code := Run(o)
			checkPublicSchema(t, buf.Bytes(), code)
			elapsed := time.Since(start)
			after, _ := os.ReadFile(statePath)
			if held != nil {
				held.Close()
			}
			if !bytes.Equal(before, after) {
				t.Fatal("state effect")
			}
			var out output
			json.Unmarshal(buf.Bytes(), &out)
			if code != 2 || out.Reason != "snapshot_unavailable" || out.Stored != nil {
				t.Fatalf("%d/%s", code, out.Reason)
			}
			if kind == "missing-lock" {
				if _, e := os.Lstat(path); !os.IsNotExist(e) {
					t.Fatal("lock created")
				}
			}
			if kind == "busy-lock" && (elapsed < 95*time.Millisecond || elapsed > 250*time.Millisecond) {
				t.Fatalf("lock bound %v", elapsed)
			}
		})
	}
}
func TestAssociationPublicationGuards(t *testing.T) {
	for _, kind := range []string{"clock-zero", "rollback", "expiry-at-publication", "lease-at-publication", "context-changed", "budget", "args"} {
		t.Run(kind, func(t *testing.T) {
			c, b, s, now := fixture(t)
			out, code, _ := execute(t, c, b, s, now, func(o *Options) {
				calls := 0
				switch kind {
				case "clock-zero":
					o.Now = func() time.Time { return time.Time{} }
				case "rollback":
					o.Now = func() time.Time {
						calls++
						if calls > 1 {
							return now.Add(-time.Millisecond)
						}
						return now
					}
				case "expiry-at-publication":
					o.Now = func() time.Time {
						calls++
						if calls > 1 {
							return now.Add(30 * time.Second)
						}
						return now
					}
				case "lease-at-publication":
					s["lease"].(map[string]any)["expires"] = now.Add(time.Millisecond).Format(time.RFC3339Nano)
					os.WriteFile(filepath.Join(c.Base, "state", c.Name+".json"), marshal(t, s), 0600)
					o.Now = func() time.Time {
						calls++
						if calls > 1 {
							return now.Add(time.Millisecond)
						}
						return now
					}
				case "context-changed":
					o.Resolve = func() (Context, error) {
						calls++
						if calls > 1 {
							c.Connection = "changed"
						}
						return c, nil
					}
				case "budget":
					o.Resolve = func() (Context, error) { time.Sleep(130 * time.Millisecond); return c, nil }
				case "args":
					o.InvalidArgs = true
				}
			})
			want := map[string]string{"clock-zero": "clock_unavailable", "rollback": "clock_unavailable", "expiry-at-publication": "input_expired", "lease-at-publication": "stale_binding", "context-changed": "context_unavailable", "budget": "budget_exceeded", "args": "input_invalid"}[kind]
			if code != 2 || out.Reason != want || out.Stored != nil {
				t.Fatalf("%d/%s want %s", code, out.Reason, want)
			}
		})
	}
}

func TestAssociationDescriptorRefusals(t *testing.T) {
	for _, kind := range []string{"missing", "named", "pipe", "writable-sealed", "unsealed", "mode", "frame-zero", "frame-suffix", "frame-oversize"} {
		t.Run(kind, func(t *testing.T) {
			c, b, _, now := fixture(t)
			_ = c
			var f *os.File
			switch kind {
			case "missing":
			case "pipe":
				var w *os.File
				var e error
				f, w, e = os.Pipe()
				if e != nil {
					t.Fatal(e)
				}
				w.Close()
			case "named":
				p := filepath.Join(t.TempDir(), "synthetic-frame")
				if e := os.WriteFile(p, b, 0600); e != nil {
					t.Fatal(e)
				}
				var e error
				f, e = os.Open(p)
				if e != nil {
					t.Fatal(e)
				}
			case "writable-sealed":
				if runtime.GOOS != "linux" {
					t.Skip("Linux sealed descriptor control")
				}
				ro := frame(t, b)
				var e error
				f, e = os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", ro.Fd()), os.O_RDWR, 0)
				ro.Close()
				if e != nil {
					t.Fatal(e)
				}
			case "unsealed":
				if runtime.GOOS != "linux" {
					t.Skip("non-Linux Unix admits unlinked read-only shape")
				}
				p := filepath.Join(t.TempDir(), "synthetic-frame")
				os.WriteFile(p, b, 0600)
				var e error
				f, e = os.Open(p)
				if e != nil {
					t.Fatal(e)
				}
				os.Remove(p)
			case "mode":
				f = frame(t, b)
				if e := f.Chmod(0644); e != nil {
					t.Fatal(e)
				}
			case "frame-zero":
				f = frame(t, nil)
			case "frame-suffix":
				f = frame(t, append(b, 'x'))
			case "frame-oversize":
				f = frame(t, bytes.Repeat([]byte(" "), 8193))
			}
			var buf bytes.Buffer
			code := Run(Options{Input: f, Output: &buf, Now: func() time.Time { return now }, Resolve: func() (Context, error) {
				t.Fatal("descriptor refusal must not resolve own state")
				return Context{}, invalid
			}})
			checkPublicSchema(t, buf.Bytes(), code)
			var out output
			if e := json.Unmarshal(buf.Bytes(), &out); e != nil {
				t.Fatal(e)
			}
			want := "input_untrusted"
			if strings.HasPrefix(kind, "frame-") {
				want = "input_invalid"
			}
			if code != 2 || out.Reason != want || out.Stored != nil || out.Digest != nil {
				t.Fatalf("%d/%s want %s", code, out.Reason, want)
			}
			if f != nil {
				if _, e := f.Stat(); e == nil {
					t.Fatal("descriptor retained")
				}
			}
		})
	}
}

type rejectWriter struct{ short bool }

func (w rejectWriter) Write(b []byte) (int, error) {
	if w.short {
		return len(b) - 1, nil
	}
	return 0, invalid
}
func TestAssociationWriteFailure(t *testing.T) {
	for _, short := range []bool{false, true} {
		t.Run(fmt.Sprint(short), func(t *testing.T) {
			c, b, s, now := fixture(t)
			os.WriteFile(filepath.Join(c.Base, "state", c.Name+".json"), marshal(t, s), 0600)
			if code := Run(Options{Input: frame(t, b), Output: rejectWriter{short}, Now: func() time.Time { return now }, Resolve: func() (Context, error) { return c, nil }}); code != 3 {
				t.Fatalf("write exit %d", code)
			}
		})
	}
}
func TestAssociationParserLimitsAndCanonicalIntegers(t *testing.T) {
	for _, kind := range []string{"case-alias", "unicode-alias", "depth", "tokens"} {
		t.Run(kind, func(t *testing.T) {
			var b []byte
			depth, tokens := 5, 512
			switch kind {
			case "case-alias":
				b = []byte(`{"target":{},"Target":{}}`)
			case "unicode-alias":
				b = []byte(`{"K":{},"K":{}}`)
			case "depth":
				b = []byte(`{"a":{"a":{"a":{"a":{"a":{"a":1}}}}}}`)
			case "tokens":
				b = []byte(`{"a":[` + strings.Repeat(`0,`, 600) + `0]}`)
			}
			if _, e := parse(b, depth, tokens); e == nil {
				t.Fatal("parser limit not enforced")
			}
		})
	}
	for _, raw := range []string{"9007199254740993", "2147483647", "0"} {
		n, ok := integer(json.Number(raw), 9223372036854775807)
		if !ok || fmt.Sprint(n) != raw {
			t.Fatal("numeric precision")
		}
	}
}

// Privacy assertions must be sensitive to each otherwise-allowed variable
// projection slot, including observation time and all private numeric leaves.
func TestAssociationPrivacySlots(t *testing.T) {
	c, b, s, _ := fixture(t)
	_ = c
	r, e := decodeRequest(b)
	if e != nil {
		t.Fatal(e)
	}
	own, ok := decodeMember(s["terminal"].(map[string]any), true)
	if !ok {
		t.Fatal("fixture")
	}
	for _, slot := range []string{"generation", "consumer-birth", "owner-birth", "runtime", "digest", "observed", "consumer-pid", "owner-pid"} {
		t.Run(slot, func(t *testing.T) {
			ms := int64(1800000000000)
			o := refusal("binding_stopped", &ms)
			o.Digest = &r.Digest
			o.Stored = project(own, true)
			m := own
			m.SessionID = "COLLISION"
			switch slot {
			case "generation":
				o.Stored.Generation = "COLLISION"
			case "consumer-birth":
				o.Stored.ConsumerBirth = "COLLISION"
			case "owner-birth":
				o.Stored.OwnerBirth = "COLLISION"
			case "runtime":
				o.Stored.Runtime = "COLLISION"
			case "digest":
				x := "COLLISION"
				o.Digest = &x
			case "observed":
				m.SessionID = "1800"
			case "consumer-pid":
				m.SessionID = "41001"
			case "owner-pid":
				m.SessionID = "42001"
			}
			if !collides(o, m) {
				t.Fatal("privacy slot not checked")
			}
		})
	}
}

func TestAssociationConsumerAndBindingValidity(t *testing.T) {
	for _, kind := range []string{"consumer-pid", "consumer-birth", "target-inconsistent", "generation-format", "null-binding", "empty-host", "malformed-lease"} {
		t.Run(kind, func(t *testing.T) {
			c, b, s, now := fixture(t)
			r := object(t, b)
			expected := r["expected"].(map[string]any)
			binding := s["terminal"].(map[string]any)
			want := "tuple_mismatch"
			wantCode := 1
			switch kind {
			case "consumer-pid":
				expected["consumer_pid"] = 41002
			case "consumer-birth":
				expected["consumer_birth"] = "SYNTHETIC_OTHER_CONSUMER_BIRTH"
			case "target-inconsistent":
				binding["target"].(map[string]any)["owner_pid"] = 42002
				want = "binding_invalid"
				wantCode = 2
			case "generation-format":
				binding["generation"] = strings.Repeat("A", 32)
				want = "binding_invalid"
				wantCode = 2
			case "null-binding":
				s["terminal"] = nil
				want = "binding_missing"
				wantCode = 2
			case "empty-host":
				binding["host"] = ""
				want = "stale_binding"
				wantCode = 2
			case "malformed-lease":
				s["lease"] = "synthetic-invalid"
				want = "stale_binding"
				wantCode = 2
			}
			out, code, _ := execute(t, c, marshal(t, r), s, now, nil)
			if code != wantCode || out.Reason != want {
				t.Fatalf("%d/%s want %d/%s", code, out.Reason, wantCode, want)
			}
		})
	}
}

func TestAssociationInterruptedSnapshot(t *testing.T) {
	for _, phase := range []string{"before-lock", "before-publication"} {
		t.Run(phase, func(t *testing.T) {
			c, b, s, now := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out, code, _ := execute(t, c, b, s, now, func(o *Options) {
				o.Context = ctx
				if phase == "before-lock" {
					cancel()
				} else {
					calls := 0
					o.Resolve = func() (Context, error) {
						calls++
						if calls == 2 {
							cancel()
						}
						return c, nil
					}
				}
			})
			if code != 2 || out.Reason != "snapshot_unavailable" || out.Stored != nil {
				t.Fatalf("%d/%s", code, out.Reason)
			}
		})
	}
}

func TestAssociationMachineLabelBoundary(t *testing.T) {
	for _, kind := range []string{"actual-host", "slug-matches-foreign", "case", "space", "absent-slug", "poison-slug", "changed-host", "missing-local-label"} {
		t.Run(kind, func(t *testing.T) {
			c, b, s, now := fixture(t)
			binding := s["terminal"].(map[string]any)
			want := "stored_tuple_equal"
			codeWant := 0
			switch kind {
			case "slug-matches-foreign":
				binding["host"] = "synthetic-host"
				want = "stale_binding"
				codeWant = 2
			case "case":
				binding["host"] = strings.ToLower(c.MachineLabel)
				want = "stale_binding"
				codeWant = 2
			case "space":
				binding["host"] = c.MachineLabel + " "
				want = "stale_binding"
				codeWant = 2
			case "absent-slug":
				if e := os.Remove(filepath.Join(c.Root, "host")); e != nil {
					t.Fatal(e)
				}
			case "poison-slug":
				p := filepath.Join(c.Root, "host")
				if e := os.Remove(p); e != nil {
					t.Fatal(e)
				}
				if e := syscall.Mkfifo(p, 0600); e != nil {
					t.Fatal(e)
				}
			case "missing-local-label":
				c.MachineLabel = ""
				want = "context_unavailable"
				codeWant = 2
			case "changed-host":
				want = "context_unavailable"
				codeWant = 2
			}
			out, code, _ := execute(t, c, b, s, now, func(o *Options) {
				if kind == "changed-host" {
					calls := 0
					o.Resolve = func() (Context, error) {
						calls++
						fresh := c
						if calls > 1 {
							fresh.MachineLabel = "Synthetic-Other.local"
						}
						return fresh, nil
					}
				}
			})
			if code != codeWant || out.Reason != want {
				t.Fatalf("%d/%s want %d/%s", code, out.Reason, codeWant, want)
			}
			if code != 0 && out.Stored != nil {
				t.Fatal("foreign binding projection")
			}
		})
	}
}

func TestAssociationRefusalPrecedenceAfterBudget(t *testing.T) {
	for _, kind := range []string{"untrusted", "invalid", "context", "future", "expired", "admitted"} {
		t.Run(kind, func(t *testing.T) {
			c, b, s, now := fixture(t)
			r := object(t, b)
			switch kind {
			case "invalid":
				r["unknown"] = true
			case "future":
				r["issued_at_ms"] = now.UnixMilli() + 1
			case "expired":
				r["issued_at_ms"] = now.UnixMilli() - 10
				r["expires_at_ms"] = now.UnixMilli()
			}
			out, code, _ := execute(t, c, marshal(t, r), s, now, func(o *Options) {
				// Delay inside the injected clock after Run starts: deterministic over-budget
				// controls without changing production deadlines or the host clock.
				calls := 0
				o.Now = func() time.Time {
					calls++
					if calls == 1 {
						time.Sleep(260 * time.Millisecond)
					}
					return now
				}
				if kind == "untrusted" {
					o.Input.Close()
					o.Input = nil
				}
				if kind == "context" {
					o.Resolve = func() (Context, error) { return Context{}, invalid }
				}
			})
			want := map[string]string{"untrusted": "input_untrusted", "invalid": "input_invalid", "context": "context_unavailable", "future": "input_invalid", "expired": "input_expired", "admitted": "budget_exceeded"}[kind]
			if code != 2 || out.Reason != want || out.Stored != nil {
				t.Fatalf("%d/%s want %s", code, out.Reason, want)
			}
			if kind != "admitted" && out.Digest != nil {
				t.Fatal("unadmitted digest")
			}
		})
	}
}
