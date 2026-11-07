package tmuxwake

import (
	"os"
	"strings"
	"testing"
)

// Render-setting audit against HEAD^ (before the test move):
//   - prompt-foreign carried indent=3 as well as transcript row 12; restored here
//     and isolated with a hint in TestCodexIndentedForeignGuard/foreign.
//   - prompt-empty/transcript carried text="" and cursor=0; parser empty geometry
//     and legacy/Codex cursor admission remain below; named cursor admission is
//     retained through real TestTerminalTranscriptPromptAdmission/claude-named/fresh.
//   - second-prompt carried row 23; prompt-above carried the runtime glyph.
//   - filled-position carried row 17/indent 3, NBSP, rule width 80 and crash hint;
//     row/column/both remain below and real recorded/no-anchor cases remain in CLI.
//   - pre-row/up/down used an on-composer cursor, unlike pre-cursor; restored in
//     Send's Codex fixtures. Post-type empty now has a live AwaitingRender row.
//
// Identity/version/width/status/time claims use actual changed keys and origin
// tables; restart, lost ACK, mail count and no-replay effects stay in dispatch.
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
				if mode == "column" || mode == "both" || mode == "prompt-foreign" {
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

// An indented foreign prompt below a transcript notice must stop the scan.
// The hint and blank separator deliberately allow the transcript to pass every
// later parser check if the guard is deleted. Exact indented input is its valid
// neighbor; no-foreign also proves that the upper notice is otherwise selectable.
func TestCodexIndentedForeignGuard(t *testing.T) {
	for _, mode := range []string{"foreign", "owned", "no-foreign"} {
		t.Run(mode, func(t *testing.T) {
			lines := make([]string, 24)
			lines[11] = "› " + Notice(1)
			lines[21] = "  ? for shortcuts"
			wantRow := 11
			switch mode {
			case "foreign":
				lines[19] = "   › HUMAN_DRAFT"
			case "owned":
				lines[19] = "   › " + Notice(1)
				wantRow = 19
			}
			screen := strings.Join(lines, "\n") + "\n"
			b, reason, _ := boundInput(screen, "codex", Notice(1), false)
			if mode == "foreign" {
				if reason != InputReasonLayoutUnrecognized {
					t.Fatal("indented foreign prompt admitted transcript", reason, b)
				}
				return
			}
			column := 2
			if mode == "owned" {
				column += 3
			}
			if reason != InputReasonNone || b.Text != Notice(1) || b.Row != wantRow || b.position().Column != column || !b.FooterHint || !completeInput(screen, b) {
				t.Fatal("valid indented guard neighbor refused", reason, b)
			}
		})
	}
}

// Owned typing normalizes actual composer geometry, while identity, width and
// registry evidence stay pinned. Cursor coordinates are not ReadinessKey fields;
// their real Send witnesses live in TestBoundSendComposerMovement.
func TestOwnedNoticeKeyNeighbors(t *testing.T) {
	base := ReadinessKey{Route: RegistryNamed, Target: Target{Runtime: "claude", OwnerPID: 123, OwnerBirth: "birth", Pane: "%0", SocketID: "socket"}, ClaudeRegistryRoot: "/synthetic", Generation: "g", SessionID: "s", RegistrySessionID: "s", RegistryVersion: "2.1.318", RegistryStatus: "idle", ComposerRow: 19, ComposerColumn: 2, PaneWidth: 120}
	for _, mode := range []string{"valid", "row-down", "row-up", "column", "width", "registry-busy", "status", "timestamp", "generation", "session", "target", "version"} {
		t.Run(mode, func(t *testing.T) {
			k := base
			switch mode {
			case "row-down":
				k.ComposerRow++
			case "row-up":
				k.ComposerRow--
			case "column":
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
			geometry := mode == "valid" || mode == "row-down" || mode == "row-up" || mode == "column"
			if sameKeyAfterOwnedTyping(k, base) != geometry {
				t.Fatal("wrong post-type comparison")
			}
			if mode == "width" || mode == "status" || mode == "timestamp" || mode == "row-down" || mode == "row-up" || mode == "column" {
				r := ReadinessAssessment{Runtime: runtimeResult(RuntimeNone, ""), Input: InputAssessment{InputEmpty, InputReasonNone}, Key: k}
				want := ReasonKeyStatusChanged
				switch mode {
				case "width":
					want = ReasonKeyWidthChanged
				case "row-down", "row-up":
					want = ReasonKeyRowChanged
				case "column":
					want = ReasonKeyChanged
				}
				if got := sendRefusalReason(r, base, Box{}, ""); got != want {
					t.Fatal("wrong pre-type refusal token", got)
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
		for _, mode := range []string{"empty", "owned", "shell", "registry-busy", "unknown", "busy", "dialog"} {
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
				case "shell":
					k.RegistryStatus = "shell"
					r.Key = k
				case "registry-busy":
					k.RegistryStatus = "busy"
					r.Key = k
				case "empty":
					r.Input = InputAssessment{InputEmpty, InputReasonNone}
				case "unknown":
					r.Input = InputAssessment{InputUnavailable, InputReasonLayoutUnrecognized}
				case "busy":
					r.Runtime = runtimeResult(RuntimeBusy, "")
				case "dialog":
					r.Input = InputAssessment{InputUnavailable, InputReasonBusyOrDialog}
				}
				if r.Eligible() != (mode == "empty") || r.OwnedNoticeEligible() != (mode == "owned" || mode == "shell" && (route == RegistryLegacy || route == RegistryNamed)) {
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
