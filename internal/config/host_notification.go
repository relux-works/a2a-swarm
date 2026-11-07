package config

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"reflect"
	"time"
)

func (e HostEnrollment) matches(k NotificationAttemptKey) bool {
	s := e.Attachment.Scope
	b := k.Binding
	return k.Session == s.Session && k.Generation == s.Generation && b.NativeThread == s.NativeThread && b.MemberRef == s.Member && b.ConnectionRef == s.Connection && b.ConfigurationRef == s.Configuration && b.EnrollmentRef == e.ReceiptRef
}

// EnableHostNotifications upgrades only the same already admitted enrollment.
// Its reference becomes a receipt ONLY after the caller's entire SaveState
// succeeds. Caller holds the original consumer/control/state serialization.
// No wire label, lease acquisition, output or implicit successor is involved.
func (s *State) EnableHostNotifications(a HostAttachment, actual HostScope, now time.Time) error {
	if err := s.CheckHostEnrollment(a, actual, now); err != nil {
		return err
	}
	if s.HostEnrollment.Version == 2 {
		return nil
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return ErrHostEnrollment
	}
	e := *s.HostEnrollment
	e.Version = 2
	e.ReceiptRef = "a2a-enrollment-" + hex.EncodeToString(nonce[:])
	e.Attempts = &NotificationAttempts{Version: 1, Entries: []NotificationAttempt{}}
	s.HostEnrollment = &e
	return nil
}

// WithHostNotificationEnrollment keeps the existing sole-consumer lock and
// publishes the actual Source receipt only after the v2 record is durable and
// fresh admission still holds. It performs no Host trigger or native launch.
func WithHostNotificationEnrollment(p Paths, name string, deadline time.Time, a HostAttachment, actual HostScope, clock func() time.Time, run func(string) error) error {
	if deadline.IsZero() || clock == nil || run == nil || !a.valid() || a.Scope != actual || actual.Agent != name {
		return ErrHostEnrollment
	}
	base, err := os.OpenRoot(p.Base)
	if err != nil {
		return ErrHostStateScope
	}
	if err := base.Close(); err != nil {
		return ErrHostStateScope
	}
	return WithRuntimeLock(p, name, "consumer", deadline, func() error {
		var receipt string
		err := WithRuntimeLock(p, name, "control", deadline, func() error {
			_, err := UpdateStateBy(p, name, deadline, func(s *State) error {
				// Construct v1 custody and its v2 receipt in the same snapshot.
				// Only UpdateStateBy's one durable save may publish either.
				if err := s.EnrollHost(a, actual, clock()); err != nil {
					return err
				}
				if err := s.EnableHostNotifications(a, actual, clock()); err != nil {
					return err
				}
				receipt = s.HostEnrollment.ReceiptRef
				return nil
			})
			return err
		})
		if err != nil {
			return err
		}
		now := clock()
		if now.IsZero() || !a.Lease.Expires.After(now) {
			return ErrHostEnrollment
		}
		return run(receipt)
	})
}

func (s State) hostNotificationCopy() (State, error) {
	if s.validateHostNotifications() != nil || s.HostEnrollment == nil || s.HostEnrollment.Version != 2 {
		return State{}, ErrNotificationAttempt
	}
	e := *s.HostEnrollment
	l := *e.Attempts
	l.Entries = append([]NotificationAttempt{}, l.Entries...)
	e.Attempts = &l
	s.HostEnrollment = &e
	if s.Notification.Reservation != nil {
		r := *s.Notification.Reservation
		s.Notification.Reservation = &r
	}
	return s, nil
}

func (s State) validateHostNotifications() error {
	e := s.HostEnrollment
	r := s.Notification.Reservation
	if e == nil || e.Version == 1 {
		if r != nil && r.Path == "host" {
			return ErrNotificationAttempt
		}
		return nil
	}
	if e.Validate() != nil {
		return ErrHostEnrollment
	}
	active := false
	for _, a := range e.Attempts.Entries {
		if a.State != "prepared" && a.State != "unknown" {
			continue
		}
		active = true
		if r == nil || r.Path != "host" || r.Token != a.Key.Token || r.Session != a.Key.Session || r.Generation != a.Key.Generation || r.At.IsZero() || r.Previous.IsZero() || s.Notification.Version != 1 || s.Notification.NotBefore.IsZero() {
			return ErrNotificationAttempt
		}
	}
	if !active && r != nil && r.Path == "host" {
		return ErrNotificationAttempt
	}
	return nil
}

// PrepareHostNotification changes the one shared reservation and its exact
// correlation together. Persist the resulting State before any transport I/O.
// legacy is the existing caller's migration timestamp, not another throttle.
func (s *State) PrepareHostNotification(a HostAttachment, actual HostScope, b HostNotificationBinding, digest string, now, legacy time.Time) (NotificationAttemptKey, error) {
	var empty NotificationAttemptKey
	if err := s.CheckHostEnrollment(a, actual, now); err != nil {
		return empty, err
	}
	next, err := s.hostNotificationCopy()
	if err != nil {
		return empty, err
	}
	for _, old := range next.HostEnrollment.Attempts.Entries {
		if old.State == "prepared" || old.State == "unknown" || old.Key.Binding != b {
			return empty, ErrNotificationAttempt
		}
	}
	if err := next.Notification.Prepare(now, legacy); err != nil {
		return empty, err
	}
	token, err := next.Notification.reserve("host", actual.Session, actual.Generation, now, false)
	if err != nil {
		return empty, err
	}
	k := NotificationAttemptKey{Token: token, Session: actual.Session, Generation: actual.Generation, NoticeSHA256: digest, Binding: b}
	if !next.HostEnrollment.matches(k) {
		return empty, ErrNotificationAttempt
	}
	if err := next.HostEnrollment.Attempts.Prepare(k); err != nil {
		return empty, err
	}
	*s = next
	return k, nil
}

// BeginHostNotification persists UNKNOWN plus the original trigger request ID
// before that single call. It never permits re-triggering UNKNOWN after restart.
func (s *State) BeginHostNotification(a HostAttachment, actual HostScope, k NotificationAttemptKey, requestID string, now time.Time) error {
	if err := s.CheckHostEnrollment(a, actual, now); err != nil {
		return err
	}
	return s.changeHostNotification(k, now, func(l *NotificationAttempts) error { return l.Begin(k, requestID) }, "retain")
}

// AcceptHostNotification requires an authoritative retained Host observation,
// already matched to this exact key by the owning adapter. It can reconcile an
// expired original enrollment; it cannot admit another trigger or binding.
func (s *State) AcceptHostNotification(k NotificationAttemptKey, sequence, stage string, now time.Time) error {
	return s.changeHostNotification(k, now, func(l *NotificationAttempts) error { return l.Accept(k, sequence, stage) }, "complete")
}

// RefuseHostNotification accepts only the original exact trigger response.
// Lookup absence, timeout or a later request's refusal must never call it.
func (s *State) RefuseHostNotification(k NotificationAttemptKey, requestID, reason string) error {
	return s.changeHostNotification(k, time.Time{}, func(l *NotificationAttempts) error { return l.Refuse(k, requestID, reason) }, "cancel")
}

func (s *State) AbandonHostNotification(k NotificationAttemptKey) error {
	return s.changeHostNotification(k, time.Time{}, func(l *NotificationAttempts) error { return l.Abandon(k) }, "cancel")
}

func (s *State) changeHostNotification(k NotificationAttemptKey, now time.Time, change func(*NotificationAttempts) error, finish string) error {
	next, err := s.hostNotificationCopy()
	if err != nil {
		return err
	}
	alreadyAccepted := false
	for _, a := range next.HostEnrollment.Attempts.Entries {
		if a.Key == k && a.State == "accepted" {
			alreadyAccepted = true
		}
	}
	if finish == "complete" && now.IsZero() {
		return ErrNotificationAttempt
	}
	if err := change(next.HostEnrollment.Attempts); err != nil {
		return err
	}
	if finish == "complete" && !alreadyAccepted {
		err = next.Notification.Complete(k.Token, now)
	}
	if finish == "cancel" {
		err = next.Notification.Cancel(k.Token)
	}
	if err != nil {
		return err
	}
	if err := next.validateHostNotifications(); err != nil {
		return err
	}
	*s = next
	return nil
}

// Cooperative writer admission: no stale snapshot can discard, reorder, or
// regress retained correlations. A valid in-memory transition is still not a
// receipt until the whole state-file durability operation succeeds.
func validHostNotificationWrite(old, next State) bool {
	if next.validateHostNotifications() != nil {
		return false
	}
	a, b := old.HostEnrollment, next.HostEnrollment
	if a == nil {
		// Initial v2 custody is admitted as one write, without pre-existing
		// attempts. Validation above still checks the complete envelope.
		return b == nil || b.Version == 1 || b.Version == 2 && len(b.Attempts.Entries) == 0
	}
	if b == nil || !a.Attachment.same(b.Attachment) {
		return false
	}
	if a.Version == 1 {
		return a.same(*b) || b.Version == 2 && len(b.Attempts.Entries) == 0
	}
	if !a.same(*b) || b.Attempts == nil || len(b.Attempts.Entries) < len(a.Attempts.Entries) || len(b.Attempts.Entries) > len(a.Attempts.Entries)+1 {
		return false
	}
	for _, prior := range a.Attempts.Entries {
		if prior.State != "prepared" && prior.State != "unknown" {
			continue
		}
		for _, current := range b.Attempts.Entries {
			if current.Key == prior.Key && (current.State == "prepared" || current.State == "unknown") && !reflect.DeepEqual(old.Notification, next.Notification) {
				return false
			}
		}
	}
	for i, prior := range a.Attempts.Entries {
		current := b.Attempts.Entries[i]
		if reflect.DeepEqual(prior, current) {
			continue
		}
		l := NotificationAttempts{Version: 1, Entries: append([]NotificationAttempt{}, a.Attempts.Entries...)}
		var err error
		switch {
		case prior.State == "prepared" && current.State == "unknown":
			err = l.Begin(prior.Key, current.RequestID)
		case prior.State == "prepared" && current.State == "not-applied":
			err = l.Abandon(prior.Key)
		case prior.State == "unknown" && current.State == "not-applied":
			err = l.Refuse(prior.Key, current.RequestID, current.Reason)
		case (prior.State == "unknown" || prior.State == "accepted") && current.State == "accepted":
			err = l.Accept(prior.Key, current.Sequence, current.NativeStage)
		default:
			return false
		}
		if err != nil || !reflect.DeepEqual(l.Entries[i], current) {
			return false
		}
	}
	if len(b.Attempts.Entries) > len(a.Attempts.Entries) {
		added := b.Attempts.Entries[len(a.Attempts.Entries)]
		l := NotificationAttempts{Version: 1, Entries: append([]NotificationAttempt{}, a.Attempts.Entries...)}
		if added.State != "prepared" || l.Prepare(added.Key) != nil {
			return false
		}
	}
	return true
}
