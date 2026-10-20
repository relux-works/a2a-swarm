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
		current.NoticePublication = "pending: own liveness publication"
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
	if noticeValid(n) != nil {
		n = config.OwnNoticeObservation{}
		noticeError = "invalid own notice telemetry"
		publication = "unavailable: invalid own notice telemetry"
	}
	if *asJSON {
		return a.printJSON(struct {
			config.OwnNoticeObservation
			Publication string `json:"publication"`
			NoticeError string `json:"notice_error,omitempty"`
		}{n, publication, noticeError})
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
	room, _, err := s.resolveRoom(ctx, StatusRoom)
	if err != nil {
		return err
	}
	_, err = config.UpdateStateBy(s.paths, s.name, time.Now().Add(nativeRPCTimeout), func(st *config.State) error {
		if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped || st.Liveness == nil {
			return errors.New("notice publication unavailable: no liveness record")
		}
		record := *st.Liveness
		if err := noticeValid(st.Terminal.OwnNotice); err != nil {
			return err
		}
		if reflect.DeepEqual(record.OwnNotice, st.Terminal.OwnNotice) {
			return nil
		}
		record.OwnNotice = st.Terminal.OwnNotice
		if _, err := s.client.PutState(ctx, room, StatusEventType, s.creds.UserID, record); err != nil {
			return fmt.Errorf("notice publication unavailable: %w", err)
		}
		st.Liveness = &record
		return nil
	})
	return err
}

// Record bounded publication results locally; never claim a failed remote write
// was visible to who. A later dispatch retries without spending a wake attempt.
func (a *App) publishTerminalNotice(ctx context.Context, s *session, generation string) {
	err := s.publishTerminalNotice(ctx, generation)
	outcome := "published"
	if err != nil {
		outcome = "unavailable: own liveness publication failed"
	}
	_ = a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.NoticePublication = outcome })
}

// Error/gate paths retain last evidence, but never advertise it as a current
// observation. A failed state write cannot authorize a publication claim.
func (a *App) unknownTerminalNotice(ctx context.Context, s *session, generation string, consumer bool) {
	err := a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
		b.Idle = nil
		b.OwnNotice.PossiblyOrphaned = nil
		b.NoticePublication = "pending: own liveness publication"
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
