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

func TestCourierRuntimeLockContext(t *testing.T) {
	for _, kind := range []string{"control", "consumer", "subscription-policy"} {
		t.Run(kind, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			p := Paths{Base: root}
			if err := os.MkdirAll(filepath.Join(root, "state"), 0700); err != nil {
				t.Fatal(err)
			}
			holder, err := os.OpenFile(filepath.Join(root, "state", "alice.runtime-"+kind+".lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer holder.Close()
			if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}
			called := false
			callback := func() error { called = true; return nil }
			if err := WithRuntimeLockContext(context.Background(), p, "alice", kind, time.Now(), callback); !errors.Is(err, ErrLockTimeout) || called {
				t.Fatal("held immediate probe", err, called)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			observed := &observedLockContext{Context: ctx, checked: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				done <- WithRuntimeLockContext(observed, p, "alice", kind, time.Now().Add(time.Hour), callback)
			}()
			select {
			case <-observed.checked:
			case err := <-done:
				t.Fatal("returned before cancellation", err)
			case <-time.After(5 * time.Second):
				t.Fatal("not entered")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) || called {
					t.Fatal("cancelled runtime lock", err, called)
				}
			case <-time.After(5 * time.Second):
				holder.Close()
				t.Fatal("runtime lock exceeded cancellation bound")
			}
			if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			if err := WithRuntimeLockContext(context.Background(), p, "alice", kind, time.Now(), callback); err != nil || !called {
				t.Fatal("unheld immediate probe neighbor", err, called)
			}
		})
	}
}

func TestCourierPostJournalLockCancellation(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			p, j := operationFixture(t)
			holder, err := j.root.OpenFile(".lock", os.O_RDWR, 0600)
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
			other, err := OpenPostJournalContext(observed, p, "alice", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			run := func(j *PostJournal) error {
				if write {
					_, _, err := j.Prepare(operationCandidate("candidate", 1))
					return err
				}
				_, _, err := j.Read("candidate")
				return err
			}
			done := make(chan error, 1)
			go func() { done <- run(other) }()
			select {
			case <-observed.checked:
			case err := <-done:
				t.Fatal("journal returned before cancellation", err)
			case <-time.After(5 * time.Second):
				t.Fatal("journal lock not entered")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, ErrPostOperation) {
					t.Fatal("journal closed reason", err)
				}
			case <-time.After(5 * time.Second):
				holder.Close()
				t.Fatal("journal retained behind flock")
			}
			if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
			neighbor, err := OpenPostJournalContext(context.Background(), p, "alice", false, false)
			if err != nil {
				t.Fatal(err)
			}
			defer neighbor.Close()
			if err := run(neighbor); err != nil {
				t.Fatal("journal neighbor", err)
			}
			_, exists, err := neighbor.Read("candidate")
			if err != nil || exists != write {
				t.Fatal("journal effects", exists, err)
			}
		})
	}
}
