package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// A genuinely typed, unsubmitted notice with all attempt IDs still pending
// must surface without inbox receipts; later mail cannot hide the suspicion.
// These synthetic screens prove diagnostics, not provider responsiveness.
func TestTerminalStrandedPendingNotice(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	id := f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	if !f.binding().Typed || len(f.binding().AttemptIDs) != 1 {
		t.Fatal("no stranded attempt")
	}
	f.control(map[string]any{})
	f.step()
	requireNotice(t, f.binding().OwnNotice, "suspected")
	f.mail()
	f.step()
	if !strings.Contains(f.e.as("alice", "bind", "status").ok(t).stdout, "SUSPECTED-DEAF") {
		t.Fatal("stranded notice invisible")
	}
	s := f.wait(func(s orphanScreen) bool { return true })
	if s.Erased != 0 || len(s.Submitted) != 0 || f.e.received("alice", id) || len(f.binding().Reservations) != 1 {
		t.Fatal("diagnostics mutated input/mail/rate")
	}
}

// An arbitrary or edited draft is never submitted, even after the alert age.
func TestTerminalStrandedOtherInputNeverSubmitted(t *testing.T) {
	isolateBindTest(t)
	for _, draft := range []string{"HUMAN_DRAFT", "a2a: 1 new peer events; run a2a inbox suffix"} {
		t.Run(draft, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			f.e.ownReportRoom("alice")
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.control(map[string]any{"text": draft, "hint": true})
			f.step()
			f.now = f.now.Add(11 * time.Minute)
			// Keep the fixture's own lease live; no production process is involved.
			f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Minute) })
			f.dispatch()
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != draft || s.Erased != 0 || len(s.Submitted) != 0 {
				t.Fatal("recovery touched other input")
			}
		})
	}
}

// No new mail or liveness record is needed: a sustained suspicion reports at
// ten minutes exactly once after the Enter retry is exhausted. Process reload and replay of a pre-ACK local record
// reuse the persisted transaction, so an ambiguous send cannot duplicate it.
func TestTerminalStrandedThresholdReportOnce(t *testing.T) {
	isolateBindTest(t)
	f := newOrphanDispatchFixture(t)
	system := f.e.ownReportRoom("alice")
	id := f.mail()
	f.control(map[string]any{"defer": true})
	f.dispatch()
	f.step()
	f.control(map[string]any{})
	f.step()
	exhaustRecovery(t, f)
	since, err := time.Parse(time.RFC3339Nano, f.binding().OwnNotice.Since)
	if err != nil {
		t.Fatal(err)
	}
	count := func() int { return len(f.e.projectionsFrom(system, "@alice:test.local")) }
	advance := func(d time.Duration) {
		f.now = since.Add(d)
		f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Minute) })
		f.dispatch()
	}
	advance(10*time.Minute - time.Nanosecond)
	if count() != 0 || f.binding().NoticeReport != nil {
		t.Fatal("early report")
	}
	advance(10 * time.Minute)
	if count() != 1 || f.binding().NoticeReport == nil || !f.binding().NoticeReport.Sent {
		t.Fatal("missing threshold report")
	}
	r := *f.binding().NoticeReport
	raw := f.e.as("alice", "bind", "status").ok(t).stdout
	if !strings.Contains(raw, "system-report=published") {
		t.Fatal("missing report status", raw)
	}
	// Simulate restart after the server applied the transaction but before the
	// local ACK was saved. The already-persisted destination/content survive.
	f.update(func(st *config.State) {
		st.Terminal.NoticeReport.Sent = false
		st.Terminal.NoticeReport.Outcome = "pending"
	})
	clone := *f.a
	f.a = &clone
	f.s, err = f.a.session()
	if err != nil {
		t.Fatal(err)
	}
	advance(11 * time.Minute)
	advance(12 * time.Minute)
	if count() != 1 || f.binding().NoticeReport.Message.Txn != r.Message.Txn || !f.binding().NoticeReport.Sent {
		t.Fatal("duplicated report across retry/restart")
	}
	ev := f.e.projectionsFrom(system, "@alice:test.local")[0]
	var content struct {
		Body string `json:"body"`
		H    Header `json:"ai.relux.a2a"`
	}
	if err := json.Unmarshal(ev.Content, &content); err != nil {
		t.Fatal(err)
	}
	if content.H.Kind != "system" || content.H.Subtype != "incident" || content.H.From != "alice" || !strings.Contains(content.Body, "SUSPECTED-DEAF") || strings.Contains(content.Body, "run a2a inbox") || strings.Contains(content.Body, f.dir) {
		t.Fatal("incorrect or leaking report", content)
	}
	s := f.wait(func(s orphanScreen) bool { return true })
	if s.Erased != 0 || len(s.Submitted) != 0 || f.e.received("alice", id) || len(f.binding().Reservations) != 1 || f.e.state("alice").Liveness != nil {
		t.Fatal("report changed input/mail/rate/liveness")
	}
}

func (e *env) projectionsFrom(room, sender string) []matrix.Event {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var out []matrix.Event
	for _, ev := range e.hs.events {
		if ev.RoomID == room && ev.Sender == sender && ev.Type == "m.room.message" {
			out = append(out, ev)
		}
	}
	return out
}

// Failure never claims publication; retry obeys the configured cap and reuses
// its transaction. Unknown/cleared observations cannot publish old suspicion.
func TestTerminalStrandedReportFailureAndUnknown(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"failed", "missing-room", "unknown", "clear", "partial", "untyped", "foreign"} {
		t.Run(mode, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			system := ""
			if mode != "missing-room" {
				system = f.e.ownReportRoom("alice")
			}
			f.mail()
			f.control(map[string]any{"defer": true})
			f.dispatch()
			f.step()
			f.control(map[string]any{})
			f.step()
			exhaustRecovery(t, f)
			f.now = f.now.Add(10 * time.Minute)
			f.update(func(st *config.State) {
				st.Lease.Expires = f.now.Add(time.Minute)
				if mode == "partial" {
					st.Terminal.TypingStarted = false // Corrupt count has no continuing typing authority.
					st.Terminal.NoticeCount = 2
					st.Terminal.Notice = tmuxwake.Notice(2)
				}
				if mode == "untyped" {
					st.Terminal.TypingStarted = false
					st.Terminal.Typed = false
				}
			})
			if mode == "unknown" {
				f.control(map[string]any{"unknown": true})
			}
			if mode == "clear" {
				f.control(map[string]any{"text": ""})
			}
			if mode == "failed" {
				f.e.hs.fail(sendRoute, 503, "M_UNKNOWN", "synthetic incident failure")
			}
			if mode == "foreign" {
				f.s.creds.UserID = "@bob:test.local"
				err := f.a.reportStrandedTerminalNotice(context.Background(), f.s, "orphan-gen")
				if err == nil || !strings.Contains(err.Error(), "refuses foreign identity") {
					t.Fatal("foreign report did not reject identity", err)
				}
				return
			}
			err := f.a.dispatchTerminal(context.Background(), f.s, "orphan-gen")
			if len(f.e.projectionsFrom(system, "@alice:test.local")) != 0 {
				t.Fatal("unexpected incident")
			}
			if mode != "failed" && mode != "missing-room" {
				if err != nil {
					t.Fatal("observation failed instead of refusing report", err)
				}
				return
			}
			if err != nil || f.binding().NoticeReport == nil || f.binding().NoticeReport.Sent {
				t.Fatal("failure claimed success", err)
			}
			txn := f.binding().NoticeReport.Message.Txn
			before := f.e.hs.CallCount(sendRoute)
			if err := f.a.reportStrandedTerminalNotice(context.Background(), f.s, "orphan-gen"); err != nil {
				t.Fatal(err)
			}
			if f.e.hs.CallCount(sendRoute) != before {
				t.Fatal("retry ignored cooldown")
			}
			f.e.hs.mu.Lock()
			delete(f.e.hs.failures, sendRoute)
			f.e.hs.mu.Unlock()
			if mode == "missing-room" {
				system = f.e.ownReportRoom("alice")
			}
			f.now = f.now.Add(f.binding().RetryMax)
			f.dispatch()
			if len(f.e.projectionsFrom(system, "@alice:test.local")) != 1 || f.binding().NoticeReport.Message.Txn != txn {
				t.Fatal("retry lost transaction/report")
			}
		})
	}
}

// Starting from a persisted attempt reservation, actual Send types into a
// named idle pane; the callback changes the synthetic registry to shell. The
// real completion and dispatch paths retain mail and escalate the stranded
// draft. This does not re-test fresh admission or observe a live provider.
func TestTerminalStrandedTypingStatusFlipReports(t *testing.T) {
	isolateBindTest(t)
	for _, version := range []string{"2.1.309", "2.1.310"} {
		t.Run(version, func(t *testing.T) {
			f := newOrphanDispatchFixture(t)
			system := f.e.ownReportRoom("alice")
			id := f.mail()
			path, record := dispatchRegistry(t, f, "idle")
			record["version"] = version
			writeDispatchRegistry(t, path, record)
			f.control(map[string]any{"claude": true, "named": true})
			f.dispatch() // ordinary initial observation also acquires the own lease
			b := *f.binding()
			terminal := boundTerminal(b)
			sample := terminal.AssessReadiness(context.Background())
			if !sample.Eligible() {
				t.Fatal("idle named setup refused", sample)
			}
			f.update(func(st *config.State) {
				token, err := st.Notification.Reserve("terminal", b.SessionID, b.Generation, f.now, false)
				if err != nil {
					t.Fatal(err)
				}
				st.Terminal.NotificationToken = token
				st.Terminal.AttemptIDs = []string{id}
				st.Terminal.AttemptAt = f.now
				st.Terminal.NoticeCount = 1
				st.Terminal.Notice = tmuxwake.Notice(1)
				st.Terminal.Reservations = []time.Time{f.now}
			})
			expected := *f.binding()
			result, sendErr := terminal.Send(context.Background(), 1, sample.Key, nil, func() {
				f.wait(func(s orphanScreen) bool { return s.Text == tmuxwake.Notice(1) })
				record["status"] = "shell"
				record["statusUpdatedAt"] = 101
				writeDispatchRegistry(t, path, record)
			})
			if sendErr != nil || result.Disposition != tmuxwake.SendDraft {
				t.Fatal("did not reproduce post-type refusal", result, sendErr)
			}
			if err := f.a.finishTerminal(f.s, b.Generation, result, sendErr, false, expected); err != nil {
				t.Fatal(err)
			}
			f.step()
			requireNotice(t, f.binding().OwnNotice, "suspected")
			if f.binding().Outcome != "suppressed: pending attempt input" {
				t.Fatal("missing pending fence")
			}
			f.now = f.now.Add(10 * time.Minute)
			f.update(func(st *config.State) { st.Lease.Expires = f.now.Add(time.Minute) })
			f.dispatch()
			s := f.wait(func(s orphanScreen) bool { return true })
			if s.Text != tmuxwake.Notice(1) || len(s.Submitted) != 0 || s.Erased != 0 || len(f.e.projectionsFrom(system, "@alice:test.local")) != 1 || f.e.received("alice", id) || len(f.binding().AttemptIDs) != 1 {
				t.Fatal("stranded draft lost or not escalated", s)
			}
		})
	}
}
