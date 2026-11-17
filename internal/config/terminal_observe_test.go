package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Claim: old state without the optional observe member loads with it
// absent, state with it round-trips byte-stable through marshal/unmarshal,
// and the clone deep-copies it (mutating the clone leaves the original).
func TestTerminalObserveStateRoundTrip(t *testing.T) {
	var old State
	if err := unmarshalState([]byte(`{"terminal":{"own_notice":{"own_notice_possibly_orphaned":null},"notice_publication":"","generation":"g","session_id":"s","host":"h","owner_pid":2,"owner_birth":"","daemon_pid":3,"daemon_birth":"","target":{},"interval":1,"retry_max":1,"max_wakes_per_hour":1,"stopped":false,"outcome":""}}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Terminal == nil || old.Terminal.Observe != nil {
		t.Fatalf("old state observe: %+v", old.Terminal)
	}
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	want := TerminalObserve{Executable: "/installed/a2a-image", Revision: strings.Repeat("a", 40), OnLaunchAlias: true, CPUAlerted: true, CPUAlertAt: at, CPUJobID: "job"}
	st := State{Terminal: &TerminalBinding{Generation: "g", Observe: &want}}
	data, err := marshalState(st)
	if err != nil {
		t.Fatal(err)
	}
	var back State
	if err := unmarshalState(data, &back); err != nil {
		t.Fatal(err)
	}
	if back.Terminal == nil || back.Terminal.Observe == nil || !reflect.DeepEqual(*back.Terminal.Observe, want) {
		t.Fatalf("round trip: %+v", back.Terminal)
	}
	clone, ok := cloneState(&st)
	if !ok {
		t.Fatal("clone refused")
	}
	clone.Terminal.Observe.Revision = "changed"
	if st.Terminal.Observe.Revision == "changed" {
		t.Fatal("clone shares the observe record")
	}
}

// Claim: the protected credential member distinguishes absent (default)
// from an explicit zero (disabled) across a write/read round trip, and an
// unrelated credential rewrite preserves it.
func TestBinderCPUPercentCredentialRoundTrip(t *testing.T) {
	p := Paths{Base: t.TempDir()}
	c := Credentials{UserID: "@a:test", AccessToken: "synthetic-token", DeviceID: "D", Homeserver: "https://example.invalid"}
	writeTestCreds(t, p, "a", c)
	got, err := LoadCredentialMetadata(p, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.BinderCPUPercent != nil {
		t.Fatalf("absent member decoded as %v", *got.BinderCPUPercent)
	}
	zero := 0
	if _, err := UpdateCredentials(p, "a", func(c *Credentials) error { c.BinderCPUPercent = &zero; return nil }); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p.AgentFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"binder_cpu_percent": 0`) {
		t.Fatalf("explicit zero not persisted: %s", raw)
	}
	if _, err := UpdateCredentials(p, "a", func(c *Credentials) error { c.OwnRoom = "!room:example.invalid"; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err = LoadCredentialMetadata(p, "a", false)
	if err != nil {
		t.Fatal(err)
	}
	if got.BinderCPUPercent == nil || *got.BinderCPUPercent != 0 {
		t.Fatalf("unrelated rewrite lost the member: %+v", got.BinderCPUPercent)
	}
}
