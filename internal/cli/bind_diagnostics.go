package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Preserve the innermost observed stage through outer retry/dispatch handlers.
// Error deliberately never formats the wrapped (possibly secret-bearing) error.
type terminalFailure struct {
	phase string
	err   error
}

func (e *terminalFailure) Error() string {
	d := terminalErrorDiagnostic(e.phase, e.err, time.Time{})
	return "binding " + d.Phase + ": " + d.Class
}
func (e *terminalFailure) Unwrap() error { return e.err }
func terminalAt(phase string, err error) error {
	if err == nil {
		return nil
	}
	var existing *terminalFailure
	if errors.As(err, &existing) {
		return err
	}
	return &terminalFailure{phase, err}
}

func terminalErrorDiagnostic(phase string, err error, now time.Time) config.TerminalDiagnostic {
	var stage *terminalFailure
	if errors.As(err, &stage) {
		phase = stage.phase
	}
	var connection *deliveryConnectionFailure
	if errors.As(err, &connection) {
		phase = "connection"
	}
	class := "unavailable"
	var lease *config.LeaseHeldError
	var syntax *json.SyntaxError
	var value *json.UnmarshalTypeError
	var remote *matrix.Error
	var transport net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		class = "timeout"
	case errors.Is(err, context.Canceled):
		class = "cancelled"
	case errors.Is(err, config.ErrLockTimeout):
		class = "locked"
	case errors.As(err, &lease):
		class = "lease_conflict"
	case errors.Is(err, os.ErrPermission):
		class = "permission"
	case errors.As(err, &syntax), errors.As(err, &value):
		class = "invalid"
	case errors.Is(err, matrix.ErrAuth):
		class = "authentication"
	case errors.As(err, &remote):
		class = "remote"
	case errors.As(err, &transport):
		class = "transport"
	case errors.Is(err, tmuxwake.ErrTargetGone):
		class = "target_gone"
	}
	return terminalDiagnostic(phase, class, "error", now)
}
func terminalDiagnostic(phase, class, result string, now time.Time) config.TerminalDiagnostic {
	d := config.TerminalDiagnostic{Phase: phase, Class: class, Result: result}
	if !now.IsZero() {
		at := now.UTC()
		d.At = &at
	}
	return d.Safe()
}
func (a *App) recordTerminalFailure(s *session, generation, phase string, err error) {
	d := terminalErrorDiagnostic(phase, err, a.Now())
	// Failure to persist does not change the original failed stage or authorize
	// dispatch. Connection guards still apply; stderr is the bounded fallback.
	if s.checkConnection() == nil {
		// One nonblocking lock attempt: diagnostics must not wait behind the
		// state-lock failure they report. Existing operational timing is unchanged.
		_, _ = config.UpdateStateBy(s.paths, s.name, time.Now(), func(st *config.State) error {
			if err := s.checkConnection(); err != nil {
				return err
			}
			if st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.Stopped {
				return errors.New("binding retired")
			}
			st.Terminal.Diagnostic = &d
			st.Terminal.Idle = nil
			return nil
		})
	}
	fmt.Fprintf(a.Stderr, "binding consumer unavailable; bounded retry phase=%s class=%s\n", d.Phase, d.Class)
}
func (a *App) terminalSuppressed(s *session, generation, phase, class, outcome string) error {
	return a.terminalUpdate(s, generation, func(b *config.TerminalBinding) {
		b.Idle = nil
		b.Outcome = outcome
		d := terminalDiagnostic(phase, class, "suppressed", a.Now())
		b.Diagnostic = &d
	})
}

// The mailbox is the only pending/receipt authority. TS is Matrix origin time,
// not first enqueue time. Unknown timestamps prevent an all-pending age claim.
type terminalPending struct {
	State                     string `json:"state"`
	Count                     *int   `json:"count"`
	AgeState                  string `json:"age_state"`
	OldestKnownEventAt        string `json:"oldest_known_event_at,omitempty"`
	OldestKnownEventAgeMillis *int64 `json:"oldest_known_event_age_ms,omitempty"`
	MissingTimestamps         int    `json:"missing_timestamps"`
	InvalidTimestamps         int    `json:"invalid_timestamps"`
	FutureTimestamps          int    `json:"future_timestamps"`
}

func terminalPendingOf(entries []config.MailEntry, now time.Time) terminalPending {
	count := len(entries)
	p := terminalPending{State: "available", Count: &count, AgeState: "known"}
	var oldest int64
	for _, e := range entries {
		switch {
		case e.TS == 0:
			p.MissingTimestamps++
		case e.TS < 0:
			p.InvalidTimestamps++
		case e.TS > now.UnixMilli():
			p.FutureTimestamps++
		default:
			if oldest == 0 || e.TS < oldest {
				oldest = e.TS
			}
		}
	}
	if oldest != 0 {
		p.OldestKnownEventAt = time.UnixMilli(oldest).UTC().Format(time.RFC3339Nano)
		age := now.UnixMilli() - oldest
		p.OldestKnownEventAgeMillis = &age
	}
	switch {
	case count == 0:
		p.AgeState = "empty"
	case p.InvalidTimestamps+p.FutureTimestamps > 0:
		p.AgeState = "invalid"
	case p.MissingTimestamps > 0:
		p.AgeState = "unknown"
	}
	return p
}

// Returning this private sentinel aborts UpdateState's save after a consistent
// locked read. Status neither rewrites state nor syncs/marks/receipts the mailbox.
var errTerminalStatusRead = errors.New("terminal status read only")

func (a *App) readTerminalStatus(s *session) (config.State, terminalPending, error) {
	pending := terminalPending{State: "unavailable", AgeState: "unknown"}
	if err := s.checkConnection(); err != nil {
		return config.State{}, pending, terminalAt("connection", err)
	}
	st, err := config.UpdateStateBy(s.paths, s.name, time.Now().Add(nativeRPCTimeout), func(_ *config.State) error {
		if err := s.checkConnection(); err != nil {
			return terminalAt("connection", err)
		}
		mb, err := config.OpenMailbox(s.paths, s.name)
		if err == nil {
			pending = terminalPendingOf(mb.Pending(), a.Now())
		}
		return errTerminalStatusRead
	})
	if errors.Is(err, errTerminalStatusRead) {
		return st, pending, nil
	}
	return st, pending, terminalAt("state", err)
}

func terminalPendingText(p terminalPending) string {
	if p.Count == nil {
		return "pending unavailable; origin age unknown"
	}
	text := fmt.Sprintf("pending=%d origin-age=%s", *p.Count, p.AgeState)
	if p.OldestKnownEventAgeMillis != nil {
		text += fmt.Sprintf(" oldest-known-event-at=%s age-ms=%d", p.OldestKnownEventAt, *p.OldestKnownEventAgeMillis)
	}
	return text
}

// Preserve known legacy outcomes without rendering arbitrary persisted text.
func safeTerminalOutcome(outcome string) string {
	switch outcome {
	case "", "starting", "unbound", "target exited or replaced", "consumer unavailable; publishing unknown", "consumer unavailable; retrying",
		"suppressed: manual gate", "suppressed: native binding", "suppressed: binding target authority", "suppressed: lease authority",
		"armed: no new mail", "suppressed: busy, draft or unknown input", "suppressed: hourly rate", "attempt reserved", "armed: qualifying idle",
		"suppressed: pending attempt input", "uncertain legacy attempt; retained mail requires inbox", "uncertain partial attempt; mail retained",
		"uncertain attempt; bounded retry held", "notice left in draft: input unavailable", "notice left in draft: changed input",
		"suppressed: target or width", "uncertain: type", "uncertain: interrupted", "uncertain: Enter", "wake submitted", "uncertain: dispatch outcome":
		return outcome
	}
	return "unknown outcome"
}
