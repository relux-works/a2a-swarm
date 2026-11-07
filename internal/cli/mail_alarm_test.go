package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func alarmFixture(t *testing.T) (*env, *App, *session, string, alarmSample) {
	t.Helper()
	e, room := world(t)
	a, s := notificationApp(t, e)
	root := e.hs.inject(room, "@alice:"+testServer, map[string]any{"msgtype": "m.text", "body": "alarm topic root"})
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	a.Now = func() time.Time { return now }
	notificationState(t, e, func(st *config.State) {
		st.Terminal = &config.TerminalBinding{Generation: "alarm"}
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room, Operator: "@operator:remote.example", Room: room, Topic: root}
	})
	return e, a, s, room, alarmSample{Count: 1, Oldest: now.Add(-time.Hour), At: now, Reason: string(tmuxwake.RuntimeNone)}
}
func primeAlarm(t *testing.T, e *env, a *App, s *session, sample alarmSample) {
	t.Helper()
	observeAlarmWindow(a, s, sample)
	if len(e.state("alice").MailAlarm.Jobs) != 2 {
		t.Fatal("stale idle alarm did not queue both destinations")
	}
}

// Supply continuous minute-spaced observations, ending at the fixture clock.
func observeAlarmWindow(a *App, s *session, sample alarmSample) {
	end := sample.At
	for at := end.Add(-20 * time.Minute); !at.After(end); at = at.Add(time.Minute) {
		sample.At = at
		a.observeMailAlarm(s, "alarm", sample)
	}
}

func alarmEvents(e *env) []string {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var bodies []string
	for _, ev := range e.hs.events {
		if strings.HasPrefix(ev.Message().Body, "Stale addressed mailbox:") {
			bodies = append(bodies, ev.Message().Body)
		}
	}
	return bodies
}

// Both targets get one metadata-only alert; cycles and a new App cannot repeat it.
// A drained episode followed by new stale mail is the independent rearm control.
func TestMailAlarmDedupeAndRearm(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if got := len(alarmEvents(e)); got != 2 {
		t.Fatalf("want two destination alerts, got %d", got)
	}
	e.hs.mu.Lock()
	operatorNotices := 0
	for _, ev := range e.hs.events {
		h, ok := decodeHeader(ev.Message().A2A)
		if ok && h.Topic == e.state("alice").MailAlarmConfig.Topic && slices.Contains(h.To, "@operator:remote.example") {
			operatorNotices++
		}
	}
	e.hs.mu.Unlock()
	if operatorNotices != 1 {
		t.Fatal("operator target or configured topic missing")
	}
	for _, body := range alarmEvents(e) {
		for _, field := range []string{"identity=alice", "count=1", "oldest_age_minutes=60", "readiness_reason=none", "host="} {
			if !strings.Contains(body, field) {
				t.Fatalf("missing %s: %s", field, body)
			}
		}
	}
	for i := 0; i < 3; i++ {
		a.observeMailAlarm(s, "alarm", sample)
		a.sendMailAlarm(context.Background(), s, "alarm")
	}
	a2, s2 := notificationApp(t, e)
	a2.Now = a.Now
	a2.observeMailAlarm(s2, "alarm", sample)
	a2.sendMailAlarm(context.Background(), s2, "alarm")
	if len(alarmEvents(e)) != 2 {
		t.Fatal("dedupe gate allowed repeat")
	}
	drained := sample
	drained.Count = 0
	a2.observeMailAlarm(s2, "alarm", drained)
	a2.observeMailAlarm(s2, "alarm", sample)
	a2.sendMailAlarm(context.Background(), s2, "alarm")
	if len(alarmEvents(e)) != 4 {
		t.Fatal("drained then stale did not rearm")
	}
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig.Minutes = 1440 })
	a2.observeMailAlarm(s2, "alarm", sample)
	if !e.state("alice").MailAlarm.Active {
		t.Fatal("config change cleared episode")
	}
	if e.state("alice").MailAlarm.LastAlert.IsZero() {
		t.Fatal("last alert not recorded")
	}
}

// Fresh mail, explicit runtime busy and busy/dialog input cannot alert; unknown
// readiness and a nonempty stranded composer remain observable alarm candidates.
func TestMailAlarmIdleRules(t *testing.T) {
	for _, mode := range []string{"fresh", "busy", "dialog", "unavailable", "unqualified", "stranded"} {
		t.Run(mode, func(t *testing.T) {
			_, _, _, _, sample := alarmFixture(t)
			c := config.MailAlarmConfig{Minutes: 20, Owner: "auto"}
			state := config.MailAlarm{IdleSince: sample.At.Add(-time.Hour), LastObserved: sample.At.Add(-time.Minute)}
			r := tmuxwake.ReadinessAssessment{Runtime: tmuxwake.RuntimeAssessment{Reason: tmuxwake.RuntimeNone}}
			want := false
			switch mode {
			case "fresh":
				sample.Oldest = sample.At.Add(-19 * time.Minute)
			case "busy":
				r.Runtime.Reason = tmuxwake.RuntimeBusy
			case "dialog":
				r.Input.Reason = tmuxwake.InputReasonBusyOrDialog
			case "unavailable":
				r.Runtime.Reason = tmuxwake.RuntimeRegistryUnavailable
				want = true
			case "unqualified":
				r.Runtime.Reason = tmuxwake.RuntimeVersionUnqualified
				want = true
			case "stranded":
				r.Input.State = tmuxwake.InputNonempty
				want = true
			}
			sample = alarmObservation(sample, r)
			if err := advanceAlarm(&state, sample, c, []config.MailAlarmJob{{Room: "!room:example.org"}}, "alice", "host"); err != nil {
				t.Fatal(err)
			}
			if state.Active != want {
				t.Fatalf("alarm active=%t, want %t", state.Active, want)
			}
		})
	}
}

// Only rel=to contributes; hostile bodies/subjects never enter the summary.
// Observer and sender preserve mailbox bytes, shown flags and receipt state.
func TestMailAlarmMailboxIsolation(t *testing.T) {
	shim := t.TempDir()
	marker := filepath.Join(shim, "pane-touched")
	if err := os.WriteFile(filepath.Join(shim, "tmux"), []byte("#!/bin/sh\necho called > '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Error("alarm invoked tmux")
		}
	})

	e, a, s, room, sample := alarmFixture(t)
	mb := e.mailbox("alice")
	var entries []config.MailEntry
	for _, rel := range []string{"to", "thread", "sub", "mention"} {
		entries = append(entries, config.MailEntry{ID: "$" + rel, Kind: config.MailEvent, Room: room, Rel: rel, TS: sample.Oldest.UnixMilli(), Event: json.RawMessage(`{"content":{"body":"hostile-secret-token","subject":"peer-path-secret"}}`)})
	}
	if _, err := mb.Append(entries); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.paths().MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	aggregated := alarmSample{At: sample.At, Reason: sample.Reason}
	for _, entry := range mb.Pending() {
		aggregated.add(entry)
	}
	if aggregated.Count != 1 {
		t.Fatalf("non-addressed mail counted: %d", aggregated.Count)
	}
	primeAlarm(t, e, a, s, aggregated)
	a.sendMailAlarm(context.Background(), s, "alarm")
	after, err := os.ReadFile(e.paths().MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("alarm changed mailbox")
	}
	for _, body := range alarmEvents(e) {
		if strings.Contains(body, "secret") || strings.Contains(body, "peer-path") {
			t.Fatal("peer data leaked")
		}
	}
	for _, entry := range e.mailbox("alice").Pending() {
		if entry.Shown || e.received("alice", entry.ID) {
			t.Fatal("alarm shown/receipted mail")
		}
	}
}

// No destination means no outbox or sends; active owner DM supplies the default.
func TestMailAlarmDestinations(t *testing.T) {
	e, a, s, room, sample := alarmFixture(t)
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig = nil })
	a.observeMailAlarm(s, "alarm", sample)
	sample.At = sample.At.Add(time.Hour)
	a.Now = func() time.Time { return sample.At }
	a.observeMailAlarm(s, "alarm", sample)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if alarmStatus(s, e.state("alice")).State != "disabled" || len(alarmEvents(e)) != 0 {
		t.Fatal("missing destinations did not disable alarm")
	}
	ownerPut(t, ownerFiles(e.paths(), "alice").path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@owner:example.org"})
	ownerPut(t, ownerFiles(e.paths(), "alice").path("json"), ownerDMConfig{V: 1, Generation: 1, Active: true, Owner: "@owner:example.org", Room: room, Render: "in-frame"})
	if alarmStatus(s, e.state("alice")).State != "armed" {
		t.Fatal("active owner DM not selected")
	}
	alarmOwnerAudience(t, e, room)
	observeAlarmWindow(a, s, sample)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if len(alarmEvents(e)) != 1 {
		t.Fatal("owner DM default missing")
	}
}

type alarmTransport func(*http.Request) (*http.Response, error)

func (f alarmTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A transport failure preserves pending state and backs off without touching
// delivery. The topic GET succeeds once; both actual PUTs fail twice across
// an App restart, reuse resolution, and then remain held without more PUTs.
func TestMailAlarmFailureBackoff(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	calls := 0
	resolutions := 0
	base := s.client.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	s.client.HTTP = &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut {
			resolutions++
			return base.RoundTrip(r)
		}
		calls++
		return nil, errors.New("synthetic offline")
	})}
	a.sendMailAlarm(context.Background(), s, "alarm")
	first := calls
	if first != 2 || resolutions != 1 {
		t.Fatalf("send calls=%d", first)
	}
	st := e.state("alice")
	if len(st.MailAlarm.Jobs) != 2 {
		t.Fatal("lost jobs")
	}
	for _, job := range st.MailAlarm.Jobs {
		if job.Uncertain != 1 || !job.Next.After(a.Now()) || job.ResolvedRoom == "" {
			t.Fatal("failure lost pending/backoff")
		}
	}
	a.sendMailAlarm(context.Background(), s, "alarm")
	if calls != first || resolutions != 1 {
		t.Fatal("backoff ignored")
	}
	later := a.Now().Add(6 * time.Minute)
	restarted, restartedSession := notificationApp(t, e)
	restartedSession.client.HTTP = s.client.HTTP
	a, s = restarted, restartedSession
	a.Now = func() time.Time { return later }
	a.sendMailAlarm(context.Background(), s, "alarm")
	later = later.Add(6 * time.Minute)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if calls != 4 || resolutions != 1 || alarmStatus(s, e.state("alice")).Held != 2 {
		t.Fatal("uncertain send exceeded single retry")
	}
	// Delivery's ordinary state update remains usable while alerts are held.
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.NextBatch = "delivery-continues"; return nil }); err != nil {
		t.Fatal(err)
	}
}

// CLI accepts per-identity remote targets and named rooms; every invalid input
// leaves the complete identity state byte-for-byte unchanged.
func TestMailAlarmConfig(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "mail-alarm", "set", "--owner", "Escalations", "--operator", "@operator:remote.example", "--room", "operations", "--topic", "alarms", "--minutes", "7", "--lead", "lead", "--lead-room", "operations", "--lead-topic", "incidents").ok(t)
	st := e.state("alice")
	if st.MailAlarmConfig == nil || st.MailAlarmConfig.Owner != "Escalations" || st.MailAlarmConfig.Minutes != 7 || st.MailAlarmConfig.Lead != "lead" || st.MailAlarmConfig.LeadRoom != "operations" || st.MailAlarmConfig.LeadTopic != "incidents" {
		t.Fatal("config not saved")
	}
	if e.state("bob").MailAlarmConfig != nil {
		t.Fatal("config crossed identities")
	}
	before, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"--lead", "lead"}, {"--lead-room", "operations", "--lead-topic", "incidents"}, {"--lead", "all", "--lead-room", "operations", "--lead-topic", "incidents"}, {"--lead", "lead", "--lead-room", "bad\nroom", "--lead-topic", "incidents"}, {"--minutes", "0"}, {"--minutes", "1441"}, {"--operator", "all", "--room", "ops", "--topic", "alarm"}, {"--operator", "@a:bad host", "--room", "ops", "--topic", "alarm"}, {"--owner", "!"}, {"--operator", "agent"}, {"--owner", "room\nsecret"}} {
		r := e.as("alice", append([]string{"mail-alarm", "set"}, args...)...)
		if r.code == 0 {
			t.Fatalf("invalid accepted: %v", args)
		}
		after, _ := os.ReadFile(e.paths().StateFile("alice"))
		if string(before) != string(after) {
			t.Fatal("invalid config wrote state")
		}
	}
	e.as("alice", "mail-alarm", "status", "--json").ok(t)
	e.as("alice", "mail-alarm", "unset").ok(t)
	if e.state("alice").MailAlarmConfig != nil {
		t.Fatal("unset retained override")
	}
}

// Old files default cleanly; malformed alarm fields cannot break State loading
// or erase the corrupt-state dedupe fence before an observed drain.
func TestMailAlarmOldAndCorruptState(t *testing.T) {
	for _, raw := range []string{`{}`, `{"mail_alarm_config":"broken","mail_alarm":{"active":"broken"}}`, `{"mail_alarm":{"last_alert":"bad-time"}}`} {
		var st config.State
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		if raw == `{}` {
			if st.MailAlarm.Active {
				t.Fatal("old state armed outstanding episode")
			}
			continue
		}
		if !st.MailAlarm.Invalid || !st.MailAlarm.Active {
			t.Fatal("corrupt state erased fence")
		}
		before := st.MailAlarm
		sample := alarmSample{Count: 1, Oldest: time.Now().Add(-time.Hour), At: time.Now()}
		_ = advanceAlarm(&st.MailAlarm, sample, config.MailAlarmConfig{Minutes: 20}, nil, "alice", "host")
		if !st.MailAlarm.Active || !st.MailAlarm.Invalid {
			t.Fatal("corrupt fence not held")
		}
		sample.Count = 0
		_ = advanceAlarm(&st.MailAlarm, sample, config.MailAlarmConfig{Minutes: 20}, nil, "alice", "host")
		if st.MailAlarm.Active || st.MailAlarm.Invalid || reflect.DeepEqual(before, st.MailAlarm) {
			t.Fatal("drain did not recover corrupt fence")
		}
	}
}

// A blocked sender cannot hold the delivery/state lock: a mailbox consumer
// callback and receipt-free read finish before the network attempt is released.
func TestMailAlarmWorkerDoesNotBlockDelivery(t *testing.T) {
	e, a, s, room, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	if _, err := e.mailbox("alice").Append([]config.MailEntry{{ID: "$delivery", Kind: config.MailEvent, Room: room, Rel: "to", TS: sample.Oldest.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	entered, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan struct{})
	s.client.HTTP = &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return nil, errors.New("offline")
	})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := make(chan struct{}, 1)
	go func() { defer close(done); a.runMailAlarm(ctx, s, "alarm", wake) }()
	wake <- struct{}{}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not enter sender")
	}
	delivery := make(chan error, 1)
	go func() {
		_, err := s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
			if len(mb.Pending()) != 1 {
				return nil, errors.New("delivery lost pending mail")
			}
			return nil, nil
		})
		delivery <- err
	}()
	select {
	case err := <-delivery:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sender delayed delivery")
	}
	close(release)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop")
	}
	if e.received("alice", "$delivery") {
		t.Fatal("alarm receipted delivery")
	}
}

// Corrupt alarm fields still permit an ordinary delivery transaction and do
// not turn invalid peer-like readiness data into alert content.
func TestMailAlarmCorruptionDoesNotBlockDelivery(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	st := e.state("alice")
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["mail_alarm_config"] = json.RawMessage(`"bad"`)
	fields["mail_alarm"] = json.RawMessage(`{"active":"bad"}`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(e.paths().StateFile("alice"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	a.observeMailAlarm(s, "alarm", sample)
	a.sendMailAlarm(context.Background(), s, "alarm")
	if _, err := s.lockedDeliver(context.Background(), time.Now().Add(time.Second), false, func(_ *config.State, _ *config.Mailbox) ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("alarm corruption broke delivery: %v", err)
	}
	r := alarmObservation(sample, tmuxwake.ReadinessAssessment{Runtime: tmuxwake.RuntimeAssessment{Reason: "peer secret"}})
	if strings.Contains(r.Reason, "peer") {
		t.Fatal("untrusted readiness reason leaked")
	}
}

// Eight actual send PUTs receive 429 across four rounds; the single topic GET
// succeeds and is cached. Neither destination spends uncertainty or becomes held.
func TestMailAlarmKnownRejectionRetries(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	calls, resolutions := 0, 0
	base := s.client.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	s.client.HTTP = &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPut {
			resolutions++
			return base.RoundTrip(r)
		}
		calls++
		body := `{"errcode":"M_LIMIT_EXCEEDED","error":"synthetic rejection"}`
		return &http.Response{StatusCode: 429, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
	})}
	now := a.Now()
	a.Now = func() time.Time { return now }
	for i := 0; i < 4; i++ {
		a.sendMailAlarm(context.Background(), s, "alarm")
		now = now.Add(6 * time.Minute)
	}
	if calls != 8 || resolutions != 1 {
		t.Fatalf("known rejection stopped retrying: calls=%d", calls)
	}
	st := e.state("alice")
	if len(st.MailAlarm.Jobs) != 2 || alarmStatus(s, st).Held != 0 || alarmStatus(s, st).State != "error" || alarmStatus(s, st).Reason != "alarm destination rejected delivery" {
		t.Fatal("known rejection lost or held pending alert")
	}
	for _, job := range st.MailAlarm.Jobs {
		if job.Delay > 5*time.Minute || job.Uncertain != 0 {
			t.Fatal("backoff/uncertainty outside bounds")
		}
	}
}

// Bind JSON exposes default disabled policy even when unbound, without a live
// readiness/pane probe; configured policy and CLI show report the same state.
func TestMailAlarmBindStatus(t *testing.T) {
	e, _ := world(t)
	r := e.as("alice", "bind", "status", "--json").ok(t)
	var out struct {
		Alarm mailAlarmStatus `json:"mail_alarm"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Alarm.State != "disabled" || out.Alarm.Threshold != 20 || out.Alarm.Outstanding {
		t.Fatal("missing default alarm diagnostic")
	}
	e.as("alice", "mail-alarm", "set", "--owner", "Escalations").ok(t)
	r = e.as("alice", "bind", "status", "--json").ok(t)
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil {
		t.Fatal(err)
	}
	if out.Alarm.State != "armed" {
		t.Fatal("configured alarm not armed in bind status")
	}
	e.as("alice", "mail-alarm", "show").ok(t)
}

// An active-looking owner config with a foreign pin never selects a default
// destination; changing the pin also pauses an already queued owner job.
func TestMailAlarmOwnerPin(t *testing.T) {
	e, a, s, room, sample := alarmFixture(t)
	notificationState(t, e, func(st *config.State) { st.MailAlarmConfig = nil })
	f := ownerFiles(e.paths(), "alice")
	ownerPut(t, f.path("json"), ownerDMConfig{V: 1, Generation: 1, Active: true, Owner: "@owner:example.org", Room: room, Render: "in-frame"})
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@other:example.org"})
	if alarmStatus(s, e.state("alice")).State != "disabled" {
		t.Fatal("foreign pin selected owner DM")
	}
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@owner:example.org"})
	a.observeMailAlarm(s, "alarm", sample)
	sample.At = sample.At.Add(time.Hour)
	a.Now = func() time.Time { return sample.At }
	observeAlarmWindow(a, s, sample)
	if len(e.state("alice").MailAlarm.Jobs) != 1 {
		t.Fatal("matching owner pin failed positive control")
	}
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@other:example.org"})
	// Keep operator enabled so this checks the per-job owner gate too.
	notificationState(t, e, func(st *config.State) {
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: "auto", Operator: "operator", Room: room, Topic: "$root"}
	})
	a.sendMailAlarm(context.Background(), s, "alarm")
	if len(alarmEvents(e)) != 0 || len(e.state("alice").MailAlarm.Jobs) != 1 {
		t.Fatal("changed owner pin sent or lost queued alert")
	}
}

func alarmOwnerAudience(t *testing.T, e *env, room string) {
	t.Helper()
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	r := e.hs.rooms[room]
	r.members = map[string]string{"@alice:" + testServer: "join", "@owner:example.org": "join"}
	r.state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"invite"}`)
	r.state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"shared"}`)
}

// Each unsafe audience change after queueing sends nothing, preserves the job
// without uncertainty, and reports a fixed error. Restoring the audience sends.
func TestMailAlarmOwnerAudience(t *testing.T) {
	for _, mode := range []string{"third-member", "public", "owner-left"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, room, sample := alarmFixture(t)
			alarmOwnerAudience(t, e, room)
			f := ownerFiles(e.paths(), "alice")
			ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@owner:example.org"})
			ownerPut(t, f.path("json"), ownerDMConfig{V: 1, Generation: 1, Active: true, Owner: "@owner:example.org", Room: room, Render: "in-frame"})
			notificationState(t, e, func(st *config.State) { st.MailAlarmConfig = nil })
			observeAlarmWindow(a, s, sample)
			if len(e.state("alice").MailAlarm.Jobs) != 1 {
				t.Fatal("owner job not queued")
			}
			e.hs.mu.Lock()
			switch mode {
			case "third-member":
				e.hs.rooms[room].members["@carol:"+testServer] = "join"
			case "public":
				e.hs.rooms[room].state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"public"}`)
			case "owner-left":
				e.hs.rooms[room].members["@owner:example.org"] = "leave"
			}
			e.hs.mu.Unlock()
			a.sendMailAlarm(context.Background(), s, "alarm")
			st := e.state("alice")
			if len(alarmEvents(e)) != 0 || len(st.MailAlarm.Jobs) != 1 || st.MailAlarm.Jobs[0].Uncertain != 0 || alarmStatus(s, st).Reason != "owner audience check failed" || alarmStatus(s, st).State != "error" {
				t.Fatal("unsafe audience sent, lost job, or lacked diagnostic")
			}
			alarmOwnerAudience(t, e, room)
			later := a.Now().Add(6 * time.Minute)
			a.Now = func() time.Time { return later }
			a.sendMailAlarm(context.Background(), s, "alarm")
			if len(alarmEvents(e)) != 1 || len(e.state("alice").MailAlarm.Jobs) != 0 || alarmStatus(s, e.state("alice")).State != "armed" {
				t.Fatal("restored audience failed positive control")
			}
		})
	}
}

// Opaque IDs are accepted; a corrupt queued destination is visible as an error
// and cannot send. A hostile hostname is sanitized before becoming a job.
func TestMailAlarmInvalidJobAndHost(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	for _, room := range []string{"!opaque", "!with:server"} {
		if !alarmRoom(room) {
			t.Fatal("opaque ID rejected")
		}
	}
	var state config.MailAlarm
	c := config.MailAlarmConfig{Minutes: 1, Owner: "!opaque"}
	for i := 0; i <= 1; i++ {
		at := sample
		at.At = sample.At.Add(time.Duration(i) * time.Minute)
		if err := advanceAlarm(&state, at, c, []config.MailAlarmJob{{Room: "!opaque"}}, "alice", "host with\nsecret"); err != nil {
			t.Fatal(err)
		}
	}
	if len(state.Jobs) != 1 || state.Jobs[0].Host != "unknown-host" {
		t.Fatal("host not sanitized at queue time")
	}
	primeAlarm(t, e, a, s, sample)
	notificationState(t, e, func(st *config.State) {
		for i := range st.MailAlarm.Jobs {
			st.MailAlarm.Jobs[i].Room = "!bad room"
		}
	})
	a.sendMailAlarm(context.Background(), s, "alarm")
	status := alarmStatus(s, e.state("alice"))
	if len(alarmEvents(e)) != 0 || status.State != "error" || status.Reason != "invalid queued alarm" {
		t.Fatal("invalid job silently armed or sent")
	}
}

// A gap discards idle credit; fresh unavailable observations must accumulate
// a whole threshold again, after which the deaf lane still queues an alert.
func TestMailAlarmObservationGap(t *testing.T) {
	_, _, _, _, sample := alarmFixture(t)
	state := config.MailAlarm{IdleSince: sample.At.Add(-time.Hour), LastObserved: sample.At.Add(-alarmObservationGap - time.Second)}
	c := config.MailAlarmConfig{Minutes: 20, Owner: "!opaque"}
	for i := 0; i <= 20; i++ {
		at := sample
		at.At = sample.At.Add(time.Duration(i) * time.Minute)
		at.Reason = string(tmuxwake.RuntimeRegistryUnavailable)
		if err := advanceAlarm(&state, at, c, []config.MailAlarmJob{{Room: "!opaque"}}, "alice", "host"); err != nil {
			t.Fatal(err)
		}
		if state.Active != (i == 20) {
			t.Fatalf("wrong idle credit at minute %d", i)
		}
	}
}

// Pause blocks queued sends; retirement blocks both sends and late completion
// writes. Unpausing the same live generation sends both destinations.
func TestMailAlarmPauseAndStop(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	notificationState(t, e, func(st *config.State) { st.HooksPaused = true })
	before := e.state("alice").MailAlarm
	a.sendMailAlarm(context.Background(), s, "alarm")
	if !reflect.DeepEqual(before, e.state("alice").MailAlarm) || len(alarmEvents(e)) != 0 {
		t.Fatal("paused alarm changed or sent")
	}
	notificationState(t, e, func(st *config.State) { st.HooksPaused = false; st.Terminal.Stopped = true })
	a.sendMailAlarm(context.Background(), s, "alarm")
	a.finishMailAlarm(s, "alarm", before.Jobs[0], true, false)
	if !reflect.DeepEqual(before, e.state("alice").MailAlarm) || len(alarmEvents(e)) != 0 {
		t.Fatal("stopped alarm changed or sent")
	}
	notificationState(t, e, func(st *config.State) { st.Terminal.Stopped = false })
	a.sendMailAlarm(context.Background(), s, "alarm")
	if len(alarmEvents(e)) != 2 {
		t.Fatal("unpause failed positive control")
	}
}

// 403/404 sends and missing-root resolution remain pending, visible as fixed
// errors and retryable past two attempts, without spending uncertainty.
func TestMailAlarmDestinationErrors(t *testing.T) {
	for _, mode := range []string{"403", "404", "resolution"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, room, sample := alarmFixture(t)
			primeAlarm(t, e, a, s, sample)
			base := s.client.HTTP.Transport
			if base == nil {
				base = http.DefaultTransport
			}
			sends, gets := 0, 0
			s.client.HTTP = &http.Client{Transport: alarmTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPut {
					sends++
				} else {
					gets++
				}
				return base.RoundTrip(r)
			})}
			e.hs.mu.Lock()
			originalRoom := e.hs.rooms[room]
			originalEvents := slices.Clone(e.hs.events)
			if mode == "resolution" {
				e.hs.events = nil
			} else if mode == "403" {
				originalRoom.members["@alice:"+testServer] = "leave"
			} else {
				delete(e.hs.rooms, room)
			}
			e.hs.mu.Unlock()
			now := a.Now()
			a.Now = func() time.Time { return now }
			for i := 0; i < 4; i++ {
				a.sendMailAlarm(context.Background(), s, "alarm")
				now = now.Add(6 * time.Minute)
			}
			st := e.state("alice")
			status := alarmStatus(s, st)
			wantPending, wantSends := 2, 4
			if mode == "resolution" {
				wantPending, wantSends = 1, 1
			}
			if status.State != "error" || status.Pending != wantPending || status.Held != 0 || status.Reason != "alarm destination resolution failed" || sends != wantSends || gets != 4 {
				t.Fatalf("missing failure/retry evidence: %+v PUT=%d GET=%d", status, sends, gets)
			}
			for _, job := range st.MailAlarm.Jobs {
				wantFailure := "resolution"
				if job.Topic == "" {
					wantFailure = "rejected"
				}
				if job.Failure != wantFailure || job.Uncertain != 0 || job.Delay > 5*time.Minute || !job.Next.After(now.Add(-6*time.Minute)) {
					t.Fatal("destination failure lost bounded retry")
				}
			}
			if len(alarmEvents(e)) != 2-wantPending {
				t.Fatal("unexpected sends to failed destination")
			}
			// Restore the actual fake-HS destination; successful sends clear the errors.
			e.hs.mu.Lock()
			originalRoom.members["@alice:"+testServer] = "join"
			e.hs.rooms[room] = originalRoom
			if mode == "resolution" {
				e.hs.events = append(e.hs.events, originalEvents...)
			}
			e.hs.mu.Unlock()
			a.sendMailAlarm(context.Background(), s, "alarm")
			if len(alarmEvents(e)) != 2 || len(e.state("alice").MailAlarm.Jobs) != 0 || alarmStatus(s, e.state("alice")).State != "armed" {
				t.Fatal("repaired destination failed positive control")
			}
		})
	}
}

// A panic in the real send transport is contained and logs only the fixed line;
// the durable reservation survives and a subsequent cycle can deliver safely.
func TestMailAlarmPanicRecovery(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	var log bytes.Buffer
	a.Stderr = &log
	original := s.client.HTTP
	s.client.HTTP = &http.Client{Transport: alarmTransport(func(*http.Request) (*http.Response, error) { panic("peer-secret") })}
	a.mailAlarmCycle(context.Background(), s, "alarm")
	if log.String() != "mail-alarm worker panic; bounded retry\n" {
		t.Fatal("panic escaped or log leaked")
	}
	s.client.HTTP = original
	later := a.Now().Add(6 * time.Minute)
	a.Now = func() time.Time { return later }
	a.mailAlarmCycle(context.Background(), s, "alarm")
	if len(alarmEvents(e)) != 2 || len(e.state("alice").MailAlarm.Jobs) != 0 {
		t.Fatal("worker failed to recover")
	}
}

// Without any dispatch wake, the production ticker delivers due jobs carrying
// a previous resolution failure. Both actual fake-HS destination sends succeed.
func TestMailAlarmTicker(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	notificationState(t, e, func(st *config.State) {
		for i := range st.MailAlarm.Jobs {
			st.MailAlarm.Jobs[i].Failure = "resolution"
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.runMailAlarm(ctx, s, "alarm", nil) }()
	defer func() { cancel(); <-done }()
	deadline := time.After(3 * alarmWorkerInterval)
	for {
		select {
		case <-deadline:
			t.Fatal("ticker did not deliver pending retry")
		case <-time.After(20 * time.Millisecond):
			if len(alarmEvents(e)) == 2 {
				return
			}
		}
	}
}

// The exact restart/interruption reset called by runTerminal clears idle credit
// while preserving the fence/outbox, and refuses a retired generation unchanged.
func TestMailAlarmRestartObservationReset(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	before := e.state("alice").MailAlarm
	if err := a.resetTerminalObservations(s, "alarm"); err != nil {
		t.Fatal(err)
	}
	after := e.state("alice").MailAlarm
	if !after.IdleSince.IsZero() || !after.LastObserved.IsZero() {
		t.Fatal("restart kept idle credit")
	}
	before.IdleSince = time.Time{}
	before.LastObserved = time.Time{}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("restart changed episode or outbox")
	}
	if err := a.resetTerminalObservations(s, "retired"); err == nil {
		t.Fatal("retired reset accepted")
	}
	if !reflect.DeepEqual(after, e.state("alice").MailAlarm) {
		t.Fatal("retired reset changed alarm")
	}
	// Without an active episode, the first new observation cannot alert instantly.
	notificationState(t, e, func(st *config.State) { st.MailAlarm.Active = false; st.MailAlarm.Jobs = nil })
	a.observeMailAlarm(s, "alarm", sample)
	if len(e.state("alice").MailAlarm.Jobs) != 0 {
		t.Fatal("restart fired immediately")
	}
	observeAlarmWindow(a, s, alarmSample{At: sample.At.Add(20 * time.Minute), Oldest: sample.Oldest, Count: 1, Reason: sample.Reason})
	if len(e.state("alice").MailAlarm.Jobs) != 2 {
		t.Fatal("fresh observation window failed positive control")
	}
}
