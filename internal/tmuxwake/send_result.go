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
	Disposition SendDisposition
	Outcome     SendOutcome
	Reason      SendReason
	ReasonAt    time.Time
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
	inspect := func(ctx context.Context, owned string) (Box, bool) {
		r, b := t.assess(ctx, owned)
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
	if result.Disposition == SendDraft || result.Disposition == SendRefused {
		result.Reason, result.ReasonAt = lastReason, lastAt
		if result.Outcome == SendSuppressedWidth && err == nil {
			result.Reason = ReasonTargetOrWidth
		}
	}
	return result, err
}

// Filling a composer can move its row as the UI reallocates vertical space.
// Only the row may differ, and only after this invocation's exact owned notice
// passed complete-input and end-cursor checks. Every other key field stays pinned.
func sameKeyAfterOwnedTyping(current, expected ReadinessKey) bool {
	current.ComposerRow = expected.ComposerRow
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
)

func SafeSendReason(r SendReason) SendReason {
	switch r {
	case ReasonUnavailable, ReasonKeyChanged, ReasonKeyRowChanged, ReasonKeyStatusChanged, ReasonKeyWidthChanged, ReasonTargetOrWidth, ReasonBusyOrDialog, ReasonLayoutUnrecognized, ReasonCursorMismatch, ReasonForeignInput, ReasonAwaitingRender:
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
		if owned == "" && r.Key.ComposerRow != expected.ComposerRow {
			return ReasonKeyRowChanged
		}
	}
	if owned != "" && b.Text == "" {
		return ReasonAwaitingRender
	}
	return ReasonUnavailable
}
