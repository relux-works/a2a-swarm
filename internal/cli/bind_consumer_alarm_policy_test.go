package cli

import (
	"bytes"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Reporting alone cannot change mail, receipts, notice/input journals, leases,
// sessions or liveness. Queued jobs cannot cross a generation or survive recovery.
// This white-box contract complements the base-compatible consumer witnesses.
func TestBindingConsumerAlarmHasNoDeliveryAuthority(t *testing.T) {
	isolateBindTest(t)
	e, a, s, room := strandedFallbackFixture(t)
	now := a.Now()
	a.Now = func() time.Time { return now }
	notificationState(t, e, func(st *config.State) {
		st.Terminal.OwnNotice = config.OwnNoticeObservation{}
		st.Terminal.SessionID = "fixture-session"
		st.Lease = &config.Lease{Holder: "fixture-session", Host: "host-a", PID: 12345, Expires: now.Add(time.Hour)}
		st.MailAlarmConfig = &config.MailAlarmConfig{Minutes: 20, Owner: room}
	})
	if _, err := e.mailbox("alice").Append([]config.MailEntry{{ID: "$consumer-pending", Kind: config.MailEvent, Room: room, Rel: "to", TS: now.UnixMilli()}}); err != nil {
		t.Fatal(err)
	}
	mailBefore, err := os.ReadFile(s.paths.MailboxFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	before := e.state("alice")
	episode := &bindingAvailability{}
	a.observeBindingConsumer(s, "gen", episode, false)
	now = now.Add(10 * time.Minute)
	a.observeBindingConsumer(s, "gen", episode, false)
	queued := e.state("alice")
	if len(queued.MailAlarm.Jobs) != 1 {
		t.Fatal("valid positive control did not queue an alarm")
	}
	job := queued.MailAlarm.Jobs[0]
	if !validAlarmJob(job, s.name) || !alarmJobEnabled(s, queued, job) {
		t.Fatal("queued consumer incident did not pass the real sender gates")
	}
	for _, change := range []func(*config.MailAlarmJob){
		func(j *config.MailAlarmJob) { j.Reason = orphanReason },
		func(j *config.MailAlarmJob) { j.Reason = "PRIVATE_PEER_TEXT" },
		func(j *config.MailAlarmJob) { j.Kind = "unknown-incident" },
		func(j *config.MailAlarmJob) { j.At = now.Add(-time.Nanosecond) },
	} {
		bad := job
		change(&bad)
		if validAlarmJob(bad, s.name) {
			t.Fatal("sender accepted an invalid reason/kind or a premature incident")
		}
	}
	// A restarted observer must reuse the durable fence, even after another
	// full bound. Its normal sender still owns the original pinned transaction.
	now = now.Add(10 * time.Minute)
	a.observeBindingConsumer(s, "gen", &bindingAvailability{}, false)
	reloaded := e.state("alice")
	if !reflect.DeepEqual(queued.MailAlarm.Jobs, reloaded.MailAlarm.Jobs) {
		t.Fatal("restart duplicated or replaced the queued incident")
	}
	wrong := queued
	wrong.Terminal = &config.TerminalBinding{Generation: "replacement"}
	if alarmJobEnabled(s, wrong, job) {
		t.Fatal("retired generation retained an enabled alarm")
	}
	a.observeBindingConsumer(s, "gen", episode, true)
	after := e.state("alice")
	if len(after.MailAlarm.Jobs) != 0 || after.Terminal.ConsumerReport != nil || alarmJobEnabled(s, after, job) {
		t.Fatal("recovery retained an obsolete alarm")
	}
	before.Terminal.ConsumerReport, after.Terminal.ConsumerReport = nil, nil
	before.MailAlarm, after.MailAlarm = config.MailAlarm{}, config.MailAlarm{}
	if !reflect.DeepEqual(typedStateWithoutWriter(before), typedStateWithoutWriter(after)) {
		t.Fatal("alarm observation changed delivery/session/lease state")
	}
	mailAfter, err := os.ReadFile(s.paths.MailboxFile(s.name))
	if err != nil || !bytes.Equal(mailBefore, mailAfter) || e.received("alice", "$consumer-pending") {
		t.Fatal("alarm observation changed mailbox bytes or receipts", err)
	}
}
