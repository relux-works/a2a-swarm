package cli

import (
	"errors"
	"slices"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const strandedAlarmKind = "stranded-notice"

// Called under the state lock: enqueue and the episode fence commit together.
// Each audience is pinned once. A missing owner route does not hide the lead
// alert, and can become available later without repeating the lead alert.
func queueStrandedAlarm(s *session, st *config.State, now time.Time) error {
	b := st.Terminal
	r := b.NoticeReport
	if st.MailAlarm.Invalid {
		r.Outcome = "unavailable: invalid alarm state"
		return errors.New(r.Outcome)
	}
	c, targets, _ := alarmPolicy(s, *st)
	if !validAlarmConfig(c) {
		r.Outcome = "unavailable: invalid alarm config"
		return errors.New(r.Outcome)
	}
	r.AlarmLeadRequired = r.AlarmLeadRequired || c.Lead != ""
	var owner *config.MailAlarmJob
	for _, target := range targets {
		if target.Target == "" {
			copy := target
			owner = &copy
			break
		}
	}
	enqueue := func(target config.MailAlarmJob, fence *string) error {
		if *fence != "" {
			return nil
		}
		id, err := alarmID()
		if err != nil {
			return err
		}
		r.Alarm = true
		target.ID, target.Identity, target.Host = id, s.name, alarmHost(b.Host)
		target.Kind, target.Since, target.Reason = strandedAlarmKind, r.Since, orphanReason
		target.At, target.Next = now, now
		st.MailAlarm.Jobs = append(st.MailAlarm.Jobs, target)
		*fence = id
		return nil
	}
	if owner != nil {
		if err := enqueue(*owner, &r.AlarmOwner); err != nil {
			return err
		}
	}
	if c.Lead != "" {
		if err := enqueue(config.MailAlarmJob{Target: c.Lead, Room: c.LeadRoom, Topic: c.LeadTopic}, &r.AlarmLead); err != nil {
			return err
		}
	}
	r.Outcome = "unavailable: alarm destination missing"
	updateStrandedAlarmOutcome(st)
	if r.AlarmOwner == "" || r.AlarmLeadRequired && r.AlarmLead == "" {
		return errors.New(r.Outcome)
	}
	return nil
}

func updateStrandedAlarmOutcome(st *config.State) {
	if st.Terminal == nil || st.Terminal.NoticeReport == nil {
		return
	}
	r := st.Terminal.NoticeReport
	if !r.Alarm {
		return
	}
	r.Sent = false
	if st.MailAlarm.Invalid {
		r.Outcome = "unavailable: invalid alarm state"
		return
	}
	if r.AlarmOwner == "" || r.AlarmLeadRequired && r.AlarmLead == "" {
		r.Outcome = "unavailable: alarm destination missing"
		return
	}
	for _, job := range st.MailAlarm.Jobs {
		if job.ID == r.AlarmOwner || job.ID == r.AlarmLead {
			r.Outcome = "queued via mail-alarm"
			return
		}
	}
	// A missing job can mean corruption or retirement, not delivery. Only
	// the sender's successful transaction ACK records an audience receipt.
	if r.AlarmOwnerAck != r.AlarmOwner || r.AlarmLeadRequired && r.AlarmLeadAck != r.AlarmLead {
		r.Sent = false
		r.Outcome = "unavailable: alarm acknowledgement missing"
		return
	}
	r.Sent, r.Outcome, r.RetryAt = true, "published via mail-alarm", time.Time{}
}

func acknowledgeStrandedAlarm(st *config.State, job config.MailAlarmJob) {
	if job.Kind != strandedAlarmKind || st.Terminal == nil || st.Terminal.NoticeReport == nil {
		return
	}
	r := st.Terminal.NoticeReport
	if !r.Alarm || r.Since != job.Since {
		return
	}
	if r.AlarmOwner == job.ID {
		r.AlarmOwnerAck = job.ID
	}
	if r.AlarmLead == job.ID {
		r.AlarmLeadAck = job.ID
	}
}

// Cleared/replaced episodes cannot keep unsent incident jobs alive. An already
// in-flight Matrix transaction may still complete; it cannot be recalled.
func dropRetiredStrandedAlarms(st *config.State) {
	st.MailAlarm.Jobs = slices.DeleteFunc(st.MailAlarm.Jobs, func(job config.MailAlarmJob) bool {
		if job.Kind != strandedAlarmKind {
			return false
		}
		b := st.Terminal
		if b == nil || b.NoticeReport == nil {
			return true
		}
		r := b.NoticeReport
		return !r.Alarm || r.Since != job.Since || job.ID != r.AlarmOwner && job.ID != r.AlarmLead
	})
}
