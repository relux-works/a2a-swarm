package tmuxwake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The muse carrier has no terminal composer. A bound Muse session is woken
// through the Muse session-message route by exact session id; liveness comes
// from the Muse session registry CLI. Pane text is never read and nothing is
// ever typed for this runtime.

var museSessionRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

// ValidMuseSessionID reports whether id can name one Muse session exactly.
func ValidMuseSessionID(id string) bool { return museSessionRE.MatchString(id) }

// MuseSessionRoute is the readiness route of the session-message carrier.
const MuseSessionRoute ReadinessRoute = "muse_session"

var (
	ErrMuseSessionDead      = errors.New("muse session dead or unknown")
	ErrMuseSenderUnverified = errors.New("muse session-message sender unverified")
	ErrMuseIngressClosed    = errors.New("muse session-message ingress closed")
	ErrMuseUnavailable      = errors.New("muse session route unavailable")
)

// MuseSender is the delivery and liveness seam of the muse carrier. The
// production implementation shells the Muse CLI; tests substitute a fake.
type MuseSender interface {
	SessionLive(ctx context.Context, sessionID string) (bool, error)
	SendSessionMessage(ctx context.Context, sessionID, body string) error
}

// museCommandTimeout bounds one Muse CLI invocation.
var museCommandTimeout = 10 * time.Second

// ExecMuseSender is the Muse CLI route: `muse sessions --json` for liveness
// and `muse session-message send --target <id> --json` for delivery. Only
// bounded classification tokens are ever read from its output.
type ExecMuseSender struct{ Binary string }

func (s ExecMuseSender) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return "muse"
}

func (s ExecMuseSender) SessionLive(ctx context.Context, sessionID string) (bool, error) {
	if !ValidMuseSessionID(sessionID) {
		return false, ErrMuseSessionDead
	}
	ctx, cancel := context.WithTimeout(ctx, museCommandTimeout)
	defer cancel()
	b, err := exec.CommandContext(ctx, s.binary(), "sessions", "--json").Output()
	if err != nil {
		return false, ErrMuseUnavailable
	}
	return parseMuseSessions(b, sessionID)
}

// parseMuseSessions reads one `muse sessions --json` document and reports
// exact-id membership. Anything malformed is unavailable, never live.
func parseMuseSessions(b []byte, sessionID string) (bool, error) {
	var list struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
		} `json:"sessions"`
	}
	if json.Unmarshal(b, &list) != nil {
		return false, ErrMuseUnavailable
	}
	for _, entry := range list.Sessions {
		if entry.SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

func (s ExecMuseSender) SendSessionMessage(ctx context.Context, sessionID, body string) error {
	if !ValidMuseSessionID(sessionID) {
		return ErrMuseSessionDead
	}
	ctx, cancel := context.WithTimeout(ctx, museCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.binary(), "session-message", "send", "--target", sessionID, "--json")
	cmd.Stdin = strings.NewReader(body)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if classified := classifyMuseSend(out.String()); classified != nil {
		return classified
	}
	if err != nil {
		return ErrMuseUnavailable
	}
	return nil
}

// classifyMuseSend maps the fixed refusal vocabulary of the session-message
// route onto sentinel errors. Output text itself is never retained.
func classifyMuseSend(out string) error {
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "sender_unverified"), strings.Contains(lower, "unverified_sender"):
		return ErrMuseSenderUnverified
	case strings.Contains(lower, "ingress_closed"):
		return ErrMuseIngressClosed
	case strings.Contains(lower, "target_not_found"),
		strings.Contains(lower, "target_gone"),
		strings.Contains(lower, "session_not_found"),
		strings.Contains(lower, "unknown session"),
		strings.Contains(lower, "no such session"),
		strings.Contains(lower, "session not found"),
		strings.Contains(lower, "unknown target"):
		return ErrMuseSessionDead
	}
	return nil
}

// PinMuse validates one Muse session as a wake target: the id must be well
// formed and the session live in the Muse registry. The returned target
// carries no tmux fields; the binding's session id names the session.
func PinMuse(ctx context.Context, sessionID string, sender MuseSender) (Target, error) {
	t := Target{Runtime: "muse"}
	if !ValidMuseSessionID(sessionID) {
		return t, errors.New("valid muse session id required")
	}
	if sender == nil {
		sender = ExecMuseSender{}
	}
	live, err := sender.SessionLive(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrMuseSessionDead) {
			return t, ErrMuseSessionDead
		}
		return t, ErrMuseUnavailable
	}
	if !live {
		return t, ErrMuseSessionDead
	}
	return t, nil
}

func (t BoundTerminal) museSender() MuseSender {
	if t.Muse != nil {
		return t.Muse
	}
	return ExecMuseSender{}
}

// assessMuse derives readiness from owner-process identity and Muse session
// liveness only. A live session has no composer state to observe: the
// session-message route queues the wake itself, so its input is empty.
func (t BoundTerminal) assessMuse(ctx context.Context) ReadinessAssessment {
	r := ReadinessAssessment{Runtime: runtimeResult(RuntimeSessionUnavailable, ""), Input: InputAssessment{State: InputUnavailable, Reason: InputReasonTargetUnavailable}}
	if !ValidMuseSessionID(t.SessionID) || t.Generation == "" || t.Target.OwnerPID <= 1 || t.Target.OwnerBirth == "" {
		r.Runtime = runtimeResult(RuntimeSessionDead, "")
		return r
	}
	birth, err := Birth(ctx, t.Target.OwnerPID)
	if err != nil || birth != t.Target.OwnerBirth {
		r.Runtime = runtimeResult(RuntimeSessionDead, "")
		return r
	}
	live, err := t.museSender().SessionLive(ctx, t.SessionID)
	if err != nil {
		if errors.Is(err, ErrMuseSessionDead) {
			r.Runtime = runtimeResult(RuntimeSessionDead, "")
		}
		return r
	}
	if !live {
		r.Runtime = runtimeResult(RuntimeSessionDead, "")
		return r
	}
	r.Runtime = runtimeResult(RuntimeNone, "")
	r.Input = InputAssessment{State: InputEmpty, Reason: InputReasonNone}
	r.InputObserved = true
	k := ReadinessKey{Route: MuseSessionRoute, Target: t.Target, Generation: t.Generation, SessionID: t.SessionID}
	if k.valid() {
		r.Key = k
	}
	return r
}

// sendMuse delivers the fixed mailbox notice through the session-message
// route. Refusals are named and final: there is no typing fallback.
func (t BoundTerminal) sendMuse(ctx context.Context, count int, expected ReadinessKey, beforeType func()) (SendResult, error) {
	at := time.Now().UTC()
	refused := func(reason SendReason) SendResult {
		return SendResult{Disposition: SendRefused, Outcome: SendMuseRefused, Reason: reason, ReasonAt: at}
	}
	if count <= 0 {
		return refused(ReasonUnavailable), errors.New("positive count required")
	}
	if !expected.valid() || expected.Route != MuseSessionRoute || expected.Target != t.Target || expected.Generation != t.Generation || expected.SessionID != t.SessionID || expected.ClaudeRegistryRoot != t.ClaudeRegistryRoot {
		return refused(ReasonUnavailable), nil
	}
	sender := t.museSender()
	live, err := sender.SessionLive(ctx, t.SessionID)
	if err != nil && errors.Is(err, ErrMuseSessionDead) || err == nil && !live {
		return refused(ReasonSessionDead), nil
	}
	if err != nil {
		return refused(ReasonSessionUnavailable), nil
	}
	// beforeType is the caller's durable attempt fence; on this carrier it
	// precedes the session-message call instead of literal typing.
	if beforeType != nil {
		beforeType()
	}
	if ctx.Err() != nil {
		return SendResult{Disposition: SendUncertain, Outcome: SendInterrupted, Reason: ReasonSessionUnavailable, ReasonAt: at}, ctx.Err()
	}
	err = sender.SendSessionMessage(ctx, t.SessionID, Notice(count))
	switch {
	case err == nil:
		return SendResult{Disposition: SendSubmitted, Outcome: SendWakeSubmitted}, nil
	case errors.Is(err, ErrMuseSenderUnverified):
		return refused(ReasonSenderUnverified), nil
	case errors.Is(err, ErrMuseSessionDead):
		return refused(ReasonSessionDead), nil
	case errors.Is(err, ErrMuseIngressClosed):
		return refused(ReasonIngressClosed), nil
	case ctx.Err() != nil:
		return SendResult{Disposition: SendUncertain, Outcome: SendInterrupted, Reason: ReasonSessionUnavailable, ReasonAt: at}, ctx.Err()
	default:
		return SendResult{Disposition: SendUncertain, Outcome: SendDeliveryUncertain, Reason: ReasonSessionUnavailable, ReasonAt: at}, err
	}
}

// resumeMuseNotice never resumes anything: no draft exists on this carrier.
// An uncertain attempt is retried only as a fresh bounded send by the caller.
func (t BoundTerminal) resumeMuseNotice(ctx context.Context) (SendResult, error) {
	reason := ReasonSessionUnavailable
	if live, err := t.museSender().SessionLive(ctx, t.SessionID); err == nil && !live || errors.Is(err, ErrMuseSessionDead) {
		reason = ReasonSessionDead
	}
	return SendResult{Disposition: SendRefused, Outcome: SendMuseRefused, Reason: reason, ReasonAt: time.Now().UTC()}, nil
}
