package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func hostNotificationFixture(t *testing.T) (Paths, State, HostAttachment, time.Time, HostNotificationBinding) {
	t.Helper()
	scope, a, now := hostFixture()
	p := testPaths(t)
	s := State{Lease: &a.Lease, NextBatch: "unread-retained"}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, scope.Agent, s); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHostNotifications(a, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, scope.Agent, s); err != nil {
		t.Fatal(err)
	}
	b := notificationAttemptKey().Binding
	b.NativeThread, b.MemberRef, b.ConnectionRef, b.ConfigurationRef, b.ConsumerGeneration, b.EnrollmentRef = scope.NativeThread, scope.Member, scope.Connection, scope.Configuration, scope.Generation, s.HostEnrollment.ReceiptRef
	return p, s, a, now, b
}

func hostNotificationReload(t *testing.T, p Paths, s State) State {
	t.Helper()
	if err := SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p, "member")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(s, got) {
		t.Fatal("state did not round-trip")
	}
	return got
}

// Proves the public writer atomically retains reservation, original request and
// observation across reload, and receipt identity survives unrelated updates.
// Acceptance proves only Host's stated native stage, never an inbox receipt.
func TestHostNotificationRoundTrip(t *testing.T) {
	p, s, a, now, b := hostNotificationFixture(t)
	ref := s.HostEnrollment.ReceiptRef
	if err := s.EnableHostNotifications(a, a.Scope, now); err != nil || s.HostEnrollment.ReceiptRef != ref {
		t.Fatalf("receipt replaced: %v", err)
	}
	k, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	if err := s.BeginHostNotification(a, a.Scope, k, "original-request", now); err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	// Admission has expired; only retained observation may resolve the old key.
	if err := s.AcceptHostNotification(k, "1", "not-evidenced", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	if s.Notification.Reservation != nil || s.NextBatch != "unread-retained" || s.HostEnrollment.ReceiptRef != ref {
		t.Fatal("incorrect reservation/mailbox/receipt state")
	}
	if err := s.AcceptHostNotification(k, "1", "native-completed", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	before, _ := json.Marshal(s)
	if err := s.AcceptHostNotification(k, "1", "native-accepted", now); !errors.Is(err, ErrNotificationAttempt) {
		t.Fatalf("stage regressed: %v", err)
	}
	after, _ := json.Marshal(s)
	if !bytes.Equal(before, after) {
		t.Fatal("rejection mutated state")
	}
}

// Proves UNKNOWN survives cooldown/expiry, cancellation and stale snapshots.
// The positive control first persists the original prepared and begun attempt.
func TestHostNotificationUnknownFence(t *testing.T) {
	p, s, a, now, b := hostNotificationFixture(t)
	k, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	stale := s
	if err := s.BeginHostNotification(a, a.Scope, k, "request", now); err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	before, _ := os.ReadFile(p.StateFile("member"))
	if err := SaveState(p, "member", stale); !errors.Is(err, ErrHostEnrollment) {
		t.Fatalf("stale write: %v", err)
	}
	for _, mutate := range []func(*State){
		func(x *State) { _ = x.Notification.Cancel(k.Token) },
		func(x *State) {
			_, _ = x.Notification.Reserve("hook", a.Scope.Session, "", now.Add(40*time.Second), false)
		},
		func(x *State) { x.HostEnrollment.Attempts.Entries = nil },
		func(x *State) {
			x.HostEnrollment.Version = 1
			x.HostEnrollment.ReceiptRef = ""
			x.HostEnrollment.Attempts = nil
		},
	} {
		x, err := LoadState(p, "member")
		if err != nil {
			t.Fatal(err)
		}
		mutate(&x)
		if err := SaveState(p, "member", x); err == nil {
			t.Fatal("lossy or unfenced writer accepted")
		}
		after, _ := os.ReadFile(p.StateFile("member"))
		if !bytes.Equal(before, after) {
			t.Fatal("rejected writer changed disk")
		}
	}
	for _, at := range []time.Time{now.Add(40 * time.Second), now.Add(time.Hour)} {
		if _, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("b", 64), at, time.Time{}); err == nil {
			t.Fatal("UNKNOWN retriggered")
		}
	}
	if err := s.BeginHostNotification(a, a.Scope, k, "request2", now); err == nil {
		t.Fatal("UNKNOWN retriggered")
	}
	if err := s.RefuseHostNotification(k, "new-request", "input-not-ready"); err == nil {
		t.Fatal("new refusal canceled original")
	}
	if err := s.AbandonHostNotification(k); err == nil {
		t.Fatal("UNKNOWN abandoned")
	}
}

// Proves only pre-I/O abandonment or the exact original definitive refusal
// restores shared spacing, with a nearby valid successor using the same ledger.
func TestHostNotificationDefinitiveCancellation(t *testing.T) {
	for _, begun := range []bool{false, true} {
		p, s, a, now, b := hostNotificationFixture(t)
		k, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		s = hostNotificationReload(t, p, s)
		if begun {
			if err := s.BeginHostNotification(a, a.Scope, k, "request", now); err != nil {
				t.Fatal(err)
			}
			s = hostNotificationReload(t, p, s)
			err = s.RefuseHostNotification(k, "request", "input-not-ready")
		} else {
			err = s.AbandonHostNotification(k)
		}
		if err != nil {
			t.Fatal(err)
		}
		s = hostNotificationReload(t, p, s)
		if s.Notification.Reservation != nil || !s.Notification.Ready(now) {
			t.Fatal("definitive no-output not reconciled")
		}
		if _, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("b", 64), now, time.Time{}); err != nil {
			t.Fatal(err)
		}
		hostNotificationReload(t, p, s)
	}
}

// Proves all source-owned scope fields are matched without replacing custody;
// the valid full binding beside each mutation still prepares and reloads.
func TestHostNotificationBindingAndKey(t *testing.T) {
	for _, field := range []string{"NativeThread", "MemberRef", "ConnectionRef", "ConfigurationRef", "ConsumerGeneration", "EnrollmentRef"} {
		t.Run(field, func(t *testing.T) {
			p, s, a, now, b := hostNotificationFixture(t)
			before, _ := json.Marshal(s)
			bad := b
			reflect.ValueOf(&bad).Elem().FieldByName(field).SetString("foreign")
			if _, err := s.PrepareHostNotification(a, a.Scope, bad, strings.Repeat("a", 64), now, time.Time{}); err == nil {
				t.Fatal("foreign binding prepared")
			}
			after, _ := json.Marshal(s)
			if !bytes.Equal(before, after) {
				t.Fatal("scope refusal mutated state")
			}
			k, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			s = hostNotificationReload(t, p, s)
			if err := s.BeginHostNotification(a, a.Scope, k, "request", now); err != nil {
				t.Fatal(err)
			}
			s = hostNotificationReload(t, p, s)
			wrong := k
			wrong.NoticeSHA256 = strings.Repeat("b", 64)
			if err := s.AcceptHostNotification(wrong, "1", "not-evidenced", now); err == nil {
				t.Fatal("wrong digest reconciled")
			}
		})
	}
}

// Proves the supported compact v2 codec reloads retained bounded records and an
// unrelated field update preserves them; terminal deletion remains forbidden.
func TestHostNotificationRetention(t *testing.T) {
	p, s, a, now, b := hostNotificationFixture(t)
	for i := 0; i < 32; i++ {
		k, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		s = hostNotificationReload(t, p, s)
		if err := s.AbandonHostNotification(k); err != nil {
			t.Fatal(err)
		}
		s = hostNotificationReload(t, p, s)
	}
	if _, err := s.PrepareHostNotification(a, a.Scope, b, strings.Repeat("a", 64), now, time.Time{}); !errors.Is(err, ErrNotificationAttemptCapacity) {
		t.Fatalf("capacity: %v", err)
	}
	s.NextBatch = "new-cursor"
	s = hostNotificationReload(t, p, s)
	s.HostEnrollment.Attempts.Entries = s.HostEnrollment.Attempts.Entries[:31]
	if err := SaveState(p, "member", s); err == nil {
		t.Fatal("terminal record deletion accepted")
	}
}

// Proves malformed/unknown/null v2 custody cannot be loaded or overwritten by
// an otherwise valid snapshot, while the unchanged exact envelope reloads.
func TestHostNotificationMalformedEnrollment(t *testing.T) {
	p, s, _, _, _ := hostNotificationFixture(t)
	before, err := os.ReadFile(p.StateFile("member"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{
		{`"receipt_ref":`, `"Receipt_ref":`},
		{`"version":2`, `"version":3`},
		{`"attempts":{"version":1,"entries":[]}`, `"attempts":null`},
		{`"receipt_ref":`, `"future":true,"receipt_ref":`},
		{`"receipt_ref":`, `"receipt_ref":"duplicate","receipt_ref":`},
	} {
		bad := bytes.Replace(before, []byte(pair[0]), []byte(pair[1]), 1)
		if bytes.Equal(before, bad) {
			t.Fatal("malformed plant not applied")
		}
		if err := os.WriteFile(p.StateFile("member"), bad, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadState(p, "member"); err == nil {
			t.Fatal("malformed enrollment loaded")
		}
		if err := SaveState(p, "member", s); err == nil {
			t.Fatal("malformed enrollment overwritten")
		}
		after, _ := os.ReadFile(p.StateFile("member"))
		if !bytes.Equal(bad, after) {
			t.Fatal("refusal changed original bytes")
		}
	}
	if err := os.WriteFile(p.StateFile("member"), before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadState(p, "member"); err != nil {
		t.Fatalf("valid nearby control: %v", err)
	}
}

// Proves the actual enrollment entry publishes a durable receipt under the sole
// consumer lock; expiry after its write retains custody without entering run.
func TestHostNotificationEnrollmentEntry(t *testing.T) {
	for _, expire := range []bool{false, true} {
		p := testPaths(t)
		scope, a, now := hostFixture()
		if err := SaveState(p, "member", State{Lease: &a.Lease}); err != nil {
			t.Fatal(err)
		}
		calls, ran := 0, false
		clock := func() time.Time {
			calls++
			if expire && calls == 3 {
				return now.Add(time.Hour)
			}
			return now
		}
		err := WithHostNotificationEnrollment(p, "member", time.Now().Add(time.Second), a, scope, clock, func(ref string) error {
			got, err := LoadState(p, "member")
			if err != nil || got.HostEnrollment.Version != 2 || got.HostEnrollment.ReceiptRef != ref {
				t.Fatalf("receipt before durability: %v", err)
			}
			if err := WithRuntimeLock(p, "member", "consumer", time.Now().Add(20*time.Millisecond), func() error { return nil }); !errors.Is(err, ErrLockTimeout) {
				t.Fatalf("consumer lock not held: %v", err)
			}
			ran = true
			return nil
		})
		if expire {
			if !errors.Is(err, ErrHostEnrollment) || ran {
				t.Fatalf("expired callback: %v %v", err, ran)
			}
		} else if err != nil || !ran {
			t.Fatalf("valid callback: %v %v", err, ran)
		}
		got, err := LoadState(p, "member")
		if err != nil || got.HostEnrollment == nil || got.HostEnrollment.Version != 2 {
			t.Fatalf("durable custody lost: %v", err)
		}
	}
}

// The second admission check occurs before the single write: fresh custody
// must not leave a persisted v1 fence, even when the v2 upgrade refuses.
// Existing v1/v2 custody is preserved and re-entry retains the same receipt.
func TestHostNotificationEnrollmentSingleWrite(t *testing.T) {
	for _, version := range []int{0, 1, 2} {
		for _, refuse := range []bool{false, true} {
			t.Run(fmt.Sprintf("version-%d/refuse-%t", version, refuse), func(t *testing.T) {
				p := testPaths(t)
				scope, a, now := hostFixture()
				s := State{Lease: &a.Lease, NextBatch: "unread-retained"}
				if version != 0 {
					if err := s.EnrollHost(a, scope, now); err != nil {
						t.Fatal(err)
					}
				}
				if version == 2 {
					if err := s.EnableHostNotifications(a, scope, now); err != nil {
						t.Fatal(err)
					}
				}
				if err := SaveState(p, scope.Agent, s); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(p.StateFile(scope.Agent))
				if err != nil {
					t.Fatal(err)
				}
				calls, ran := 0, false
				clock := func() time.Time {
					calls++
					if calls == 2 {
						disk, err := os.ReadFile(p.StateFile(scope.Agent))
						if err != nil || !bytes.Equal(disk, before) {
							t.Fatalf("intermediate enrollment write: %v", err)
						}
						if refuse {
							return a.Lease.Expires
						}
					}
					return now
				}
				var receipt string
				err = WithHostNotificationEnrollment(p, scope.Agent, time.Now().Add(time.Second), a, scope, clock, func(ref string) error {
					ran, receipt = true, ref
					// Short locks must be released before user code runs.
					return WithRuntimeLock(p, scope.Agent, "control", time.Now().Add(time.Second), func() error {
						_, err := UpdateStateBy(p, scope.Agent, time.Now().Add(time.Second), func(current *State) error {
							if current.HostEnrollment.Version != 2 || current.HostEnrollment.ReceiptRef != ref || current.NextBatch != "unread-retained" {
								t.Fatal("callback lacks durable exact custody")
							}
							return nil
						})
						return err
					})
				})
				if refuse {
					if !errors.Is(err, ErrHostEnrollment) || ran || calls != 2 {
						t.Fatalf("upgrade refusal: err=%v ran=%t calls=%d", err, ran, calls)
					}
					after, readErr := os.ReadFile(p.StateFile(scope.Agent))
					if readErr != nil || !bytes.Equal(before, after) {
						t.Fatalf("refused upgrade persisted partial custody: %v", readErr)
					}
					return
				}
				if err != nil || !ran || calls != 3 {
					t.Fatalf("valid enrollment: err=%v ran=%t calls=%d", err, ran, calls)
				}
				if version == 2 && receipt != s.HostEnrollment.ReceiptRef {
					t.Fatal("existing v2 receipt replaced")
				}
				if err := WithHostNotificationEnrollment(p, scope.Agent, time.Now().Add(time.Second), a, scope, func() time.Time { return now }, func(ref string) error {
					if ref != receipt {
						t.Fatal("idempotent entry replaced receipt")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// Proves byte capacity (which can precede 32 records) uses the stored compact
// representation. Indented encoding exceeds the decoder budget in this fixture
// but the actual persisted state remains reloadable, without dropping records.
func TestHostNotificationEncodedCapacity(t *testing.T) {
	p := testPaths(t)
	scope, a, now := hostFixture()
	scope.Member = strings.Repeat("m", 512)
	scope.Configuration = strings.Repeat("c", 512)
	scope.Session = strings.Repeat("s", 200)
	scope.Generation = strings.Repeat("g", 128)
	a.Scope = scope
	a.Lease.Holder = scope.Session
	s := State{Lease: &a.Lease}
	if err := s.EnrollHost(a, scope, now); err != nil {
		t.Fatal(err)
	}
	if err := SaveState(p, "member", s); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableHostNotifications(a, scope, now); err != nil {
		t.Fatal(err)
	}
	s = hostNotificationReload(t, p, s)
	b := notificationAttemptKey().Binding
	b.MemberRef, b.ConfigurationRef, b.ConnectionRef, b.NativeThread, b.ConsumerGeneration, b.EnrollmentRef, b.ProfileLockRef = scope.Member, scope.Configuration, scope.Connection, scope.NativeThread, scope.Generation, s.HostEnrollment.ReceiptRef, strings.Repeat("p", 512)
	for i := 0; i < 33; i++ {
		k, err := s.PrepareHostNotification(a, scope, b, strings.Repeat("a", 64), now, time.Time{})
		if errors.Is(err, ErrNotificationAttemptCapacity) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		s = hostNotificationReload(t, p, s)
		if err := s.AbandonHostNotification(k); err != nil {
			t.Fatal(err)
		}
		s = hostNotificationReload(t, p, s)
	}
	compact, _ := json.Marshal(s.HostEnrollment.Attempts)
	indented, _ := json.MarshalIndent(s.HostEnrollment.Attempts, "", "  ")
	if len(compact) > notificationAttemptBytes || len(indented) <= notificationAttemptBytes {
		t.Fatalf("capacity control not at intended boundary: compact=%d indented=%d", len(compact), len(indented))
	}
	if len(s.HostEnrollment.Attempts.Entries) == 0 || len(s.HostEnrollment.Attempts.Entries) >= 32 {
		t.Fatal("byte capacity did not precede count ceiling")
	}
}
