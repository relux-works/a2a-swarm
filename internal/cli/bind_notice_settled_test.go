package cli

import (
	"reflect"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Actual in-process settlement leaves historical count/time while clearing pending
// IDs/Notice. A later nonempty observation is clear, with no further journal changes.
// SendResult is supplied: no physical submission, draft, Enter or erase is exercised.
func TestNoticeSettledAttemptDraft(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"submitted", "fresh-refused"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := reportFixture(t)
			var expected config.TerminalBinding
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				b := st.Terminal
				b.OwnNotice = config.OwnNoticeObservation{}
				b.NoticeReport = nil
				b.SessionID = "fixture-session"
				token, err := st.Notification.Reserve("terminal", b.SessionID, b.Generation, a.Now(), false)
				if err != nil {
					return err
				}
				b.NotificationToken = token
				b.AttemptAt = a.Now()
				b.AttemptIDs = []string{"fixture-mail"}
				b.NoticeCount = 1
				b.Notice = tmuxwake.Notice(1)
				b.Typed = false // reservation precedes the first send, including fresh refusal
				expected = *b
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			result := tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}
			if mode == "fresh-refused" {
				result = tmuxwake.SendResult{Disposition: tmuxwake.SendRefused, Outcome: tmuxwake.SendSuppressedInput}
			}
			if err := a.finishTerminal(s, "gen", result, nil, false, expected); err != nil {
				t.Fatal(err)
			}
			settled := *e.state("alice").Terminal
			if settled.Typed || len(settled.AttemptIDs) != 0 || settled.Notice != "" || settled.NoticeCount != 1 || !settled.AttemptAt.Equal(expected.AttemptAt) || settled.AttemptAt.IsZero() {
				t.Fatal("not the production settlement shape", settled)
			}
			state := e.state("alice")
			if state.Notification.Reservation != nil || (mode == "submitted" && (!reflect.DeepEqual(settled.Rung, []string{"fixture-mail"}) || settled.NotificationToken != "")) || (mode == "fresh-refused" && (len(settled.Rung) != 0 || settled.NotificationToken != expected.NotificationToken)) {
				t.Fatal("settlement ledger mismatch")
			}
			if err := a.observeTerminalNotice(s, "gen", settled, "nonempty"); err != nil {
				t.Fatal(err)
			}
			after := e.state("alice")
			requireNotice(t, after.Terminal.OwnNotice, "clear")
			after.Terminal.OwnNotice = settled.OwnNotice
			if !reflect.DeepEqual(state, after) || e.hs.CallCount(sendRoute) != 0 {
				t.Fatal("classification changed settlement journal or sent a report")
			}
		})
	}
}
