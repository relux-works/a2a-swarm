package cli

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Persist typing intent before the literal command, including for a lost type
// ACK. Failure cancels Send before its final strict empty/pre-type check.
func (a *App) sendTerminalAttempt(ctx context.Context, s *session, expected config.TerminalBinding, key tmuxwake.ReadinessKey) (tmuxwake.SendResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var fenceErr error
	result, err := boundTerminal(expected).Send(ctx, expected.NoticeCount, key, func() {
		_, fenceErr = config.UpdateState(s.paths, s.name, func(st *config.State) error {
			b := st.Terminal
			if b == nil || b.Stopped || !sameTerminalScope(*b, expected) || b.NotificationToken != expected.NotificationToken || b.Notice != expected.Notice || !b.AttemptAt.Equal(expected.AttemptAt) || !slices.Equal(b.AttemptIDs, expected.AttemptIDs) || st.HooksPaused || manualWakeGate(*st) != "" {
				return errors.New("terminal typing attempt changed")
			}
			b.TypingStarted = true
			return nil
		})
		if fenceErr != nil {
			cancel()
		}
	}, nil)
	if fenceErr != nil {
		return result, fenceErr
	}
	return result, err
}

// The saved empty observation is an identity constraint, never permission.
// Daemon identity and registry timestamps may change on same-generation
// restart. Composer position must match the saved empty origin; cursor position
// itself is not authority. Target, session, route, version and width stay pinned.
func sameNoticeOrigin(b config.TerminalBinding, k tmuxwake.ReadinessKey) bool {
	o := b.NoticeOrigin
	return o != nil && o.Generation == b.Generation && o.SessionID == b.SessionID && o.Host == b.Host && o.Target == b.Target && o.ClaudeRegistryRoot == b.ClaudeRegistryRoot &&
		k.Generation == o.Generation && k.SessionID == o.SessionID && k.Target == o.Target && k.ClaudeRegistryRoot == o.ClaudeRegistryRoot && k.Route == o.ReadinessRoute && k.RegistrySessionID == o.RegistrySessionID && k.RegistryVersion == o.RegistryVersion && k.PaneWidth == o.PaneWidth && k.ComposerRow == o.ComposerRow && k.ComposerColumn == o.ComposerColumn
}

// An in-window reservation covers continuation. Aged recovery must reserve
// its actual submission hour at the Enter fence.
func noticeRecoveryBudget(b config.TerminalBinding, now time.Time) bool {
	others := 0
	for _, at := range b.Reservations {
		if now.Sub(at) < time.Hour && !at.Equal(b.AttemptAt) {
			others++
		}
	}
	return b.MaxWakes > 0 && others < b.MaxWakes
}

func (a *App) resumeTerminalNotice(ctx context.Context, s *session, generation string, b config.TerminalBinding) error {
	held := func() error {
		return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) { b.Idle = nil; b.Outcome = "suppressed: owned notice recovery held" })
	}
	if !b.TypingStarted || b.NoticeCount <= 0 || b.Notice != tmuxwake.Notice(b.NoticeCount) || b.NotificationToken == "" || b.Interval < time.Second || b.RetryMax < b.Interval || b.RetryMax > 5*time.Minute {
		return held()
	}
	now := a.Now()
	if b.Retried || now.Before(b.AttemptAt.Add(max(2*b.Interval, b.RetryMax))) || !noticeRecoveryBudget(b, now) {
		return held()
	}
	r := boundTerminal(b).AssessOwnedNotice(ctx, b.Notice)
	if !r.OwnedNoticeEligible() || !sameNoticeOrigin(b, r.Key) {
		return held()
	}
	expected := b
	beforeEnter := func() error {
		now = a.Now()
		reserved := false
		_, err := config.UpdateState(s.paths, s.name, func(st *config.State) error {
			current := st.Terminal
			if current == nil || current.Stopped || !sameTerminalScope(*current, b) || current.Generation != generation || !current.AttemptAt.Equal(b.AttemptAt) || current.NotificationToken != b.NotificationToken || !slices.Equal(current.AttemptIDs, b.AttemptIDs) || current.Notice != b.Notice || current.NoticeCount != b.NoticeCount || current.Interval != b.Interval || current.RetryMax != b.RetryMax || current.MaxWakes != b.MaxWakes || !current.TypingStarted || !sameNoticeOrigin(*current, r.Key) {
				return errors.New("terminal recovery attempt changed")
			}
			if err := s.checkConnection(); err != nil {
				return err
			}
			if st.HooksPaused || manualWakeGate(*st) != "" || st.Runtime != nil && !st.Runtime.Retired || current.Retried || !ownTerminalTarget(current, b.Target) || !noticeRecoveryBudget(*current, now) {
				return nil
			}
			if l := st.Lease; l == nil || l.Holder != b.SessionID || l.Host != machineLabel() || !l.Expires.After(now) {
				return nil
			}
			// Honor common spacing as well as the binding retry delay, without minting
			// a new notification token. Invalid notification state fails closed.
			if err := st.Notification.Prepare(now, notificationLegacy(st)); err != nil {
				return err
			}
			if !a.notificationReady(s, st.Notification, now) {
				return nil
			}
			current.Retried = true // Durable fence runs only after the final Enter check.
			if now.Sub(current.AttemptAt) >= time.Hour {
				current.Reservations = append(current.Reservations, now)
			}
			current.Idle = nil
			expected = *current
			expected.AttemptIDs = slices.Clone(current.AttemptIDs)
			reserved = true
			return nil
		})
		if err != nil {
			return err
		}
		if !reserved {
			return errors.New("terminal recovery held")
		}
		return nil
	}
	result, sendErr := boundTerminal(expected).ResumeNotice(ctx, expected.NoticeCount, expected.Notice, r.Key, beforeEnter)
	return a.finishTerminal(s, generation, result, sendErr, true, expected)
}
