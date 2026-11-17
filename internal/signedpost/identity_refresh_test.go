package signedpost

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Claim: re-preparation admits an advanced issue clock but promptly refuses cancellation or stalled/backwards clocks beside that control.
func TestIdentityChangeWait(t *testing.T) {
	issued := time.Unix(1767225600, 0)
	for _, mode := range []string{"valid", "cancelled", "stalled", "backwards"} {
		t.Run(mode, func(t *testing.T) {
			now := issued.Add(time.Second)
			if mode == "stalled" {
				now = issued.Add(time.Second - time.Millisecond)
			} else if mode == "backwards" {
				now = issued.Add(-time.Millisecond)
			}
			s := Sender{Now: func() time.Time { return now }}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			start := time.Now()
			err := s.waitForNextIssue(ctx, issued)
			if time.Since(start) > 250*time.Millisecond {
				t.Fatal("issue-clock check waited beyond its prompt-refusal budget")
			}
			if mode == "valid" {
				if err != nil {
					t.Fatal("advanced issue clock refused", err)
				}
			} else if !errors.Is(err, ErrUnavailable) {
				t.Fatal("invalid issue clock or context admitted", err)
			}
		})
	}
}
