package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// NotificationEvery is the fixed anti-jitter protocol interval, not a retry,
// hourly quota, safety fence or receipt boundary.
const NotificationEvery = 30 * time.Second

var ErrNotificationSpacing = errors.New("notification cooldown")
var ErrNotificationStale = errors.New("notification completion stale")

// Notification is shared by all doorbell paths in one identity namespace.
// Reservation records possible output, not a channel-specific safety fence.
// It cannot prevent another admitted path from ringing after its cooldown.
type Notification struct {
	Version     int                      `json:"version"`
	NotBefore   time.Time                `json:"not_before"`
	Reservation *NotificationReservation `json:"reservation,omitempty"`
}
type NotificationReservation struct {
	Token      string    `json:"token"`
	Path       string    `json:"path"`
	Session    string    `json:"session"`
	Generation string    `json:"generation,omitempty"`
	At         time.Time `json:"at"`
	Previous   time.Time `json:"previous"`
}

// Prepare migrates once and clamps a backwards wall-clock jump. Call under
// the state lock and save even when spacing refuses the outward effect.
func (n *Notification) Prepare(now, legacy time.Time) error {
	if n.Version == 0 {
		if !n.NotBefore.IsZero() || n.Reservation != nil {
			return errors.New("invalid notification migration state")
		}
		n.Version = 1
		n.NotBefore = now.UTC()
		if !legacy.IsZero() {
			n.NotBefore = now.Add(NotificationEvery).UTC()
		}
	} else if n.Version != 1 || n.NotBefore.IsZero() {
		return errors.New("invalid notification ledger")
	}
	if r := n.Reservation; r != nil {
		if len(r.Token) != 32 || (r.Path != "hook" && r.Path != "native" && r.Path != "terminal") || r.Session == "" || r.At.IsZero() || r.Previous.IsZero() || len(r.Session) > 200 || len(r.Generation) > 128 {
			return errors.New("invalid notification reservation")
		}
		if _, err := hex.DecodeString(r.Token); err != nil {
			return errors.New("invalid notification reservation")
		}
	}
	if n.NotBefore.After(now.Add(NotificationEvery)) {
		n.NotBefore = now.Add(NotificationEvery).UTC()
	}
	return nil
}
func (n Notification) Ready(now time.Time) bool {
	return n.Version == 1 && !n.NotBefore.IsZero() && !now.Before(n.NotBefore)
}

// Reserve must be saved before output. bypass is only the hook-only Stop
// exception; its existing Stop60s and loop limits are enforced by the caller.
func (n *Notification) Reserve(path, session, generation string, now time.Time, bypass bool) (string, error) {
	if (path != "hook" && path != "native" && path != "terminal") || session == "" || len(session) > 200 || len(generation) > 128 || now.IsZero() {
		return "", errors.New("invalid notification scope")
	}
	if err := n.Prepare(now, time.Time{}); err != nil {
		return "", err
	}
	if !bypass && !n.Ready(now) {
		return "", ErrNotificationSpacing
	}
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	token := hex.EncodeToString(bytes[:])
	n.Reservation = &NotificationReservation{Token: token, Path: path, Session: session, Generation: generation, At: now.UTC(), Previous: n.NotBefore}
	n.NotBefore = now.Add(NotificationEvery).UTC()
	return token, nil
}
func (n *Notification) Complete(token string, now time.Time) error {
	if n.Reservation == nil || n.Reservation.Token != token {
		return ErrNotificationStale
	}
	n.NotBefore = now.Add(NotificationEvery).UTC()
	n.Reservation = nil
	return nil
}

// Cancel releases a proven no-output reservation only. Uncertain writes and
// channel safety fences must never use it.
func (n *Notification) Cancel(token string) error {
	if n.Reservation == nil || n.Reservation.Token != token {
		return ErrNotificationStale
	}
	n.NotBefore = n.Reservation.Previous
	n.Reservation = nil
	return nil
}
