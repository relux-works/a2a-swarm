package cli

import (
	"context"
	"errors"
	"fmt"
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
		eligible := b.Typed && b.NoticeCount > 0 && len(b.AttemptIDs) == 0
		if input == "unknown" || b.Typed && (b.NoticeCount <= 0 || b.Notice != tmuxwake.Notice(b.NoticeCount)) || input == "nonempty" && n.Since != "" && !eligible {
			n.PossiblyOrphaned = nil
			current.Idle = nil
		} else {
			suspect := input == "nonempty" && eligible
			n.PossiblyOrphaned = &suspect
			n.ObservedAt = a.Now().UTC().Format(time.RFC3339Nano)
			if suspect {
				n.Reason = orphanReason
				if n.Since == "" {
					n.Since = n.ObservedAt
				}
			} else {
				n.Reason = ""
				n.Since = ""
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
		return "SUSPECTED-DEAF: own notice possibly orphaned since=" + n.Since + " last-rendered-at=" + n.ObservedAt
	}
	return "own notice clear (last rendered observation)"
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
	if b := st.Terminal; b != nil {
		live := tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeUnavailable, Reason: tmuxwake.RuntimeTargetUnavailable}
		if !b.Stopped && ownTerminalTarget(b, b.Target) {
			probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
			live = boundTerminal(*b).AssessReadiness(probe).Runtime
			cancel()
		}
		projected := readinessStatus(live, b.Readiness, a.Now())
		readiness = &projected
	}
	if noticeValid(n) != nil {
		n = config.OwnNoticeObservation{}
		noticeError = "invalid own notice telemetry"
		publication = "unavailable: invalid own notice telemetry"
	}
	if *asJSON {
		return a.printJSON(struct {
			config.OwnNoticeObservation
			Publication  string                   `json:"publication"`
			NoticeError  string                   `json:"notice_error,omitempty"`
			Notification notificationStatus       `json:"notification"`
			Readiness    *terminalReadinessStatus `json:"readiness,omitempty"`
		}{n, publication, noticeError, notificationDiagnostic(st.Notification, a.Now()), readiness})
	}
	a.printf("notification=%s\n", notificationDiagnostic(st.Notification, a.Now()).State)
	if readiness != nil {
		a.printf("readiness=%s reason=%s version=%s since=%s unknown_keys=%s\n", readiness.State, readiness.Reason, readiness.Version, readiness.Since.Format(time.RFC3339Nano), readiness.UnknownKeys)
	}
	if st.Terminal == nil {
		a.printf("%s unbound; %s\n", s.name, noticeText(n))
		return nil
	}
	b := st.Terminal
	a.printf("%s generation=%s pid=%d stopped=%t outcome=%s; %s; publication=%s\n", s.name, b.Generation, b.DaemonPID, b.Stopped, b.Outcome, noticeText(n), publication)
	return nil
}

// This updates only the own state key, preserving the existing heartbeat and
// all status fields. Reporting an observation is not a heartbeat or wake proof.
func (s *session) publishTerminalNotice(ctx context.Context, generation string) error {
	name, ok := doorbellName(s.creds.UserID, s.server)
	if !ok || name != s.name {
		return errors.New("notice publication refuses foreign liveness key")
	}
	deadline := time.Now().Add(nativeRPCTimeout)
	if callerDeadline, ok := ctx.Deadline(); ok && callerDeadline.Before(deadline) {
		deadline = callerDeadline
	}
	writeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var publicationErr error
	// Preserve the existing bounded state critical section. The acknowledged
	// snapshot and queued local writers remain serialized through resolve + PUT.
	_, err := config.UpdateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
			return errors.New("binding retired")
		}
		b := st.Terminal
		if err := writeCtx.Err(); err != nil {
			return err
		}
		if st.Liveness == nil {
			b.NoticePublication = "unavailable: no liveness record (a2a status set)"
			return nil
		}
		if err := noticeValid(b.OwnNotice); err != nil {
			return err
		}
		if reflect.DeepEqual(st.Liveness.OwnNotice, b.OwnNotice) {
			if !st.NoticeWriteUncertain {
				b.NoticePublication = "published"
				return nil
			}
		}
		// Explicit binding policy also bounds notice-only retries. A replacement
		// generation or fresh observation cannot bypass identity-owned eligibility.
		if b.Interval < time.Second || b.Interval > maxPoll || b.RetryMax < b.Interval || b.RetryMax > 5*time.Minute {
			b.NoticePublication = "unavailable: invalid notice retry policy"
			publicationErr = errors.New(b.NoticePublication)
			return nil
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
				return nil
			}
		}
		record := *st.Liveness
		record.OwnNotice = b.OwnNotice
		failed := func(err error) error {
			delay := b.Interval
			if st.NoticeRetry != nil {
				delay = min(b.RetryMax, max(b.Interval, st.NoticeRetry.Delay*2))
			}
			failedAt := s.app.Now().UTC()
			st.NoticeRetry = &config.NoticeRetry{FailedAt: failedAt, Next: failedAt.Add(delay), Delay: delay}
			b.NoticePublication = "unavailable: own liveness publication failed"
			publicationErr = fmt.Errorf("notice publication unavailable: %w", err)
			// Save the failure without acknowledging the PUT.
			return nil
		}
		if err := s.putOwnStatus(writeCtx, st, record); err != nil {
			return failed(err)
		}
		return nil
	})
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
	err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
		b.Idle = nil
		if b.OwnNotice.PossiblyOrphaned != nil && b.NoticePublication == "published" {
			b.NoticePublication = "unavailable: own notice changed"
		}
		b.OwnNotice.PossiblyOrphaned = nil
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
