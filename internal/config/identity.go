package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Stable identities (owner decision, #a2a-swarm stable-identity): an agent's
// name is <host>_<project>_<role>, with no per-session suffix, so a session
// that restarts on the same machine, in the same project, in the same role is
// the same identity again. One live session holds it at a time (the lease).

// Limits of the parts of a composite identity.
const (
	HostMax    = 24
	ProjectMax = 40
	RoleMax    = 24
)

// IdentitySep joins the parts of a composite identity.
const IdentitySep = "_"

var partRE = regexp.MustCompile(`^[a-z0-9-]+$`)

// ValidatePart checks one part of a composite identity: [a-z0-9-], 1..max
// characters. kind names the part in the error (host, project, role).
func ValidatePart(kind, v string, max int) error {
	if v == "" || len(v) > max || !partRE.MatchString(v) {
		return fmt.Errorf("invalid %s %q: use lowercase letters, digits and -, 1-%d characters", kind, v, max)
	}
	return nil
}

// ValidateHost checks a host slug.
func ValidateHost(v string) error { return ValidatePart("host", v, HostMax) }

// ComposeIdentity returns <host>_<project>_<role>. Each part must match
// [a-z0-9-] within its limit (the error names the offending part), and the
// whole name must be a valid agent name (at most 64 characters).
func ComposeIdentity(host, project, role string) (string, error) {
	if err := ValidatePart("host", host, HostMax); err != nil {
		return "", err
	}
	if err := ValidatePart("project", project, ProjectMax); err != nil {
		return "", err
	}
	if err := ValidatePart("role", role, RoleMax); err != nil {
		return "", err
	}
	name := host + IdentitySep + project + IdentitySep + role
	if err := ValidateName(name); err != nil {
		return "", fmt.Errorf("identity %q is %d characters, longer than the 64 an agent name allows: shorten the project or the role", name, len(name))
	}
	return name, nil
}

// HostFile holds this machine's host slug.
func (p Paths) HostFile() string { return filepath.Join(p.ConfigRoot(), "host") }

// ErrNoHost is returned by LoadHost when no host slug is set.
var ErrNoHost = errors.New("no host slug on this machine: run a2a host init <slug> once per machine")

// LoadHost reads the host slug; a missing file is ErrNoHost. There is no
// default from the OS hostname.
func LoadHost(p Paths) (string, error) {
	data, err := os.ReadFile(p.HostFile())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNoHost
		}
		return "", err
	}
	slug := strings.TrimSpace(string(data))
	if err := ValidateHost(slug); err != nil {
		return "", fmt.Errorf("%s: %v; fix it with a2a host init <slug> --force", p.HostFile(), err)
	}
	return slug, nil
}

// SaveHost writes the host slug (mode 0600, slug + newline). It refuses to
// replace a different existing slug unless force is set, since a new slug
// means new identities. It reports whether the file changed.
func SaveHost(p Paths, slug string, force bool) (bool, error) {
	if err := ValidateHost(slug); err != nil {
		return false, err
	}
	cur, err := LoadHost(p)
	switch {
	case err == nil && cur == slug:
		return false, nil
	case err == nil && !force:
		return false, fmt.Errorf("this machine's host slug is %q; changing it to %q means new identities for every agent here: pass --force to do it anyway", cur, slug)
	case err != nil && !errors.Is(err, ErrNoHost) && !force:
		return false, err
	}
	if err := os.MkdirAll(p.Base, 0o700); err != nil {
		return false, err
	}
	if err := writeSecretAtomic(p.HostFile(), "host", []byte(slug+"\n")); err != nil {
		return false, err
	}
	return true, nil
}

// LeaseTTL is how long a lease lasts without a refresh. wait, watch, inbox
// and the hooks refresh it every time they run (wait and watch once per sync).
const LeaseTTL = 10 * time.Minute

// Lease records the one live session that holds an identity.
type Lease struct {
	// Holder is the session id: the hook's session_id, A2A_SESSION_ID, or "pid:<pid>@<host>".
	Holder  string    `json:"holder"`
	Host    string    `json:"host,omitempty"`
	PID     int       `json:"pid"`
	Expires time.Time `json:"expires"`
}

// LeaseHeldError is returned when another live session holds the identity.
type LeaseHeldError struct {
	Name  string
	Lease Lease
}

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("identity %s is held by %s until %s", e.Name, e.Lease.Holder, e.Lease.Expires.UTC().Format(time.RFC3339)) +
		"; only if that session is gone: a2a identity release --force"
}

// Acquire takes or refreshes the lease of name for holder until now+ttl (LeaseTTL).
// A lease of another holder is refused while it is live: unexpired and, for a
// "pid:" holder on this host (dead reports whether such a holder's process has
// exited), its process still running. An expired or dead lease is taken over.
//
// Compatibility on one host: a "pid:" holder is a process that does not know
// its session (e.g. a background wait started before the session exported
// A2A_SESSION_ID). It cannot be told apart from the session it belongs to, so
// it is compatible with any holder on the SAME host: a known session replaces
// a pid lease, and a pid process leaves a session's lease in place. Two
// different known sessions, or holders on different hosts, still conflict,
// which is the case the lease exists for (one identity used twice at once).
func (s *State) Acquire(name, holder, host string, pid int, now time.Time, ttl time.Duration, dead func(holder string) bool) error {
	// Under Host custody ordinary member commands only borrow the admitted
	// unexpired lease. They never replace its process identity or renew it.
	if e := s.HostEnrollment; e != nil {
		a := e.Attachment
		if a.Scope.Agent != name || holder != a.Scope.Session || s.CheckHostEnrollment(a, a.Scope, now) != nil {
			return ErrHostEnrollment
		}
		return nil
	}
	isPID := func(h string) bool { return strings.HasPrefix(h, "pid:") }
	if l := s.Lease; l != nil && l.Holder != holder && now.Before(l.Expires) && (dead == nil || !dead(l.Holder)) {
		sameHost := l.Host != "" && l.Host == host
		switch {
		case sameHost && isPID(holder) && !isPID(l.Holder):
			return nil // a session-less process of the live session's host: leave the session's lease
		case sameHost && isPID(l.Holder):
			// a known session (or another process) of the same host replaces a pid lease
		default:
			return &LeaseHeldError{Name: name, Lease: *l}
		}
	}
	s.Lease = &Lease{Holder: holder, Host: host, PID: pid, Expires: now.Add(ttl).UTC()}
	return nil
}

// Release clears the lease if holder holds it, if it is no longer live
// (expired, or dead), or with force; otherwise it returns a LeaseHeldError. It reports
// whether a lease was cleared. Persisted Host enrollment always refuses;
// ordinary release is not a Host retirement or handover operation.
func (s *State) Release(name, holder string, force bool, now time.Time, dead func(holder string) bool) (bool, error) {
	if s.HostEnrollment != nil {
		return false, ErrHostEnrollment
	}
	l := s.Lease
	if l == nil {
		return false, nil
	}
	if !force && l.Holder != holder && now.Before(l.Expires) && (dead == nil || !dead(l.Holder)) {
		return false, &LeaseHeldError{Name: name, Lease: *l}
	}
	s.Lease = nil
	return true, nil
}
