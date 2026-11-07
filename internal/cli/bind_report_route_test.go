package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Fixture metadata grants only an explicit own room; no membership is inferred.
func (e *env) ownReportRoom(name string) string {
	room := e.hs.addRoom("project-lane", "@"+name+":test.local")
	e.hs.mu.Lock()
	e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"events_default":0}`)
	e.hs.mu.Unlock()
	if _, err := config.UpdateCredentials(e.paths(), name, func(c *config.Credentials) error { c.OwnRoom = room; return nil }); err != nil {
		e.t.Fatal(err)
	}
	return room
}

func reportFixture(t *testing.T) (*env, *App, *session, string) {
	e := newEnv(t)
	e.agent("alice")
	room := e.ownReportRoom("alice")
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	var a *App
	e.configure = func(app *App) { a = app }
	e.as("alice", "whoami").ok(t)
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	yes := true
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "gen", Interval: time.Second, RetryMax: time.Second, OwnNotice: config.OwnNoticeObservation{PossiblyOrphaned: &yes, Reason: orphanReason, Since: now.Add(-10 * time.Minute).Format(time.RFC3339Nano), ObservedAt: now.Format(time.RFC3339Nano)}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, a, s, room
}

// A configured joined own-room sends once; absent, unjoined, forbidden and
// unknown routes send nothing and remain locally explicit. Global membership
// alone grants nothing. All services and credentials here are fixture-local.
func TestNoticeOwnRoomRoute(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"joined", "missing", "nonmember", "denied", "unknown", "global-only", "global-config", "missing-alias", "foreign-metadata"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, room := reportFixture(t)
			switch mode {
			case "missing", "global-only":
				if mode == "global-only" {
					e.hs.addRoom("system", s.creds.UserID)
				}
				if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.OwnRoom = ""; return nil }); err != nil {
					t.Fatal(err)
				}
			case "nonmember":
				e.hs.mu.Lock()
				delete(e.hs.rooms[room].members, s.creds.UserID)
				e.hs.mu.Unlock()
			case "denied":
				e.hs.mu.Lock()
				e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"events_default":50}`)
				e.hs.mu.Unlock()
			case "unknown":
				e.hs.mu.Lock()
				delete(e.hs.rooms[room].state, "m.room.power_levels|")
				e.hs.mu.Unlock()
			case "missing-alias":
				e.hs.mu.Lock()
				delete(e.hs.rooms[room].state, "m.room.canonical_alias|")
				e.hs.mu.Unlock()
			case "foreign-metadata":
				if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.UserID = "@other:test.local"; return nil }); err != nil {
					t.Fatal(err)
				}
			case "global-config":
				room = e.hs.addRoom("system", s.creds.UserID)
				e.hs.mu.Lock()
				e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"events_default":0}`)
				e.hs.mu.Unlock()
				if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.OwnRoom = room; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			// These negatives differ from a usable route only in canonical alias.
			// Restore that field to prove membership and power cannot mask refusal.
			if mode == "global-config" || mode == "missing-alias" {
				if _, err := s.strandedOwnRoom(context.Background(), ""); err == nil || err.Error() != "unavailable: incident route" {
					t.Fatal("alias guard did not refuse", err)
				}
				e.hs.mu.Lock()
				saved := e.hs.rooms[room].state["m.room.canonical_alias|"]
				e.hs.rooms[room].state["m.room.canonical_alias|"] = json.RawMessage(`{"alias":"#project-lane:test.local"}`)
				e.hs.mu.Unlock()
				if got, err := s.strandedOwnRoom(context.Background(), ""); err != nil || got != room {
					t.Fatal("alias positive control unusable", err)
				}
				e.hs.mu.Lock()
				if saved == nil {
					delete(e.hs.rooms[room].state, "m.room.canonical_alias|")
				} else {
					e.hs.rooms[room].state["m.room.canonical_alias|"] = saved
				}
				e.hs.mu.Unlock()
			}
			wantStatus := "unavailable: incident route"
			if mode == "joined" {
				wantStatus = "published"
			} else if mode == "missing" || mode == "global-only" {
				wantStatus = "unavailable: alarm destination missing"
			}
			err := a.reportStrandedTerminalNotice(context.Background(), s, "gen")
			b := e.state("alice").Terminal
			count := len(e.projectionsFrom(room, s.creds.UserID))
			if mode == "joined" {
				if err != nil || count != 1 || !b.NoticeReport.Sent {
					t.Fatal("missing own-room report", err, count)
				}
				if err = a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil || len(e.projectionsFrom(room, s.creds.UserID)) != 1 {
					t.Fatal("duplicate", err)
				}
				ev := e.projectionsFrom(room, s.creds.UserID)[0]
				var content struct {
					H Header `json:"ai.relux.a2a"`
				}
				if err = json.Unmarshal(ev.Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.H.Kind != "system" || content.H.Subtype != "incident" || content.H.Subject != "identity:alice" {
					t.Fatal("schema", content)
				}
			} else if err == nil || count != 0 || b.NoticeReport.Sent || e.hs.CallCount(sendRoute) != 0 || terminalNoticeReportStatus(b) == "published" {
				t.Fatal("unavailable route sent or claimed success", err, b.NoticeReport)
			}
			if terminalNoticeReportStatus(b) != wantStatus {
				t.Fatal("unexpected local status", terminalNoticeReportStatus(b), wantStatus)
			}
			if mode == "foreign-metadata" {
				e.as("alice", "bind", "status").failsWith(t, ExitError, "credential user_id does not match selected identity")
				return
			}
			if text := e.as("alice", "bind", "status").ok(t).stdout; !strings.Contains(text, "system-report="+wantStatus) {
				t.Fatal("text report status", text)
			}
			var status struct {
				Report string `json:"system_report"`
			}
			if err := json.Unmarshal([]byte(e.as("alice", "bind", "status", "--json").ok(t).stdout), &status); err != nil || status.Report != wantStatus {
				t.Fatal("JSON report status", err, status.Report)
			}
		})
	}
}

// An ambiguous applied send cannot be silently re-minted or rerouted when
// metadata changes or permission disappears. Restoring the original route
// reuses its immutable pin; deduplication here has the fake server's lifetime.
func TestNoticeOwnRoomAmbiguousPin(t *testing.T) {
	isolateBindTest(t)
	for _, mode := range []string{"changed", "missing", "denied"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, room := reportFixture(t)
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
				t.Fatal(err)
			}
			pin := *e.state("alice").Terminal.NoticeReport
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal.NoticeReport.Sent = false
				st.Terminal.NoticeReport.Outcome = "pending"
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "denied" {
				e.hs.mu.Lock()
				e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"events_default":50}`)
				e.hs.mu.Unlock()
			} else {
				next := ""
				if mode == "changed" {
					next = e.hs.addRoom("other", s.creds.UserID)
				}
				if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.OwnRoom = next; return nil }); err != nil {
					t.Fatal(err)
				}
			}
			before := e.hs.CallCount(sendRoute)
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err == nil {
				t.Fatal("route refusal missing")
			}
			r := e.state("alice").Terminal.NoticeReport
			if r.Sent || !reflect.DeepEqual(r.Message, pin.Message) || e.hs.CallCount(sendRoute) != before {
				t.Fatal("pin mutated/replayed", r)
			}
			wantStatus := "unavailable: pinned incident route changed"
			if mode == "missing" {
				wantStatus = "incident route not configured"
			} else if mode == "denied" {
				wantStatus = "unavailable: incident route"
			}
			if terminalNoticeReportStatus(e.state("alice").Terminal) != wantStatus {
				t.Fatal("wrong refusal status")
			}
			if text := e.as("alice", "bind", "status").ok(t).stdout; !strings.Contains(text, "system-report="+wantStatus) {
				t.Fatal("wrong text refusal status", text)
			}
			var status struct {
				Report string `json:"system_report"`
			}
			if err := json.Unmarshal([]byte(e.as("alice", "bind", "status", "--json").ok(t).stdout), &status); err != nil || status.Report != wantStatus {
				t.Fatal("wrong JSON refusal status", err, status.Report)
			}
			// Restore the original room without recreating the fake room or events.
			e.hs.mu.Lock()
			e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"events_default":0}`)
			e.hs.mu.Unlock()
			if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.OwnRoom = room; return nil }); err != nil {
				t.Fatal(err)
			}
			_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.NoticeReport.RetryAt = time.Time{}; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
				t.Fatal(err)
			}
			if !e.state("alice").Terminal.NoticeReport.Sent || e.hs.CallCount(sendRoute) != before+1 || len(e.projectionsFrom(room, s.creds.UserID)) != 1 || !reflect.DeepEqual(e.state("alice").Terminal.NoticeReport.Message, pin.Message) {
				t.Fatal("retry changed immutable pin")
			}
		})
	}
}

// Direct helper calls preserve suspicion/outcome on successful and failed reports
// and return no consumer error. Dispatch/runTerminal wiring is inspected statically;
// this test cannot detect a caller bypassing the helper. No terminal/process probe.
func TestNoticeReportCompletionIsolation(t *testing.T) {
	isolateBindTest(t)
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[failed], func(t *testing.T) {
			e, a, s, _ := reportFixture(t)
			before := e.state("alice").Terminal.OwnNotice
			if failed {
				e.hs.fail(sendRoute, 503, "M_UNKNOWN", "fixture failure")
			}
			if err := a.completeTerminalObservation(context.Background(), s, "gen"); err != nil {
				t.Fatal("report escaped as consumer error", err)
			}
			b := e.state("alice").Terminal
			if !reflect.DeepEqual(before, b.OwnNotice) || strings.Contains(b.Outcome, "consumer unavailable") || b.NoticeReport.Sent == failed {
				t.Fatal("report degraded consumer", b)
			}
		})
	}
}

// First observation and actual persisted SessionStart keep retained partial/untyped
// input unknown; typed and empty neighbours yield suspicion and clear. This does
// not exercise manual bind's journal-cloning/process boundary or terminal input.
func TestNoticeRetainedAttemptUncertainty(t *testing.T) {
	isolateBindTest(t)
	for _, edge := range []string{"first", "session-start"} {
		for _, mode := range []string{"partial", "untyped", "untyped-receipted", "typed", "empty", "no-attempt"} {
			t.Run(edge+"/"+mode, func(t *testing.T) {
				e, a, s, _ := reportFixture(t)
				b := *e.state("alice").Terminal
				b.OwnNotice = config.OwnNoticeObservation{}
				b.NoticeReport = nil
				b.Typed = mode != "untyped" && mode != "untyped-receipted"
				b.NoticeCount = 1
				b.Notice = tmuxwake.Notice(1)
				b.AttemptIDs = []string{"mail"}
				if mode == "partial" {
					b.NoticeCount = 2
					b.Notice = tmuxwake.Notice(2)
				}
				// Dispatch prunes receipted IDs but retains Notice until settlement or
				// empty typed reconciliation: no IDs alone does not prove settlement.
				if mode == "untyped-receipted" {
					b.AttemptIDs = nil
				}
				if mode == "no-attempt" {
					b.Typed = false
					b.Notice = ""
					b.NoticeCount = 0
					b.AttemptIDs = nil
				}
				_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = &b; return nil })
				if err != nil {
					t.Fatal(err)
				}
				if edge == "session-start" {
					b.Host = machineLabel()
					b.Target.OwnerPID = b.OwnerPID
					b.Target.OwnerBirth = b.OwnerBirth
					_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = &b; return nil })
					if err != nil {
						t.Fatal(err)
					}
					st, err := a.handoverTerminalSession(s, &b, b.Target, "next")
					if err != nil {
						t.Fatal(err)
					}
					b = *st.Terminal
				}
				input := "nonempty"
				want := "unknown"
				if mode == "typed" {
					want = "suspected"
				}
				if mode == "empty" {
					input = "empty"
					want = "clear"
				}
				if mode == "no-attempt" {
					want = "clear"
				}
				if err := a.observeTerminalNotice(s, "gen", b, input); err != nil {
					t.Fatal(err)
				}
				requireNotice(t, e.state("alice").Terminal.OwnNotice, want)
				if e.hs.CallCount(sendRoute) != 0 || e.state("alice").Terminal.NotificationToken != "" {
					t.Fatal("observation granted send authority")
				}
			})
		}
	}
}

// The closed status vocabulary includes idle, pending, published and invalid.
func TestNoticeReportStatusVocabulary(t *testing.T) {
	isolateBindTest(t)
	for _, status := range []string{"pending", "published", "invalid", "incident route not configured", "unavailable: incident route", "unavailable: pinned incident route changed", "unavailable: system incident report failed"} {
		b := &config.TerminalBinding{NoticeReport: &config.NoticeReport{Outcome: status}}
		want := status
		if status == "invalid" {
			want = "unavailable: invalid report state"
		}
		if terminalNoticeReportStatus(b) != want {
			t.Fatal(status)
		}
	}
	if terminalNoticeReportStatus(nil) != "not due" {
		t.Fatal("unbound")
	}
	b := &config.TerminalBinding{NoticeReport: &config.NoticeReport{Since: "old", Outcome: "published"}}
	if terminalNoticeReportStatus(b) != "not due" {
		t.Fatal("stale episode")
	}
}

// Exact threshold age respects the accepted lowercase/fraction grammar; a
// just-before observation cannot send, and content preparation records now.
func TestNoticeReportThresholdGrammar(t *testing.T) {
	isolateBindTest(t)
	for _, since := range []string{"2026-11-01t11:50:00z", "2026-11-01T11:50:00.0000000001Z", "2026-11-01T11:49:59.9999999999Z"} {
		t.Run(since, func(t *testing.T) {
			e, a, s, room := reportFixture(t)
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal.OwnNotice.Since = since; return nil })
			if err != nil {
				t.Fatal(err)
			}
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
				t.Fatal(err)
			}
			want := 1
			if strings.Contains(since, "50:00.") {
				want = 0
			}
			if len(e.projectionsFrom(room, s.creds.UserID)) != want {
				t.Fatal("threshold mismatch", since)
			}
			if want == 1 {
				var content struct {
					H Header `json:"ai.relux.a2a"`
				}
				if err := json.Unmarshal(e.projectionsFrom(room, s.creds.UserID)[0].Content, &content); err != nil {
					t.Fatal(err)
				}
				if content.H.At != a.Now().UTC().Format(time.RFC3339Nano) {
					t.Fatal("header time")
				}
			}
		})
	}
}

// An existing canonical-alias event with content {} is unavailable locally and
// sends nothing. Restoring just its alias permits one fake-server-applied report.
// This covers an empty event, independently of the missing-event error path.
func TestNoticeOwnRoomEmptyAlias(t *testing.T) {
	isolateBindTest(t)
	t.Run("empty-content", func(t *testing.T) {
		e, a, s, room := reportFixture(t)
		e.hs.mu.Lock()
		e.hs.rooms[room].state["m.room.canonical_alias|"] = json.RawMessage(`{}`)
		e.hs.mu.Unlock()
		err := a.reportStrandedTerminalNotice(context.Background(), s, "gen")
		b := e.state("alice").Terminal
		if err == nil || err.Error() != "unavailable: incident route" || b.NoticeReport == nil || b.NoticeReport.Sent || terminalNoticeReportStatus(b) != "unavailable: incident route" || e.hs.CallCount(sendRoute) != 0 || len(e.projectionsFrom(room, s.creds.UserID)) != 0 {
			t.Fatal("empty alias must refuse locally without remote sends")
		}
		pin := b.NoticeReport.Message.Txn
		e.hs.mu.Lock()
		e.hs.rooms[room].state["m.room.canonical_alias|"] = json.RawMessage(`{"alias":"#project-lane:test.local"}`)
		e.hs.mu.Unlock()
		// Advance through the ordinary retry cap rather than removing the fence.
		now := b.NoticeReport.RetryAt
		a.Now = func() time.Time { return now }
		if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
			t.Fatal("restored alias remains unusable", err)
		}
		b = e.state("alice").Terminal
		if !b.NoticeReport.Sent || b.NoticeReport.Message.Txn != pin || terminalNoticeReportStatus(b) != "published" || e.hs.CallCount(sendRoute) != 1 || len(e.projectionsFrom(room, s.creds.UserID)) != 1 {
			t.Fatal("restored alias must publish once with the existing transaction")
		}
	})
}
