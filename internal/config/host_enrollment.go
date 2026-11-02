package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"time"
	"unicode"
	"unicode/utf8"
)

var ErrHostEnrollment = errors.New("host enrollment unverified")

// HostScope is exact source identity, not an environment-based grant. Session
// is the source session (up to 200 bytes), distinct from Host's native thread.
type HostScope struct {
	Agent         string `json:"agent"`
	Member        string `json:"member"`
	Connection    string `json:"connection"`
	Configuration string `json:"configuration"`
	Session       string `json:"session"`
	NativeThread  string `json:"native_thread"`
	Generation    string `json:"generation"`
	Consumer      string `json:"consumer"`
}

// HostAttachment comes ONLY from the admitted manager's actual launch receipt.
// Callers must compare actual provider/tool identity at their owning boundary;
// decoding a JSON object or reading environment selectors cannot supply trust.
// These checks assume cooperating same-UID binaries, not OS-user isolation.
type HostAttachment struct {
	Reference string    `json:"reference"`
	Scope     HostScope `json:"scope"`
	Lease     Lease     `json:"lease"`
}

type HostEnrollment struct {
	Version    int            `json:"version"`
	Attachment HostAttachment `json:"attachment"`
}

func hostRef(s string, limit int) bool {
	if len(s) == 0 || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func (s HostScope) valid() bool {
	if ValidateName(s.Agent) != nil || !hostRef(s.Member, 512) || !hostRef(s.Connection, 128) || !hostRef(s.Configuration, 512) || !hostRef(s.Session, 200) || !hostRef(s.NativeThread, 128) || !hostRef(s.Generation, 128) || !hostRef(s.Consumer, 128) {
		return false
	}
	for _, c := range s.NativeThread {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == ':' || c == '/' || c == '@' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}
func (a HostAttachment) valid() bool {
	return hostRef(a.Reference, 512) && a.Scope.valid() && a.Lease.Holder == a.Scope.Session && hostRef(a.Lease.Host, 128) && a.Lease.PID > 0 && !a.Lease.Expires.IsZero()
}
func (e HostEnrollment) Validate() error {
	if e.Version != 1 || !e.Attachment.valid() {
		return ErrHostEnrollment
	}
	return nil
}

// Unknown fields/versions are never discarded by a cooperative state writer.
// An old executable that does not contain this guard is outside this guarantee.
func (e *HostEnrollment) UnmarshalJSON(data []byte) error {
	type plain HostEnrollment
	var next plain
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&next); err != nil {
		return ErrHostEnrollment
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrHostEnrollment
	}
	if err := HostEnrollment(next).Validate(); err != nil {
		return err
	}
	*e = HostEnrollment(next)
	return nil
}

func (s *State) hostAdmission(a HostAttachment, actual HostScope, now time.Time) error {
	if now.IsZero() || !a.valid() || !actual.valid() || a.Scope != actual || !a.Lease.Expires.After(now) || s.Lease == nil || *s.Lease != a.Lease {
		return ErrHostEnrollment
	}
	// Existing native/terminal consumers must be retired by their own lifecycle
	// before Host enrollment. Enrollment never force-releases or heals one.
	if s.Runtime != nil && !s.Runtime.Retired || s.Terminal != nil && !s.Terminal.Stopped {
		return ErrHostEnrollment
	}
	return nil
}

// EnrollHost is called under the existing control + state serialization, with
// the sole consumer lock held by the admitted adapter. It performs no I/O,
// mailbox receipt, dispatch or lease acquisition. SaveState must succeed before
// its caller returns an enrollment receipt; 133 is required before any effect.
func (s *State) EnrollHost(a HostAttachment, actual HostScope, now time.Time) error {
	if err := s.hostAdmission(a, actual, now); err != nil {
		return err
	}
	if s.HostEnrollment != nil {
		if s.HostEnrollment.Validate() != nil || s.HostEnrollment.Attachment != a {
			return ErrHostEnrollment
		}
		return nil
	}
	s.HostEnrollment = &HostEnrollment{Version: 1, Attachment: a}
	return nil
}

func (s *State) CheckHostEnrollment(a HostAttachment, actual HostScope, now time.Time) error {
	if err := s.hostAdmission(a, actual, now); err != nil {
		return err
	}
	if s.HostEnrollment == nil || s.HostEnrollment.Validate() != nil || s.HostEnrollment.Attachment != a {
		return ErrHostEnrollment
	}
	return nil
}

// WithHostEnrollment holds the existing sole-consumer lock for the entire
// adapter lifetime, acquiring control then state only for enrollment. Caller
// supplies a finite lock deadline and its admitted provider observation. This
// API does not launch a process, acquire a lease or enable a delivery backend.
func WithHostEnrollment(p Paths, name string, deadline time.Time, a HostAttachment, actual HostScope, clock func() time.Time, run func() error) error {
	if deadline.IsZero() || clock == nil || run == nil || !a.valid() || a.Scope != actual || actual.Agent != name {
		return ErrHostEnrollment
	}
	return WithRuntimeLock(p, name, "consumer", deadline, func() error {
		err := WithRuntimeLock(p, name, "control", deadline, func() error {
			_, err := UpdateStateBy(p, name, deadline, func(s *State) error { return s.EnrollHost(a, actual, clock()) })
			return err
		})
		if err != nil {
			return err
		}
		// Persistence may itself cross expiry. Retain the fenced record, but
		// never enter the adapter with expired admission.
		now := clock()
		if now.IsZero() || !a.Lease.Expires.After(now) {
			return ErrHostEnrollment
		}
		return run()
	})
}
