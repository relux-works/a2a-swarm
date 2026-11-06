package tmuxwake

import (
	"context"
	"strings"
	"time"
)

type SendDisposition string
type SendOutcome string

const (
	SendRefused          SendDisposition = "refused"
	SendDraft            SendDisposition = "draft"
	SendUncertain        SendDisposition = "uncertain"
	SendSubmitted        SendDisposition = "submitted"
	SendSuppressedInput  SendOutcome     = "suppressed: busy, draft or unknown input"
	SendSuppressedWidth  SendOutcome     = "suppressed: target or width"
	SendDraftUnavailable SendOutcome     = "notice left in draft: input unavailable"
	SendDraftChanged     SendOutcome     = "notice left in draft: changed input"
	SendTypeUncertain    SendOutcome     = "uncertain: type"
	SendEnterUncertain   SendOutcome     = "uncertain: Enter"
	SendInterrupted      SendOutcome     = "uncertain: interrupted"
	SendWakeSubmitted    SendOutcome     = "wake submitted"
)

type SendResult struct {
	Disposition    SendDisposition
	Outcome        SendOutcome
	Reason         SendReason
	ReasonAt       time.Time
	NoticePosition *ComposerPosition
}

// Valid rejects inconsistent/unknown pairs. In particular a nil transport
// error is never sufficient to credit a submitted wake.
func (r SendResult) Valid() bool {
	switch r.Disposition {
	case SendRefused:
		return r.Outcome == SendSuppressedInput || r.Outcome == SendSuppressedWidth
	case SendDraft:
		return r.Outcome == SendDraftUnavailable || r.Outcome == SendDraftChanged
	case SendUncertain:
		return r.Outcome == SendTypeUncertain || r.Outcome == SendEnterUncertain || r.Outcome == SendInterrupted
	case SendSubmitted:
		return r.Outcome == SendWakeSubmitted
	default:
		return false
	}
}
func (r SendResult) Text() string {
	if !r.Valid() {
		return string(SendInterrupted)
	}
	return string(r.Outcome)
}

func (t BoundTerminal) Send(ctx context.Context, count int, expected ReadinessKey, beforeType, afterType func()) (SendResult, error) {
	refused := SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput, Reason: ReasonUnavailable, ReasonAt: time.Now().UTC()}
	if !expected.valid() || expected.Target != t.Target || expected.Generation != t.Generation || expected.SessionID != t.SessionID || expected.ClaudeRegistryRoot != t.ClaudeRegistryRoot {
		return refused, nil
	}
	lastReason, lastAt := ReasonUnavailable, time.Now().UTC()
	var lastPosition *ComposerPosition
	inspect := func(ctx context.Context, owned string) (Box, bool) {
		r, b := t.assess(ctx, owned)
		if owned != "" && b.Text == owned && r.Input.State == InputNonempty {
			position := b.position()
			lastPosition = &position
		}
		keyMatches := r.Key == expected
		if owned != "" && b.Text == owned && r.Input.State == InputNonempty {
			keyMatches = sameKeyAfterOwnedTyping(r.Key, expected)
		}
		ok := keyMatches && r.runtimeEligible() && (b.Text == "" && r.Eligible() || owned != "" && b.Text == owned && r.Input.State == InputNonempty)
		if !ok || owned != "" && b.Text != owned {
			lastReason, lastAt = sendRefusalReason(r, expected, b, owned), time.Now().UTC()
		}
		return b, ok
	}
	// The last check follows beforeType, so a test/caller callback cannot turn
	// a qualified key into unconditional permission. The final I/O gap remains.
	preType := func() bool { _, ok := inspect(ctx, ""); return ok && ctx.Err() == nil }
	result, err := t.Target.sendWith(ctx, count, "", beforeType, afterType, inspect, preType)
	if result.Disposition == SendDraft || result.Disposition == SendUncertain {
		result.NoticePosition = t.captureNoticePosition(ctx, Notice(count), expected.Route == RegistryNamed)
		if result.NoticePosition == nil {
			result.NoticePosition = lastPosition
		}
	}
	if result.Disposition == SendDraft || result.Disposition == SendRefused {
		result.Reason, result.ReasonAt = lastReason, lastAt
		if result.Outcome == SendSuppressedWidth && err == nil {
			result.Reason = ReasonTargetOrWidth
		}
	}
	return result, err
}

// Exact complete owned input may move within the composer or leave the cursor
// elsewhere. Runtime identity, registry evidence and width stay pinned.
func sameKeyAfterOwnedTyping(current, expected ReadinessKey) bool {
	current.ComposerRow = expected.ComposerRow
	current.ComposerColumn = expected.ComposerColumn
	return current == expected
}

// SendReason is a closed, bounded vocabulary: never copy captured text or errors.
type SendReason string

const (
	ReasonUnavailable        SendReason = "unavailable"
	ReasonKeyChanged         SendReason = "key_changed"
	ReasonKeyRowChanged      SendReason = "key_row_changed"
	ReasonKeyStatusChanged   SendReason = "key_status_changed"
	ReasonKeyWidthChanged    SendReason = "key_width_changed"
	ReasonTargetOrWidth      SendReason = "target_or_width"
	ReasonBusyOrDialog       SendReason = "busy_or_dialog"
	ReasonLayoutUnrecognized SendReason = "layout_unrecognized"
	ReasonCursorMismatch     SendReason = "cursor_mismatch"
	ReasonForeignInput       SendReason = "input_nonempty_foreign"
	ReasonAwaitingRender     SendReason = "awaiting_render"
	ReasonRecoveryHeld       SendReason = "recovery_held"
	ReasonFenceTimeout       SendReason = "recovery_fence_timeout"
)

func SafeSendReason(r SendReason) SendReason {
	switch r {
	case ReasonUnavailable, ReasonKeyChanged, ReasonKeyRowChanged, ReasonKeyStatusChanged, ReasonKeyWidthChanged, ReasonTargetOrWidth, ReasonBusyOrDialog, ReasonLayoutUnrecognized, ReasonCursorMismatch, ReasonForeignInput, ReasonAwaitingRender, ReasonRecoveryHeld, ReasonFenceTimeout:
		return r
	}
	return ReasonUnavailable
}
func sendRefusalReason(r ReadinessAssessment, expected ReadinessKey, b Box, owned string) SendReason {
	if r.Runtime.Reason == RuntimeBusy || r.Input.Reason == InputReasonBusyOrDialog {
		return ReasonBusyOrDialog
	}
	if r.Runtime.State == RuntimeRefused {
		return ReasonUnavailable
	}
	switch r.Input.Reason {
	case InputReasonCursorMismatch:
		return ReasonCursorMismatch
	case InputReasonLayoutUnrecognized:
		return ReasonLayoutUnrecognized
	case InputReasonTargetUnavailable, InputReasonCaptureUnavailable:
		return ReasonUnavailable
	}
	if b.Text != "" && b.Text != owned {
		if owned != "" && strings.HasPrefix(owned, b.Text) {
			return ReasonAwaitingRender
		}
		return ReasonForeignInput
	}
	if r.Key != (ReadinessKey{}) {
		if r.Key.PaneWidth != expected.PaneWidth {
			return ReasonKeyWidthChanged
		}
		if r.Key.RegistryStatus != expected.RegistryStatus || r.Key.RegistryStatusAt != expected.RegistryStatusAt {
			return ReasonKeyStatusChanged
		}
		if !sameKeyAfterOwnedTyping(r.Key, expected) {
			return ReasonKeyChanged
		}
		if owned == "" && r.Key.ComposerColumn != expected.ComposerColumn {
			return ReasonKeyChanged
		}
		if owned == "" && r.Key.ComposerRow != expected.ComposerRow {
			return ReasonKeyRowChanged
		}
	}
	if owned != "" && b.Text == "" {
		return ReasonAwaitingRender
	}
	return ReasonUnavailable
}

// ResumeNotice sends Enter only, never literal text or clearing keys. The
// consumer supplies a journal-fenced identity; each capture rechecks readiness.
func (t BoundTerminal) ResumeNotice(ctx context.Context, count int, notice string, expected ReadinessKey, beforeEnter ...func() error) (SendResult, error) {
	refused := SendResult{Disposition: SendRefused, Outcome: SendSuppressedInput, Reason: ReasonUnavailable, ReasonAt: time.Now().UTC()}
	if count <= 0 || notice != Notice(count) || !expected.valid() || expected.Target != t.Target || expected.Generation != t.Generation || expected.SessionID != t.SessionID || expected.ClaudeRegistryRoot != t.ClaudeRegistryRoot {
		return refused, nil
	}
	lastReason, lastAt := ReasonUnavailable, time.Now().UTC()
	inspect := func(ctx context.Context, owned string) (Box, bool) {
		r, b := t.assessRecovery(ctx, owned)
		// Unlike fresh typing, continuation cannot normalize a changed composer.
		ok := owned == notice && b.Text == notice && r.OwnedNoticeEligible() && r.Key == expected
		if !ok {
			lastReason, lastAt = sendRefusalReason(r, expected, b, owned), time.Now().UTC()
			if r.OwnedNoticeEligible() && sameKeyAfterOwnedTyping(r.Key, expected) && r.Key != expected {
				lastReason = ReasonKeyChanged
				if r.Key.ComposerRow != expected.ComposerRow {
					lastReason = ReasonKeyRowChanged
				}
			}
		}
		return b, ok
	}
	result, err := t.Target.sendWith(ctx, count, notice, nil, nil, inspect, nil, beforeEnter...)
	if result.Disposition == SendRefused || result.Disposition == SendDraft {
		if result.Reason == "" {
			result.Reason, result.ReasonAt = lastReason, lastAt
		}
		if result.Outcome == SendSuppressedWidth {
			result.Reason = ReasonTargetOrWidth
		}
	}
	return result, err
}
