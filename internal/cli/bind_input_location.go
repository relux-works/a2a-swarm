package cli

import (
	"context"
	"os/exec"
	"regexp"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

var diagnosticSGR = regexp.MustCompile(`\x1b\[[0-9;]*m`)
var diagnosticRule = regexp.MustCompile(`^─{5,}$`)
var diagnosticTopRule = regexp.MustCompile(`^─{5,}( .+)?$`)

// This extra status-only capture is not the admission snapshot. Failures or a
// vanished match report unavailable; no captured bytes leave this helper.
func observeBusyMatchLocation(ctx context.Context, target tmuxwake.Target) string {
	if _, _, _, err := target.Inspect(ctx); err != nil {
		return "unavailable"
	}
	screen, err := exec.CommandContext(ctx, "tmux", "-S", target.Socket, "capture-pane", "-e", "-p", "-t", target.Pane).Output()
	if err != nil {
		return "unavailable"
	}
	if _, _, _, err := target.Inspect(ctx); err != nil {
		return "unavailable"
	}
	return busyMatchLocation(string(screen), target.Runtime)
}

// Geometry and phrase matching here describe location only, never eligibility.
// Keep the phrase vocabulary and 18-row window aligned with the base parser.
// If matches occur on both sides, report at_or_below_composer.
func busyMatchLocation(screen, runtime string) string {
	lines := strings.Split(strings.TrimSuffix(diagnosticSGR.ReplaceAllString(screen, ""), "\n"), "\n")
	start := max(0, len(lines)-18)
	boundary := -1
	for i := len(lines) - 1; i >= start; i-- {
		if runtime == "claude" && i >= start+2 && diagnosticRule.MatchString(strings.TrimSpace(lines[i])) && diagnosticTopRule.MatchString(strings.TrimSpace(lines[i-2])) && diagnosticPrompt(lines[i-1], "❯") {
			boundary = i - 2
			break
		}
		if runtime == "codex" && diagnosticPrompt(lines[i], "›") && i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
			boundary = i
			break
		}
	}
	found := false
	for i := start; i < len(lines); i++ {
		line := strings.ToLower(lines[i])
		for _, phrase := range []string{"esc to interrupt", "escape to interrupt", "do you want to", "would you like to", "implement this plan", "permission required", "permission request", "approval required", "approve this", "allow once", "bypass permissions?", "accept edits?", "enter to confirm"} {
			if strings.Contains(line, phrase) {
				found = true
				if boundary >= 0 && i >= boundary {
					return "at_or_below_composer"
				}
			}
		}
	}
	if !found {
		return "unavailable"
	}
	if boundary < 0 {
		return "no_composer"
	}
	return "above_composer"
}

func diagnosticPrompt(line, glyph string) bool {
	line = strings.TrimLeft(line, " ")
	return line == glyph || strings.HasPrefix(line, glyph+" ") || strings.HasPrefix(line, glyph+"\u00a0")
}
