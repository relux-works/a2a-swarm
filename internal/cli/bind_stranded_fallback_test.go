package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func strandedFallbackFixture(t *testing.T) (*env, *App, *session, string) {
	t.Helper()
	e, a, s, room := reportFixture(t)
	if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.OwnRoom = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	root := e.hs.inject(room, s.creds.UserID, map[string]any{"msgtype": "m.text", "body": "synthetic root"})
	notificationState(t, e, func(st *config.State) {
		st.Terminal.Host = "host-a"
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room, Lead: "@lead:remote.example", LeadRoom: room, LeadTopic: root}
	})
	return e, a, s, room
}

func strandedAlerts(e *env) []matrix.Event {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var events []matrix.Event
	for _, ev := range e.hs.events {
		if strings.HasPrefix(ev.Message().Body, "SUSPECTED-DEAF: identity=") {
			events = append(events, ev)
		}
	}
	return events
}

// Actual state-file reload and a new App preserve each audience's fence and
// transaction. Recovery rearms a second episode without repeating the first.
func TestStrandedFallbackEpisodes(t *testing.T) {
	e, a, s, _ := strandedFallbackFixture(t)
	ctx := context.Background()
	if err := a.reportStrandedTerminalNotice(ctx, s, "gen"); err != nil {
		t.Fatal(err)
	}
	pending := e.state("alice")
	if len(pending.MailAlarm.Jobs) != 2 || pending.Terminal.NoticeReport.Sent {
		t.Fatal("outbox/fence not persisted before send")
	}
	a2, s2 := notificationApp(t, e)
	a2.Now = a.Now
	a2.sendMailAlarm(ctx, s2, "gen")
	if len(strandedAlerts(e)) != 2 || !e.state("alice").Terminal.NoticeReport.Sent {
		t.Fatal("missing owner/lead delivery")
	}
	lead := 0
	for _, ev := range strandedAlerts(e) {
		h, ok := decodeHeader(ev.Message().A2A)
		if ok && len(h.To) == 1 && h.To[0] == "@lead:remote.example" {
			lead++
		}
	}
	if lead != 1 {
		t.Fatal("configured lead not addressed")
	}
	for i := 0; i < 3; i++ {
		_ = a2.reportStrandedTerminalNotice(ctx, s2, "gen")
		a2.sendMailAlarm(ctx, s2, "gen")
	}
	if len(strandedAlerts(e)) != 2 {
		t.Fatal("restart repeated episode")
	}
	b := *e.state("alice").Terminal
	if err := a2.observeTerminalNotice(s2, "gen", b, "empty"); err != nil {
		t.Fatal(err)
	}
	now := a.Now().Add(time.Hour)
	a2.Now = func() time.Time { return now }
	notificationState(t, e, func(st *config.State) {
		st.Terminal.OwnNotice = pending.Terminal.OwnNotice
		st.Terminal.OwnNotice.Since = now.Add(-10 * time.Minute).Format(time.RFC3339Nano)
		st.Terminal.OwnNotice.ObservedAt = now.Format(time.RFC3339Nano)
	})
	if err := a2.reportStrandedTerminalNotice(ctx, s2, "gen"); err != nil {
		t.Fatal(err)
	}
	a2.sendMailAlarm(ctx, s2, "gen")
	if len(strandedAlerts(e)) != 4 {
		t.Fatal("new episode not rearmed")
	}
	if e.state("alice").Liveness != nil {
		t.Fatal("alert invented liveness")
	}
}

// Per-audience receipts survive reload. Missing jobs, wrong transaction ACKs
// and a malformed outbox followed by a drain cannot establish delivery; the
// neighboring real successful sends establish both owner and lead ACKs.
func TestStrandedFallbackRequiresAudienceACK(t *testing.T) {
	for _, mode := range []string{"delivered", "missing-owner", "missing-lead", "wrong-ack", "corrupt-drained"} {
		t.Run(mode, func(t *testing.T) {
			e, a, session, _ := strandedFallbackFixture(t)
			if err := a.reportStrandedTerminalNotice(context.Background(), session, "gen"); err != nil {
				t.Fatal(err)
			}
			jobs := e.state("alice").MailAlarm.Jobs
			if mode == "corrupt-drained" {
				raw, err := os.ReadFile(session.paths.StateFile(session.name))
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["mail_alarm"] = json.RawMessage(`{"jobs":"invalid"}`)
				raw, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(session.paths.StateFile(session.name), raw, 0600); err != nil {
					t.Fatal(err)
				}
				if !e.state("alice").MailAlarm.Invalid {
					t.Fatal("corruption control did not invalidate outbox")
				}
				notificationState(t, e, func(st *config.State) {
					if err := advanceAlarm(&st.MailAlarm, alarmSample{At: a.Now()}, *st.MailAlarmConfig, nil, session.name, "host-a"); err != nil {
						t.Fatal(err)
					}
				})
			} else {
				for _, job := range jobs {
					r := e.state("alice").Terminal.NoticeReport
					if mode == "missing-owner" && job.ID == r.AlarmOwner || mode == "missing-lead" && job.ID == r.AlarmLead {
						continue
					}
					// Exercise the real sender for just this audience.
					notificationState(t, e, func(st *config.State) {
						for i := range st.MailAlarm.Jobs {
							st.MailAlarm.Jobs[i].Next = a.Now().Add(time.Hour)
							if st.MailAlarm.Jobs[i].ID == job.ID {
								st.MailAlarm.Jobs[i].Next = a.Now()
							}
						}
					})
					a.sendMailAlarm(context.Background(), session, "gen")
				}
			}
			notificationState(t, e, func(st *config.State) {
				st.MailAlarm.Jobs = nil
				if mode == "wrong-ack" {
					st.Terminal.NoticeReport.AlarmLeadAck = "another-transaction"
				}
				updateStrandedAlarmOutcome(st)
			})
			r := e.state("alice").Terminal.NoticeReport
			if mode == "delivered" {
				if !r.Sent || r.Outcome != "published via mail-alarm" || r.AlarmOwnerAck != r.AlarmOwner || r.AlarmLeadAck != r.AlarmLead || len(strandedAlerts(e)) != 2 {
					t.Fatal("real ACKs lost after reload", r)
				}
			} else if r.Sent || r.Outcome != "unavailable: alarm acknowledgement missing" {
				t.Fatal("job absence fabricated delivery", r)
			}
		})
	}
}

// An ordinary not-due cycle and unchanged queued/delivered outcomes do not
// replace state just to log the same outcome. A changed due outcome is
// the positive control and persists its logged transition before this check.
func TestStrandedReportUnchangedDoesNotSave(t *testing.T) {
	for _, mode := range []string{"not-due", "queued", "delivered"} {
		t.Run(mode, func(t *testing.T) {
			e, a, session, _ := strandedFallbackFixture(t)
			if mode == "not-due" {
				notificationState(t, e, func(st *config.State) {
					st.Terminal.OwnNotice.Since = a.Now().Add(-time.Minute).Format(time.RFC3339Nano)
				})
			} else {
				a.reportTerminalIncident(context.Background(), session, "gen")
				if mode == "delivered" {
					a.sendMailAlarm(context.Background(), session, "gen")
					a.reportTerminalIncident(context.Background(), session, "gen")
				}
				r := e.state("alice").Terminal.NoticeReport
				if r == nil || r.LoggedOutcome != r.Outcome {
					t.Fatal("transition control was not persisted")
				}
			}
			path := session.paths.StateFile(session.name)
			before, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			a.reportTerminalIncident(context.Background(), session, "gen")
			after, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || !bytes.Equal(raw, data) {
				t.Fatal("unchanged report rewrote state")
			}
		})
	}
}

// Exact threshold is the positive control for younger, clear, unknown, invalid,
// retired and foreign observations. Configured incidents never use fallback,
// including when the configured room refuses access.
func TestStrandedFallbackAdmission(t *testing.T) {
	for _, mode := range []string{"due", "below", "invalid-outbox", "clear", "unknown", "invalid", "foreign", "stopped", "generation", "incident", "incident-denied"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, room := strandedFallbackFixture(t)
			generation := "gen"
			notificationState(t, e, func(st *config.State) {
				switch mode {
				case "invalid-outbox":
					st.MailAlarm.Invalid = true
				case "below":
					st.Terminal.OwnNotice.Since = a.Now().Add(-10*time.Minute + time.Nanosecond).Format(time.RFC3339Nano)
				case "clear":
					no := false
					st.Terminal.OwnNotice = config.OwnNoticeObservation{PossiblyOrphaned: &no, ObservedAt: a.Now().Format(time.RFC3339Nano)}
				case "unknown":
					st.Terminal.OwnNotice.PossiblyOrphaned = nil
				case "invalid":
					st.Terminal.OwnNotice.Reason = "screen-content"
				case "stopped":
					st.Terminal.Stopped = true
				}
			})
			if mode == "generation" {
				generation = "old"
			}
			if mode == "foreign" {
				s.creds.UserID = "@other:test.local"
			}
			if strings.HasPrefix(mode, "incident") {
				if _, err := config.UpdateCredentials(e.paths(), "alice", func(c *config.Credentials) error { c.OwnRoom = room; return nil }); err != nil {
					t.Fatal(err)
				}
				if mode == "incident-denied" {
					e.hs.mu.Lock()
					e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"users_default":0,"events_default":50}`)
					e.hs.mu.Unlock()
				}
			}
			err := a.reportStrandedTerminalNotice(context.Background(), s, generation)
			if mode == "invalid-outbox" && (err == nil || err.Error() != "unavailable: invalid alarm state") {
				t.Fatal("wrong corrupt outbox rejection", err)
			}
			if mode == "foreign" && (err == nil || !strings.Contains(err.Error(), "refuses foreign identity")) {
				t.Fatal("wrong foreign rejection", err)
			}
			if mode == "incident-denied" && (err == nil || err.Error() != "unavailable: incident route") {
				t.Fatal("wrong access rejection", err)
			}
			jobs := len(e.state("alice").MailAlarm.Jobs)
			if mode == "due" {
				if err != nil || jobs != 2 {
					t.Fatal("due positive control", err, jobs)
				}
			} else if jobs != 0 {
				t.Fatal("forbidden fallback queued", mode)
			}
			if mode == "incident" && (err != nil || !e.state("alice").Terminal.NoticeReport.Sent) {
				t.Fatal("incident route lost", err)
			}
			if mode != "incident" && len(strandedAlerts(e)) != 0 {
				t.Fatal("admission performed a send")
			}
		})
	}
}

// A lost ACK after an applied send persists the same transaction/content;
// restart/backoff retries it without duplicate events. A definite rejection
// retries as well. No terminal fixtures or process probes are involved.
func TestStrandedFallbackRetry(t *testing.T) {
	for _, mode := range []string{"lost-ack", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := strandedFallbackFixture(t)
			notificationState(t, e, func(st *config.State) {
				st.MailAlarmConfig.Lead = ""
				st.MailAlarmConfig.LeadRoom = ""
				st.MailAlarmConfig.LeadTopic = ""
			})
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
				t.Fatal(err)
			}
			original := e.state("alice").MailAlarm.Jobs[0]
			base := s.client.HTTP.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			calls := 0
			s.client.HTTP = &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodPut {
					return base.RoundTrip(r)
				}
				calls++
				if mode == "rejected" {
					return nil, &matrix.Error{Status: 403, ErrCode: "M_FORBIDDEN", OriginalRequest: true}
				}
				resp, err := base.RoundTrip(r)
				if resp != nil {
					resp.Body.Close()
				}
				if err != nil {
					return nil, err
				}
				return nil, errors.New("synthetic lost ACK")
			})}
			a.sendMailAlarm(context.Background(), s, "gen")
			failed := e.state("alice").MailAlarm.Jobs[0]
			if failed.ID != original.ID || !failed.Next.After(a.Now()) || e.state("alice").Terminal.NoticeReport.Sent {
				t.Fatal("failure lost retry/fence")
			}
			a.sendMailAlarm(context.Background(), s, "gen")
			if calls != 1 {
				t.Fatal("backoff ignored")
			}
			if mode == "lost-ack" {
				// More than two ambiguous attempts must still retry this incident.
				for i := 0; i < 2; i++ {
					now := e.state("alice").MailAlarm.Jobs[0].Next
					a.Now = func() time.Time { return now }
					a.sendMailAlarm(context.Background(), s, "gen")
				}
				failed = e.state("alice").MailAlarm.Jobs[0]
				if calls != 3 || len(strandedAlerts(e)) != 1 || failed.ID != original.ID {
					t.Fatal("ambiguous retry duplicated or was held")
				}
			}
			restarted, session := notificationApp(t, e)
			now := failed.Next
			restarted.Now = func() time.Time { return now }
			restarted.sendMailAlarm(context.Background(), session, "gen")
			if len(strandedAlerts(e)) != 1 || len(e.state("alice").MailAlarm.Jobs) != 0 || !e.state("alice").Terminal.NoticeReport.Sent {
				t.Fatal("retry duplicated or lost alert")
			}
		})
	}
}

// Alert bytes come only from identity, host, since and a fixed reason, even
// with hostile notice/mail data. Corrupt outbox payloads refuse validation;
// the nearby unmodified job remains admissible.
func TestStrandedFallbackPayload(t *testing.T) {
	e, a, s, room := strandedFallbackFixture(t)
	notificationState(t, e, func(st *config.State) { st.Terminal.Notice = "SCREEN_SECRET" })
	mb := e.mailbox("alice")
	if _, err := mb.Append([]config.MailEntry{{ID: "$mail", Kind: config.MailEvent, Room: room, Rel: "to", TS: a.Now().UnixMilli(), Event: json.RawMessage(`{"content":{"body":"MAIL_SECRET"}}`)}}); err != nil {
		t.Fatal(err)
	}
	before := mb.Pending()
	if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
		t.Fatal(err)
	}
	job := e.state("alice").MailAlarm.Jobs[0]
	if !validAlarmJob(job, "alice") {
		t.Fatal("valid control refused")
	}
	want := "SUSPECTED-DEAF: identity=alice host=host-a since=" + job.Since + " reason=" + orphanReason
	if alarmBody(job) != want {
		t.Fatal("unexpected payload", alarmBody(job))
	}
	for _, mode := range []string{"reason", "since", "kind", "early", "count", "identity"} {
		bad := job
		switch mode {
		case "reason":
			bad.Reason = "SCREEN_SECRET"
		case "since":
			bad.Since = "MAIL_SECRET"
		case "kind":
			bad.Kind = "foreign"
		case "early":
			bad.At = bad.At.Add(-time.Nanosecond)
		case "count":
			bad.Count = 1
		case "identity":
			bad.Identity = "other"
		}
		if validAlarmJob(bad, "alice") {
			t.Fatal("invalid job accepted", mode)
		}
	}
	a.sendMailAlarm(context.Background(), s, "gen")
	for _, ev := range strandedAlerts(e) {
		if ev.Message().Body != want {
			t.Fatal("content crossed alert boundary", ev.Message().Body)
		}
	}
	if !reflect.DeepEqual(before, e.mailbox("alice").Pending()) {
		t.Fatal("alert changed mailbox")
	}
}

// Missing owner routing still alerts the lead once. Provisioning an owner
// later adds only that audience; hook pause cannot suppress incident delivery.
func TestStrandedFallbackLateOwner(t *testing.T) {
	e, a, s, room := strandedFallbackFixture(t)
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig.Owner = "auto"; st.HooksPaused = true })
	if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err == nil || err.Error() != "unavailable: alarm destination missing" {
		t.Fatal("missing owner not explicit", err)
	}
	a.sendMailAlarm(context.Background(), s, "gen")
	if len(strandedAlerts(e)) != 1 {
		t.Fatal("missing owner suppressed lead")
	}
	now := a.Now().Add(time.Minute)
	a.Now = func() time.Time { return now }
	_ = a.reportStrandedTerminalNotice(context.Background(), s, "gen")
	a.sendMailAlarm(context.Background(), s, "gen")
	if len(strandedAlerts(e)) != 1 {
		t.Fatal("lead repeated while owner missing")
	}
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig.Owner = room })
	now = now.Add(time.Minute)
	if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
		t.Fatal(err)
	}
	a.sendMailAlarm(context.Background(), s, "gen")
	if len(strandedAlerts(e)) != 2 || !e.state("alice").Terminal.NoticeReport.Sent {
		t.Fatal("late owner missing or lead duplicated")
	}
}

// Default routing uses the pinned active owner DM. Audience and pin changes
// refuse a queued send without consuming it; restoring both sends once.
func TestStrandedFallbackOwnerDM(t *testing.T) {
	e, a, s, room := strandedFallbackFixture(t)
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig = nil })
	f := ownerFiles(e.paths(), "alice")
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@owner:example.org"})
	ownerPut(t, f.path("json"), ownerDMConfig{V: 1, Generation: 1, Active: true, Owner: "@owner:example.org", Room: room, Render: "in-frame"})
	alarmOwnerAudience(t, e, room)
	if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
		t.Fatal(err)
	}
	e.hs.mu.Lock()
	e.hs.rooms[room].members["@other:test.local"] = "join"
	e.hs.mu.Unlock()
	a.sendMailAlarm(context.Background(), s, "gen")
	job := e.state("alice").MailAlarm.Jobs[0]
	if len(strandedAlerts(e)) != 0 || job.Failure != "audience" || job.Uncertain != 0 {
		t.Fatal("unsafe audience sent/lost job")
	}
	alarmOwnerAudience(t, e, room)
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@other:example.org"})
	now := job.Next
	a.Now = func() time.Time { return now }
	a.sendMailAlarm(context.Background(), s, "gen")
	job = e.state("alice").MailAlarm.Jobs[0]
	if len(strandedAlerts(e)) != 0 || job.Failure != "owner" {
		t.Fatal("changed owner pin sent")
	}
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@owner:example.org"})
	now = job.Next
	a.sendMailAlarm(context.Background(), s, "gen")
	if len(strandedAlerts(e)) != 1 {
		t.Fatal("safe owner control did not send")
	}
}

// Repeated route failures and a new App emit one diagnostic for that state;
// queue and delivered transitions each emit a new fixed diagnostic.
func TestStrandedReportTransitionLog(t *testing.T) {
	e, a, s, room := strandedFallbackFixture(t)
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: "off"} })
	var log bytes.Buffer
	a.Stderr = &log
	now := a.Now()
	a.Now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		a.reportTerminalIncident(context.Background(), s, "gen")
		now = now.Add(time.Minute)
	}
	restarted, session := notificationApp(t, e)
	restarted.Now = a.Now
	restarted.Stderr = &log
	restarted.reportTerminalIncident(context.Background(), session, "gen")
	if strings.Count(log.String(), "binding incident report:") != 1 {
		t.Fatal("per-retry/restart diagnostic spam", log.String())
	}
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig.Owner = room })
	now = now.Add(time.Minute)
	restarted.reportTerminalIncident(context.Background(), session, "gen")
	restarted.sendMailAlarm(context.Background(), session, "gen")
	restarted.reportTerminalIncident(context.Background(), session, "gen")
	if strings.Count(log.String(), "binding incident report:") != 3 || !strings.Contains(log.String(), "published via mail-alarm") {
		t.Fatal("missing transitions", log.String())
	}
}
