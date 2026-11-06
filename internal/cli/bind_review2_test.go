package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func review2Layout(runtime string) map[string]any {
	return map[string]any{"claude": runtime != "codex", "named": runtime == "claude-named", "nbsp": true, "rule-width": 80}
}

// An actual 80x24 private grid with a transcript prompt in the bottom 18 rows
// still admits an empty composer and sends one normal wake. Claude uses NBSP,
// two full-width rules and a footer starting with two spaces and ⏵.
func TestTerminalTranscriptPromptAdmission(t *testing.T) {
	for _, runtime := range []string{"claude", "claude-named", "codex"} {
		t.Run(runtime+"/fresh", func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			if runtime != "codex" {
				dispatchRegistry(t, f, "idle")
			}
			if out, err := exec.Command("tmux", "-S", f.binding().Target.Socket, "resize-window", "-t", "%0", "-x", "80", "-y", "24").CombinedOutput(); err != nil {
				t.Fatalf("resize: %v %s", err, out)
			}
			mode := review2Layout(runtime)
			// Named-session labels take space in the top rule. The observed legacy
			// Claude control retains two full-width 80-column rules.
			if runtime == "claude-named" {
				mode["rule-width"] = 60
			}
			if _, _, width, err := f.binding().Target.Inspect(context.Background()); err != nil || width != 80 {
				t.Fatalf("fixture width=%d error=%v; want 80", width, err)
			}
			glyph := "❯ "
			if runtime == "codex" {
				glyph = "› "
			}
			mode["diagnostic-row"], mode["diagnostic-text"] = 12, glyph+tmuxwake.Notice(1)
			f.control(mode)
			id := f.mail()
			f.dispatch()
			f.step()
			s := f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
			if s.Submitted[0] != tmuxwake.Notice(1) || s.Erased != 0 || s.Typed != len(tmuxwake.Notice(1)) || f.e.received("alice", id) {
				t.Fatal("transcript deafened or changed wake")
			}
		})
	}
}

// Filled geometry is recorded by the real typing invocation, survives disk
// reload, and differs from empty origin in row/column. A further move away
// from the record refuses. Without a record, Claude rules anchor recovery;
// filled Codex has no hint, so it holds and escalates instead.
func TestTerminalRecoveryFilledPosition(t *testing.T) {
	for _, runtime := range []string{"claude", "claude-named", "codex"} {
		for _, untyped := range []bool{false, true} {
			for _, axis := range []string{"both"} {
				for _, recovery := range []string{"recorded", "crash", "moved-again", "no-anchor"} {
					if untyped && (runtime != "codex" || recovery != "recorded") || (recovery == "no-anchor" || recovery == "moved-again") && runtime != "codex" {
						continue
					}
					t.Run(fmt.Sprintf("%s/%t/%s/%s", runtime, untyped, axis, recovery), func(t *testing.T) {
						f := newOrphanDispatchFixture(t)
						if runtime != "codex" {
							dispatchRegistry(t, f, "idle")
						}
						f.mail()
						row, indent := 20, 0
						if axis != "column" {
							row = 17
						}
						if axis != "row" {
							indent = 3
						}
						mode := review2Layout(runtime)
						mode["filled-row"], mode["filled-indent"], mode["defer"] = row, indent, true
						if untyped {
							loseTerminalTypeACK(t, f)
						}
						f.control(mode)
						f.dispatch()
						f.step()
						b := f.binding()
						if b.NoticePosition == nil || b.NoticePosition.Row != row-1 || b.NoticePosition.Column != indent+2 || b.Typed == untyped {
							t.Fatal("typing invocation did not journal filled position", b.NoticePosition, b.Typed)
						}
						if b.NoticeOrigin.ComposerRow != 19 || b.NoticeOrigin.ComposerColumn != 2 {
							t.Fatal("fixture failed to preserve empty origin")
						}
						before := f.wait(func(s orphanScreen) bool { return s.Text == tmuxwake.Notice(1) })
						if recovery == "crash" || recovery == "no-anchor" {
							f.update(func(st *config.State) { st.Terminal.NoticePosition = nil })
						}
						if recovery == "moved-again" {
							row--
							indent++
						}
						mode = review2Layout(runtime)
						mode["row"], mode["indent"], mode["cursor"] = row, indent, -5

						glyph := "❯ "
						if runtime == "codex" {
							glyph = "› "
						}
						mode["diagnostic-row"], mode["diagnostic-text"] = 10, glyph+tmuxwake.Notice(1)
						if recovery == "no-anchor" {
							mode["no-composer"] = true
						}
						f.control(mode)
						f.now = f.now.Add(config.NotificationEvery)
						clone := *f.a
						clone.notification = nil
						f.a = &clone
						var err error
						f.s, err = f.a.session()
						if err != nil {
							t.Fatal(err)
						}
						f.dispatch()
						positive := recovery == "recorded" || recovery == "crash" && runtime != "codex"
						if positive {
							f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
						}
						f.control(renderWithoutText(mode))
						after := f.wait(func(s orphanScreen) bool { return true })
						if after.Typed != before.Typed || after.Erased != 0 {
							t.Fatal("recovery typed or erased")
						}
						if positive {
							if len(after.Submitted) != 1 || after.Submitted[0] != b.Notice || f.binding().Recovery == nil {
								t.Fatal("moved filled notice did not recover")
							}
						} else {
							if len(after.Submitted) != 0 || f.binding().Retried {
								t.Fatal("geometry/hint refusal spent key or retry")
							}
							f.e.ownReportRoom("alice")
							f.now = b.AttemptAt.Add(10 * time.Minute)
							f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Hour) })
							f.dispatch()
							requireNotice(t, f.binding().OwnNotice, "suspected")
							if f.binding().NoticeReport == nil || !f.binding().NoticeReport.Sent {
								t.Fatal("held filled notice did not escalate")
							}
							f.control(renderWithoutText(mode))
							held := f.wait(func(s orphanScreen) bool { return true })
							if held.Text != b.Notice || held.Typed != before.Typed || held.Erased != 0 || len(held.Submitted) != 0 {
								t.Fatal("escalation changed held editor")
							}
						}
					})
				}
			}
		}
	}
}

// Every retained fixed notice alerts at the original attempt's ten-minute mark,
// without first spending Enter. Untyped, legitimately pruned partial IDs,
// geometry holds, no-hint, busy/unknown, pause and missing recovery authority
// all preserve the input/mail. These are conservative suspicion assertions.
func TestTerminalHeldNoticeReportsWithoutFence(t *testing.T) {
	for _, mode := range []string{"position", "partial", "untyped", "no-hint", "busy", "unknown", "paused", "unrecorded"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			room := f.e.ownReportRoom("alice")
			first := f.mail()
			second := f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			if mode == "partial" {
				_, err := f.s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
					_, err := mb.Receive([]string{first})
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			f.update(func(st *config.State) {
				b := st.Terminal
				b.Typed = false
				// A distinct later reservation exhausts the hourly budget;
				// f.now itself equals AttemptAt and would be excluded as our own slot.
				// The untyped/partial controls use that exhausted budget;
				// other cases independently exercise their named refusal.
				if mode == "untyped" || mode == "partial" {
					b.MaxWakes = 1
					b.Reservations = append(b.Reservations, f.now.Add(time.Second))
				}
				if mode == "position" {
					b.NoticePosition.Row--
				}
				if mode == "no-hint" {
					b.NoticePosition = nil
				}
				if mode == "paused" {
					st.HooksPaused = true
				}
				if mode == "unrecorded" {
					b.TypingStarted = false
					b.NoticeOrigin = nil
					b.NoticePosition = nil
				}
			})
			frame := map[string]any{"cursor": -5}
			if mode == "busy" || mode == "unknown" {
				frame[mode] = true
			}
			f.control(frame)
			before := f.wait(func(s orphanScreen) bool { return true })
			attempt := f.binding().AttemptAt
			f.now = attempt.Add(10*time.Minute - time.Nanosecond)
			f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Hour) })
			f.dispatch()
			if len(f.e.projectionsFrom(room, "@alice:test.local")) != 0 {
				t.Fatal("incident before threshold")
			}
			held := f.binding()
			if held.Notice != before.Text || held.NoticeCount != 2 || held.Typed || held.Retried || !held.AttemptAt.Equal(attempt) || len(held.AttemptIDs) != map[bool]int{true: 1, false: 2}[mode == "partial"] {
				t.Fatal("pre-threshold dispatch settled or changed retained journal")
			}
			f.now = attempt.Add(10 * time.Minute)
			f.dispatch()
			f.dispatch()
			b := f.binding()
			requireNotice(t, b.OwnNotice, "suspected")
			if b.Retried || b.NoticeReport == nil || !b.NoticeReport.Sent || len(f.e.projectionsFrom(room, "@alice:test.local")) != 1 || f.e.received("alice", second) {
				t.Fatal("held intent failed to alert once without fence")
			}
			f.control(frame)
			after := f.wait(func(s orphanScreen) bool { return true })
			if after.Text != before.Text || after.Typed != before.Typed || after.Erased != 0 || len(after.Submitted) != 0 {
				t.Fatal("report changed editor")
			}
		})
	}
}

// Fail only after initial recovery/width checks, throughout the poll loop.
// A successful next cycle proves the retry was retained; the counter establishes
// that the intended poll branch ran. Registry refusal is tested on Claude too.
func TestTerminalRecoveryPollFlicker(t *testing.T) {
	for _, mode := range []string{"capture", "busy", "registry"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			runtime := "codex"
			var registryPath string
			var registry map[string]any
			if mode == "registry" {
				runtime = "claude-named"
				registryPath, registry = dispatchRegistry(t, f, "idle")
			}
			layout := review2Layout(runtime)
			layout["defer"] = true
			f.mail()
			f.control(layout)
			f.dispatch()
			f.step()
			f.now = f.now.Add(config.NotificationEvery)
			f.control(review2Layout(runtime))
			real, err := exec.LookPath("tmux")
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			flag := filepath.Join(dir, "flicker")
			counter := filepath.Join(dir, "captures")
			if err := os.WriteFile(flag, []byte("active"), 0600); err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys,subprocess,json
if 'capture-pane' in sys.argv:
 try:
  with open(%q) as f: n=int(f.read())
 except FileNotFoundError: n=0
 n+=1
 with open(%q,'w') as f:f.write(str(n))
 if n>=4 and os.path.exists(%q):
  if %q=='capture':sys.exit(1)
  if %q=='busy':
   out=subprocess.check_output([%q]+sys.argv[1:]);sys.stdout.buffer.write(out+b'esc to interrupt\n');sys.exit(0)
  with open(%q) as f:r=json.load(f)
  r['status']='busy'
  with open(%q,'w') as f:json.dump(r,f)
os.execv(%q,[%q]+sys.argv[1:])
`, counter, counter, flag, mode, mode, real, registryPath, registryPath, real, real)
			if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			f.dispatch()
			raw, err := os.ReadFile(counter)
			if err != nil {
				t.Fatal(err)
			}
			n, err := strconv.Atoi(string(raw))
			if err != nil || n < 4 {
				t.Fatal("poll plant not reached")
			}
			b := f.binding()
			want := tmuxwake.ReasonUnavailable
			if mode != "capture" {
				want = tmuxwake.ReasonBusyOrDialog
			}
			if b.Retried || b.SendRefusal == nil || b.SendRefusal.Reason != want || b.Outcome != string(tmuxwake.SendDraftUnavailable) {
				t.Fatal("poll refusal spent fence or lost reason", b.SendRefusal, b.Outcome)
			}
			if err := os.Remove(flag); err != nil {
				t.Fatal(err)
			}
			if mode == "registry" {
				writeDispatchRegistry(t, registryPath, registry)
			}
			f.dispatch()
			f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
			if !f.binding().Retried {
				t.Fatal("stable neighbor did not spend Enter fence")
			}
		})
	}
}

// Real filled geometry survives journal reload and clears with the origin
// after qualified empty evidence; the private constraint stays out of status.
func TestTerminalFilledPositionJournal(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.control(map[string]any{"defer": true, "filled-row": 17, "filled-indent": 3})
	f.dispatch()
	f.step()
	b := f.binding()
	if !reflect.DeepEqual(b.NoticePosition, &tmuxwake.ComposerPosition{Row: 16, Column: 5}) {
		t.Fatal("missing persisted filled geometry")
	}
	f.now = f.now.Add(config.NotificationEvery)
	f.control(map[string]any{"text": ""})
	f.dispatch()
	if b := f.binding(); b.NoticeOrigin != nil || b.NoticePosition != nil || b.TypingStarted {
		t.Fatal("empty observation retained geometry")
	}
	if strings.Contains(f.e.as("alice", "bind", "status", "--json").ok(t).stdout, "notice_position") {
		t.Fatal("private constraint entered status")
	}
}

// Real state-lock contention is introduced only at the Enter fence. Timeout,
// pause and lease refusal send no key, preserve retry/mail, and persist the fence
// reason rather than stale unavailable. Unlock/unpause restores one valid Enter.
func TestTerminalRecoveryFenceRefusals(t *testing.T) {
	for _, mode := range []string{"lock", "paused", "lease"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.now = f.now.Add(config.NotificationEvery)
			f.control(map[string]any{})
			b := *f.binding()
			bound := boundTerminal(b)
			sample := bound.AssessOwnedNotice(context.Background(), b.Notice)
			if !sample.OwnedNoticeEligible() {
				t.Fatal("positive recovery setup refused")
			}
			var lockDone chan error
			var release chan struct{}
			var fenceDuration time.Duration
			fences := 0
			result, sendErr := bound.ResumeNotice(context.Background(), b.NoticeCount, b.Notice, sample.Key, func() error {
				fences++
				if mode == "lock" {
					acquired := make(chan struct{})
					release = make(chan struct{})
					lockDone = make(chan error, 1)
					go func() {
						_, err := config.UpdateState(f.s.paths, f.s.name, func(*config.State) error {
							close(acquired)
							select {
							case <-release:
							case <-time.After(2 * time.Second):
							}
							return nil
						})
						lockDone <- err
					}()
					<-acquired
				} else {
					f.update(func(st *config.State) {
						if mode == "paused" {
							st.HooksPaused = true
						} else {
							st.Lease.Expires = f.now
						}
					})
				}
				start := time.Now()
				_, err := f.a.reserveTerminalRecovery(context.Background(), f.s, "orphan-gen", b, sample.Key)
				fenceDuration = time.Since(start)
				return err
			})
			if release != nil {
				close(release)
				if err := <-lockDone; err != nil {
					t.Fatal(err)
				}
			}
			want := tmuxwake.ReasonRecoveryHeld
			if mode == "lock" {
				want = tmuxwake.ReasonFenceTimeout
			}
			if fences != 1 || sendErr == nil || result.Disposition != tmuxwake.SendRefused || result.Reason != want || f.binding().Retried {
				t.Fatal("fence refusal lost reason or spent retry", result)
			}
			if mode == "lock" && fenceDuration > 500*time.Millisecond {
				t.Fatal("fence lock was not bounded", fenceDuration)
			}
			if err := f.a.finishTerminal(f.s, "orphan-gen", result, sendErr, true, b); err != nil {
				t.Fatal(err)
			}
			if f.binding().SendRefusal == nil || f.binding().SendRefusal.Reason != want || f.e.received("alice", id) {
				t.Fatal("fence reason not journaled or mail lost")
			}
			f.control(map[string]any{})
			if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 0 || s.Text != b.Notice || s.Erased != 0 {
				t.Fatal("fence refusal changed input")
			}
			f.update(func(st *config.State) { st.HooksPaused = false; st.Lease.Expires = f.now.Add(time.Hour) })
			f.dispatch()
			f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
		})
	}
}
