//go:build unix

package config

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive flock on path and returns the unlock function.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, SecretFileMode)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return unlocker(f), nil
}

// lockRetry is the pause between two non-blocking lock attempts.
const lockRetry = 10 * time.Millisecond

// lockFileBy is lockFile with non-blocking attempts until deadline; a lock
// still held then is ErrLockTimeout.
func lockFileBy(path string, deadline time.Time) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, SecretFileMode)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return unlocker(f), nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(min(left, lockRetry))
	}
}

func unlocker(f *os.File) func() {
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
