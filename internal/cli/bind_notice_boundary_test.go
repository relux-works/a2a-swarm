package cli

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Synthetic equal-cell countermodel, using real PTY captures and production
// journal/dispatch. Oracle editor state is read only by assertions. Equal
// rendered cells/cursors cannot authorize Enter; every draft stays unchanged.
// The styled renderer is synthetic and does not establish a field trigger.
func TestTerminalNoticeRenderedSpaceBoundary(t *testing.T) {
	for _, layout := range []string{"codex", "claude-legacy", "claude-named"} {
		for _, variant := range []string{"exact", "space-left", "two-spaces-left", "nonspace-left", "space-end"} {
			t.Run(layout+"/"+variant, func(t *testing.T) {
				source := strings.ReplaceAll(orphanFixture, "prefix+text+suffix", `prefix+text.rstrip(' ')+'\x1b[2m'+text[len(text.rstrip(' ')):].ljust(100-len(text.rstrip(' ')))+'\x1b[0m'+suffix`)
				f := newOrphanDispatchFixtureScript(t, source)
				id := f.mail()
				initial := map[string]any{"defer": true}
				if layout != "codex" {
					dispatchRegistry(t, f, "idle")
					initial["claude"] = true
					initial["named"] = layout == "claude-named"
				}
				f.control(initial)
				f.dispatch()
				f.step()
				if !f.binding().Typed {
					t.Fatal("setup: no actual typed journal")
				}
				mode := map[string]any{"text": tmuxwake.Notice(1), "hint": true}
				if layout != "codex" {
					mode["claude"] = true
					mode["named"] = layout == "claude-named"
				}
				f.control(mode)
				target := f.binding().Target
				capture := func() string {
					out, err := exec.Command("tmux", "-S", target.Socket, "capture-pane", "-e", "-p", "-T", "-N", "-S", "18", "-E", "23", "-t", target.Pane).Output()
					if err != nil {
						t.Fatal(err)
					}
					return string(out)
				}
				exactCapture := capture()
				x0, y0, w0, err := target.Inspect(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(exactCapture, "\x1b[2m") {
					t.Fatal("setup: no dim ASCII padding captured")
				}
				exactObservation := boundTerminal(*f.binding()).ObserveReadiness(context.Background(), tmuxwake.Notice(1))
				if exactObservation.Input.State != tmuxwake.InputNonempty || exactObservation.Eligible() || exactObservation.Key != (tmuxwake.ReadinessKey{}) {
					t.Fatal("setup: exact nonempty diagnostic unexpectedly grants authority")
				}
				text := tmuxwake.Notice(1)
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
				mode["text"] = text
				mode["cursor"] = cursor
				f.control(mode)
				x, y, w, err := target.Inspect(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				observation := boundTerminal(*f.binding()).ObserveReadiness(context.Background(), tmuxwake.Notice(1))
				equal := capture() == exactCapture
				sameCursor := x == x0 && y == y0 && w == w0
				ambiguous := variant == "space-left" || variant == "two-spaces-left"
				if ambiguous && (!equal || !sameCursor || observation.Input != exactObservation.Input) {
					t.Fatal("countermodel lacks equal capture/cursor/assessment")
				}
				if variant == "nonspace-left" && equal {
					t.Fatal("visible suffix control failed")
				}
				if variant == "space-end" && sameCursor {
					t.Fatal("true-end cursor control failed")
				}
				t.Logf("editor=%q equal-capture=%v equal-cursor=%v rendered-input=%s", text, equal, sameCursor, observation.Input.State)
				f.dispatch()
				time.Sleep(50 * time.Millisecond)
				state := f.wait(func(s orphanScreen) bool { return true })
				if len(state.Submitted) != 0 || state.Erased != 0 {
					t.Fatalf("forbidden effect: editor=%q submitted=%q erased=%d", text, state.Submitted, state.Erased)
				}
				if state.Text != text {
					t.Fatal("draft changed")
				}
				if f.e.received("alice", id) || len(f.binding().Reservations) != 1 {
					t.Fatal("mail/rate changed")
				}
			})
		}
	}
}
