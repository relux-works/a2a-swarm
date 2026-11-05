package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Draft/refused completion persists a bounded reason/time in the journal, log
// and both status forms. Injected screen text maps to unavailable; mail remains
// unreceived and no draft is credited as submitted. Raw state bytes also exclude
// injected text before the sanitizing reload. No terminal is inspected.
func TestTerminalSendRefusalDiagnostic(t *testing.T) {
	for _, disposition := range []tmuxwake.SendDisposition{tmuxwake.SendDraft, tmuxwake.SendRefused} {
		for _, reason := range []tmuxwake.SendReason{tmuxwake.ReasonKeyWidthChanged, tmuxwake.ReasonBusyOrDialog, "PRIVATE_SCREEN\nSECRET"} {
			t.Run(string(disposition)+"/"+string(reason), func(t *testing.T) {
				e, room := world(t)
				clock := e.clock()
				id := runtimeMail(e, room, "retained mail")
				a, s := notificationApp(t, e)
				a.Now = clock.now
				var expected config.TerminalBinding
				notificationState(t, e, func(st *config.State) {
					token, err := st.Notification.Reserve("terminal", "s", "g", clock.now(), false)
					if err != nil {
						t.Fatal(err)
					}
					expected = config.TerminalBinding{Generation: "g", SessionID: "s", NotificationToken: token, AttemptAt: clock.now(), AttemptIDs: []string{id}, Notice: "notice"}
					copy := expected
					st.Terminal = &copy
				})
				outcome := tmuxwake.SendDraftChanged
				if disposition == tmuxwake.SendRefused {
					outcome = tmuxwake.SendSuppressedInput
				}
				result := tmuxwake.SendResult{Disposition: disposition, Outcome: outcome, Reason: reason, ReasonAt: clock.now()}
				if err := a.finishTerminal(s, "g", result, nil, false, expected); err != nil {
					t.Fatal(err)
				}
				raw, err := os.ReadFile(s.paths.StateFile(s.name))
				if err != nil {
					t.Fatal(err)
				}
				if bytes.Contains(raw, []byte("PRIVATE")) || bytes.Contains(raw, []byte("SECRET")) {
					t.Fatal("unsafe raw state")
				}
				b := e.state("alice").Terminal
				safe := tmuxwake.SafeSendReason(reason)
				if b.SendRefusal == nil || b.SendRefusal.Reason != safe || !b.SendRefusal.At.Equal(clock.now()) || len(b.Rung) != 0 || e.received("alice", id) {
					t.Fatal("wrong refusal state")
				}
				log, err := os.ReadFile(filepath.Join(e.dir, "state", "alice.binding.log"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(log), "send_refusal="+string(safe)) || !strings.Contains(string(log), "at=") || strings.Contains(string(log), "PRIVATE") || strings.Contains(string(log), "SECRET") {
					t.Fatal("unsafe/missing log")
				}
				for _, asJSON := range []bool{false, true} {
					var out bytes.Buffer
					a.Stdout = &out
					args := []string{}
					if asJSON {
						args = []string{"--json"}
					}
					if err := a.terminalStatus(context.Background(), args); err != nil {
						t.Fatal(err)
					}
					if !strings.Contains(out.String(), string(safe)) || strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "SECRET") {
						t.Fatal("unsafe/missing status")
					}
					if asJSON {
						var view struct {
							SendRefusal *config.TerminalSendRefusal `json:"send_refusal"`
						}
						if json.Unmarshal(out.Bytes(), &view) != nil || view.SendRefusal == nil || !view.SendRefusal.At.Equal(clock.now()) {
							t.Fatal("missing JSON timestamp")
						}
					}
				}
			})
		}
	}
}

// A rejected diagnostic append cannot fail completion or alter the committed
// draft/refused effects. A writable log is the positive control for each result.
// This exercises the completion boundary, not a live terminal poll.
func TestTerminalSendRefusalLogFailureDoesNotFailCompletion(t *testing.T) {
	for _, disposition := range []tmuxwake.SendDisposition{tmuxwake.SendDraft, tmuxwake.SendRefused} {
		for _, blockedLog := range []bool{false, true} {
			t.Run(string(disposition)+map[bool]string{false: "/writable", true: "/blocked"}[blockedLog], func(t *testing.T) {
				e, room := world(t)
				clock := e.clock()
				id := runtimeMail(e, room, "retained mail")
				a, s := notificationApp(t, e)
				a.Now = clock.now
				var expected config.TerminalBinding
				notificationState(t, e, func(st *config.State) {
					token, err := st.Notification.Reserve("terminal", "s", "g", clock.now(), false)
					if err != nil {
						t.Fatal(err)
					}
					expected = config.TerminalBinding{Generation: "g", SessionID: "s", NotificationToken: token, AttemptAt: clock.now(), AttemptIDs: []string{id}, Notice: "notice"}
					copy := expected
					st.Terminal = &copy
				})
				logPath := filepath.Join(s.paths.Base, "state", s.name+".binding.log")
				if blockedLog {
					// A directory deterministically rejects append, including as root.
					if err := os.Mkdir(logPath, 0700); err != nil {
						t.Fatal(err)
					}
				}
				outcome := tmuxwake.SendDraftChanged
				if disposition == tmuxwake.SendRefused {
					outcome = tmuxwake.SendSuppressedInput
				}
				result := tmuxwake.SendResult{Disposition: disposition, Outcome: outcome, Reason: tmuxwake.ReasonBusyOrDialog, ReasonAt: clock.now()}
				if err := a.finishTerminal(s, "g", result, nil, false, expected); err != nil {
					t.Fatal("diagnostics failed completion", err)
				}
				st := e.state("alice")
				b := st.Terminal
				draft := disposition == tmuxwake.SendDraft
				if b.Outcome != string(outcome) || b.SendRefusal == nil || b.SendRefusal.Reason != result.Reason || !b.SendRefusal.At.Equal(clock.now()) || b.Typed != draft || len(b.Rung) != 0 || e.received("alice", id) {
					t.Fatal("diagnostics changed delivery")
				}
				if draft {
					if len(b.AttemptIDs) != 1 || b.AttemptIDs[0] != id || b.Notice != expected.Notice || st.Notification.Reservation == nil || st.Notification.Reservation.Token != expected.NotificationToken {
						t.Fatal("draft lost recovery state")
					}
				} else if len(b.AttemptIDs) != 0 || b.Notice != "" || st.Notification.Reservation != nil {
					t.Fatal("refusal retained attempt or reservation")
				}
				if !blockedLog {
					data, err := os.ReadFile(logPath)
					if err != nil || !bytes.Contains(data, []byte("send_refusal=busy_or_dialog at=")) {
						t.Fatal("writable log missing diagnostic", err)
					}
				}
			})
		}
	}
}
