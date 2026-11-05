package tmuxwake

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (f *terminalFixture) render(options map[string]any) {
	f.t.Helper()
	options["render"] = 1
	data, err := json.Marshal(options)
	if err != nil {
		f.t.Fatal(err)
	}
	if err = os.WriteFile(f.path+".render.next", data, 0600); err != nil {
		f.t.Fatal(err)
	}
	if err = os.Rename(f.path+".render.next", f.path+".render"); err != nil {
		f.t.Fatal(err)
	}
	if _, err = f.target.command(context.Background(), "send-keys", "-t", f.target.Pane, "C-l"); err != nil {
		f.t.Fatal(err)
	}
	row := options["row"].(int)
	state := f.wait(func(s fixtureState) bool { return s.Render == 1 && s.Row == row })
	shown := state.Text
	if options["empty"] == true {
		shown = ""
	}
	column, _ := options["column"].(int)
	cursor, _ := options["cursor"].(int)
	extra, _ := options["extra"].(string)
	prompt, footer := "› "+shown, "  Codex · Ready · Context 50% left"
	if shown == "" {
		footer = "  ? for shortcuts"
	}
	if f.target.Runtime == "claude" {
		content := shown
		if content == "" {
			content = "Continue"
		}
		prompt, footer = "❯\u00a0"+content, "  ⏵⏵ bypass permissions on"
	}
	footerRow := row + 1
	prompt = strings.Repeat(" ", column) + prompt
	// The Python state file acknowledges the pty write, not tmux's redraw.
	// Wait on literal grid content and cursor, even for intentionally invalid
	// layouts; using the product parser here would hide negative fixtures.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		screen, err := f.target.command(context.Background(), "capture-pane", "-p", "-t", f.target.Pane)
		x, y, _, inspectErr := f.target.Inspect(context.Background())
		lines := strings.Split(strings.TrimSuffix(screen, "\n"), "\n")
		if err == nil && inspectErr == nil && footerRow < len(lines) &&
			strings.TrimRight(lines[row-1], " ") == strings.TrimRight(prompt, " ") &&
			strings.TrimRight(lines[footerRow], " ") == footer+extra &&
			x == 2+column+len(shown)+cursor && y == row-1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("synthetic redraw did not reach tmux grid")
}

// Moving only the owned composer's row submits once upward and downward on all four routes.
// Every neighboring refusal preserves input and sends no Enter, including row
// movement before typing. These synthetic fixtures do not prove a live UI cause.
func TestBoundSendComposerMovement(t *testing.T) {
	for _, runtime := range []string{"claude", "claude-named", "claude-fallback", "codex"} {
		for _, mode := range []string{"valid", "valid-down", "pre-row-down", "empty", "foreign", "dialog", "rendered-busy", "registry-busy", "status", "timestamp", "width", "column", "cursor", "pre-row"} {
			if (runtime == "codex" || runtime == "claude-fallback") && (mode == "registry-busy" || mode == "status" || mode == "timestamp") {
				continue
			}
			t.Run(runtime+"/"+mode, func(t *testing.T) {
				fixtureRuntime := runtime
				if runtime == "claude-fallback" {
					fixtureRuntime = "claude"
				}
				f := newBoundFixture(t, fixtureRuntime, "2.1.318", "idle")
				if runtime == "claude-fallback" {
					f.b.ClaudeRegistryRoot = ""
				}
				sample := f.b.AssessReadiness(context.Background())
				routes := map[string]ReadinessRoute{"claude": RegistryLegacy, "claude-named": RegistryNamed, "claude-fallback": LegacyFallback, "codex": LegacyCodex}
				if !sample.Eligible() || sample.Key.Route != routes[runtime] {
					t.Fatal("positive setup refused", sample)
				}
				wantText := Notice(1)
				move := func() {
					options := map[string]any{"row": 17}
					if mode == "valid-down" || mode == "pre-row-down" {
						options["row"] = 21
					}
					switch mode {
					case "empty":
						options["empty"] = true
					case "dialog":
						options["extra"] = " Permission required"
					case "rendered-busy":
						options["extra"] = " (esc to interrupt)"
					case "column":
						options["column"] = 1
					case "cursor":
						options["cursor"] = -1
					case "registry-busy":
						f.m["status"] = "busy"
						f.write(t)
					case "status":
						f.m["status"] = "shell"
						f.write(t)
					case "timestamp":
						f.m["statusUpdatedAt"] = int64(1793016002001)
						f.write(t)
					case "width":
						if _, err := f.f.target.command(context.Background(), "resize-window", "-t", f.f.target.Pane, "-x", "121", "-y", "24"); err != nil {
							t.Fatal(err)
						}
					case "foreign":
						f.f.typeText("PRIVATE_SUFFIX")
						wantText += "PRIVATE_SUFFIX"
					}
					f.f.render(options)
				}
				var before, after func()
				if mode == "pre-row" || mode == "pre-row-down" {
					before = move
					wantText = ""
				} else {
					after = func() { f.f.wait(func(s fixtureState) bool { return s.Text == Notice(1) }); move() }
				}
				got, err := f.b.Send(context.Background(), 1, sample.Key, before, after)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "valid" || mode == "valid-down" {
					if got.Disposition != SendSubmitted {
						t.Fatal("row movement refused", got)
					}
					state := f.f.wait(func(s fixtureState) bool { return len(s.Submitted) > 0 })
					if len(state.Submitted) != 1 || state.Submitted[0] != Notice(1) || state.Text != "" {
						t.Fatal("wrong submission")
					}
					return
				}
				want := SendDraft
				if mode == "pre-row" || mode == "pre-row-down" {
					want = SendRefused
				}
				state := f.f.wait(func(s fixtureState) bool { return s.Render == 1 })
				if got.Disposition != want || state.Text != wantText || len(state.Submitted) != 0 {
					t.Fatal("refusal input effects", got)
				}
				reasons := map[string]SendReason{"empty": ReasonAwaitingRender, "dialog": ReasonBusyOrDialog, "rendered-busy": ReasonBusyOrDialog, "registry-busy": ReasonBusyOrDialog, "status": ReasonKeyStatusChanged, "timestamp": ReasonKeyStatusChanged, "width": ReasonKeyWidthChanged, "column": ReasonCursorMismatch, "cursor": ReasonCursorMismatch, "pre-row": ReasonKeyRowChanged, "pre-row-down": ReasonKeyRowChanged, "foreign": ReasonForeignInput}
				expected := reasons[mode]
				// Codex rejects both no-hint foreign text and an indented prompt
				// during layout recognition, before it can compare the cursor.
				if runtime == "codex" && (mode == "foreign" || mode == "column") {
					expected = ReasonLayoutUnrecognized
				}
				if got.Reason != expected || got.ReasonAt.IsZero() || SafeSendReason(got.Reason) != got.Reason {
					t.Fatal("wrong bounded refusal", got)
				}
			})
		}
	}
}

// The named row exception compares every other field, including future fields
// via struct equality. Pre-type equality remains separate in Send.
func TestOwnedTypingKeyComparison(t *testing.T) {
	base := ReadinessKey{ComposerRow: 20}
	moved := base
	moved.ComposerRow = 17
	if !sameKeyAfterOwnedTyping(moved, base) {
		t.Fatal("row refused")
	}
	for _, change := range []func(*ReadinessKey){
		func(k *ReadinessKey) { k.PaneWidth++ }, func(k *ReadinessKey) { k.ComposerColumn++ },
		func(k *ReadinessKey) { k.Route = LegacyCodex }, func(k *ReadinessKey) { k.Target.Pane = "other" },
		func(k *ReadinessKey) { k.Generation = "other" }, func(k *ReadinessKey) { k.SessionID = "other" },
		func(k *ReadinessKey) { k.ClaudeRegistryRoot = "other" }, func(k *ReadinessKey) { k.RegistrySessionID = "other" },
		func(k *ReadinessKey) { k.RegistryVersion = "other" }, func(k *ReadinessKey) { k.RegistryStatus = "other" }, func(k *ReadinessKey) { k.RegistryStatusAt++ },
	} {
		bad := moved
		change(&bad)
		if sameKeyAfterOwnedTyping(bad, base) {
			t.Fatal("non-row field ignored")
		}
	}
}

// At the notice width boundary Send records a specific suppression token and
// types/submits nothing; one extra column admits exactly one complete notice.
func TestBoundSendWidthSuppressionReason(t *testing.T) {
	for _, fits := range []bool{false, true} {
		t.Run(map[bool]string{false: "suppressed", true: "fits"}[fits], func(t *testing.T) {
			f := newBoundFixture(t, "codex", "", "")
			width := len(Notice(1)) + 3
			if fits {
				width++
			}
			if _, err := f.f.target.command(context.Background(), "resize-window", "-t", f.f.target.Pane, "-x", strconv.Itoa(width), "-y", "24"); err != nil {
				t.Fatal(err)
			}
			f.f.render(map[string]any{"row": 20})
			sample := f.b.AssessReadiness(context.Background())
			if !sample.Eligible() || sample.Key.PaneWidth != width {
				t.Fatal("empty narrow composer refused", sample)
			}
			result, err := f.b.Send(context.Background(), 1, sample.Key, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if fits {
				state := f.f.wait(func(s fixtureState) bool { return len(s.Submitted) > 0 })
				if result.Disposition != SendSubmitted || len(state.Submitted) != 1 || state.Submitted[0] != Notice(1) || state.Text != "" {
					t.Fatal("fitting notice not submitted once", result)
				}
			} else {
				if result.Disposition != SendRefused || result.Outcome != SendSuppressedWidth || result.Reason != ReasonTargetOrWidth || result.ReasonAt.IsZero() || SafeSendReason(result.Reason) != result.Reason {
					t.Fatal("wrong width suppression", result)
				}
				f.empty(t)
			}
		})
	}
}
