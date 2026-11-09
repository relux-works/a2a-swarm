//go:build unix

package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

type observedLockContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *observedLockContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

// A real contender remains held after cancellation. Observing the first Err
// snapshot ensures cancellation cannot bypass the first attempted flock. A
// blocking-flock mutant must fail this test, even on a fast machine.
func TestCourierContextFlockContender(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := &observedLockContext{Context: ctx, checked: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		unlock, err := lockFileContext(observed, path)
		if err == nil {
			unlock()
		}
		done <- err
	}()
	select {
	case <-observed.checked:
	case err := <-done:
		t.Fatal("contender returned early", err)
	case <-time.After(5 * time.Second):
		t.Fatal("acquisition not entered")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("wrong cancellation reason", err)
		}
	case <-time.After(5 * time.Second):
		holder.Close()
		t.Fatal("cancelled flock exceeded 5s with real contender")
	}
	syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)
	unlock, err := lockFileContext(context.Background(), path)
	if err != nil {
		t.Fatal("uncontended neighbor", err)
	}
	unlock()
}
