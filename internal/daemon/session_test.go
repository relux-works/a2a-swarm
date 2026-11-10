//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sessionWire(t *testing.T, r *running, advertise bool, value json.RawMessage) []byte {
	t.Helper()
	c, nonce := connect(t, r)
	defer c.Close()
	helloBytes := hello(r.secret, nonce, "station", "identity")
	if advertise {
		var fields map[string]json.RawMessage
		if json.Unmarshal(helloBytes, &fields) != nil {
			t.Fatal("hello setup")
		}
		fields["capabilities"] = json.RawMessage(`["session-evidence/1"]`)
		helloBytes, _ = json.Marshal(fields)
	}
	if writeFrame(c, helloBytes) != nil {
		t.Fatal("hello write")
	}
	b, err := readFrame(c)
	if err != nil {
		t.Fatal("hello read", err)
	}
	fields, err := object(b, nil)
	if err != nil || (fields["capabilities"] != nil) != advertise {
		t.Fatal("capability not connection scoped")
	}
	raw, _ := json.Marshal(Request{Type: "request", ID: strings.Repeat("1", 32), Argv: []string{"whoami"}, Files: map[int]InputFile{}})
	if value != nil {
		var f map[string]json.RawMessage
		if json.Unmarshal(raw, &f) != nil {
			t.Fatal("request setup")
		}
		f["session"] = value
		raw, _ = json.Marshal(f)
	}
	if writeFrame(c, raw) != nil {
		t.Fatal("request write")
	}
	b, err = readFrame(c)
	if err != nil {
		t.Fatal("request response", err)
	}
	return b
}

// Actual handshakes cover a new peer, an old station, and a strict old daemon.
// The downgrade neighbor executes once with no Session; it must never leak it.
func TestDaemonSessionNegotiation(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "new", true: "legacy"}[legacy], func(t *testing.T) {
			seen := make(chan string, 3)
			var calls atomic.Int32
			// Register before server cleanup: assert only after all connections
			// have joined, so a late duplicate cannot escape the live check.
			wantFinal := int32(1)
			if !legacy {
				wantFinal = 2 // explicit old-station neighbor below
			}
			t.Cleanup(func() {
				if calls.Load() != wantFinal {
					t.Error("final request count", calls.Load(), wantFinal)
				}
			})
			r := executionServerOptions(t, func(ctx context.Context, inv Invocation) int {
				calls.Add(1)
				select {
				case seen <- inv.Request.Session:
					return 0
				case <-ctx.Done():
					return 1
				}
			}, time.Second, func(s *server) { s.legacyHello = legacy })
			code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}, Session: "owner-1"}, io.Discard, io.Discard)
			want := "owner-1"
			if legacy {
				want = ""
			}
			if code != 0 || err != nil {
				t.Fatal("negotiated invocation", code, err)
			}
			if calls.Load() != 1 {
				t.Fatal("negotiated invocation must execute exactly one request", calls.Load())
			}
			select {
			case got := <-seen:
				if got != want {
					t.Fatal("session crossed capability boundary")
				}
			case <-r.done:
				t.Fatal("server stopped")
			case <-time.After(LockReleaseBound):
				t.Fatal("handler missing")
			}
			if !legacy {
				b := sessionWire(t, r, false, nil)
				var p packet
				if json.Unmarshal(b, &p) != nil || p.Type != "exit" || p.Code != 0 {
					t.Fatal("old station neighbor")
				}
				select {
				case got := <-seen:
					if got != "" {
						t.Fatal("old station acquired evidence")
					}
				case <-r.done:
					t.Fatal("server stopped")
				case <-time.After(LockReleaseBound):
					t.Fatal("old station handler missing")
				}
			}
		})
	}
}

// The shared decoder and live daemon reject every malformed present value.
// A typed empty Session is absent, while an explicit wire empty string refuses.
func TestDaemonSessionGrammar(t *testing.T) {
	var calls atomic.Int32
	r := executionServer(t, func(context.Context, Invocation) int { calls.Add(1); return 0 }, time.Second)
	good := func() {
		t.Helper()
		before := calls.Load()
		b := sessionWire(t, r, true, json.RawMessage(`"owner-1"`))
		var p packet
		if json.Unmarshal(b, &p) != nil || p.Type != "exit" || p.Code != 0 || calls.Load() != before+1 {
			t.Fatal("grammar valid neighbor")
		}
	}
	for _, id := range []string{"a", "CodeX.thread_7", strings.Repeat("x", 128)} {
		if !ValidSession(id) {
			t.Fatal("valid grammar refused")
		}
	}
	for _, raw := range []json.RawMessage{json.RawMessage(`""`), json.RawMessage(`null`), json.RawMessage(`1`), json.RawMessage(`[]`), json.RawMessage(`{}`), json.RawMessage(`"a\n"`), json.RawMessage(`"a\u0000"`), json.RawMessage(`"a b"`), json.RawMessage(`"pid:1@host"`), json.RawMessage(`"-"`), json.RawMessage(`"é"`), json.RawMessage(`"` + strings.Repeat("x", 129) + `"`)} {
		before := calls.Load()
		b := sessionWire(t, r, true, raw)
		if diagnosticFrame(b, strings.Repeat("1", 32)) != NotAllowed || calls.Load() != before {
			t.Fatal("malformed session did not refuse exactly not_allowed")
		}
		good()
	}
	before := calls.Load()
	b := sessionWire(t, r, false, json.RawMessage(`"owner-1"`))
	if diagnosticFrame(b, strings.Repeat("1", 32)) != NotAllowed || calls.Load() != before {
		t.Fatal("unadvertised evidence admitted")
	}
	good()
	for _, id := range []string{strings.Repeat("x", 129), "a\n", "pid:1@host", "-"} {
		before := calls.Load()
		code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}, Session: id}, io.Discard, io.Discard)
		if code != 1 || err != NotAllowed || calls.Load() != before {
			t.Fatal("station grammar refusal", code, err)
		}
		good()
	}
}

// Literal contract membership is independent of class constants and wire data.
// Extending the admitted array, echoing unknown classes, or losing the seventh
// class fails this test. Each refusal has a normal invocation neighbor.
func TestDaemonSevenDiagnosticClasses(t *testing.T) {
	want := []string{"enrollment_invalid", "daemon_unavailable", "identity_mismatch", "not_allowed", "daemon_idle_timeout", "daemon_busy", "daemon_lease_conflict"}
	if len(diagnosticClasses) != 7 {
		t.Fatal("closed diagnostic set changed")
	}
	for i, class := range diagnosticClasses {
		if string(class) != want[i] || !validDiagnostic(class) {
			t.Fatal("closed class membership changed")
		}
	}
	for _, class := range []Diagnostic{"", "daemon_future", "daemon_lease_conflict\n", "lease_conflict"} {
		if validDiagnostic(class) || Class(class) != Unavailable {
			t.Fatal("unknown diagnostic admitted")
		}
	}
	for _, class := range append(append([]Diagnostic{}, diagnosticClasses[:]...), Diagnostic("daemon_future")) {
		r := executionServer(t, func(_ context.Context, inv Invocation) int {
			if inv.Request.Argv[0] == "inbox" {
				return inv.Refuse(class)
			}
			return 0
		}, time.Second)
		var out, diag bytes.Buffer
		code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"inbox"}}, &out, &diag)
		if code != 1 || err != Class(class) || out.Len() != 0 || diag.Len() != 0 {
			t.Fatal("wire class refused incorrectly", code, err)
		}
		code, err = Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, &out, &diag)
		if code != 0 || err != nil {
			t.Fatal("diagnostic neighbor", code, err)
		}
	}
}

// Capability metadata uses a positive, versioned list, never substring matching.
// The original four-field hello and the exact extension remain valid neighbors.
func TestDaemonSessionCapabilitySchema(t *testing.T) {
	base := hello(make([]byte, 32), make([]byte, 32), "station", "identity")
	for _, raw := range []json.RawMessage{json.RawMessage(`[]`), json.RawMessage(`null`), json.RawMessage(`"session-evidence/1"`), json.RawMessage(`["session-evidence/2"]`), json.RawMessage(`["session-evidence/1","other"]`), json.RawMessage(`["session-evidence/1","session-evidence/1"]`)} {
		var fields map[string]json.RawMessage
		if json.Unmarshal(base, &fields) != nil {
			t.Fatal("hello fixture")
		}
		fields["capabilities"] = raw
		b, _ := json.Marshal(fields)
		if sessionCapabilities(raw) {
			t.Fatal("invalid capability admitted")
		}
		if _, _, _, err := parseHello(b); err != errRefused {
			t.Fatal("hello capability refusal", err)
		}
		fields["capabilities"] = json.RawMessage(`["session-evidence/1"]`)
		b, _ = json.Marshal(fields)
		if _, _, _, err := parseHello(b); err != nil {
			t.Fatal("extended hello neighbor", err)
		}
		if _, _, _, err := parseHello(base); err != nil {
			t.Fatal("legacy hello neighbor", err)
		}
	}
}
