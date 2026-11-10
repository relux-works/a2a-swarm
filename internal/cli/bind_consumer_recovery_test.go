//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Only the fixture's pinned socket is ever queried. A failed display command
// leaves the socket/server identity intact, so clearing the gate is reconnectable.
func consumerConnectionGate(t *testing.T) string {
	t.Helper()
	real, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	gate := filepath.Join(dir, "unavailable")
	script := fmt.Sprintf("#!/bin/sh\nif [ -e %q ]; then\n case \"$*\" in *display-message*) exit 1;; esac\nfi\nexec %q \"$@\"\n", gate, real)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return gate
}

func setConsumerConnectionGate(t *testing.T, gate string, failed bool) {
	t.Helper()
	if failed {
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
	} else if err := os.Remove(gate); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

// W1 witness: the actual consumer logs exactly one ready line after N failures,
// and two healthy cycles cannot repeat it. C1/C2 controls: first-try success is
// quiet, and connection failures retain the exact existing closed-format line.
// These tests use only a private terminal/fake homeserver, never a live identity.
func TestBindingConsumerRecoveryLogs(t *testing.T) {
	isolateBindTest(t)
	for _, failures := range []int{0, 1, 3, 6} {
		t.Run(fmt.Sprintf("failures-%d", failures), func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			gate := consumerConnectionGate(t)
			setConsumerConnectionGate(t, gate, failures > 0)
			f.update(func(st *config.State) {
				st.HooksPaused = true
				st.Terminal.Interval, st.Terminal.RetryMax = time.Millisecond, 4*time.Millisecond
			})
			var log strings.Builder
			f.a.Stderr = &log
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cycles := 0
			outcome, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() {
				cycles++
				if cycles == failures {
					setConsumerConnectionGate(t, gate, false)
				}
				if cycles == failures+2 {
					cancel()
				}
			})
			if err != nil || outcome != "cancelled" || cycles != failures+2 {
				t.Fatalf("consumer outcome=%q err=%v cycles=%d log=%s", outcome, err, cycles, log.String())
			}
			wantReady := 1
			if failures == 0 {
				wantReady = 0
			}
			want := strings.Repeat("binding consumer unavailable; bounded retry cause=connection\n", failures) + strings.Repeat("binding consumer ready\n", wantReady)
			if log.String() != want {
				t.Fatalf("log=%q want=%q", log.String(), want)
			}
			if f.binding().Outcome != "suppressed: manual gate" {
				t.Fatal("connection did not recover into dispatch")
			}
		})
	}
}

// W3 witness/C3 control: fake time drives the real consumer through just below
// and exactly at ten minutes, a second bound, recovery, and a second episode.
// Exactly one owner alarm is delivered per episode without any pending notice.
func TestBindingConsumerUnavailableAlarmEpisodes(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	gate := consumerConnectionGate(t)
	room := f.e.ownReportRoom("alice")
	f.update(func(st *config.State) {
		st.HooksPaused = true
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room}
		st.Terminal.Interval, st.Terminal.RetryMax = time.Millisecond, 4*time.Millisecond
	})
	start := f.now
	steps := []struct {
		at     time.Duration
		failed bool
		alarms int
		ready  int
	}{
		{0, true, 0, 0},
		{10*time.Minute - time.Nanosecond, true, 0, 0},
		{10 * time.Minute, true, 1, 0},
		{20 * time.Minute, true, 1, 0},
		{20*time.Minute + time.Second, false, 1, 1},
		{21 * time.Minute, true, 1, 1},
		{31 * time.Minute, true, 2, 1},
		{32 * time.Minute, false, 2, 2},
		{33 * time.Minute, false, 2, 2},
	}
	var log strings.Builder
	f.a.Stderr = &log
	f.now = start.Add(steps[0].at)
	setConsumerConnectionGate(t, gate, steps[0].failed)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cycle := 0
	_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() {
		step := steps[cycle]
		// Exercise the existing sender and its ACK/fence, not a mock alarm call.
		f.a.sendMailAlarm(ctx, f.s, "orphan-gen")
		alerts := strandedAlerts(f.e)
		if len(alerts) != step.alarms || strings.Count(log.String(), "binding consumer ready\n") != step.ready {
			t.Fatalf("cycle=%d alarms=%d want=%d log=%s", cycle, len(alerts), step.alarms, log.String())
		}
		for _, alert := range alerts {
			if !strings.Contains(alert.Message().Body, "reason=consumer unavailable") {
				t.Fatal("consumer alarm has an unqualified reason")
			}
		}
		if f.binding().OwnNotice.Since != "" || f.binding().NoticeReport != nil {
			t.Fatal("consumer alarm fabricated a stranded notice")
		}
		cycle++
		if cycle == len(steps) {
			cancel()
			return
		}
		f.now = start.Add(steps[cycle].at)
		setConsumerConnectionGate(t, gate, steps[cycle].failed)
	})
	if err != nil || cycle != len(steps) {
		t.Fatalf("consumer failed: %v cycles=%d", err, cycle)
	}
}

// C3 control: recovery one nanosecond before the bound sends no alarm, even
// after time later advances beyond the old episode's deadline. Base-compatible.
func TestBindingConsumerRecoversBeforeAlarm(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	gate := consumerConnectionGate(t)
	room := f.e.ownReportRoom("alice")
	f.update(func(st *config.State) {
		st.HooksPaused = true
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room}
		st.Terminal.Interval, st.Terminal.RetryMax = time.Millisecond, 4*time.Millisecond
	})
	setConsumerConnectionGate(t, gate, true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cycle := 0
	_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() {
		cycle++
		f.a.sendMailAlarm(ctx, f.s, "orphan-gen")
		if len(strandedAlerts(f.e)) != 0 || len(f.e.state("alice").MailAlarm.Jobs) != 0 {
			t.Fatal("short episode raised an alarm")
		}
		switch cycle {
		case 1:
			f.now = f.now.Add(10*time.Minute - time.Nanosecond)
			setConsumerConnectionGate(t, gate, false)
		case 2:
			f.now = f.now.Add(20 * time.Minute)
		case 3:
			cancel()
		}
	})
	if err != nil || cycle != 3 {
		t.Fatalf("consumer failed: %v cycles=%d", err, cycle)
	}
}

// N1 negative: unavailable alarm routes/config/state cannot fail or spin the
// consumer. The existing closed outcome appears once, repeated bounded retries
// continue, recovery is logged, and no incident reaches the fake homeserver.
func TestBindingConsumerAlarmUnavailableContinues(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"missing-route", "invalid-config", "invalid-state", "send-failure"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			gate := consumerConnectionGate(t)
			room := f.e.ownReportRoom("alice")
			sendFailures := 0
			if mode == "send-failure" {
				base := f.s.client.HTTP.Transport
				if base == nil {
					base = http.DefaultTransport
				}
				f.s.client.HTTP = &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/") {
						sendFailures++
						return nil, errors.New("synthetic alarm transport unavailable")
					}
					return base.RoundTrip(r)
				})}
			}
			f.update(func(st *config.State) {
				st.HooksPaused = true
				st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: "off"}
				if mode == "invalid-config" {
					st.MailAlarmConfig.Minutes = 0
				}
				if mode == "invalid-state" {
					st.MailAlarm.Invalid = true
				}
				if mode == "send-failure" {
					st.MailAlarmConfig.Owner = room
				}
				st.Terminal.Interval, st.Terminal.RetryMax = time.Millisecond, 4*time.Millisecond
			})
			var log strings.Builder
			f.a.Stderr = &log
			setConsumerConnectionGate(t, gate, true)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cycle := 0
			_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), func() {
				cycle++
				f.a.sendMailAlarm(ctx, f.s, "orphan-gen")
				f.now = f.now.Add(11 * time.Minute)
				if cycle == 3 {
					setConsumerConnectionGate(t, gate, false)
				}
				if cycle == 5 {
					cancel()
				}
			})
			want := "unavailable: alarm destination missing"
			if mode == "invalid-config" {
				want = "unavailable: invalid alarm config"
			}
			if mode == "invalid-state" {
				want = "unavailable: invalid alarm state"
			}
			if mode == "send-failure" {
				want = "queued via mail-alarm"
				if sendFailures == 0 {
					t.Fatal("negative control never attempted the failing transport")
				}
			}
			if err != nil || cycle != 5 || strings.Count(log.String(), "binding incident report: "+want+"\n") != 1 || strings.Count(log.String(), "binding consumer ready\n") != 1 || len(strandedAlerts(f.e)) != 0 {
				t.Fatalf("consumer failed: %v cycles=%d log=%s", err, cycle, log.String())
			}
		})
	}
}

// C2 control: one existing retry line keeps its exact format independently of
// the new recovery signal. The injected connection failure must be observed.
func TestBindingConsumerFailureLogFormat(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	gate := consumerConnectionGate(t)
	setConsumerConnectionGate(t, gate, true)
	var log strings.Builder
	f.a.Stderr = &log
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := f.a.runTerminalWithRefresh(ctx, f.s, "orphan-gen", *f.binding(), cancel)
	if err != nil || log.String() != "binding consumer unavailable; bounded retry cause=connection\n" {
		t.Fatalf("failure log=%q err=%v", log.String(), err)
	}
}
