package tmuxwake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

// The muse carrier has no terminal composer. A bound Muse session is woken
// through the Muse session-message route by exact session id; liveness comes
// from the Muse session registry CLI. Pane text is never read and nothing is
// ever typed for this runtime.

// The first character is alphanumeric, so an id can never be read as a flag.
var museSessionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

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

// museWaitDelay bounds how long a cancelled invocation may linger when a
// descendant of the Muse launcher inherited its output pipe.
const museWaitDelay = time.Second

// Output retained from one Muse invocation is bounded: the sessions
// document is parsed whole, a send only yields classification tokens.
const (
	museSessionsOutputMax = 1 << 20
	museSendOutputMax     = 64 << 10
)

// ExecMuseSender is the Muse CLI route: `muse sessions --json` for liveness
// and `muse session-message send --target <id> --json` for delivery. Only
// bounded classification tokens are ever read from its output. Every
// invocation runs with an explicit environment: Env when set (tests and
// pinned callers), the process environment otherwise, always with
// MUSE_NO_AUTO_UPDATE=1 so the supervised consumer can never trigger the
// launcher's credential-bearing self-update. XDG_DATA_HOME in that
// environment selects the Muse session registry, so the detached consumer
// carries the bind-time value (see the bind environment allowlist).
type ExecMuseSender struct {
	Binary string
	Env    []string
}

func (s ExecMuseSender) binary() string {
	if s.Binary != "" {
		return s.Binary
	}
	return "muse"
}

func (s ExecMuseSender) env() []string {
	base := s.Env
	if base == nil {
		base = os.Environ()
	}
	env := make([]string, 0, len(base)+1)
	for _, kv := range base {
		if strings.HasPrefix(kv, "MUSE_NO_AUTO_UPDATE=") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "MUSE_NO_AUTO_UPDATE=1")
}

func (s ExecMuseSender) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, s.binary(), args...)
	cmd.Env = s.env()
	cmd.WaitDelay = museWaitDelay
	return cmd
}

// museOutput is a bounded sink: past the cap, bytes are counted and
// dropped. One sink may serve stdout and stderr together, hence the lock.
type museOutput struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (o *museOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if room := o.max - len(o.buf); room > 0 {
		o.buf = append(o.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

func (s ExecMuseSender) SessionLive(ctx context.Context, sessionID string) (bool, error) {
	if !ValidMuseSessionID(sessionID) {
		return false, ErrMuseSessionDead
	}
	ctx, cancel := context.WithTimeout(ctx, museCommandTimeout)
	defer cancel()
	cmd := s.command(ctx, "sessions", "--json")
	out := &museOutput{max: museSessionsOutputMax}
	cmd.Stdout = out
	cmd.Stderr = &museOutput{max: museSendOutputMax}
	if err := cmd.Run(); err != nil {
		return false, ErrMuseUnavailable
	}
	return parseMuseSessions(out.buf, sessionID)
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
	cmd := s.command(ctx, "session-message", "send", "--target", sessionID, "--json")
	cmd.Stdin = strings.NewReader(body)
	out := &museOutput{max: museSendOutputMax}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err == nil {
		// Exit 0 is the success shape. Refusal tokens are classified only
		// on a non-zero exit, never from a successful reply's text.
		return nil
	}
	if classified := classifyMuseSend(string(out.buf)); classified != nil {
		return classified
	}
	return ErrMuseUnavailable
}

// classifyMuseSend maps the fixed refusal vocabulary of the session-message
// route onto sentinel errors. It runs only after a non-zero exit. Output
// text itself is never retained. Tokens that name a target which cannot
// resolve to the pinned session are dead; admission and availability
// tokens stay unavailable, which the caller records as uncertain delivery
// (never submitted), eligible for its one bounded fresh retry.
func classifyMuseSend(out string) error {
	lower := strings.ToLower(out)
	switch {
	case strings.Contains(lower, "sender_unverified"), strings.Contains(lower, "unverified_sender"):
		return ErrMuseSenderUnverified
	case strings.Contains(lower, "ingress_closed"):
		return ErrMuseIngressClosed
	case strings.Contains(lower, "target_not_found"),
		strings.Contains(lower, "target_gone"),
		strings.Contains(lower, "target_not_running"),
		strings.Contains(lower, "session_not_found"),
		strings.Contains(lower, "wrong_target"),
		strings.Contains(lower, "ambiguous_target_name"),
		strings.Contains(lower, "unknown session"),
		strings.Contains(lower, "no such session"),
		strings.Contains(lower, "session not found"),
		strings.Contains(lower, "unknown target"):
		return ErrMuseSessionDead
	case strings.Contains(lower, "admission_queue_full"),
		strings.Contains(lower, "admission_retry_later"),
		strings.Contains(lower, "target_unavailable"),
		strings.Contains(lower, "target_unverified"),
		strings.Contains(lower, "self_send_wrong_path"),
		strings.Contains(lower, "session_messaging_unavailable"),
		strings.Contains(lower, "delivery unconfirmed"):
		return ErrMuseUnavailable
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
