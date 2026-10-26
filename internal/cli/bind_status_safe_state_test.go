package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Real bind-status text/JSON entries return only fixed state refusal codes,
// without raw paths/errors or state writes; a valid state still produces status.
// These local fixtures cover malformed JSON and mode-0000 refusal, not dispatch
// effects (covered by TestReviewerStateFailureRefusals) or live delivery.
func TestBindStatusSafeStateRefusalFormatting(t *testing.T) {
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) {
			for _, kind := range []string{"valid", "malformed", "permission-read"} {
				t.Run(kind, func(t *testing.T) {
					e, _, _ := bindDiagnosticEnv(t)
					path := e.paths().StateFile("alice")
					wantError := ""
					switch kind {
					case "malformed":
						if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
							t.Fatal(err)
						}
						wantError = "a2a: binding state: invalid\n"
					case "permission-read":
						wantError = "a2a: binding state: unavailable\n"
					}
					before := readFile(t, path)
					if kind == "permission-read" {
						if err := os.Chmod(path, 0000); err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { os.Chmod(path, 0600) })
					}
					args := []string{"bind", "status"}
					if format == "json" {
						args = append(args, "--json")
					}
					got := e.as("alice", args...)
					if wantError != "" {
						if got.code != ExitError || got.stderr != wantError || got.stdout != "" {
							t.Errorf("safe state refusal: got %+v; want exit=%d stderr=%q and empty stdout", got, ExitError, wantError)
						}
					} else {
						got.ok(t)
						if got.stderr != "" {
							t.Errorf("valid status stderr: %q", got.stderr)
						}
						if format == "json" {
							var status struct {
								Diagnostic struct{ Phase, Class, Result string }
							}
							if err := json.Unmarshal([]byte(got.stdout), &status); err != nil {
								t.Fatal(err)
							}
							if d := status.Diagnostic; d.Phase != "unknown" || d.Class != "none" || d.Result != "unknown" {
								t.Errorf("valid status diagnostic: %+v", d)
							}
						} else if !strings.Contains(got.stdout, "generation=diagnostic-generation") {
							t.Errorf("valid status missing binding: %q", got.stdout)
						}
					}
					for _, raw := range []string{path, "not valid state JSON", "has mode 0000", "unexpected end of JSON input", "permission denied"} {
						if strings.Contains(got.stdout+got.stderr, raw) {
							t.Errorf("raw state material %q exposed in status", raw)
						}
					}
					if kind == "permission-read" {
						info, err := os.Stat(path)
						if err != nil {
							t.Fatal(err)
						}
						if info.Mode().Perm() != 0000 {
							t.Error("status changed unreadable state permissions")
						}
						if err := os.Chmod(path, 0600); err != nil {
							t.Fatal(err)
						}
					}
					if readFile(t, path) != before {
						t.Error("status changed state bytes")
					}
				})
			}
		})
	}
}
