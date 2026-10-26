package tmuxwake

import "context"

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
	refused := SendResult{SendRefused, SendSuppressedInput}
	if !expected.valid() || expected.Target != t.Target || expected.Generation != t.Generation || expected.SessionID != t.SessionID || expected.ClaudeRegistryRoot != t.ClaudeRegistryRoot {
		return refused, nil
	}
	inspect := func(ctx context.Context, owned string) (Box, bool) {
		r, b := t.assess(ctx, owned)
		return b, r.Key == expected && r.runtimeEligible() && (b.Text == "" && r.Eligible() || owned != "" && b.Text == owned && r.Input.State == InputNonempty)
	}
	// The last check follows beforeType, so a test/caller callback cannot turn
	// a qualified key into unconditional permission. The final I/O gap remains.
	preType := func() bool { _, ok := inspect(ctx, ""); return ok && ctx.Err() == nil }
	return t.Target.sendWith(ctx, count, "", beforeType, afterType, inspect, preType)
}
