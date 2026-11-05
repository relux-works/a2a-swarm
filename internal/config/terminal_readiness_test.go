package config

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Diagnostic corruption never prevents loading the surrounding journal or
// turns diagnostic bytes into a new binding/root. Valid neighbors round-trip.
func TestTerminalReadinessDiagnosticDecode(t *testing.T) {
	for _, raw := range []string{
		`{"reason":"busy","version":"2.1.310","since":"2026-10-26T00:00:00Z"}`,
		`{"reason":{"private":"value"},"version":[],"since":"private-time"}`,
		`42`, `"private"`, `null`,
	} {
		var b TerminalBinding
		if err := json.Unmarshal([]byte(`{"generation":"g","session_id":"s","attempt_ids":["mail"],"notification_token":"token","readiness":`+raw+`}`), &b); err != nil {
			t.Fatal("diagnostic blocked state read", err)
		}
		if b.Generation != "g" || b.SessionID != "s" || b.NotificationToken != "token" || len(b.AttemptIDs) != 1 || b.ClaudeRegistryRoot != "" {
			t.Fatal("diagnostic changed binding", b)
		}
		if raw[0] == '{' && b.Readiness == nil {
			t.Fatal("missing diagnostic object")
		}
		if raw == `{"reason":"busy","version":"2.1.310","since":"2026-10-26T00:00:00Z"}` {
			if b.Readiness.Reason != tmuxwake.RuntimeBusy || b.Readiness.Version != "2.1.310" || !b.Readiness.Since.Equal(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)) {
				t.Fatal("valid diagnostic lost")
			}
		} else if b.Readiness != nil && !b.Readiness.Since.IsZero() {
			t.Fatal("invalid timestamp became authority")
		}
	}
}

// Old diagnostics without drift remain readable; corrupt drift fields cannot
// break the journal, and hostile stored names are bounded before reuse.
func TestTerminalReadinessUnknownKeysDecode(t *testing.T) {
	for _, raw := range []string{`{}`, `{"unknown_keys":42}`, `{"unknown_keys":"future"}`, `{"unknown_keys":"bad\nname"}`} {
		var b TerminalBinding
		if err := json.Unmarshal([]byte(`{"generation":"g","readiness":`+raw+`}`), &b); err != nil {
			t.Fatal(err)
		}
		if b.Generation != "g" || b.Readiness == nil || b.Readiness.UnknownKeys != tmuxwake.SafeRegistryUnknownKeys(b.Readiness.UnknownKeys) {
			t.Fatal("drift broke old state", b)
		}
		if raw == `{"unknown_keys":"future"}` && b.Readiness.UnknownKeys != "future" {
			t.Fatal("name lost")
		}
	}
}
