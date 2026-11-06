package cli

import (
	"context"
	"crypto/sha256"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"os/exec"
	"strings"
	"testing"
)

// Feasibility diagnostic, not a safety pass: observations are made without
// keystrokes against each stable private draft. screen.json is an assertion
// oracle only; no candidate guard reads it. Captured range excludes only the
// setup acknowledgement in row zero, never any composer/footer cells.
func TestRev3TransportObservations(t *testing.T) {
	isolateBindTest(t)
	for _, hint := range []bool{true, false} {
		t.Run(map[bool]string{true: "hint", false: "no-hint"}[hint], func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			notice := tmuxwake.Notice(1)
			modes := [][]string{{}, {"-N"}, {"-T"}, {"-T", "-N"}, {"-J"}, {"-C", "-T", "-N"}}
			base := map[string]string{}
			for _, variant := range []string{"valid", "space-left", "two-spaces-left", "space-end", "nonspace-left"} {
				text := notice
				cursor := 0
				switch variant {
				case "space-left":
					text += " "
					cursor = -1
				case "two-spaces-left":
					text += "  "
					cursor = -2
				case "space-end":
					text += " "
				case "nonspace-left":
					text += "X"
					cursor = -1
				}
				f.control(map[string]any{"text": text, "cursor": cursor, "hint": hint})
				target := f.binding().Target
				x, y, w, err := target.Inspect(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("variant=%s editor-oracle=%q cursor=%d,%d width=%d classification=%s", variant, text, x, y, w, target.PendingInput(context.Background(), notice))
				for _, flags := range modes {
					args := []string{"-S", target.Socket, "capture-pane", "-e", "-p", "-S", "19", "-E", "23", "-t", target.Pane}
					args = append(args, flags...)
					data, err := exec.Command("tmux", args...).Output()
					if err != nil {
						t.Fatal(err)
					}
					key := strings.Join(flags, " ")
					if variant == "valid" {
						base[key] = string(data)
					}
					t.Logf("variant=%s flags=%q equal-valid=%v bytes=%d sha256=%x capture=%q", variant, key, string(data) == base[key], len(data), sha256.Sum256(data), string(data))
				}
				s := f.wait(func(s orphanScreen) bool { return true })
				if s.Text != text || s.Erased != 0 || len(s.Submitted) != 0 {
					t.Fatal("observation mutated private draft")
				}
			}
		})
	}
}

// A counterexample to using rendered line extent as editor authority. This
// does not claim that a particular installed provider emits this exact frame.
// It establishes that the supported screen grammar admits a common cell
// renderer with no observable editor-length information, including -T -N.
func TestRev3PaddedRendererObservations(t *testing.T) {
	isolateBindTest(t)
	for _, layout := range []string{"codex-hint", "codex-no-hint", "claude"} {
		t.Run(layout, func(t *testing.T) {
			runtime := "codex"
			if layout == "claude" {
				runtime = "claude"
			}
			f := newRev3PaddedFixture(t, runtime)
			notice := tmuxwake.Notice(1)
			flags := [][]string{{}, {"-N"}, {"-T", "-N"}, {"-J"}, {"-C", "-T", "-N"}}
			base := map[string]string{}
			var position string
			for _, variant := range []string{"valid", "space-left", "two-spaces-left", "nonspace-left", "space-end"} {
				text := notice
				cursor := 0
				switch variant {
				case "space-left":
					text += " "
					cursor = -1
				case "two-spaces-left":
					text += "  "
					cursor = -2
				case "nonspace-left":
					text += "X"
					cursor = -1
				case "space-end":
					text += " "
				}
				f.control(map[string]any{"text": text, "cursor": cursor, "hint": layout != "codex-no-hint"})
				target := f.binding().Target
				x, y, w, err := target.Inspect(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				pos := strings.Join([]string{string(rune(x)), string(rune(y)), string(rune(w))}, "/")
				if variant == "valid" {
					position = pos
				}
				same := variant == "valid" || variant == "space-left" || variant == "two-spaces-left"
				if same && position != pos {
					t.Fatal("counterexample cursor mismatch")
				}
				t.Logf("layout=%s variant=%s actual-editor-oracle=%q cursor=%d,%d width=%d classification=%s", layout, variant, text, x, y, w, target.PendingInput(context.Background(), notice))
				for _, mode := range flags {
					args := []string{"-S", target.Socket, "capture-pane", "-e", "-p", "-S", "18", "-E", "23", "-t", target.Pane}
					args = append(args, mode...)
					data, err := exec.Command("tmux", args...).Output()
					if err != nil {
						t.Fatal(err)
					}
					key := strings.Join(mode, " ")
					if variant == "valid" {
						base[key] = string(data)
					}
					equal := base[key] == string(data)
					if same && !equal {
						t.Fatalf("unequal countermodel frame for %s", key)
					}
					if variant == "nonspace-left" && equal {
						t.Fatal("nonspace control not distinguished")
					}
					t.Logf("flags=%q equal-valid=%v bytes=%d sha256=%x capture=%q", key, equal, len(data), sha256.Sum256(data), string(data))
				}
				s := f.wait(func(s orphanScreen) bool { return true })
				if s.Text != text || s.Erased != 0 || len(s.Submitted) != 0 {
					t.Fatal("nonmutating capture changed draft")
				}
			}
		})
	}
}

// Known-red behavioral companion to the equal-observation countermodel.
// Both effect paths use real dispatch, durable journal and own fake inbox.
// The padded renderer is synthetic; it does not attest any installed runtime.
func TestRev3PaddedDraftEffects(t *testing.T) {
	isolateBindTest(t)
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			for _, path := range []string{"live", "orphan"} {
				t.Run(path, func(t *testing.T) {
					for _, variant := range []string{"valid", "space-left", "two-spaces-left"} {
						t.Run(variant, func(t *testing.T) {
							f := newRev3PaddedFixture(t, runtime)
							old := f.mail()
							notice := tmuxwake.Notice(1)
							f.control(map[string]any{"defer": true})
							f.dispatch()
							f.step()
							if !f.binding().Typed {
								t.Fatal("setup: dispatch did not leave typed journal")
							}
							current := old
							if path == "orphan" {
								f.e.as("alice", "inbox").ok(t)
								current = f.mail()
							}
							text := notice
							cursor := 0
							if variant == "space-left" || variant == "two-spaces-left" {
								text += " "
								cursor = -1
								if variant == "two-spaces-left" {
									text += " "
									cursor = -2
								}
							}
							f.control(map[string]any{"text": text, "cursor": cursor})
							f.dispatch()
							f.control(map[string]any{})
							s := f.wait(func(s orphanScreen) bool { return true })
							if f.e.received("alice", current) {
								t.Fatal("current mail receipted")
							}
							// Keeper o-JKabzo: exact-looking and changed frames both refuse recovery.
							if s.Text != text || s.Erased != 0 || len(s.Submitted) != 0 {
								t.Fatalf("ambiguous space suffix touched: %+v", s)
							}
						})
					}
				})
			}
		})
	}
}
