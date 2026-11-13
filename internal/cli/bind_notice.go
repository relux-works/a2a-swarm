package cli

import (
	"context"
	"errors"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

const orphanReason = "owned-notice-orphan-suspected"

// Fresh observations, not time or publication, govern the nullable state.
func (a *App) observeTerminalNotice(s *session, generation string, b config.TerminalBinding, input string) error {
	return a.terminalUpdate(s, generation, func(current *config.TerminalBinding) {
		n := current.OwnNotice
		// A known-unsubmitted pending attempt is just as capable of blocking
		// later mail as a fully receipted orphan. A spent journal-fenced Enter
		// also surfaces lost-ACK/partial attempts; reporting grants no input authority.
		// Count/time survive completed submits and fresh refusals. Only retained
		// IDs/text or prior suspicion establish an unsettled attempt here; Notice
		// remains evidence even after dispatch prunes all receipted IDs.
		retained := b.NoticeCount > 0 && b.Notice == tmuxwake.Notice(b.NoticeCount) && !b.AttemptAt.IsZero() && !b.AttemptAt.After(a.Now())
		pending := retained && b.TypingStarted && b.NoticeOrigin != nil
		// Alerting must not depend on reaching an Enter fence. An aged retained
		// fixed-notice journal is suspicious even if a gate/unknown frame prevents a
		// new observation. Missing typing authority cannot silence that alarm.
		aged := retained && !a.Now().Before(b.AttemptAt.Add(strandedNoticeThreshold)) && input != "empty"
		eligible := pending || b.Typed && b.NoticeCount > 0 && (len(b.AttemptIDs) == 0 || len(b.AttemptIDs) == b.NoticeCount)
		if !aged && (input == "unknown" || b.Typed && (b.NoticeCount <= 0 || b.Notice != tmuxwake.Notice(b.NoticeCount)) || input == "nonempty" && !eligible && (n.Since != "" || len(b.AttemptIDs) > 0 || b.Notice != "")) {
			n.PossiblyOrphaned = nil
			current.Idle = nil
		} else {
			suspect := aged || input == "nonempty" && eligible
			n.PossiblyOrphaned = &suspect
			n.ObservedAt = a.Now().UTC().Format(time.RFC3339Nano)
			if suspect {
				n.Reason = orphanReason
				if n.Since == "" {
					n.Since = n.ObservedAt
				}
				if since, err := time.Parse(time.RFC3339Nano, n.Since); aged && (err != nil || b.AttemptAt.Before(since)) {
					n.Since = b.AttemptAt.UTC().Format(time.RFC3339Nano)
				}
			} else {
				n.Reason = ""
				n.Since = ""
				current.NoticeReport = nil
			}
		}
		// Invalidate a prior visibility claim; retain stable unavailable causes.
		if !reflect.DeepEqual(current.OwnNotice, n) && current.NoticePublication == "published" {
			current.NoticePublication = "unavailable: own notice changed"
		}
		current.OwnNotice = n
	})
}

// Notice timestamps use the RFC3339 5.6 lexical grammar, then the existing
// non-leap-second calendar semantics. A successful Go parse alone is not a
// grammar check (it also accepts one-digit hours and comma fractions).
// UTC Z/z and numeric zero offsets are retained verbatim; -00:00 denotes a
// known UTC instant with unknown local offset (RFC3339 4.3). Fractions are
// unbounded decimal digits, not restricted to the emitter's nanoseconds.
var noticeTimestampGrammar = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}[Tt][0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?([Zz]|[+-]00:00)$`)

type noticeTimestamp struct {
	second   time.Time
	fraction string
}

func parseNoticeTimestamp(value string) (noticeTimestamp, bool) {
	if !noticeTimestampGrammar.MatchString(value) {
		return noticeTimestamp{}, false
	}
	// Normalize only for the semantic parser, never the stored/public bytes.
	value = strings.ToUpper(value)
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return noticeTimestamp{}, false
	}
	fraction := ""
	if value[19] == '.' {
		end := strings.IndexAny(value[20:], "Z+-") + 20
		fraction = strings.TrimRight(value[20:end], "0")
	}
	return noticeTimestamp{second: t.Truncate(time.Second), fraction: fraction}, true
}

func (t noticeTimestamp) before(other noticeTimestamp) bool {
	if !t.second.Equal(other.second) {
		return t.second.Before(other.second)
	}
	// Exact decimal comparison keeps sub-nanosecond order rather than silently
	// equating distinct valid fractions truncated by time.Parse.
	for i := 0; i < len(t.fraction) || i < len(other.fraction); i++ {
		left, right := byte('0'), byte('0')
		if i < len(t.fraction) {
			left = t.fraction[i]
		}
		if i < len(other.fraction) {
			right = other.fraction[i]
		}
		if left != right {
			return left < right
		}
	}
	return false
}

// Validate self-reported evidence, not its objective truth. One grammar and
// semantic boundary serves who/status reads and both own publication routes.
func noticeValid(n config.OwnNoticeObservation) error {
	invalid := func() error { return errors.New("invalid own notice telemetry") }

	var observed noticeTimestamp
	if n.ObservedAt != "" {
		var ok bool
		observed, ok = parseNoticeTimestamp(n.ObservedAt)
		if !ok {
			return invalid()
		}
	}
	suspected := n.PossiblyOrphaned != nil && *n.PossiblyOrphaned
	if suspected || n.Reason != "" || n.Since != "" {
		since, ok := parseNoticeTimestamp(n.Since)
		if !ok || n.Reason != orphanReason || n.ObservedAt == "" || observed.before(since) || n.PossiblyOrphaned != nil && !*n.PossiblyOrphaned {
			return invalid()
		}
	}
	if n.PossiblyOrphaned != nil && n.ObservedAt == "" {
		return invalid()
	}
	return nil
}

func noticeText(n config.OwnNoticeObservation) string {
	if noticeValid(n) != nil {
		return "own notice unknown (invalid own notice telemetry)"
	}
	if n.PossiblyOrphaned == nil {
		if n.Since != "" {
			return "own notice unknown; last suspected since=" + n.Since
		}
		return "own notice unknown"
	}
	if *n.PossiblyOrphaned {
		return "SUSPECTED-DEAF: own notice possibly orphaned since=" + n.Since + " last-observed-at=" + n.ObservedAt
	}
	return "own notice clear (last observation or turn)"
}

func (a *App) terminalStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("bind status")
	asJSON := fs.Bool("json", false, "recorded notice observation as JSON")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("bind status takes only --json")
	}
	s, err := a.session()
	if transientCredentialReason(err) != "" {
		s, err = a.sessionMetadata()
	}
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	n := config.OwnNoticeObservation{}
	publication := "unavailable: unbound"
	if st.Terminal != nil {
		n = st.Terminal.OwnNotice
		publication = st.Terminal.NoticePublication
	}
	noticeError := ""
	var readiness *terminalReadinessStatus
	var sendRefusal *config.TerminalSendRefusal
	var recovery *config.TerminalRecovery
	var input *tmuxwake.InputAssessment
	matchLocation := ""
	if b := st.Terminal; b != nil {
		if b.Recovery != nil && b.Recovery.Reason == config.TerminalRecovered && !b.Recovery.At.IsZero() {
			copy := *b.Recovery
			recovery = &copy
		}
		if b.SendRefusal != nil {
			copy := *b.SendRefusal
			copy.Reason = tmuxwake.SafeSendReason(copy.Reason)
			sendRefusal = &copy
		}
		live := tmuxwake.ReadinessAssessment{
			Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeUnavailable, Reason: tmuxwake.RuntimeTargetUnavailable},
			Input:   tmuxwake.InputAssessment{State: tmuxwake.InputUnavailable, Reason: tmuxwake.InputReasonTargetUnavailable},
		}
		if !b.Stopped && ownTerminalTarget(b, b.Target) {
			probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
			live = a.terminalEffects(*b).ObserveReadiness(probe, b.Notice)
			if live.Input.Reason == tmuxwake.InputReasonBusyOrDialog {
				matchLocation = observeBusyMatchLocation(probe, b.Target)
			}
			cancel()
		}
		// Registry refusal short-circuits input assessment; the default layout
		// reason is not a parser diagnosis. Project only, never change admission.
		if live.Runtime.State == tmuxwake.RuntimeRefused && live.Input.State == tmuxwake.InputUnavailable && live.Input.Reason == tmuxwake.InputReasonLayoutUnrecognized {
			live.Input.Reason = tmuxwake.InputReason("not_observed")
		}
		projected := readinessStatus(live.Runtime, b.Readiness, a.Now())
		input = &live.Input
		readiness = &projected
	}
	if noticeValid(n) != nil {
		n = config.OwnNoticeObservation{}
		noticeError = "invalid own notice telemetry"
		publication = "unavailable: invalid own notice telemetry"
	}
	principalError := ""
	if _, err := ownerFiles(s.paths, s.name).principals(); err != nil {
		principalError = principalConfigProblem
	}
	stateWriters := s.stateWriterWarnings(ctx, st)
	if *asJSON {
		return a.printJSON(struct {
			config.OwnNoticeObservation
			Publication         string                      `json:"publication"`
			Report              string                      `json:"system_report"`
			NoticeError         string                      `json:"notice_error,omitempty"`
			Notification        notificationStatus          `json:"notification"`
			Readiness           *terminalReadinessStatus    `json:"readiness,omitempty"`
			Input               *tmuxwake.InputAssessment   `json:"input,omitempty"`
			BusyMatchLocation   string                      `json:"busy_match_location,omitempty"`
			SendRefusal         *config.TerminalSendRefusal `json:"send_refusal,omitempty"`
			Recovery            *config.TerminalRecovery    `json:"recovery,omitempty"`
			MailAlarm           mailAlarmStatus             `json:"mail_alarm"`
			Refresh             string                      `json:"refresh"`
			PrincipalError      string                      `json:"principal_error,omitempty"`
			StateWriterWarnings []stateWriterWarning        `json:"state_writer_warnings,omitempty"`
		}{n, publication, terminalNoticeReportStatus(st.Terminal), noticeError, notificationDiagnostic(st.Notification, a.Now()), readiness, input, matchLocation, sendRefusal, recovery, alarmStatus(s, st), terminalRefreshStatus(st.Terminal), principalError, stateWriters})
	}
	for _, warning := range stateWriters {
		a.printf("warning: %s: %s\n", warning.Source, warning.Detail)
	}
	if principalError != "" {
		a.printf("%s\n", principalError)
	}
	if recovery != nil {
		a.printf("recovery=%s at=%s\n", recovery.Reason, recovery.At.Format(time.RFC3339Nano))
	}
	if sendRefusal != nil {
		a.printf("send_refusal=%s at=%s\n", sendRefusal.Reason, sendRefusal.At.Format(time.RFC3339Nano))
	}
	a.printAlarmStatus(alarmStatus(s, st))
	a.printf("refresh=%s\n", terminalRefreshStatus(st.Terminal))
	a.printf("notification=%s\n", notificationDiagnostic(st.Notification, a.Now()).State)
	if readiness != nil {
		a.printf("readiness=%s reason=%s version=%s since=%s unknown_keys=%s\n", readiness.State, readiness.Reason, readiness.Version, readiness.Since.Format(time.RFC3339Nano), readiness.UnknownKeys)
	}
	if input != nil {
		a.printf("input=%s reason=%s\n", input.State, input.Reason)
		if matchLocation != "" {
			a.printf("busy_match_location=%s\n", matchLocation)
		}
	}
	if st.Terminal == nil {
		a.printf("%s unbound; %s\n", s.name, noticeText(n))
		return nil
	}
	b := st.Terminal
	a.printf("%s generation=%s pid=%d stopped=%t outcome=%s; %s; publication=%s; system-report=%s\n", s.name, b.Generation, b.DaemonPID, b.Stopped, b.Outcome, noticeText(n), publication, terminalNoticeReportStatus(b))
	return nil
}

// This updates only the own state key, preserving the existing heartbeat and
// all status fields. Reporting an observation is not a heartbeat or wake proof.
func (s *session) publishTerminalNotice(ctx context.Context, generation string) error {
	name, ok := doorbellName(s.creds.UserID, s.server)
	if !ok || name != s.name {
		return errors.New("notice publication refuses foreign liveness key")
	}
	var publicationErr error
	err := s.writeOwnStatus(ctx, func(st *config.State) (config.AgentStatus, bool, error) {
		if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
			return config.AgentStatus{}, false, errors.New("binding retired")
		}
		b := st.Terminal
		if st.Liveness == nil {
			b.NoticePublication = "unavailable: no liveness record (a2a status set)"
			return config.AgentStatus{}, false, nil
		}
		ack := st.Liveness

		if err := noticeValid(b.OwnNotice); err != nil {
			return config.AgentStatus{}, false, err
		}
		if ack != nil && reflect.DeepEqual(ack.OwnNotice, b.OwnNotice) {
			if !st.NoticeWriteUncertain {
				b.NoticePublication = "published"
				return config.AgentStatus{}, false, nil
			}
		}
		// Explicit binding policy bounds automatic notice retries. A replacement
		// generation or fresh observation cannot bypass identity-owned eligibility.
		if b.Interval < time.Second || b.Interval > maxPoll || b.RetryMax < b.Interval || b.RetryMax > 5*time.Minute {
			b.NoticePublication = "unavailable: invalid notice retry policy"
			publicationErr = errors.New(b.NoticePublication)
			return config.AgentStatus{}, false, nil
		}
		now := s.app.Now().UTC()
		if retry := st.NoticeRetry; retry != nil {
			// Old state without metadata takes the ordinary first attempt. Invalid
			// semantic metadata is repaired to one bounded cooldown, not ignored on
			// every cycle or allowed to permanently suppress publication.
			if retry.Delay <= 0 || retry.Delay > 5*time.Minute || retry.FailedAt.IsZero() || !retry.Next.Equal(retry.FailedAt.Add(retry.Delay)) {
				st.NoticeRetry = &config.NoticeRetry{FailedAt: now, Next: now.Add(b.Interval), Delay: b.Interval}
			}
			if now.Before(st.NoticeRetry.Next) {
				b.NoticePublication = "unavailable: own liveness publication retry pending"
				publicationErr = errors.New(b.NoticePublication)
				return config.AgentStatus{}, false, nil
			}
		}
		record := *st.Liveness
		record.OwnNotice = b.OwnNotice
		return record, true, nil
	}, func(st *config.State, err error) {
		b := st.Terminal
		if err == nil || st.Liveness == nil || b == nil || b.Generation != generation || b.Stopped {
			return
		}
		delay := b.Interval
		if st.NoticeRetry != nil {
			delay = min(b.RetryMax, max(b.Interval, st.NoticeRetry.Delay*2))
		}
		failedAt := s.app.Now().UTC()
		st.NoticeRetry = &config.NoticeRetry{FailedAt: failedAt, Next: failedAt.Add(delay), Delay: delay}
	})
	// Contention is not a failed publication: leave bounded retry eligibility
	// unchanged and let the next ordinary hook/consumer cycle try again.
	if errors.Is(err, ErrOwnStatusPending) {
		return nil
	}
	if err != nil {
		return err
	}
	return publicationErr
}

// Record bounded publication results locally; never claim a failed remote write
// was visible to who. An eligible later dispatch retries without a wake attempt.
func (a *App) publishTerminalNotice(ctx context.Context, s *session, generation string) {
	_ = s.publishTerminalNotice(ctx, generation)
}

// Error/gate paths retain last evidence, but never advertise it as a current
// observation. A failed state write cannot authorize a publication claim.
func (a *App) unknownTerminalNotice(ctx context.Context, s *session, generation string, consumer bool) {
	// A consumer/read failure cannot indefinitely silence an aged notice journal.
	// This is suspicion from retained journal evidence, not a fresh screen claim.
	if st, err := config.LoadState(s.paths, s.name); err == nil && st.Terminal != nil && st.Terminal.Generation == generation && !st.Terminal.Stopped {
		if a.observeTerminalNotice(s, generation, *st.Terminal, "unknown") == nil {
			a.reportTerminalIncident(ctx, s, generation)
		}
	}
	err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
		b.Idle = nil
		if b.OwnNotice.PossiblyOrphaned != nil && b.NoticePublication == "published" {
			b.NoticePublication = "unavailable: own notice changed"
		}
		// observeTerminalNotice already classified unknown input, including the
		// aged-intent exception. Do not overwrite its suspicion here.
		if consumer {
			b.Outcome = "consumer unavailable; publishing unknown"
		}
	})
	if err == nil {
		a.publishTerminalNotice(ctx, s, generation)
		if consumer {
			_ = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Outcome = "consumer unavailable; retrying" })
		}
	}
}

// A matching runtime hook is evidence of a turn, even without opted-in
// liveness. Retire the old typing authority; pending mail is never receipted.
func (a *App) clearTerminalNoticeOnTurn(ctx context.Context, s *session, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	deadline := time.Now().Add(nativeRPCTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return a.withRuntimeLock(s.paths, s.name, "control", deadline, func() error {
		_, err := a.updateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
			b := st.Terminal
			if st.HooksPaused || b == nil || b.Stopped || b.SessionID != sessionID || b.Host != machineLabel() {
				return nil
			}
			if b.OwnNotice.PossiblyOrphaned != nil && !*b.OwnNotice.PossiblyOrphaned && b.Notice == "" {
				return nil
			}
			clear := false
			b.OwnNotice = config.OwnNoticeObservation{PossiblyOrphaned: &clear, ObservedAt: a.Now().UTC().Format(time.RFC3339Nano)}
			b.NoticeReport = nil
			dropRetiredStrandedAlarms(st)
			b.WakeMuteHold = false // A real matching turn, never thaw, retires the old draft hold.
			b.AttemptIDs, b.Notice, b.Typed, b.TypingStarted = nil, "", false, false
			b.NoticeOrigin, b.NoticePosition, b.Idle = nil, nil, nil
			b.NoticePublication = "unavailable: own notice changed"
			return nil
		})
		return err
	})
}
