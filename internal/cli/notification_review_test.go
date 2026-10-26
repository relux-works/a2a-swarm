package cli

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// A draft or send-error terminal result retains its own attempt fence, but
// common uncertainty spaces a live-owner Stop/prompt only until reservation+30s.
// This drives real finishTerminal and hooks; Send failures are supplied at its
// typed completion seam, not claimed as a live terminal transport observation.
func TestNotificationHostUncertainTerminalHookBoundary(t *testing.T) {
	for _, outcome := range []string{"draft", "send-error"} {
		for _, hook := range []string{"stop", "prompt"} {
			t.Run(outcome+"/"+hook, func(t *testing.T) {
				e, room, _, g := runtimeWorld(t)
				clock := e.clock()
				id := runtimeMail(e, room, "retained private mail")
				a, s := notificationApp(t, e)
				notificationConsumerLock(t, e)
				if err := a.nativeConsumer(context.Background(), s, g, true); err != nil {
					t.Fatal(err)
				}
				var expected config.TerminalBinding
				notificationState(t, e, func(st *config.State) {
					if err := st.Notification.Prepare(clock.now(), time.Time{}); err != nil {
						t.Fatal(err)
					}
					token, err := st.Notification.Reserve("terminal", "owned-thread", "terminal-g", clock.now(), false)
					if err != nil {
						t.Fatal(err)
					}
					expected = config.TerminalBinding{Generation: "terminal-g", SessionID: "owned-thread", NotificationToken: token, AttemptAt: clock.now(), AttemptIDs: []string{id}}
					copy := expected
					copy.AttemptIDs = append([]string(nil), expected.AttemptIDs...)
					st.Terminal = &copy
				})
				text := "notice left in draft: synthetic typed deferral"
				var sendErr error
				if outcome == "send-error" {
					text = ""
					sendErr = errors.New("synthetic ambiguous transport error")
				}
				if err := a.finishTerminal(s, "terminal-g", text, sendErr, false, expected); err != nil {
					t.Fatal(err)
				}
				initial := e.state("alice")
				if initial.Notification.Reservation == nil || initial.Notification.Reservation.Token != expected.NotificationToken || len(initial.Terminal.AttemptIDs) != 1 || initial.Terminal.Typed != (outcome == "draft") {
					t.Fatal("completion lost uncertain attempt")
				}
				initial.Terminal.NoticePublication = "" // Hook status publication is permitted telemetry.
				terminalBefore, _ := json.Marshal(initial.Terminal)
				clock.add(29999 * time.Millisecond)
				if out := e.hook("alice", hook, "{\"session_id\":\"owned-thread\"}").ok(t).stdout; out != "" {
					t.Fatal("hook escaped spacing at29.999s", out)
				}
				if st := e.state("alice"); st.Notification.Reservation == nil || st.Notification.Reservation.Token != expected.NotificationToken {
					t.Fatal("early hook cleared uncertain reservation")
				}
				clock.add(time.Millisecond)
				out := e.hook("alice", hook, "{\"session_id\":\"owned-thread\"}").ok(t).stdout
				if hook == "stop" {
					stopReason(t, out)
				} else {
					bellContext(t, out)
				}
				final := e.state("alice")
				final.Terminal.NoticePublication = ""
				terminalAfter, _ := json.Marshal(final.Terminal)
				if string(terminalBefore) != string(terminalAfter) || e.received("alice", id) {
					t.Fatalf("before=%s after=%s received=%v", terminalBefore, terminalAfter, e.received("alice", id))
				}
				if final.Notification.Reservation != nil || !final.Notification.NotBefore.Equal(clock.now().Add(30*time.Second)) {
					t.Fatal("hook failed to replace and complete eligible shared reservation")
				}
			})
		}
	}
}

// Mail selection is rechecked under the saved-reservation lock. If every ID
// disappears or becomes rung between the first read and that check, the status
// must say no new mail, without reserving or touching the terminal. The valid
// neighbor still submits. The fixture's clock callback injects the competing
// local update after lease checking and initial mail selection, before idle
// qualification; it does not alter production timing or authority.
func TestNotificationTerminalSelectionBecameEmpty(t *testing.T) {
	for _, mode := range []string{"received", "rung", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			id := f.mail()
			f.dispatch()
			f.now = f.now.Add(time.Second)
			calls := 0
			injected := false
			f.a.Now = func() time.Time {
				pc, _, _, _ := runtime.Caller(1)
				if fn := runtime.FuncForPC(pc); fn != nil && strings.HasSuffix(fn.Name(), ".dispatchTerminal.func1") {
					calls++
				}
				if calls == 2 && !injected {
					injected = true
					_, err := f.s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(st *config.State, mb *config.Mailbox) ([]string, error) {
						if mode == "received" {
							_, err := mb.Receive([]string{id})
							return nil, err
						}
						if mode == "rung" {
							st.Terminal.Rung = append(st.Terminal.Rung, id)
						}
						return nil, nil
					})
					if err != nil {
						t.Fatal(err)
					}
				}
				return f.now
			}
			f.dispatch()
			if !injected {
				t.Fatal("selection-race control did not run")
			}
			b := f.binding()
			frame := f.wait(func(s orphanScreen) bool { return true })
			if mode == "unchanged" {
				if len(frame.Submitted) != 1 || len(b.Rung) != 1 {
					t.Fatal("valid selection failed", b, frame)
				}
			} else {
				if b.Outcome != "armed: no new mail" {
					t.Fatalf("empty selection retained stale outcome %q", b.Outcome)
				}
				if b.Idle != nil || len(b.AttemptIDs) != 0 || len(b.Reservations) != 0 || f.e.state("alice").Notification.Reservation != nil || len(frame.Submitted) != 0 || frame.Text != "" || frame.Erased != 0 {
					t.Fatal("empty selection spent or retained an effect", b, frame)
				}
			}
			if f.e.received("alice", id) != (mode == "received") {
				t.Fatal("dispatch changed receipt authority")
			}
		})
	}
}
