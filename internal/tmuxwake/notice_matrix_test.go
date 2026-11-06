package tmuxwake

import (
	"os"
	"strings"
	"testing"
)

// Captured text and synthetic neighboring cells exercise the parser without a
// terminal. They prove classification/geometry, not delivery or key effects.
func TestOwnedNoticeCapturedMatrix(t *testing.T) {
	for _, layout := range []string{"claude", "claude-named", "codex"} {
		path := "testdata/claude-idle-bypass-footer-empty.txt"
		if layout == "codex" {
			path = "testdata/codex-owned-notice-no-hint.txt"
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"exact", "empty", "foreign", "prefix", "suffix", "partial", "count", "dialog", "busy", "unknown", "dim", "transcript", "prompt-above", "prompt-foreign", "prompt-empty", "second-prompt", "row", "column", "both"} {
			t.Run(layout+"/"+mode, func(t *testing.T) {
				// Preserve captured prompt/footer spelling; fit it into a 24-row grid.
				lines := make([]string, 24)
				text := Notice(1)
				switch mode {
				case "empty", "transcript", "prompt-empty":
					text = ""
				case "foreign", "prompt-foreign":
					text = "HUMAN_DRAFT"
				case "prefix":
					text = "KEEP" + text
				case "suffix":
					text += "KEEP"
				case "partial":
					text = "a2a: 1 new peer"
				case "count":
					text = Notice(2)
				case "dim":
					text += "\x1b[2mKEEP\x1b[0m"
				}
				row, indent := 19, 0
				if mode == "row" || mode == "both" {
					row = 16
				}
				if mode == "column" || mode == "both" {
					indent = 3
				}
				glyph, footer := "❯\u00a0", ""
				captured := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
				if layout == "codex" {
					glyph = "› "
					footer = captured[len(captured)-1]
					if text == "" {
						footer = "  ? for shortcuts"
					}
				} else {
					for _, line := range captured {
						if strings.Contains(line, "bypass permissions on") {
							footer = line
						}
					}
					lines[row-1], lines[row+1] = strings.Repeat("─", 80), strings.Repeat("─", 80)
					if layout == "claude-named" {
						lines[row-1] = strings.Repeat("─", 60) + " synthetic-session"
					}
				}
				lines[row] = strings.Repeat(" ", indent) + glyph + text
				lines[row+2] = footer
				switch mode {
				case "dialog":
					lines[23] = "Permission required"
				case "busy":
					lines[23] = "esc to interrupt"
				case "unknown":
					lines = make([]string, 24)
					lines[19] = "unknown input"
				case "transcript":
					lines[8] = Notice(1)
				case "prompt-above", "prompt-foreign", "prompt-empty":
					lines[11] = glyph + Notice(1)
				case "second-prompt":
					lines[23] = glyph + "HUMAN_DRAFT"
				}
				screen := strings.Join(lines, "\n") + "\n"
				runtime := "claude"
				if layout == "codex" {
					runtime = "codex"
				}
				b, reason, _ := boundInput(screen, runtime, Notice(1), layout == "claude-named")
				want := InputReasonNone
				switch mode {
				case "dialog", "busy":
					want = InputReasonBusyOrDialog
				case "unknown", "second-prompt":
					want = InputReasonLayoutUnrecognized
				}
				if layout == "codex" {
					switch mode {
					case "foreign", "prefix", "suffix", "partial", "count", "dim", "prompt-foreign":
						want = InputReasonLayoutUnrecognized
					}
				}
				if reason != want {
					t.Fatalf("classification=%s want=%s", reason, want)
				}
				if reason != InputReasonNone {
					return
				}
				exact := mode == "exact" || mode == "prompt-above" || mode == "row" || mode == "column" || mode == "both"
				if (b.Text == Notice(1) && completeInput(screen, b)) != exact {
					t.Fatalf("wrong owned classification: %+v", b)
				}
				if exact && b.position() != (ComposerPosition{Row: row, Column: 2 + indent}) {
					t.Fatal("wrong filled geometry", b)
				}
				if layout == "codex" && exact && b.FooterHint {
					t.Fatal("filled captured Codex unexpectedly has a hint")
				}
				if text == "" && (b.Text != "" || b.Row != row) {
					t.Fatal("history displaced empty composer")
				}
				// Empty admission requires the actual cursor even though
				// exact owned typing may leave the cursor elsewhere.
				if text == "" && layout != "claude-named" {
					valid := assessInput(screen, runtime, b.Column, b.Row)
					shifted := assessInput(screen, runtime, b.Column+1, b.Row)
					if valid.State != InputEmpty || valid.Reason != InputReasonNone || shifted.State != InputUnavailable || shifted.Reason != InputReasonCursorMismatch {
						t.Fatal("empty cursor admission changed", valid, shifted)
					}
				}
			})
		}
	}
}

// Exact owned typing normalizes geometry only; pre-type keys retain full
// equality. Each changed runtime/identity/width/status field still refuses.
func TestOwnedNoticeKeyNeighbors(t *testing.T) {
	base := ReadinessKey{Route: RegistryNamed, Target: Target{Runtime: "claude", OwnerPID: 123, OwnerBirth: "birth", Pane: "%0", SocketID: "socket"}, ClaudeRegistryRoot: "/synthetic", Generation: "g", SessionID: "s", RegistrySessionID: "s", RegistryVersion: "2.1.318", RegistryStatus: "idle", ComposerRow: 19, ComposerColumn: 2, PaneWidth: 120}
	for _, mode := range []string{"valid-down", "pre-row-down", "pre-row", "pre-cursor", "column", "cursor", "cursor-row", "width", "registry-busy", "status", "timestamp", "generation", "session", "target", "version"} {
		t.Run(mode, func(t *testing.T) {
			k := base
			switch mode {
			case "valid-down", "pre-row-down":
				k.ComposerRow++
			case "pre-row":
				k.ComposerRow--
			case "pre-cursor", "column":
				k.ComposerColumn++
			case "width":
				k.PaneWidth++
			case "registry-busy":
				k.RegistryStatus = "busy"
			case "status":
				k.RegistryStatus = "shell"
			case "timestamp":
				k.RegistryStatusAt++
			case "generation":
				k.Generation = "other"
			case "session":
				k.SessionID = "other"
			case "target":
				k.Target.Pane = "%2"
			case "version":
				k.RegistryVersion = "2.1.319"
			}
			geometry := mode == "valid-down" || mode == "pre-row-down" || mode == "pre-row" || mode == "pre-cursor" || mode == "column" || mode == "cursor" || mode == "cursor-row"
			if sameKeyAfterOwnedTyping(k, base) != geometry {
				t.Fatal("wrong post-type comparison")
			}
			if strings.HasPrefix(mode, "pre-") && k == base {
				t.Fatal("pre-type movement lost equality constraint")
			}
			if mode == "width" || mode == "status" || mode == "timestamp" {
				r := ReadinessAssessment{Runtime: runtimeResult(RuntimeNone, ""), Input: InputAssessment{InputNonempty, InputReasonInputNonempty}, Key: k}
				want := ReasonKeyStatusChanged
				if mode == "width" {
					want = ReasonKeyWidthChanged
				}
				if got := sendRefusalReason(r, base, Box{Text: Notice(1)}, Notice(1)); got != want {
					t.Fatal("wrong refusal token", got)
				}
			}
		})
	}
}

// Known empty/nonempty/unavailable observations exercise both eligibility
// directions; a busy registry cannot be rescued by exact owned input. These
// are supplied assessments, not a claim that capture or registry I/O succeeded.
func TestOwnedNoticeEligibilityNeighbors(t *testing.T) {
	for _, route := range []ReadinessRoute{LegacyCodex, RegistryLegacy, RegistryNamed, LegacyFallback} {
		for _, mode := range []string{"empty", "owned", "unknown", "busy", "dialog"} {
			t.Run(string(route)+"/"+mode, func(t *testing.T) {
				k := ReadinessKey{Route: route, Target: Target{Runtime: "claude", OwnerPID: 123, OwnerBirth: "birth", SocketID: "socket", Pane: "%0"}, Generation: "g", SessionID: "s", ComposerRow: 19, ComposerColumn: 2, PaneWidth: 120}
				reason := RuntimeNone
				if route == LegacyCodex {
					k.Target.Runtime = "codex"
				} else if route == LegacyFallback {
					reason = RuntimeRootUnavailable
				} else {
					k.ClaudeRegistryRoot = "/synthetic"
					k.RegistrySessionID = "s"
					k.RegistryVersion = "2.1.318"
					k.RegistryStatus = "idle"
				}
				r := ReadinessAssessment{Runtime: runtimeResult(reason, ""), Input: InputAssessment{InputNonempty, InputReasonInputNonempty}, Key: k}
				switch mode {
				case "empty":
					r.Input = InputAssessment{InputEmpty, InputReasonNone}
				case "unknown":
					r.Input = InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}
				case "busy":
					r.Runtime = runtimeResult(RuntimeBusy, "")
				case "dialog":
					r.Input = InputAssessment{InputUnavailable, InputReasonBusyOrDialog}
				}
				if r.Eligible() != (mode == "empty") || r.OwnedNoticeEligible() != (mode == "owned") {
					t.Fatal("wrong eligibility", r)
				}
				if mode == "busy" || mode == "dialog" {
					if sendRefusalReason(r, k, Box{Text: Notice(1)}, Notice(1)) != ReasonBusyOrDialog {
						t.Fatal("busy/dialog reason lost")
					}
				}
			})
		}
	}
}
