package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// In private CI panes, transcript/footer/missing-composer matches all retain
// base busy_or_dialog refusal. Both status forms expose only location tokens,
// preserve the assessment and stored state, and send no input. Empty controls
// admit. These synthetic layouts do not establish real dialog geometry.
func TestReadinessBusyMatchLocation(t *testing.T) {
	for _, runtime := range []string{"codex", "claude", "claude-named"} {
		t.Run(runtime, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			if runtime != "codex" {
				dispatchRegistry(t, f, "idle")
			}
			control := func() map[string]any {
				return map[string]any{"text": "", "claude": runtime != "codex", "named": runtime == "claude-named"}
			}
			f.control(control())
			if !boundTerminal(*f.binding()).AssessReadiness(context.Background()).Eligible() {
				t.Fatal("empty positive control refused")
			}
			for _, location := range []string{"above_composer", "at_or_below_composer", "no_composer"} {
				t.Run(location, func(t *testing.T) {
					c := control()
					c["diagnostic-text"] = "PRIVATE_SCREEN_SENTINEL permission required"
					c["diagnostic-row"] = 10
					if location == "at_or_below_composer" {
						c["diagnostic-row"] = 23
					}
					if location == "no_composer" {
						c["no-composer"] = true
					}
					f.control(c)
					lane := boundTerminal(*f.binding())
					before := lane.AssessReadiness(context.Background())
					want := tmuxwake.InputAssessment{State: tmuxwake.InputUnavailable, Reason: tmuxwake.InputReasonBusyOrDialog}
					if before.Input != want || before.Eligible() || before.Key != (tmuxwake.ReadinessKey{}) {
						t.Fatal("base refusal lost", before)
					}
					stateBefore, _ := json.Marshal(f.e.state("alice"))
					for _, asJSON := range []bool{false, true} {
						var out bytes.Buffer
						f.a.Stdout = &out
						var args []string
						if asJSON {
							args = []string{"--json"}
						}
						if err := f.a.terminalStatus(context.Background(), args); err != nil {
							t.Fatal(err)
						}
						if asJSON {
							var got struct {
								Input    tmuxwake.InputAssessment `json:"input"`
								Location string                   `json:"busy_match_location"`
							}
							if err := json.Unmarshal(out.Bytes(), &got); err != nil {
								t.Fatal(err)
							}
							if got.Input != want || got.Location != location {
								t.Fatal("wrong diagnostic", got)
							}
						} else if !strings.Contains(out.String(), "input=unavailable reason=busy_or_dialog\n") || !strings.Contains(out.String(), "busy_match_location="+location+"\n") {
							t.Fatal("missing diagnostic", out.String())
						}
						for _, private := range []string{"PRIVATE_SCREEN_SENTINEL", "permission required", "a2a-fixture-frame", "synthetic-session"} {
							if strings.Contains(out.String(), private) {
								t.Fatal("screen leaked")
							}
						}
					}
					stateAfter, _ := json.Marshal(f.e.state("alice"))
					if !bytes.Equal(stateBefore, stateAfter) {
						t.Fatal("status mutated state")
					}
					if after := lane.AssessReadiness(context.Background()); after != before {
						t.Fatal("status changed refusal", after)
					}
					screen := f.wait(func(s orphanScreen) bool { return s.Rev == f.rev })
					if screen.Text != "" || len(screen.Submitted) != 0 || screen.Erased != 0 {
						t.Fatal("status sent input")
					}
				})
			}
		})
	}
}

// The location hint honors the 18-row window and SGR stripping; a match on
// the boundary or either side of it never gets described as above-only.
// Missing matches report unavailable, not a fabricated composer location.
func TestBusyMatchLocationBoundaries(t *testing.T) {
	for _, tc := range []struct{ name, screen, want string }{
		{"styled-above", "\x1b[2mAPPROVAL REQUIRED\x1b[0m\n› \n\n? for shortcuts", "above_composer"},
		{"prompt", "› approval required\n\n? for shortcuts", "at_or_below_composer"},
		{"both", "approval required\n› \n\napproval required", "at_or_below_composer"},
		{"missing", "approval required", "no_composer"},
		{"vanished", "› \n\n? for shortcuts", "unavailable"},
		{"outside-window", "approval required\n" + strings.Repeat("\n", 18) + "› \n\n? for shortcuts", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := busyMatchLocation(tc.screen, "codex"); got != tc.want {
				t.Fatalf("location=%s want=%s", got, tc.want)
			}
		})
	}
}
