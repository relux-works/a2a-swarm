package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

const defaultAlarmMinutes = 20
const alarmObservationGap = 2 * time.Minute
const alarmWorkerInterval = 10 * time.Second

var alarmServerRE = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]{1,5})?$`)
var alarmEventRE = regexp.MustCompile(`^\$[^\s\x00-\x1f]{1,255}$`)

func alarmTarget(v string) bool {
	if strings.HasPrefix(v, "@") {
		local, server, ok := strings.Cut(v[1:], ":")
		return ok && aliasLocalRE.MatchString(local) && alarmServerRE.MatchString(server)
	}
	return v != "all" && config.ValidateName(v) == nil
}
func alarmRoom(v string) bool {
	if strings.HasPrefix(v, "!") {
		return ownerRoomID.MatchString(v)
	}
	v = strings.TrimPrefix(v, "#")
	local, server, ok := strings.Cut(v, ":")
	return aliasLocalRE.MatchString(local) && (!ok || alarmServerRE.MatchString(server))
}
func validAlarmConfig(c config.MailAlarmConfig) bool {
	return !c.Invalid && c.Minutes >= 1 && c.Minutes <= 1440 && (c.Owner == "auto" || c.Owner == "off" || alarmRoom(c.Owner)) &&
		((c.Operator == "" && c.Room == "" && c.Topic == "") || (alarmTarget(c.Operator) && alarmRoom(c.Room) && (slugRE.MatchString(c.Topic) || alarmEventRE.MatchString(c.Topic))))
}
func alarmPolicy(s *session, st config.State) (config.MailAlarmConfig, []config.MailAlarmJob, string) {
	c := config.MailAlarmConfig{Minutes: defaultAlarmMinutes, Owner: "auto"}
	if st.MailAlarmConfig != nil {
		c = *st.MailAlarmConfig
	}
	if !validAlarmConfig(c) {
		return c, nil, "invalid config"
	}
	var targets []config.MailAlarmJob
	if c.Owner == "auto" {
		owner, _, err := ownerFiles(s.paths, s.name).load()
		if err == nil && owner != nil && owner.Active && !owner.Pending && ownerFiles(s.paths, s.name).pin().Owner == owner.Owner {
			targets = append(targets, config.MailAlarmJob{Room: owner.Room, AutoOwner: owner.Owner})
		}
	} else if c.Owner != "off" {
		targets = append(targets, config.MailAlarmJob{Room: c.Owner})
	}
	if c.Operator != "" {
		targets = append(targets, config.MailAlarmJob{Target: c.Operator, Room: c.Room, Topic: c.Topic})
	}
	if len(targets) == 0 {
		return c, nil, "no destinations (no active owner DM)"
	}
	return c, targets, ""
}

type mailAlarmStatus struct {
	State       string                 `json:"state"`
	Reason      string                 `json:"reason,omitempty"`
	Threshold   int                    `json:"threshold_minutes"`
	Outstanding bool                   `json:"outstanding"`
	Pending     int                    `json:"pending"`
	Held        int                    `json:"held"`
	LastAlert   time.Time              `json:"last_alert_time"`
	Config      config.MailAlarmConfig `json:"config"`
}

func alarmStatus(s *session, st config.State) mailAlarmStatus {
	c, _, reason := alarmPolicy(s, st)
	out := mailAlarmStatus{State: "armed", Reason: reason, Threshold: c.Minutes, Outstanding: st.MailAlarm.Active || len(st.MailAlarm.Jobs) > 0, Pending: len(st.MailAlarm.Jobs), LastAlert: st.MailAlarm.LastAlert, Config: c}
	if reason != "" {
		out.State = "disabled"
	}
	if st.MailAlarm.Invalid {
		out.State = "error"
		out.Reason = "invalid alarm state; held until addressed stale set drains"
	}
	if c.Invalid || !validAlarmConfig(c) {
		out.State = "error"
	}
	for _, j := range st.MailAlarm.Jobs {
		if !validAlarmJob(j, s.name) {
			out.State, out.Reason = "error", "invalid queued alarm"
		} else if j.Failure != "" {
			out.State, out.Reason = "error", alarmFailureReason(j.Failure)
		}
		if j.Uncertain >= 2 {
			out.Held++
			out.State, out.Reason = "error", "ambiguous alarm delivery; held"
		}
	}
	return out
}
func cmdMailAlarm(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("mail-alarm set|unset|status [--json]")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	switch args[0] {
	case "set":
		fs := newFlagSet("mail-alarm set")
		minutes := fs.Int("minutes", defaultAlarmMinutes, "threshold 1..1440 minutes")
		owner := fs.String("owner", "auto", "auto, off, or room")
		operator := fs.String("operator", "", "agent name or full Matrix user")
		room := fs.String("room", "", "operator room")
		topic := fs.String("topic", "", "operator topic slug or root event")
		pos, e := parseInterspersed(fs, args[1:])
		if e != nil {
			return e
		}
		if len(pos) > 0 {
			return usagef("mail-alarm set takes flags only")
		}
		c := config.MailAlarmConfig{Minutes: *minutes, Owner: *owner, Operator: *operator, Room: *room, Topic: *topic}
		if !validAlarmConfig(c) {
			return usagef("invalid mail-alarm config: minutes 1..1440; owner auto|off|room; operator, room and topic required together")
		}
		_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.MailAlarmConfig = &c; return nil })
	case "unset":
		if len(args) != 1 {
			return usagef("mail-alarm unset takes no flags")
		}
		_, err = config.UpdateState(s.paths, s.name, func(st *config.State) error { st.MailAlarmConfig = nil; return nil })
	case "status", "show":
		fs := newFlagSet("mail-alarm status")
		js := fs.Bool("json", false, "JSON")
		pos, e := parseInterspersed(fs, args[1:])
		if e != nil {
			return e
		}
		if len(pos) > 0 {
			return usagef("mail-alarm status takes only --json")
		}
		st, e := config.LoadState(s.paths, s.name)
		if e != nil {
			return e
		}
		status := alarmStatus(s, st)
		if *js {
			return a.printJSON(status)
		}
		a.printAlarmStatus(status)
		return nil
	default:
		return usagef("mail-alarm set|unset|status")
	}
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	a.printAlarmStatus(alarmStatus(s, st))
	return nil
}
func (a *App) printAlarmStatus(s mailAlarmStatus) {
	a.printf("mail-alarm=%s threshold=%dm outstanding=%t pending=%d held=%d last_alert=%s reason=%s\n", s.State, s.Threshold, s.Outstanding, s.Pending, s.Held, s.LastAlert.UTC().Format(time.RFC3339), s.Reason)
}

// Only existing classification and timestamps cross this boundary; no peer text.
type alarmSample struct {
	Count  int
	Oldest time.Time
	Busy   bool
	Reason string
	At     time.Time
}

func (sample *alarmSample) add(e config.MailEntry) {
	if e.Kind != config.MailEvent || e.Rel != string(relTo) || e.TS <= 0 {
		return
	}
	at := time.UnixMilli(e.TS)
	sample.Count++
	if sample.Oldest.IsZero() || at.Before(sample.Oldest) {
		sample.Oldest = at
	}
}
func alarmObservation(sample alarmSample, r tmuxwake.ReadinessAssessment) alarmSample {
	sample.Busy = r.Runtime.Reason == tmuxwake.RuntimeBusy || r.Input.Reason == tmuxwake.InputReasonBusyOrDialog
	sample.Reason = string(r.Runtime.Reason)
	if !tmuxwake.ValidRuntimeReason(r.Runtime.Reason) {
		sample.Reason = string(tmuxwake.RuntimeRegistryUnavailable)
	}
	if r.Input.State == tmuxwake.InputNonempty {
		sample.Reason += "; composer-nonempty"
	}
	return sample
}
func alarmID() (string, error) {
	var b [16]byte
	_, err := rand.Read(b[:])
	return "a2a-mail-alarm-" + hex.EncodeToString(b[:]), err
}

// Drain rearms; busy, configuration edits and binding replacement never rearm.
func advanceAlarm(state *config.MailAlarm, sample alarmSample, c config.MailAlarmConfig, targets []config.MailAlarmJob, identity, host string) error {
	drainMinutes := c.Minutes
	if state.Active && state.EpisodeMinutes >= 1 && state.EpisodeMinutes <= 1440 {
		drainMinutes = state.EpisodeMinutes
	}
	if sample.Count == 0 || sample.Oldest.After(sample.At.Add(-time.Duration(drainMinutes)*time.Minute)) {
		state.Active = false
		state.Invalid = false
	}
	if state.LastObserved.IsZero() || sample.At.Before(state.LastObserved) || sample.At.Sub(state.LastObserved) > alarmObservationGap {
		state.IdleSince = time.Time{}
	}
	state.LastObserved = sample.At
	if sample.Busy {
		state.IdleSince = time.Time{}
		return nil
	}
	if state.IdleSince.IsZero() || state.IdleSince.After(sample.At) {
		state.IdleSince = sample.At
	}
	if state.Invalid || state.Active || len(targets) == 0 || sample.Count == 0 || sample.Oldest.After(sample.At.Add(-time.Duration(c.Minutes)*time.Minute)) || sample.At.Before(state.IdleSince.Add(time.Duration(c.Minutes)*time.Minute)) {
		return nil
	}
	for _, target := range targets {
		id, err := alarmID()
		if err != nil {
			return err
		}
		target.ID = id
		target.Identity = identity
		target.Host = alarmHost(host)
		target.Count = sample.Count
		target.Minutes = int64(sample.At.Sub(sample.Oldest) / time.Minute)
		target.Reason = sample.Reason
		target.At = sample.At
		target.Next = sample.At
		state.Jobs = append(state.Jobs, target)
	}
	state.Active = true
	state.EpisodeMinutes = c.Minutes
	return nil
}

// A nonblocking, bounded lock attempt keeps alarm work outside delivery's budget.
func (a *App) observeMailAlarm(s *session, generation string, sample alarmSample) {
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return
	}
	c, targets, reason := alarmPolicy(s, st)
	if reason != "" {
		targets = nil
		c.Minutes = defaultAlarmMinutes
	}
	next := st.MailAlarm
	next.Jobs = slices.Clone(next.Jobs)
	if advanceAlarm(&next, sample, c, targets, s.name, machineLabel()) != nil || reflect.DeepEqual(next, st.MailAlarm) {
		return
	}
	_, _ = config.UpdateStateBy(s.paths, s.name, time.Now(), func(current *config.State) error {
		if current.Terminal == nil || current.Terminal.Stopped || current.Terminal.Generation != generation || !reflect.DeepEqual(current.MailAlarmConfig, st.MailAlarmConfig) {
			return errors.New("alarm observation changed")
		}
		// The sender may have acknowledged a job since the snapshot; preserve
		// its latest outbox while applying this drain/busy observation.
		return advanceAlarm(&current.MailAlarm, sample, c, targets, s.name, machineLabel())
	})
}
func alarmBody(j config.MailAlarmJob) string {
	return fmt.Sprintf("Stale addressed mailbox: identity=%s count=%d oldest_age_minutes=%d readiness_reason=%s host=%s", j.Identity, j.Count, j.Minutes, j.Reason, j.Host)
}

// One independent worker per consumer. It never opens the mailbox or touches a pane.
func (a *App) runMailAlarm(ctx context.Context, s *session, generation string, wake <-chan struct{}) {
	ticker := time.NewTicker(alarmWorkerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wake:
			if !ok {
				wake = nil
			}
		case <-ticker.C:
		}
		a.mailAlarmCycle(ctx, s, generation)
	}
}

// A peer-influenced resolution panic must never escape the optional worker.
func (a *App) mailAlarmCycle(ctx context.Context, s *session, generation string) {
	defer func() {
		if recover() != nil {
			fmt.Fprintln(a.Stderr, "mail-alarm worker panic; bounded retry")
		}
	}()
	a.sendMailAlarm(ctx, s, generation)
}

func alarmHost(host string) string {
	if aliasLocalRE.MatchString(host) {
		return host
	}
	return "unknown-host"
}

func alarmFailureReason(reason string) string {
	switch reason {
	case "audience":
		return "owner audience check failed"
	case "owner":
		return "owner binding or pin changed"
	case "resolution":
		return "alarm destination resolution failed"
	case "rejected":
		return "alarm destination rejected delivery"
	case "send":
		return "alarm send failed"
	default:
		return "invalid queued alarm failure"
	}
}

func validAlarmJob(job config.MailAlarmJob, identity string) bool {
	return job.Identity == identity && config.ValidateName(job.Identity) == nil && aliasLocalRE.MatchString(job.Host) && job.Count > 0 && job.Minutes >= 0 && strings.HasPrefix(job.ID, "a2a-mail-alarm-") && len(job.ID) == 47 && (job.Topic == "" || slugRE.MatchString(job.Topic) || alarmEventRE.MatchString(job.Topic)) && (job.ResolvedRoom == "" || alarmRoom(job.ResolvedRoom)) && (job.Root == "" || alarmEventRE.MatchString(job.Root)) && alarmRoom(job.Room) && (job.Target == "" || alarmTarget(job.Target)) && alarmSafeReason(job.Reason)
}

func (a *App) sendMailAlarm(ctx context.Context, s *session, generation string) {
	st, err := config.LoadState(s.paths, s.name)
	if err != nil || st.HooksPaused || st.Terminal == nil || st.Terminal.Stopped || st.Terminal.Generation != generation {
		return
	}
	_, _, disabled := alarmPolicy(s, st)
	if disabled != "" {
		return
	}
	for _, job := range st.MailAlarm.Jobs {
		if ctx.Err() != nil {
			return
		}
		if job.Uncertain >= 2 || a.Now().Before(job.Next) {
			continue
		}
		// Invalid persisted outbox data is held, never reflected into a message.
		if !validAlarmJob(job, s.name) {
			continue
		}
		sendCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel() // also release the attempt context on a recovered panic

		if job.AutoOwner != "" {
			f := ownerFiles(s.paths, s.name)
			owner, _, err := f.load()
			if err != nil || owner == nil || !owner.Active || owner.Pending || owner.Owner != job.AutoOwner || owner.Room != job.Room || f.pin().Owner != owner.Owner {
				cancel()
				job.Failure = "owner"
				a.finishMailAlarm(s, generation, job, false, false)
				continue
			}
			if joined, err := s.ownerAudience(sendCtx, owner.Room, owner.Owner, false); err != nil || !joined {
				cancel()
				job.Failure = "audience"
				a.finishMailAlarm(s, generation, job, false, false)
				continue
			}
		}
		room, root := job.ResolvedRoom, job.Root
		var resolveErr error
		if room == "" {
			room, _, resolveErr = s.resolveRoom(sendCtx, job.Room)
			if resolveErr == nil && job.Topic != "" {
				root, _, resolveErr = s.resolveTopic(sendCtx, room, room, job.Topic)
			}
		}
		if resolveErr != nil {
			cancel()
			job.Failure = "resolution"
			a.finishMailAlarm(s, generation, job, false, false)
			continue
		}
		job.ResolvedRoom = room
		job.Root = root
		reserved := false
		_, err = config.UpdateStateBy(s.paths, s.name, time.Now(), func(current *config.State) error {
			if current.HooksPaused || current.Terminal == nil || current.Terminal.Stopped || current.Terminal.Generation != generation {
				return errors.New("retired alarm worker")
			}
			if _, _, disabled := alarmPolicy(s, *current); disabled != "" {
				return errors.New("alarm disabled before reservation")
			}
			for i := range current.MailAlarm.Jobs {
				j := &current.MailAlarm.Jobs[i]
				if j.ID == job.ID && j.Uncertain == job.Uncertain {
					job.Uncertain++
					job.Delay = min(5*time.Minute, max(10*time.Second, job.Delay*2))
					job.Next = a.Now().Add(job.Delay)
					*j = job
					reserved = true
					break
				}
			}
			return nil
		})
		if err != nil || !reserved {
			cancel()
			continue
		}
		content := map[string]any{"msgtype": "m.text", "body": alarmBody(job)}
		if job.Topic != "" {
			names, mentions, _ := addressees(job.Target, s.server)
			h := Header{V: 1, Type: "message", Topic: root, From: s.name, To: names, At: job.At.UTC().Format(time.RFC3339), Kind: "notice"}
			content = threadReply(h, mentions)
			content["msgtype"] = "m.text"
			content["body"] = alarmBody(job)
		}
		_, sendErr := s.client.WithoutRedirects().SendMessageTxn(sendCtx, room, job.ID, content)
		cancel()
		// Only explicit server rejection is known not to have sent. Transport,
		// cancellation and malformed ACKs are uncertain, including process death.
		var httpErr *matrix.Error
		definite := errors.As(sendErr, &httpErr) && httpErr.OriginalRequest && httpErr.Status >= 400 && httpErr.Status < 500 && httpErr.Status != http.StatusRequestTimeout
		job.Failure = "send"
		if definite {
			job.Failure = "rejected"
		}
		a.finishMailAlarm(s, generation, job, sendErr == nil, definite)
	}
}
func alarmSafeReason(reason string) bool {
	reason = strings.TrimSuffix(reason, "; composer-nonempty")
	return tmuxwake.ValidRuntimeReason(tmuxwake.RuntimeReason(reason))
}
func (a *App) finishMailAlarm(s *session, generation string, job config.MailAlarmJob, success, definite bool) {
	_, _ = config.UpdateStateBy(s.paths, s.name, time.Now(), func(st *config.State) error {
		if st.Terminal == nil || st.Terminal.Stopped || st.Terminal.Generation != generation {
			return errors.New("retired alarm worker")
		}
		for i := range st.MailAlarm.Jobs {
			j := &st.MailAlarm.Jobs[i]
			if j.ID != job.ID {
				continue
			}
			if success {
				st.MailAlarm.LastAlert = a.Now()
				st.MailAlarm.Jobs = slices.Delete(st.MailAlarm.Jobs, i, i+1)
			} else {
				j.Failure = job.Failure
				if definite && j.Uncertain > 0 {
					j.Uncertain--
				}
				j.Delay = min(5*time.Minute, max(10*time.Second, j.Delay*2))
				j.Next = a.Now().Add(j.Delay)
			}
			break
		}
		return nil
	})
}
