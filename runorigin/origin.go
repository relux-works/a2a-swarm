// Package runorigin defines the optional P17 typed-result seam. Stock upstream
// builds do not link a Producer. Generic admission.Decision is not this capability.
//
// Generations come only from the user manager's generation journal: exactly 32
// lowercase hexadecimal characters, excluding the all-zero value. They are opaque
// strings compared bytewise, never numbers. Principal.Generation and
// GenerationAuthority.CurrentGeneration now use string instead of uint64; this
// is a breaking public API change and callers must migrate to the journal spelling.
package runorigin

import (
	"context"
	"errors"
	"strings"
)

const (
	Refused      = "run_origin_refused"
	Unavailable  = "run_origin_unavailable"
	NotSupported = "origin_not_supported"
)

// Errors are closed, secret-free classifications. Only ErrUnavailable is retryable.
var (
	ErrRefused      = errors.New(Refused)
	ErrUnavailable  = errors.New(Unavailable)
	ErrNotSupported = errors.New(NotSupported)
)

type Principal struct {
	UID        uint32 `json:"uid"`
	Generation string `json:"generation"`
}

type EventRef struct {
	RoomID  string `json:"room_id"`
	EventID string `json:"event_id"`
}

// Origin contains only verified inner-request facts, never a Matrix event ID.
// Shape validation cannot establish signature/profile/grant authenticity: the
// qualified downstream Producer is responsible for those checks.
type Origin struct {
	Kind      string `json:"kind"`
	EventID   string `json:"event_id"`
	GrantID   string `json:"grant_id"`
	TipHash   string `json:"tip_hash"`
	Mode      string `json:"mode"`
	CommandID string `json:"command_id"`
}

func (o Origin) Valid() bool {
	return o.Kind == "admission" && o.Mode == "enforcing" && o.CommandID == "" &&
		LowerHex(o.EventID, 64) && LowerHex(o.GrantID, 64) && LowerHex(o.TipHash, 64)
}

func LowerHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}

// ValidGeneration checks the user manager generation journal's token spelling.
func ValidGeneration(s string) bool {
	return LowerHex(s, 32) && s != "00000000000000000000000000000000"
}

type Result struct {
	IdentityRegistrationNumber string
	Origin                     Origin
}

// Selection has no station-supplied identity, session token, policy or origin.
// Launcher is derived from kernel credentials and current protected role pins.
type Selection struct {
	Launcher Principal
	Event    EventRef
	Input    Principal
}

// Producer is an explicit build-time dependency, not a checker-name lookup.
// Open must independently resolve protected identity/connection/Matrix-user/
// policy/Keymaker-registration mapping; read the retained event itself; create
// its own bounded checker and private scope; verify exact signed inner profile,
// grant and current tip. Missing mapping/facts refuses, never generic Live/none.
// Open and Session methods must honor ctx. A session belongs to one preparation.
// On Open error, a nonnil session is still closed by the caller.
type Producer interface {
	Open(context.Context, Selection) (Session, error)
}

// Session owns its checker lifetime. Prepare returns rich authenticated facts;
// Validate freshly rechecks admission and pins and returns the SAME immutable
// mapping/origin. Close is called exactly once even on lost response/timeout.
// Close must release local resources promptly and must not perform network IO.
type Session interface {
	Prepare(context.Context) (Result, error)
	Validate(context.Context) (Result, error)
	Close() error
}

// GenerationAuthority reads CURRENT protected authority, never the station
// registry or runtime notification generation. Values must pass ValidGeneration
// and come from the user manager's generation journal. Missing authority is a refusal;
// return ErrUnavailable only for a retryable dependency outage.
type GenerationAuthority interface {
	CurrentGeneration(context.Context, uint32) (string, error)
}

type PreparedOrigin struct {
	RequestID                  string    `json:"request_id"`
	LaunchNonce                string    `json:"launch_nonce"`
	InputPrincipal             Principal `json:"input_principal"`
	IdentityRegistrationNumber string    `json:"identity_registration_number"`
	SourceIncarnation          string    `json:"source_incarnation"`
	PreparedID                 string    `json:"prepared_id"`
	Origin                     Origin    `json:"origin"`
}
