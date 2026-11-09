//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"golang.org/x/sys/unix"
)

type callbackLockContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *callbackLockContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}
func (c *callbackLockContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.checked) })
	return c.Context.Done()
}

// Exercise the same adapters reached by post/topic bookkeeping and indirect
// native hook callbacks, with real state/control contenders still held.
func TestCourierCallbackLockCancellation(t *testing.T) {
	for _, kind := range []string{"state", "deadline", "thread", "runtime"} {
		t.Run(kind, func(t *testing.T) {
			p := config.Paths{Base: canonicalRoot(t)}
			if err := os.MkdirAll(filepath.Join(p.Base, "state"), 0700); err != nil {
				t.Fatal(err)
			}
			name := "alice.lock"
			if kind == "runtime" {
				name = "alice.runtime-control.lock"
			}
			holder, err := os.OpenFile(filepath.Join(p.Base, "state", name), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &callbackLockContext{Context: ctx, checked: make(chan struct{})}
			a := &App{courierEngine: true, ctx: observed}
			s := &session{app: a, paths: p, name: "alice"}
			called := false
			run := func() error {
				callback := func(st *config.State) error { called = true; st.AddThread("$neighbor"); return nil }
				switch kind {
				case "state":
					_, err := a.updateState(p, "alice", callback)
					return err
				case "deadline":
					_, err := a.updateStateBy(p, "alice", time.Now().Add(time.Hour), callback)
					return err
				case "thread":
					return s.rememberThread("$neighbor")
				default:
					return a.withRuntimeLock(p, "alice", "control", time.Now().Add(time.Hour), func() error { called = true; return nil })
				}
			}
			done := make(chan error, 1)
			go func() { done <- run() }()
			select {
			case <-observed.checked:
			case err := <-done:
				t.Fatal("returned before cancellation", err)
			case <-time.After(courier.LockReleaseBound):
				t.Fatal("callback lock not entered")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || called {
					t.Fatal("callback cancellation", err, called)
				}
			case <-time.After(courier.LockReleaseBound):
				holder.Close()
				t.Fatal("callback retained behind flock")
			}
			if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			a.ctx = context.Background()
			if err := run(); err != nil {
				t.Fatal("uncontended neighbor", err)
			}
			if kind == "thread" {
				st, err := config.LoadState(p, "alice")
				if err != nil || len(st.Threads) != 1 || st.Threads[0] != "$neighbor" {
					t.Fatal("thread not recorded", err)
				}
			} else if !called {
				t.Fatal("neighbor callback not called")
			}
			// Local cleanup retains its original semantics even with a cancelled
			// App context; only authenticated Courier invocations opt in.
			a.courierEngine = false
			a.ctx = ctx
			if err := run(); err != nil {
				t.Fatal("local cleanup neighbor", err)
			}
		})
	}
}
