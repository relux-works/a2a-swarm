//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Adopting the same open file description preserves exclusion without acquiring
// a new lock. The adopted fd becomes CLOEXEC again for inspection subprocesses.
func TestConsumerLockAdoption(t *testing.T) {
	p := testPaths(t)
	path := filepath.Join(p.Base, "state", "member.runtime-consumer.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original, err := lockedFileBy(path, time.Now().Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Dup(int(original.Fd()))
	original.Close()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	err = WithConsumerLock(p, "member", time.Now().Add(time.Second), fd, func(f *os.File) error {
		calls++
		err := WithRuntimeLock(p, "member", "consumer", time.Now(), func() error { return nil })
		if !errors.Is(err, ErrLockTimeout) {
			t.Fatal("adopted lock lost exclusion")
		}
		flags, err := unix.FcntlInt(f.Fd(), unix.F_GETFD, 0)
		if err != nil || flags&unix.FD_CLOEXEC == 0 {
			t.Fatal("adopted descriptor leaked to subprocesses")
		}
		return nil
	})
	if err != nil || calls != 1 {
		t.Fatalf("adoption failed: %v calls=%d", err, calls)
	}
	if err := WithRuntimeLock(p, "member", "consumer", time.Now(), func() error { return nil }); err != nil {
		t.Fatal("consumer lock not released on ordinary exit")
	}
}

// Wrong-file and independently opened descriptors cannot bypass a live consumer;
// rejection never enters the callback or releases the actual consumer's lock.
func TestConsumerLockAdoptionRefusals(t *testing.T) {
	p := testPaths(t)
	err := WithConsumerLock(p, "member", time.Now().Add(time.Second), 0, func(_ *os.File) error {
		for _, name := range []string{"wrong-file", "independent-open"} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(p.Base, "state", "member.runtime-consumer.lock")
				if name == "wrong-file" {
					path = filepath.Join(p.Base, "state", "unrelated")
				}
				f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				fd, err := unix.Dup(int(f.Fd()))
				if err != nil {
					t.Fatal(err)
				}
				called := false
				err = WithConsumerLock(p, "member", time.Now(), fd, func(*os.File) error { called = true; return nil })
				if err == nil || called {
					t.Fatal("invalid inherited lock admitted second consumer")
				}
				if name == "independent-open" && !errors.Is(err, syscall.EWOULDBLOCK) {
					t.Fatalf("wrong lock rejection: %v", err)
				}
				err = WithRuntimeLock(p, "member", "consumer", time.Now(), func() error { return nil })
				if !errors.Is(err, ErrLockTimeout) {
					t.Fatal("refusal released original lock")
				}
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
