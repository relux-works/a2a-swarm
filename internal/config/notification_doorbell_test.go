package config

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// The additive path persists/reloads using the SAME ledger and30-second due
// time as all three existing paths; arbitrary path strings still fail closed.
func TestNotificationDoorbellPath(t *testing.T) {
	now := time.Date(2026, 10, 29, 3, 0, 0, 0, time.UTC)
	for _, path := range []string{"doorbell", "hook", "native", "terminal"} {
		t.Run(path, func(t *testing.T) {
			var n Notification
			token, err := n.Reserve(path, "owned-session", "generation", now, false)
			if err != nil || token == "" {
				t.Fatalf("valid path refused: %v", err)
			}
			data, err := json.Marshal(n)
			if err != nil {
				t.Fatal(err)
			}
			var reload Notification
			if json.Unmarshal(data, &reload) != nil || reload.Prepare(now.Add(time.Second), time.Time{}) != nil || reload.Reservation.Path != path {
				t.Fatal("valid saved path refused")
			}
			if err := reload.Complete(token, now); err != nil {
				t.Fatal(err)
			}
			if _, err = reload.Reserve("doorbell", "owned-session", "generation", now.Add(15*time.Second), false); !errors.Is(err, ErrNotificationSpacing) {
				t.Fatal("doorbell bypassed existing spacing")
			}
			if _, err = reload.Reserve("doorbell", "owned-session", "generation", now.Add(30*time.Second), false); err != nil {
				t.Fatal("trailing doorbell lost")
			}
		})
	}
	for _, path := range []string{"", "Doorbell", "doorbell\npeer", "doorbell\x1b[31m", "arbitrary"} {
		t.Run("invalid-"+path, func(t *testing.T) {
			var n Notification
			if _, err := n.Reserve(path, "s", "g", now, false); err == nil || n.Version != 0 || n.Reservation != nil {
				t.Fatal("invalid path mutated ledger")
			}
			token, err := n.Reserve("hook", "s", "g", now, false)
			if err != nil || token == "" {
				t.Fatal("invalid-path control setup failed")
			}
			n.Reservation.Path = path
			if err := n.Prepare(now.Add(time.Second), time.Time{}); err == nil {
				t.Fatal("invalid saved path admitted")
			}
		})
	}
}
