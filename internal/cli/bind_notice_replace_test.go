package cli

import (
	"context"
	"reflect"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// This deterministic state-boundary witness takes a stale load, commits a real
// report through the loopback fake, then uses the production replacement writer.
// It proves pin/Sent preservation at that writer, NOT bindTerminal's original
// load/guard/death/startup path: no owned consumer is stopped here. The full
// owned-consumer interleaving and its old-snapshot plant require separate admission.
func TestNoticeReplacementCurrentState(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"pin-after-load", "ack-after-load", "unchanged"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, room := reportFixture(t)
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				b := st.Terminal
				b.Host = machineLabel()
				b.SessionID = "fixture-session"
				b.Typed = true
				b.NoticeCount = 1
				b.Notice = "fixture-journal"
				b.AttemptIDs = []string{"fixture-mail"}
				b.Rung = []string{"fixture-mail"}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mode != "pin-after-load" {
				if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ack-after-load" {
				_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
					st.Terminal.NoticeReport.Sent = false
					st.Terminal.NoticeReport.Outcome = "pending"
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			loaded := e.state("alice").Terminal
			// The unchanged case is the nearby positive; the other cases insert
			// the report writer strictly after this load and before replacement.
			if mode != "unchanged" {
				if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
					t.Fatal(err)
				}
			}
			pin := e.state("alice").Terminal.NoticeReport
			if pin == nil || !pin.Sent || len(e.projectionsFrom(room, s.creds.UserID)) != 1 {
				t.Fatal("setup: report not applied and acknowledged")
			}
			next := *loaded
			next.Generation = "replacement"
			next.OwnNotice = config.OwnNoticeObservation{}
			next.NoticeReport = nil
			if err := persistTerminalReplacement(s, &next, true); err != nil {
				t.Fatal(err)
			}
			got := e.state("alice").Terminal
			if !reflect.DeepEqual(got.NoticeReport, pin) || !got.NoticeReport.Sent {
				t.Fatal("replacement lost current pin or Sent")
			}
			if !reflect.DeepEqual(got.AttemptIDs, loaded.AttemptIDs) || !reflect.DeepEqual(got.Rung, loaded.Rung) || got.Notice != loaded.Notice || got.NoticeCount != loaded.NoticeCount || got.Typed != loaded.Typed || got.NotificationToken != "" {
				t.Fatal("replacement changed journal or notification authority")
			}
			calls := e.hs.CallCount(sendRoute)
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "replacement"); err != nil {
				t.Fatal(err)
			}
			if e.hs.CallCount(sendRoute) != calls || len(e.projectionsFrom(room, s.creds.UserID)) != 1 || !reflect.DeepEqual(e.state("alice").Terminal.NoticeReport, pin) {
				t.Fatal("replacement minted a transaction or repeated the event")
			}
		})
	}
}

// Startup must retain the authoritative report and journal, updating only its
// PID/birth; a different or absent generation refuses without writing anything.
// This is a state-writer control, not a child-process lifecycle test.
func TestNoticeConsumerMetadata(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"same", "different-generation", "absent"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := reportFixture(t)
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
				t.Fatal(err)
			}
			started := *e.state("alice").Terminal
			started.NoticeReport = nil
			started.DaemonPID = 123
			started.DaemonBirth = "fixture-birth"
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				if mode == "different-generation" {
					st.Terminal.Generation = "other"
				} else if mode == "absent" {
					st.Terminal = nil
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			before := e.state("alice")
			err = persistTerminalConsumer(s, &started)
			if mode == "same" {
				if err != nil {
					t.Fatal(err)
				}
				before.Terminal.DaemonPID = started.DaemonPID
				before.Terminal.DaemonBirth = started.DaemonBirth
			} else if err == nil || err.Error() != "binding changed before consumer startup" {
				t.Fatal("startup must refuse a retired generation", err)
			}
			if !reflect.DeepEqual(before, e.state("alice")) {
				t.Fatal("consumer metadata update changed report or other state")
			}
		})
	}
}
