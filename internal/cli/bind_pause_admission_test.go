package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Compare delivery evidence separately from observations and fixed outcomes.
func pauseAttemptEvidence(b config.TerminalBinding) config.TerminalBinding {
	return config.TerminalBinding{AttemptIDs: b.AttemptIDs, Notice: b.Notice, NoticeCount: b.NoticeCount, AttemptAt: b.AttemptAt, Typed: b.Typed, Retried: b.Retried, Rung: b.Rung, Reservations: b.Reservations, NotificationToken: b.NotificationToken}
}

func pauseMailboxBytes(t *testing.T, f *orphanDispatchFixture) []byte {
	t.Helper()
	data, err := os.ReadFile(f.e.paths().MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// The clock seam targets the last qualified sample before the reservation.
// This deliberately writes fixture state without the control lock to exercise
// the reservation guard independently. It is not a public pause interleaving:
// public pause shares dispatch's control lock and also invalidates idle state.
func TestTerminalPauseAdmission(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		for _, boundary := range []string{"dispatch", "reservation"} {
			for _, gate := range []string{"unpaused", "paused", "manual"} {
				t.Run(fmt.Sprintf("recovery=%t/%s/%s", recovery, boundary, gate), func(t *testing.T) {
					f := newOrphanDispatchFixture(t)
					f.mail()
					if recovery {
						failFirstTerminalType(t, f)
						f.now = f.now.Add(config.NotificationEvery)
					}
					f.dispatch() // first qualified empty sample
					before := pauseAttemptEvidence(*f.binding())
					mailbox := pauseMailboxBytes(t, f)
					apply := func() {
						f.update(func(st *config.State) {
							if gate == "paused" {
								st.HooksPaused = true
							}
							if gate == "manual" {
								st.Liveness = &config.AgentStatus{State: "waiting-human"}
								st.LivenessSource = "manual"
							}
						})
					}
					injected := false
					if boundary == "dispatch" {
						if gate == "paused" {
							f.e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
						} else {
							apply()
						}
						injected = true
					} else {
						f.a.Now = func() time.Time {
							pc, _, _, _ := runtime.Caller(1)
							if fn := runtime.FuncForPC(pc); !injected && fn != nil && strings.HasSuffix(fn.Name(), ".qualifyTerminalIdle") {
								injected = true
								apply()
							}
							return f.now
						}
					}
					f.step()
					if boundary == "dispatch" {
						f.step()
					}
					if !injected {
						t.Fatal("reservation boundary was not exercised")
					}
					f.a.Now = func() time.Time { return f.now }
					f.control(map[string]any{}) // synchronize renderer after actual effects
					screen := f.wait(func(orphanScreen) bool { return true })
					if gate == "unpaused" {
						if len(screen.Submitted) != 1 || screen.Submitted[0] != tmuxwake.Notice(1) || screen.Text != "" || screen.Erased != 0 {
							t.Fatalf("eligible neighbor did not deliver exactly once: %+v", screen)
						}
					} else {
						if screen.Text != "" || screen.Erased != 0 || len(screen.Submitted) != 0 || !reflect.DeepEqual(before, pauseAttemptEvidence(*f.binding())) {
							t.Fatalf("pause/manual admission produced terminal effects or changed attempt evidence: %+v %+v", screen, f.binding())
						}
						if f.binding().Outcome != "suppressed: manual gate" {
							t.Fatalf("wrong refusal boundary: %q", f.binding().Outcome)
						}
						// Public resume does not clear the independent manual hold.
						f.e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
						if gate == "manual" {
							f.step()
							if f.binding().Outcome != "suppressed: manual gate" {
								t.Fatal("resume cleared manual hold")
							}
							f.update(func(st *config.State) { st.Liveness = nil; st.LivenessSource = "" })
						}
						f.dispatch()
						f.step()
						f.control(map[string]any{})
						screen = f.wait(func(orphanScreen) bool { return true })
						if len(screen.Submitted) != 1 || screen.Submitted[0] != tmuxwake.Notice(1) || screen.Text != "" || screen.Erased != 0 {
							t.Fatalf("resumed eligible empty input did not deliver exactly once: %+v", screen)
						}
					}
					if !bytes.Equal(mailbox, pauseMailboxBytes(t, f)) {
						t.Fatal("admission/resume changed mailbox bytes")
					}
				})
			}
		}
	}
}

// Probe the actual control lock after reservation, before literal typing and
// before Enter. A public pause cannot acknowledge inside this admitted Send.
// Once pause has acknowledged, subsequent dispatch must admit no new effect.
func TestTerminalPauseSendBoundary(t *testing.T) {
	for _, recovery := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%t", recovery), func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.mail()
			if recovery {
				failFirstTerminalType(t, f)
				f.now = f.now.Add(config.NotificationEvery)
			}
			f.dispatch()
			real, err := exec.LookPath("tmux")
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(f.dir, "send-boundary")
			if err = os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(dir, "boundaries")
			lock := filepath.Join(f.e.paths().Base, "state", "alice.runtime-control.lock")
			script := fmt.Sprintf(`#!/usr/bin/env python3
import os,sys,fcntl
if "send-keys" in sys.argv:
    boundary="type" if "-l" in sys.argv else "enter" if "Enter" in sys.argv else "other"
    with open(%q,"r+") as lock:
        try:
            fcntl.flock(lock,fcntl.LOCK_EX|fcntl.LOCK_NB)
            held=False
        except BlockingIOError:
            held=True
    with open(%q,"a") as evidence: evidence.write(boundary+(":held\n" if held else ":UNLOCKED\n"))
os.execv(%q,[%q]+sys.argv[1:])
`, lock, marker, real, real)
			if err = os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			pauseStarted := make(chan struct{}, 1)
			pauseDone := make(chan error, 1)
			pauseApp := *f.a
			getenv := pauseApp.Getenv
			pauseApp.Getenv = func(k string) string {
				value := getenv(k)
				if k == config.EnvConfigDir {
					select {
					case pauseStarted <- struct{}{}:
					default:
					}
				}
				return value
			}
			reserved := false
			f.a.afterNotificationReserve = func() error {
				reserved = true
				go func() { pauseDone <- cmdHookPause(&pauseApp, []string{"--agent", "alice"}, true) }()
				select {
				case <-pauseStarted:
				case <-time.After(3 * time.Second):
					t.Fatal("public pause did not start at the reserved Send boundary")
				}
				err := config.WithRuntimeLock(f.e.paths(), "alice", "control", time.Now(), func() error { return errors.New("control lock unexpectedly available") })
				if !errors.Is(err, config.ErrLockTimeout) {
					t.Fatalf("reservation did not retain pause serialization: %v", err)
				}
				return nil
			}
			f.step()
			f.a.afterNotificationReserve = nil
			data, err := os.ReadFile(marker)
			if err != nil || !reserved || string(data) != "type:held\nenter:held\n" {
				t.Fatalf("send boundary evidence: %q reserved=%t err=%v", data, reserved, err)
			}
			select {
			case err := <-pauseDone:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("public pause did not acknowledge after admitted Send")
			}
			if !f.e.state("alice").HooksPaused {
				t.Fatal("pause acknowledgement did not persist its gate")
			}
			f.mail() // newly pending mail after the public pause acknowledged
			// Make pause the active gate rather than an unexpired cooldown.
			f.now = f.now.Add(config.NotificationEvery)
			f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Minute) })
			mailbox := pauseMailboxBytes(t, f)
			attempt := pauseAttemptEvidence(*f.binding())
			for i := 0; i < 3; i++ {
				f.step()
			}
			f.control(map[string]any{})
			screen := f.wait(func(orphanScreen) bool { return true })
			if len(screen.Submitted) != 1 || screen.Submitted[0] != tmuxwake.Notice(1) || screen.Text != "" || screen.Erased != 0 || !reflect.DeepEqual(attempt, pauseAttemptEvidence(*f.binding())) || !bytes.Equal(mailbox, pauseMailboxBytes(t, f)) {
				t.Fatalf("acknowledged pause admitted a new send or lost evidence: %+v", screen)
			}
			data, err = os.ReadFile(marker)
			if err != nil || string(data) != "type:held\nenter:held\n" {
				t.Fatal("paused dispatch reached another send boundary")
			}
			f.e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
			f.dispatch()
			f.step()
			f.control(map[string]any{})
			screen = f.wait(func(orphanScreen) bool { return true })
			if len(screen.Submitted) != 2 || screen.Submitted[1] != tmuxwake.Notice(1) || screen.Text != "" || screen.Erased != 0 || !bytes.Equal(mailbox, pauseMailboxBytes(t, f)) {
				t.Fatalf("resumed Send-boundary neighbor did not deliver exactly once or preserve mail: %+v", screen)
			}
			data, err = os.ReadFile(marker)
			if err != nil || string(data) != "type:held\nenter:held\ntype:held\nenter:held\n" {
				t.Fatal("resumed send lost control-lock boundary coverage")
			}
		})
	}
}

// Neither known-unsubmitted evidence nor an UNKNOWN historical typing time
// permits pause/resume to submit or erase a pre-existing nonempty composer.
func TestTerminalPausePreservesDraft(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, hold := range []string{"none", "manual", "native"} {
			t.Run(fmt.Sprintf("known=%t/%s", known, hold), func(t *testing.T) {
				f := newOrphanDispatchFixture(t)
				id := f.mail()
				f.control(map[string]any{"text": tmuxwake.Notice(1)})
				f.update(func(st *config.State) {
					b := st.Terminal
					b.AttemptIDs = []string{id}
					b.Notice = tmuxwake.Notice(1)
					b.NoticeCount = 1
					b.Typed = known
					if known {
						b.AttemptAt = f.now.Add(-time.Minute)
					} // UNKNOWN is left zero
					b.Reservations = []time.Time{f.now.Add(-time.Minute)}
					if hold == "manual" {
						st.Liveness = &config.AgentStatus{State: "waiting-human"}
						st.LivenessSource = "manual"
					}
					if hold == "native" {
						st.Runtime = &codexwake.Binding{}
					}
				})
				before := f.e.state("alice")
				attempt := pauseAttemptEvidence(*before.Terminal)
				mailbox := pauseMailboxBytes(t, f)
				for _, command := range []string{"pause", "resume"} {
					f.e.as("alice", "hook", command, "--agent", "alice").ok(t)
					afterCommand := f.e.state("alice")
					if !reflect.DeepEqual(attempt, pauseAttemptEvidence(*afterCommand.Terminal)) || !reflect.DeepEqual(before.Terminal.OwnNotice, afterCommand.Terminal.OwnNotice) || !reflect.DeepEqual(before.Runtime, afterCommand.Runtime) || !reflect.DeepEqual(before.Liveness, afterCommand.Liveness) || before.LivenessSource != afterCommand.LivenessSource {
						t.Fatal("pause/resume changed retained attempt, UNKNOWN evidence or independent hold")
					}
					for i := 0; i < 3; i++ {
						f.step()
					}
					f.control(map[string]any{})
					screen := f.wait(func(orphanScreen) bool { return true })
					after := f.e.state("alice")
					if screen.Text != tmuxwake.Notice(1) || screen.Erased != 0 || len(screen.Submitted) != 0 || !reflect.DeepEqual(attempt, pauseAttemptEvidence(*after.Terminal)) || !bytes.Equal(mailbox, pauseMailboxBytes(t, f)) || !reflect.DeepEqual(before.Runtime, after.Runtime) || !reflect.DeepEqual(before.Liveness, after.Liveness) || before.LivenessSource != after.LivenessSource {
						t.Fatalf("pause/resume submitted/erased stale draft or changed preserved evidence: %+v", screen)
					}
					if !known && (!after.Terminal.AttemptAt.IsZero() || after.Terminal.Typed || after.Terminal.OwnNotice.PossiblyOrphaned != nil) {
						t.Fatal("UNKNOWN typing history became a claimed observation")
					}
				}
			})
		}
	}
}
