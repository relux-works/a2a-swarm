package daemon

import (
	"errors"
	"time"

	"github.com/relux-works/a2a-swarm/internal/signedpost"
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
	EnrollmentInvalid         Diagnostic = "enrollment_invalid"
	Unavailable               Diagnostic = "daemon_unavailable"
	IdentityMismatch          Diagnostic = "identity_mismatch"
	NotAllowed                Diagnostic = "not_allowed"
	IdleTimeout               Diagnostic = "daemon_idle_timeout"
	Busy                      Diagnostic = "daemon_busy"
	LeaseConflict             Diagnostic = "daemon_lease_conflict"
	StationAuthorsUnavailable Diagnostic = "station_authors_unavailable"
	SignedCommandRefused      Diagnostic = "signed_command_refused"
	SignedPostRequired        Diagnostic = "signed_post_required"
)

func (d Diagnostic) Error() string { return string(d) }

var diagnosticClasses = [...]Diagnostic{EnrollmentInvalid, Unavailable, IdentityMismatch, NotAllowed, IdleTimeout, Busy, LeaseConflict}

// SignedPolicyRefusal identifies deliberate host-policy refusals, not hook
// transport failures. Stations must not turn these into failure-journal writes.
func SignedPolicyRefusal(d Diagnostic) bool {
	switch d {
	case StationAuthorsUnavailable, SignedCommandRefused, SignedPostRequired,
		Diagnostic(signedpost.InvalidEnvelope), Diagnostic(signedpost.AuthorMismatch), Diagnostic(signedpost.InvalidSignature),
		Diagnostic(signedpost.RoomMismatch), Diagnostic(signedpost.RecipientsMismatch), Diagnostic(signedpost.TextMismatch),
		Diagnostic(signedpost.MetadataMismatch), Diagnostic(signedpost.Expired), Diagnostic(signedpost.InvalidLifetime),
		Diagnostic(signedpost.MissingAttestation), Diagnostic(signedpost.InvalidAttestation), Diagnostic(signedpost.LargeAttestation),
		Diagnostic(signedpost.AttestationMismatch), Diagnostic(signedpost.BindingUnavailable):
		return true
	}
	return false
}

func validDiagnostic(d Diagnostic) bool {
	if SignedPolicyRefusal(d) {
		return true
	}
	for _, admitted := range diagnosticClasses {
		if d == admitted {
			return true
		}
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
