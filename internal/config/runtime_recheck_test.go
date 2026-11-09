//go:build unix

package config

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Model cancellation just after a successful pre-acquisition Err snapshot.
// The real flock is uncontended; a final callback guard must see cancellation.
type cancelAfterSnapshot struct {
	context.Context
	cancel    context.CancelFunc
	at, calls int
}

func (c *cancelAfterSnapshot) Err() error {
	err := c.Context.Err()
	c.calls++
	if c.calls == c.at {
		c.cancel()
	}
	return err
}

func TestCourierRuntimeLockRecheck(t *testing.T) {
	for _, kind := range []string{"control", "consumer", "subscription-policy"} {
		for _, immediate := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/ordinary", true: "/immediate"}[immediate], func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				p := Paths{Base: root}
				parent, cancel := context.WithCancel(context.Background())
				defer cancel()
				ctx := &cancelAfterSnapshot{Context: parent, cancel: cancel, at: 2}
				deadline := time.Time{}
				if immediate {
					deadline = time.Now()
					ctx.at = 1
				}
				called := false
				err = WithRuntimeLockContext(ctx, p, "alice", kind, deadline, func() error { called = true; return nil })
				if !errors.Is(err, context.Canceled) || called {
					t.Fatal("runtime callback ran after cancellation", err, called)
				}
				neighbor := errors.New("neighbor callback result")
				err = WithRuntimeLockContext(context.Background(), p, "alice", kind, time.Now(), func() error { called = true; return neighbor })
				if err != neighbor || !called {
					t.Fatal("unheld immediate neighbor", err, called)
				}
			})
		}
	}
}
