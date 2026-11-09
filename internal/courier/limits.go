package courier

import (
	"errors"
	"time"
)

const (
	KeepaliveInterval     = 20 * time.Second
	StationReadDeadline   = 3 * KeepaliveInterval
	LockReleaseBound      = 5 * time.Second
	HookBudget            = 5 * time.Second
	HookStateReserve      = 250 * time.Millisecond
	MaxStationInvocations = 4
	MaxInvocations        = 32
	OutputChunkBytes      = 1024
	HookOutputBytes       = 1 << 20
	HookFailureLines      = 256
	LogQueueLines         = 64
	AcceptBackoff         = 50 * time.Millisecond
)

// Diagnostic is a closed, request-independent error class.
type Diagnostic string

const (
	EnrollmentInvalid Diagnostic = "enrollment_invalid"
	Unavailable       Diagnostic = "courier_unavailable"
	IdentityMismatch  Diagnostic = "identity_mismatch"
	NotAllowed        Diagnostic = "not_allowed"
	IdleTimeout       Diagnostic = "courier_idle_timeout"
	Busy              Diagnostic = "courier_busy"
)

func (d Diagnostic) Error() string { return string(d) }
func validDiagnostic(d Diagnostic) bool {
	switch d {
	case EnrollmentInvalid, Unavailable, IdentityMismatch, NotAllowed, IdleTimeout, Busy:
		return true
	}
	return false
}
func Class(err error) Diagnostic {
	var d Diagnostic
	if errors.As(err, &d) && validDiagnostic(d) {
		return d
	}
	return Unavailable
}
