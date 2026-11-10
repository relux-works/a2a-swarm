package cli

import (
	"errors"
	"slices"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const strandedAlarmKind = "stranded-notice"
const consumerUnavailableAlarmKind = "consumer-unavailable"
const consumerUnavailableReason = "consumer unavailable"

func incidentAlarmKind(kind string) bool {
	return kind == strandedAlarmKind || kind == consumerUnavailableAlarmKind
}

func incidentAlarmReport(st *config.State, kind string) *config.NoticeReport {
	if st.Terminal == nil {
		return nil
	}
	if kind == consumerUnavailableAlarmKind {
		return st.Terminal.ConsumerReport
	}
	if kind == strandedAlarmKind {
		return st.Terminal.NoticeReport
	}
	return nil
}

// Called under the state lock: enqueue and the episode fence commit together.
// Each audience is pinned once. A missing owner route does not hide the lead
// alert, and can become available later without repeating the lead alert.
func queueStrandedAlarm(s *session, st *config.State, now time.Time) error {
	return queueIncidentAlarm(s, st, st.Terminal.NoticeReport, strandedAlarmKind, orphanReason, now)
}

func queueIncidentAlarm(s *session, st *config.State, r *config.NoticeReport, kind, reason string, now time.Time) error {
	b := st.Terminal
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
		target.Kind, target.Since, target.Reason = kind, r.Since, reason
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
	updateIncidentAlarmOutcome(st, r)
	if r.AlarmOwner == "" || r.AlarmLeadRequired && r.AlarmLead == "" {
		return errors.New(r.Outcome)
	}
	return nil
}

func updateStrandedAlarmOutcome(st *config.State) {
	updateIncidentAlarmOutcome(st, incidentAlarmReport(st, strandedAlarmKind))
	updateIncidentAlarmOutcome(st, incidentAlarmReport(st, consumerUnavailableAlarmKind))
}

func updateIncidentAlarmOutcome(st *config.State, r *config.NoticeReport) {
	if r == nil {
		return
	}
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
	r := incidentAlarmReport(st, job.Kind)
	if r == nil {
		return
	}
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
		if !incidentAlarmKind(job.Kind) {
			return false
		}
		r := incidentAlarmReport(st, job.Kind)
		if r == nil {
			return true
		}
		return !r.Alarm || r.Since != job.Since || job.ID != r.AlarmOwner && job.ID != r.AlarmLead
	})
}
