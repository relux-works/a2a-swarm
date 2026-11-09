package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Intercept only this test's private tmux commands. No Enter is delivered on a
// synthetic transport failure; recovery must nevertheless fence that attempt.
func interceptRecovery(t *testing.T, f *orphanDispatchFixture, capture int, enter bool, flicker ...string) {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mode := "capture"
	if len(flicker) > 0 {
		mode = flicker[0]
	}
	script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys,subprocess
mode=%q
counter=%q
if (mode=='width' and 'display-message' in sys.argv and '#{pane_width}' in ' '.join(sys.argv)) or (mode!='width' and 'capture-pane' in sys.argv):
 try:
  with open(counter) as f: n=int(f.read())
 except FileNotFoundError: n=0
 n+=1
 with open(counter,'w') as f: f.write(str(n))
 if n==%d:
  if mode=='busy':
   out=subprocess.check_output([%q]+sys.argv[1:]); sys.stdout.buffer.write(out+b'esc to interrupt\n'); sys.exit(0)
  sys.exit(1)
if %s and 'Enter' in sys.argv: sys.exit(1)
os.execv(%q,[%q]+sys.argv[1:])
`, mode, filepath.Join(dir, "captures"), capture, real, map[bool]string{true: "True", false: "False"}[enter], real, real)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func exhaustRecovery(t *testing.T, f *orphanDispatchFixture) {
	t.Helper()
	interceptRecovery(t, f, 0, true)
	f.now = f.now.Add(config.NotificationEvery)
	f.control(map[string]any{"cursor": -5})
	f.dispatch()
	if b := f.binding(); !b.Retried || b.Outcome != string(tmuxwake.SendEnterUncertain) {
		t.Fatal("Enter failure did not spend fence", b.Outcome)
	}
	f.dispatch() // The next healthy observation must diagnose the exhausted attempt.
}

// A single capture failure after recovery qualification spends no retry. Once
// the frame stabilizes, the same journal gets exactly one Enter, without typing.
// The private fixture must acknowledge the exact submitted notice within its
// step-derived wait budget; this does not prove behavior in a live provider pane.
func TestTerminalRecoveryPreKeyFlicker(t *testing.T) {
	for _, mode := range []string{"capture", "busy", "width"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.now = f.now.Add(config.NotificationEvery)
			f.control(map[string]any{"cursor": -5})
			nth := 3
			if mode == "width" {
				nth = 7
			}
			interceptRecovery(t, f, nth, false, mode)
			f.dispatch()
			b := f.binding()
			want := tmuxwake.ReasonUnavailable
			if mode == "busy" {
				want = tmuxwake.ReasonBusyOrDialog
			}
			if mode == "width" {
				want = tmuxwake.ReasonTargetOrWidth
			}
			if b.Retried || b.SendRefusal == nil || b.SendRefusal.Reason != want {
				t.Fatal("pre-key refusal spent retry or lost reason")
			}
			f.dispatch()
			s := f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
			if s.Submitted[0] != tmuxwake.Notice(1) || !f.binding().Retried {
				t.Fatal("stable neighbor did not submit once")
			}
		})
	}
}

// Aged empty input revokes even an uncertain partial origin. A lagging empty
// frame inside the window preserves it and permits the later exact notice once.
func TestTerminalRecoveryEmptyContinuity(t *testing.T) {
	for _, aged := range []bool{false, true} {
		t.Run(fmt.Sprint(aged), func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			notice := f.binding().Notice
			f.update(func(st *config.State) { st.Terminal.Typed = false; st.Terminal.AttemptIDs = st.Terminal.AttemptIDs[:1] })
			delay := time.Second
			if aged {
				delay = config.NotificationEvery
			}
			f.now = f.now.Add(delay)
			f.control(map[string]any{"text": ""})
			f.dispatch()
			if b := f.binding(); (b.NoticeOrigin == nil) != aged || b.TypingStarted == aged {
				t.Fatal("wrong empty continuity")
			}
			f.now = f.now.Add(config.NotificationEvery)
			f.control(map[string]any{"text": notice, "cursor": -5})
			f.dispatch()
			if !aged {
				f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
			}
			f.control(map[string]any{"cursor": -5})
			s := f.wait(func(s orphanScreen) bool { return true })
			want := 0
			if !aged {
				want = 1
			}
			if len(s.Submitted) != want || s.Erased != 0 {
				t.Fatal("stale origin adopted later text or lagging control refused")
			}
		})
	}
}

// A recovery from the previous hour reserves this hour at the key fence. The
// final available slot is consumed; the next fresh mail cannot exceed the cap.
func TestTerminalRecoveryAgedBudget(t *testing.T) {
	f := newOrphanDispatchFixture(t)
	f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.now = f.now.Add(time.Hour + time.Minute)
	f.update(func(st *config.State) {
		st.Lease.Expires = f.now.Add(time.Hour)
		st.Terminal.MaxWakes = 2
		st.Terminal.Reservations = append(st.Terminal.Reservations, f.now.Add(-time.Minute))
	})
	f.control(map[string]any{})
	f.dispatch()
	f.wait(func(s orphanScreen) bool { return len(s.Submitted) == 1 })
	if noticeRecoveryBudget(*f.binding(), f.now) {
		t.Fatal("aged recovery left an extra slot")
	}
	f.mail()
	f.now = f.now.Add(config.NotificationEvery)
	f.dispatch()
	f.step()
	if !strings.Contains(f.binding().Outcome, "hourly rate") {
		t.Fatal("fresh wake exceeded cap")
	}
	f.control(map[string]any{})
	if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 1 {
		t.Fatal("extra Enter")
	}
}

// A real failed post-fence key with cursor off the composer remains visible in
// status and reaches the existing incident route, without retrying the key,
// including uncertain attempts with legitimately receipted partial IDs.
func TestTerminalRecoveryExhaustionReports(t *testing.T) {
	for _, variant := range []string{"typed", "uncertain", "partial"} {
		t.Run(variant, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			room := f.e.ownReportRoom("alice")
			first := f.mail()
			if variant == "partial" {
				f.mail()
			}
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			if variant != "typed" {
				f.update(func(st *config.State) { st.Terminal.Typed = false })
			}
			if variant == "partial" {
				_, err := f.s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
					_, err := mb.Receive([]string{first})
					return nil, err
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			exhaustRecovery(t, f)
			if variant == "partial" && (len(f.binding().AttemptIDs) != 1 || f.binding().NoticeCount != 2 || !f.e.received("alice", first)) {
				t.Fatal("partial-ID exhaustion control was not applied")
			}
			requireNotice(t, f.binding().OwnNotice, "suspected")
			if !strings.Contains(f.e.as("alice", "bind", "status").ok(t).stdout, "SUSPECTED-DEAF") {
				t.Fatal("spent fence invisible")
			}
			f.now = f.now.Add(10 * time.Minute)
			f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Minute) })
			f.dispatch()
			if len(f.e.projectionsFrom(room, "@alice:test.local")) != 1 || !f.binding().NoticeReport.Sent {
				t.Fatal("no post-fence incident")
			}
			f.control(map[string]any{"cursor": -5})
			if s := f.wait(func(s orphanScreen) bool { return true }); len(s.Submitted) != 0 || s.Erased != 0 {
				t.Fatal("report changed input")
			}
		})
	}
}
