package wakediag

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Valid closed records round-trip and coalesce; invalid optional history neither
// breaks decoding nor projects untrusted text. The valid record is the control.
func TestWakeDiagnosticClosedRecord(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, kind := range []Kind{TerminalLayoutRefused, NativeDefiniteRefusal, NativeTransportUncertain} {
		r := Retain(nil, kind, at)
		if r == nil || Retain(r, kind, at.Add(time.Hour)) != r || !r.At.Equal(at) {
			t.Fatal("identical observation refreshed history")
		}
		b, _ := json.Marshal(r)
		var copy Record
		if json.Unmarshal(b, &copy) != nil || copy != *r || Project(&copy, kind) == nil {
			t.Fatal("valid observation lost")
		}
		other := TerminalLayoutRefused
		if other == kind {
			other = NativeDefiniteRefusal
		}
		if Project(r, other) != nil {
			t.Fatal("foreign stage projected")
		}
	}
	for _, raw := range []string{`42`, `{"kind":"PRIVATE_PAYLOAD","at":"2026-01-01T00:00:00Z"}`, `{"kind":"native_definite_refusal","at":"PRIVATE_PAYLOAD"}`, `{"kind":"terminal_pre_send_layout_refused"}`, `{"kind":42}`} {
		var r Record
		if err := json.Unmarshal([]byte(raw), &r); err != nil || r.Valid() || Project(&r, TerminalLayoutRefused, NativeDefiniteRefusal, NativeTransportUncertain) != nil {
			t.Fatal("invalid history became a diagnosis or blocked loading")
		}
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "PRIVATE_PAYLOAD") {
			t.Fatal("untrusted text retained")
		}
	}
	if Retain(nil, Kind("PRIVATE_PAYLOAD"), at) != nil || Retain(nil, TerminalLayoutRefused, time.Time{}) != nil {
		t.Fatal("invalid programmatic record accepted")
	}
}
