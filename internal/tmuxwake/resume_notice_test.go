package tmuxwake

import (
	"context"
	"testing"
)

// Continuation submits only exact complete owned input, never types into an
// empty box, and pins the supplied composer and runtime key.
// These use private synthetic fixtures, not installed runtime observations.
func TestBoundResumeNotice(t *testing.T) {
	for _, runtime := range []string{"claude", "claude-named", "codex"} {
		for _, mode := range []string{"valid", "foreign", "dialog"} {
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				f := newBoundFixture(t, runtime, "2.1.318", "idle")
				expected := f.b.AssessReadiness(context.Background()).Key
				expected.ComposerRow = 16 // Pin the actual recovery composer, not the earlier empty row.
				f.b.NoticePosition = &ComposerPosition{Row: 16, Column: 2}
				text := Notice(1)
				if mode == "foreign" {
					text = "FOREIGN"
				}
				if text != "" {
					f.f.typeText(text)
					f.f.wait(func(s fixtureState) bool { return s.Text == text })
				}
				options := map[string]any{"row": 17, "cursor": -1, "cursor-row": 10}
				if mode == "dialog" {
					options["extra"] = " Permission required"
				}
				f.f.render(options)
				fences := 0
				result, err := f.b.ResumeNotice(context.Background(), 1, Notice(1), expected, func() error { fences++; return nil })
				if err != nil {
					t.Fatal(err)
				}
				if (mode == "valid" && fences != 1) || (mode != "valid" && fences != 0) {
					t.Fatal("Enter fence spent before a key attempt")
				}
				if mode == "valid" {
					s := f.f.wait(func(s fixtureState) bool { return len(s.Submitted) == 1 })
					if result.Disposition != SendSubmitted || s.Text != "" || s.Submitted[0] != text {
						t.Fatal("owned continuation refused")
					}
					again, err := f.b.ResumeNotice(context.Background(), 1, Notice(1), expected)
					if err != nil || again.Disposition != SendRefused {
						t.Fatal("empty continuation not refused")
					}
					// A capture round-trip follows the second attempt; no second Enter or typing occurred.
					f.f.render(map[string]any{"row": 20})
					s = f.f.wait(func(s fixtureState) bool { return s.Text == "" })
					if len(s.Submitted) != 1 {
						t.Fatal("continuation replayed Enter")
					}
				} else {
					s := f.f.wait(func(s fixtureState) bool { return s.Render == 1 })
					if mode == "dialog" && result.Reason != ReasonBusyOrDialog {
						t.Fatal("lost specific refusal reason")
					}
					if result.ReasonAt.IsZero() {
						t.Fatal("missing refusal timestamp")
					}
					if result.Disposition == SendSubmitted || s.Text != text || len(s.Submitted) != 0 {
						t.Fatal("continuation refusal had effects")
					}
				}
			})
		}
	}
}

// Every current deny-list phrase blocks an otherwise exact owned composer for
// both parsers. The adjacent no-dialog frame proves the grammar still admits.
func TestOwnedNoticeDenyList(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		for _, denied := range []string{"", "esc to interrupt", "escape to interrupt", "do you want to", "would you like to", "implement this plan", "permission required", "permission request", "approval required", "approve this", "allow once", "bypass permissions?", "accept edits?", "enter to confirm"} {
			t.Run(runtime+"/"+denied, func(t *testing.T) {
				screen := "──────────\n❯ " + Notice(1) + "\n──────────\n" + denied + "\n"
				if runtime == "codex" {
					screen = "› " + Notice(1) + "\n\nCodex · Ready\n" + denied + "\n"
				}
				b, reason := inputWithNoticeReason(screen, runtime, Notice(1))
				if denied == "" {
					if reason != InputReasonNone || b.Text != Notice(1) || !completeInput(screen, b) {
						t.Fatal("exact no-dialog control refused")
					}
				} else if reason != InputReasonBusyOrDialog {
					t.Fatal("owned notice bypassed deny list")
				}
			})
		}
	}
}
