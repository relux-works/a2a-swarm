package tmuxwake

import (
	"context"
	"testing"
)

// An actual layout parse is witnessed separately from the default layout token
// on early registry refusal. Healthy readiness remains eligible; this flag
// never participates in admission and establishes no live incident cause.
func TestWakeDiagnosticParserWitness(t *testing.T) {
	for _, mode := range []string{"ready", "layout", "registry-refused"} {
		t.Run(mode, func(t *testing.T) {
			runtime, status := "claude-named", "idle"
			if mode == "layout" {
				runtime = "claude-named-unknown"
			}
			if mode == "registry-refused" {
				status = "busy"
			}
			f := newBoundFixture(t, runtime, "3.0.0", status)
			r := f.b.ObserveReadiness(context.Background(), "")
			if r.InputObserved != (mode != "registry-refused") || r.Eligible() != (mode == "ready") {
				t.Fatal("parser witness or existing eligibility changed")
			}
			if mode != "ready" && r.Input.Reason != InputReasonLayoutUnrecognized {
				t.Fatal("control did not exercise ambiguous layout token")
			}
			r.InputObserved = !r.InputObserved
			if r.Eligible() != (mode == "ready") {
				t.Fatal("diagnostic flag changed admission")
			}
		})
	}
}
