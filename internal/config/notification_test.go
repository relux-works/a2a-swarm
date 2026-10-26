package config

import (
	"encoding/json"
	"testing"
	"time"
)

// These controls prove fixed completion-anchored spacing, a leading positive,
// exact boundary admission, and no sliding deadline under repeated arrivals.
func TestNotificationSpacingAndCompletion(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	var n Notification
	if err := n.Prepare(now, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if !n.Ready(now) {
		t.Fatal("new identity cannot lead")
	}
	token, err := n.Reserve("hook", "session", "", now, false)
	if err != nil {
		t.Fatal(err)
	}
	if err = n.Complete(token, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	due := now.Add(31 * time.Second)
	for _, d := range []time.Duration{time.Second, 10 * time.Second, 30 * time.Second, 31*time.Second - time.Nanosecond} {
		at := now.Add(d)
		if err = n.Prepare(at, time.Time{}); err != nil {
			t.Fatal(err)
		}
		if n.Ready(at) || !n.NotBefore.Equal(due) {
			t.Fatalf("early/sliding at %s: %+v", d, n)
		}
	}
	if !n.Ready(due) {
		t.Fatal("trailing not ready without new mail")
	}
	if _, err = n.Reserve("native", "session", "generation", due, false); err != nil {
		t.Fatal(err)
	}
}

// An interrupted reservation spends spacing only: another admitted path can
// announce after30s; stale completions never modify its newer token.
func TestNotificationUncertainAndStaleCompletion(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	var n Notification
	n.Prepare(now, time.Time{})
	old, err := n.Reserve("hook", "session", "", now, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = n.Reserve("native", "session", "g", now.Add(30*time.Second-time.Nanosecond), false); err != ErrNotificationSpacing {
		t.Fatal(err)
	}
	newer, err := n.Reserve("native", "session", "g", now.Add(30*time.Second), false)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(n)
	if err = n.Complete(old, now.Add(31*time.Second)); err != ErrNotificationStale {
		t.Fatal(err)
	}
	after, _ := json.Marshal(n)
	if string(before) != string(after) {
		t.Fatal("stale completion mutated state")
	}
	if err = n.Complete(newer, now.Add(31*time.Second)); err != nil {
		t.Fatal(err)
	}
}

// Restart preserves UTC due. A detected backward jump clamps once to30s,
// forward jump permits one early batch then creates a fresh fixed window.
func TestNotificationClockAndRestart(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	var n Notification
	n.Prepare(now, time.Time{})
	token, _ := n.Reserve("hook", "s", "", now, false)
	n.Complete(token, now)
	data, _ := json.Marshal(n)
	var restart Notification
	if err := json.Unmarshal(data, &restart); err != nil {
		t.Fatal(err)
	}
	restart.Prepare(now.Add(10*time.Second), time.Time{})
	if !restart.NotBefore.Equal(now.Add(30*time.Second)) || restart.Ready(now.Add(29999*time.Millisecond)) {
		t.Fatal("restart lost due")
	}
	back := now.Add(-time.Hour)
	restart.Prepare(back, time.Time{})
	clamp := back.Add(30 * time.Second)
	if !restart.NotBefore.Equal(clamp) {
		t.Fatal("backward clamp absent")
	}
	restart.Prepare(back.Add(time.Second), time.Time{})
	if !restart.NotBefore.Equal(clamp) {
		t.Fatal("clamp slid")
	}
	forward := now.Add(time.Hour)
	if !restart.Ready(forward) {
		t.Fatal("forward control")
	}
	token, err := restart.Reserve("hook", "s", "", forward, false)
	if err != nil {
		t.Fatal(err)
	}
	restart.Complete(token, forward)
	if restart.Ready(forward) || restart.Ready(forward.Add(29999*time.Millisecond)) || !restart.Ready(forward.Add(30*time.Second)) {
		t.Fatal("forward jump permits burst")
	}
}

// Unsupported/malformed state refuses; migration holds only once and leaves
// unrelated state and legacy channel fences outside this ledger untouched.
func TestNotificationMigrationAndInvalid(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	for _, n := range []Notification{{Version: 2}, {Version: 1}, {Version: 1, NotBefore: now, Reservation: &NotificationReservation{}}} {
		if err := n.Prepare(now, time.Time{}); err == nil {
			t.Fatal("invalid ledger accepted")
		}
	}
	var n Notification
	if err := n.Prepare(now, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	due := n.NotBefore
	if n.Ready(now) {
		t.Fatal("unknown legacy history led immediately")
	}
	n.Prepare(now.Add(time.Second), now)
	if !n.NotBefore.Equal(due) {
		t.Fatal("migration slid")
	}
	if !n.Ready(now.Add(30 * time.Second)) {
		t.Fatal("migration held forever")
	}
}

// Invalid caller scope cannot persist a reservation or mutate an existing
// deadline; a valid scope still reserves at the same instant.
func TestNotificationReserveInvalidScope(t *testing.T) {
	now := time.Date(2026, 10, 26, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ path, session, generation string }{{"other", "s", "g"}, {"hook", "", ""}, {"native", string(make([]byte, 201)), "g"}, {"terminal", "s", string(make([]byte, 129))}} {
		var n Notification
		n.Prepare(now, time.Time{})
		before, _ := json.Marshal(n)
		if _, err := n.Reserve(tc.path, tc.session, tc.generation, now, false); err == nil {
			t.Fatal("invalid scope accepted")
		}
		after, _ := json.Marshal(n)
		if string(before) != string(after) {
			t.Fatal("invalid scope mutated ledger")
		}
		if _, err := n.Reserve("hook", "s", "", now, false); err != nil {
			t.Fatal("valid control refused", err)
		}
	}
}
