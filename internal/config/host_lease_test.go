package config

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

// An enrolled member may read under its exact unexpired Host lease without
// refreshing it. Foreign, altered and expired leases never acquire custody.
// The adjacent never-enrolled control retains ordinary lease refresh.
func TestHostLeaseReadCustody(t *testing.T) {
	for _, mode := range []string{"same", "foreign", "expired", "changed-pid", "changed-expiry"} {
		t.Run(mode, func(t *testing.T) {
			scope, a, now := hostFixture()
			s := State{Lease: &a.Lease}
			if err := s.EnrollHost(a, scope, now); err != nil {
				t.Fatal(err)
			}
			holder := scope.Session
			switch mode {
			case "foreign":
				holder = "foreign"
			case "expired":
				now = a.Lease.Expires
			case "changed-pid":
				l := *s.Lease
				l.PID++
				s.Lease = &l
			case "changed-expiry":
				l := *s.Lease
				l.Expires = l.Expires.Add(time.Second)
				s.Lease = &l
			}
			before := *s.Lease
			err := s.Acquire("member", holder, "member-process-host", 999, now, time.Hour, func(string) bool { return true })
			if mode == "same" {
				if err != nil || s.CheckHostEnrollment(a, scope, now) != nil {
					t.Fatalf("member admission lost: %v", err)
				}
			} else if !errors.Is(err, ErrHostEnrollment) {
				t.Fatalf("host custody not refused: %v", err)
			}
			if !reflect.DeepEqual(before, *s.Lease) {
				t.Fatal("ordinary read changed Host-owned lease")
			}
		})
	}
	_, a, now := hostFixture()
	s := State{Lease: &a.Lease}
	if err := s.Acquire("member", a.Scope.Session, "member-process-host", 999, now, time.Hour, nil); err != nil || s.Lease.PID != 999 || !s.Lease.Expires.Equal(now.Add(time.Hour)) {
		t.Fatalf("legacy refresh suppressed: %v", err)
	}
}

// Equal instants in distinct non-UTC Location objects survive enrollment and
// JSON persistence; an actual one-nanosecond lease change still refuses.
func TestHostLeaseTimestampIdentity(t *testing.T) {
	scope, a, now := hostFixture()
	a.Lease.Expires = a.Lease.Expires.In(time.FixedZone("MSK-a", 3*60*60))
	l := a.Lease
	l.Expires = l.Expires.In(time.FixedZone("MSK-b", 3*60*60))
	s := State{Lease: &l}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatalf("same instant refused: %v", err)
	}
	if s.HostEnrollment.Attachment.Lease.Expires.Location() != time.UTC {
		t.Fatal("attachment timestamp is not canonical UTC")
	}
	p := Paths{Base: t.TempDir()}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	if err := got.CheckHostEnrollment(a, scope, now); err != nil {
		t.Fatalf("round-trip admission: %v", err)
	}
	if err := got.EnrollHost(a, scope, now); err != nil {
		t.Fatalf("round-trip idempotence: %v", err)
	}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatalf("equivalent writer refused: %v", err)
	}
	changed := a
	changed.Lease.Expires = changed.Lease.Expires.Add(time.Nanosecond)
	if err := got.CheckHostEnrollment(changed, scope, now); !errors.Is(err, ErrHostEnrollment) {
		t.Fatalf("different instant admitted: %v", err)
	}
}
