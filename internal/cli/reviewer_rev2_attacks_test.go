package cli

import (
	"context"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"os/exec"
	"testing"
	"time"
)

// Independently replay the keeper-reported active-turn receipt ordering.
// Later synthetic mail arrives before the active turn returns idle. No pane,
// installed binary or live Matrix is observed. These are production dispatch
// calls and own inbox receipts; fixture time advances only between calls.
func TestReviewRev2ActiveTurnReceipt(t *testing.T) {
	for _, neighbor := range []string{"later-mail", "no-new-then-later", "human-lookalike"} {
		t.Run(neighbor, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			old := f.mail()
			notice := tmuxwake.Notice(1)
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			if !f.binding().Typed {
				t.Fatal("setup: real typed journal absent")
			}
			f.e.as("alice", "inbox").ok(t)
			if !f.e.received("alice", old) {
				t.Fatal("setup: old mail unreceived")
			}
			var current string
			if neighbor != "no-new-then-later" {
				current = f.mail()
			}
			f.control(map[string]any{"busy": true})
			f.step()
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != notice || s.Erased != 0 || len(s.Submitted) != 0 {
				t.Fatalf("active turn touched: %+v", s)
			}
			f.control(map[string]any{})
			f.step() // detection only; manual fixture clearing below is external
			f.control(map[string]any{})
			s = f.wait(func(s orphanScreen) bool { return true })
			if s.Text != notice || s.Erased != 0 || len(s.Submitted) != 0 {
				t.Fatalf("idle orphan modified draft: %+v", s)
			}
			if neighbor == "no-new-then-later" {
				f.step()
				f.now = f.now.Add(3 * time.Second)
				current = f.mail()
			}
			if neighbor == "human-lookalike" {
				f.control(map[string]any{"text": notice})
				for i := 0; i < 3; i++ {
					f.step()
				}
				f.control(map[string]any{})
				s = f.wait(func(s orphanScreen) bool { return true })
				if s.Text != notice || s.Erased != 0 || len(s.Submitted) != 0 {
					t.Fatalf("later human adopted: %+v", s)
				}
				f.control(map[string]any{"text": ""})
			}
			f.now = f.now.Add(config.NotificationEvery) // later valid delivery still requires fresh idle qualification
			f.control(map[string]any{"text": ""})       // external manual recovery
			f.dispatch()
			before := f.binding()
			if len(before.Reservations) > 1 {
				t.Fatalf("first glimpse spent new reservation: %+v", before)
			}
			f.step()
			f.step()
			f.control(map[string]any{})
			s = f.wait(func(s orphanScreen) bool { return true })
			if s.Text != "" || len(s.Submitted) != 1 || s.Submitted[0] != notice || f.e.received("alice", current) {
				t.Fatalf("current wake lost/duplicated: %+v", s)
			}
		})
	}
}

// A real trailing ASCII space with the cursor moved back onto the preceding
// notice is not capture padding. Invalid drafts must have no erase/Enter.
func TestReviewRev2TrailingSpaceCursor(t *testing.T) {
	for _, path := range []string{"orphan", "live"} {
		t.Run(path, func(t *testing.T) {
			for _, variant := range []string{"valid", "space-at-end", "space-cursor-left", "two-spaces-cursor-left", "nonspace-cursor-left", "no-journal-space"} {
				t.Run(variant, func(t *testing.T) {
					f := newOrphanDispatchFixture(t)
					f.mail()
					notice := tmuxwake.Notice(1)
					f.control(map[string]any{"defer": true})
					f.dispatch()
					f.step()
					if !f.binding().Typed {
						t.Fatal("setup: no real typed journal")
					}
					if path == "orphan" {
						f.e.as("alice", "inbox").ok(t)
						f.mail()
					}
					text := notice
					cursor := 0
					switch variant {
					case "space-at-end":
						text += " "
					case "space-cursor-left", "no-journal-space":
						text += " "
						cursor = -1
					case "two-spaces-cursor-left":
						text += "  "
						cursor = -2
					case "nonspace-cursor-left":
						text += "X"
						cursor = -1
					}
					if variant == "no-journal-space" {
						f.update(func(st *config.State) {
							st.Terminal.Typed = false
							st.Terminal.Notice = ""
							st.Terminal.NoticeCount = 0
							st.Terminal.AttemptIDs = nil
						})
					}
					f.control(map[string]any{"text": text, "cursor": cursor, "hint": true})
					capture, err := exec.Command("tmux", "-S", f.binding().Target.Socket, "capture-pane", "-e", "-p", "-t", "%0").Output()
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("input=%q classification=%s capture=%q", text, f.binding().Target.PendingInput(context.Background(), notice), string(capture))
					captureN, err := exec.Command("tmux", "-S", f.binding().Target.Socket, "capture-pane", "-N", "-e", "-p", "-t", "%0").Output()
					if err != nil {
						t.Fatal(err)
					}
					t.Logf("preserve-trailing capture=%q", string(captureN))
					f.dispatch()
					f.control(map[string]any{})
					s := f.wait(func(s orphanScreen) bool { return true })
					if s.Text != text || s.Erased != 0 || len(s.Submitted) != 0 {
						t.Fatalf("trailing-space cursor draft touched: %+v", s)
					}
				})
			}
		})
	}
}

// The incident's filled Codex composer hides shortcuts. The same ambiguity
// must not acquire ownership through inputWithNotice's no-hint branch.
func TestReviewRev2NoHintSpaceCursor(t *testing.T) {
	for _, path := range []string{"orphan", "live"} {
		t.Run(path, func(t *testing.T) {
			for _, variant := range []string{"valid", "space-cursor-left"} {
				t.Run(variant, func(t *testing.T) {
					f := newOrphanDispatchFixture(t)
					f.mail()
					notice := tmuxwake.Notice(1)
					f.control(map[string]any{"defer": true})
					f.dispatch()
					f.step()
					if !f.binding().Typed {
						t.Fatal("setup: missing real typed journal")
					}
					if path == "orphan" {
						f.e.as("alice", "inbox").ok(t)
						f.mail()
					}
					text := notice
					cursor := 0
					if variant != "valid" {
						text += " "
						cursor = -1
					}
					f.control(map[string]any{"text": text, "cursor": cursor, "hint": false})
					f.dispatch()
					f.control(map[string]any{})
					s := f.wait(func(s orphanScreen) bool { return true })
					if s.Text != text || s.Erased != 0 || len(s.Submitted) != 0 {
						t.Fatalf("no-hint trailing-space draft touched: %+v", s)
					}
				})
			}
		})
	}
}
