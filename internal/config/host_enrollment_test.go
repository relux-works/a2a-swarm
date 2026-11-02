package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func hostFixture() (HostScope, HostAttachment, time.Time) {
	now := time.Date(2026, 11, 2, 21, 0, 0, 0, time.UTC)
	scope := HostScope{Agent: "member", Member: "@member:example.org", Connection: "selected", Configuration: "own-config", Session: "source-session", NativeThread: "native-thread", Generation: "generation", Consumer: "consumer"}
	return scope, HostAttachment{Reference: "admitted-attachment", Scope: scope, Lease: Lease{Holder: scope.Session, Host: "host", PID: 42, Expires: now.Add(time.Minute)}}, now
}

// Proves source enrollment is idempotent for the same trusted attachment and
// actual scope; nearby wrong callers cannot mutate even unrelated state.
func TestHostEnrollmentScope(t *testing.T) {
	scope, attachment, now := hostFixture()
	s := State{NextBatch: "pending", LegacyAcked: []string{"retained"}, Lease: &attachment.Lease}
	if err := s.EnrollHost(attachment, scope, now); err != nil {
		t.Fatal(err)
	}
	before := s.HostEnrollment
	if err := s.EnrollHost(attachment, scope, now); err != nil || !reflect.DeepEqual(before, s.HostEnrollment) {
		t.Fatalf("idempotence: %v", err)
	}
	for _, field := range []string{"Agent", "Member", "Connection", "Configuration", "Session", "NativeThread", "Generation", "Consumer"} {
		t.Run(field, func(t *testing.T) {
			bad := scope
			reflect.ValueOf(&bad).Elem().FieldByName(field).SetString("foreign")
			original := *s.HostEnrollment
			if err := s.CheckHostEnrollment(attachment, bad, now); !errors.Is(err, ErrHostEnrollment) {
				t.Fatalf("wrong scope: %v", err)
			}
			if err := s.EnrollHost(attachment, bad, now); !errors.Is(err, ErrHostEnrollment) {
				t.Fatalf("enroll wrong scope: %v", err)
			}
			if !reflect.DeepEqual(original, *s.HostEnrollment) || s.NextBatch != "pending" || s.LegacyAcked[0] != "retained" {
				t.Fatal("refusal mutated state")
			}
		})
	}
}

// Proves missing attachment, expired admission and a foreign live identity
// lease fail closed, while a matching lease is accepted without force release.
func TestHostEnrollmentAdmission(t *testing.T) {
	scope, a, now := hostFixture()
	cases := []struct {
		name       string
		attachment HostAttachment
		lease      *Lease
	}{
		{"missing", HostAttachment{}, nil},
		{"no-source-lease", a, nil},
		{"expired", func() HostAttachment { b := a; b.Lease.Expires = now; return b }(), nil},
		{"foreign-lease", a, &Lease{Holder: "foreign", Host: "host", PID: 41, Expires: now.Add(time.Minute)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := State{NextBatch: "pending", Lease: c.lease}
			before := s
			if err := s.EnrollHost(c.attachment, scope, now); !errors.Is(err, ErrHostEnrollment) {
				t.Fatalf("admission: %v", err)
			}
			if !reflect.DeepEqual(before, s) {
				t.Fatal("rejected enrollment changed state")
			}
		})
	}
	s := State{Lease: &a.Lease}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatal(err)
	}
	if *s.Lease != a.Lease {
		t.Fatal("identity lease replaced")
	}
}

// Proves a successor consumer/generation cannot replace existing custody,
// including after expiry; explicit retirement/handover is a separate contract.
func TestHostEnrollmentNoImplicitSuccessor(t *testing.T) {
	scope, a, now := hostFixture()
	s := State{Lease: &a.Lease}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatal(err)
	}
	old := *s.HostEnrollment
	for _, delta := range []func(*HostAttachment){func(b *HostAttachment) { b.Reference = "new" }, func(b *HostAttachment) { b.Scope.Consumer = "second" }, func(b *HostAttachment) { b.Scope.Generation = "successor" }} {
		b := a
		delta(&b)
		b.Lease.Expires = now.Add(time.Hour)
		if err := s.EnrollHost(b, b.Scope, now.Add(2*time.Minute)); !errors.Is(err, ErrHostEnrollment) {
			t.Fatalf("successor: %v", err)
		}
		if !reflect.DeepEqual(old, *s.HostEnrollment) {
			t.Fatal("successor overwrote custody")
		}
	}
}

// Proves strict bounded native/session/reference inputs are rejected without
// sanitizing two different identities into the same stored scope.
func TestHostEnrollmentStrictInputs(t *testing.T) {
	scope, a, now := hostFixture()
	for _, field := range []string{"Session", "NativeThread", "Generation", "Configuration"} {
		for _, value := range []string{"", "bad\nvalue", string(make([]byte, 513))} {
			bad := scope
			reflect.ValueOf(&bad).Elem().FieldByName(field).SetString(value)
			b := a
			b.Scope = bad
			b.Lease.Holder = bad.Session
			s := State{Lease: &b.Lease}
			if err := s.EnrollHost(b, bad, now); !errors.Is(err, ErrHostEnrollment) || s.HostEnrollment != nil {
				t.Fatalf("field %s accepted", field)
			}
		}
	}
}

// Proves the supported state writer preserves existing enrollment across an
// unrelated mailbox update, rejects dropping/replacing custody, and refuses
// unknown enrollment versions/fields without rewriting their fixture bytes.
func TestHostEnrollmentWriterGuard(t *testing.T) {
	scope, a, now := hostFixture()
	p := testPaths(t)
	s := State{Lease: &a.Lease, NextBatch: "pending"}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	got.NextBatch = "next"
	if err := SaveState(p, "member", got); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.StateFile("member"))
	if err != nil {
		t.Fatal(err)
	}
	for _, drop := range []bool{true, false} {
		bad := got
		if drop {
			bad.HostEnrollment = nil
		} else {
			e := *got.HostEnrollment
			e.Attachment.Reference = "replacement"
			bad.HostEnrollment = &e
		}
		if err := SaveState(p, "member", bad); !errors.Is(err, ErrHostEnrollment) {
			t.Fatalf("lossy writer: %v", err)
		}
		after, _ := os.ReadFile(p.StateFile("member"))
		if !bytes.Equal(before, after) {
			t.Fatal("lossy write changed disk")
		}
	}
	for _, payload := range [][]byte{bytes.Replace(before, []byte(`"version": 1`), []byte(`"version": 2`), 1), bytes.Replace(before, []byte(`"version": 1`), []byte(`"future": true, "version": 1`), 1)} {
		if bytes.Equal(payload, before) {
			t.Fatal("plant not applied")
		}
		if err := os.WriteFile(p.StateFile("member"), payload, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(p, "member"); !errors.Is(err, ErrHostEnrollment) {
			t.Fatalf("unknown version/field: %v", err)
		}
		if err := SaveState(p, "member", got); !errors.Is(err, ErrHostEnrollment) {
			t.Fatalf("overwrite unknown state: %v", err)
		}
		after, _ := os.ReadFile(p.StateFile("member"))
		if !bytes.Equal(payload, after) {
			t.Fatal("unknown state overwritten")
		}
	}
}

// Proves bounds distinguish a 200-byte source session from Host's 128-byte
// native thread; normalization and silent truncation are forbidden.
func TestHostEnrollmentBounds(t *testing.T) {
	scope, a, now := hostFixture()
	scope.Session = strings.Repeat("s", 200)
	scope.NativeThread = strings.Repeat("n", 128)
	scope.Generation = strings.Repeat("g", 128)
	a.Scope = scope
	a.Lease.Holder = scope.Session
	s := State{Lease: &a.Lease}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"Session", "NativeThread", "Generation"} {
		bad := scope
		v := reflect.ValueOf(&bad).Elem().FieldByName(field)
		v.SetString(v.String() + "x")
		b := a
		b.Scope = bad
		b.Lease.Holder = bad.Session
		s := State{Lease: &b.Lease}
		if err := s.EnrollHost(b, bad, now); !errors.Is(err, ErrHostEnrollment) {
			t.Fatalf("unbounded %s", field)
		}
	}
}

// Proves the adapter entry persists custody before running, and a concurrent
// second consumer times out without executing. It uses only isolated fixtures.
func TestHostEnrollmentConsumerLock(t *testing.T) {
	scope, a, now := hostFixture()
	p := testPaths(t)
	s := State{Lease: &a.Lease}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	ran := false
	err := WithHostEnrollment(p, "member", time.Now().Add(time.Second), a, scope, func() time.Time { return now }, func() error {
		got, err := LoadState(p, "member")
		if err != nil || got.HostEnrollment == nil {
			t.Fatalf("not persisted before callback: %v", err)
		}
		secondRan := false
		err = WithHostEnrollment(p, "member", time.Now().Add(20*time.Millisecond), a, scope, func() time.Time { return now }, func() error { secondRan = true; return nil })
		if !errors.Is(err, ErrLockTimeout) || secondRan {
			t.Fatalf("duplicate: %v, ran=%v", err, secondRan)
		}
		ran = true
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("valid consumer: %v, ran=%v", err, ran)
	}
	if err := WithRuntimeLock(p, "member", "consumer", time.Now().Add(time.Second), func() error { return nil }); err != nil {
		t.Fatalf("consumer lock leaked: %v", err)
	}
}

// Proves fresh time is sampled inside locked admission and again after its
// write: expiry refuses adapter entry, retaining an already persisted fence.
// The clock sequences model those two boundaries without timing sleeps.
func TestHostEnrollmentFreshClock(t *testing.T) {
	for _, expiredAt := range []int{1, 2, 0} {
		t.Run(fmt.Sprint(expiredAt), func(t *testing.T) {
			scope, a, now := hostFixture()
			p := testPaths(t)
			if err := SaveState(p, "member", State{Lease: &a.Lease}); err != nil {
				t.Fatal(err)
			}
			calls := 0
			clock := func() time.Time {
				calls++
				if calls == expiredAt {
					return a.Lease.Expires
				}
				return now
			}
			ran := false
			err := WithHostEnrollment(p, "member", time.Now().Add(time.Second), a, scope, clock, func() error { ran = true; return nil })
			if expiredAt == 0 {
				if err != nil || !ran || calls != 2 {
					t.Fatalf("valid: err=%v ran=%v clock=%d", err, ran, calls)
				}
			} else if !errors.Is(err, ErrHostEnrollment) || ran {
				t.Fatalf("expired: err=%v ran=%v", err, ran)
			}
			st, loadErr := LoadState(p, "member")
			if loadErr != nil || (st.HostEnrollment != nil) != (expiredAt != 1) {
				t.Fatalf("persisted fence: %+v %v", st.HostEnrollment, loadErr)
			}
		})
	}
}

// Proves an explicit null enrollment cannot masquerade as a legacy state with
// enrollment absent. A genuine legacy state remains readable and writable.
func TestHostEnrollmentNullRefuses(t *testing.T) {
	p := testPaths(t)
	if err := SaveState(p, "member", State{NextBatch: "old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, "member"); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"next_batch":"pending","host_enrollment":null}`)
	if err := os.WriteFile(p.StateFile("member"), payload, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, "member"); !errors.Is(err, ErrHostEnrollment) {
		t.Fatalf("null loaded: %v", err)
	}
	if err := SaveState(p, "member", State{}); !errors.Is(err, ErrHostEnrollment) {
		t.Fatalf("null overwritten: %v", err)
	}
	got, _ := os.ReadFile(p.StateFile("member"))
	if !bytes.Equal(got, payload) {
		t.Fatal("null refusal changed disk")
	}
}
