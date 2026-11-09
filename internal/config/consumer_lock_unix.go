//go:build unix

package config

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// WithConsumerLock retains the terminal consumer's flock across exec. A resumed
// child adopts the same open file description, never releases and reacquires it.
// The descriptor is CLOEXEC except for the actual exec syscall, so subprocesses
// used for inspection cannot keep a dead consumer's lock alive.
func WithConsumerLock(p Paths, name string, deadline time.Time, inherited int, fn func(*os.File) error) error {
	f, release, err := acquireConsumerLock(p, name, deadline, inherited)
	if err != nil {
		return err
	}
	defer release()
	return fn(f)
}

// AcquireConsumerLock holds the existing selected-identity lock until release.
// Take it before control/state locks; never wait for it while holding either.
func AcquireConsumerLock(p Paths, name string, deadline time.Time) (func(), error) {
	_, release, err := acquireConsumerLock(p, name, deadline, 0)
	return release, err
}

// AcquireRawConsumerLock shares monitor/native exclusion on Unix. Its other
// platform implementation preserves legacy raw delivery where those consumers
// cannot be admitted at all.
func AcquireRawConsumerLock(p Paths, name string, deadline time.Time) (func(), error) {
	return AcquireConsumerLock(p, name, deadline)
}

func acquireConsumerLock(p Paths, name string, deadline time.Time, inherited int) (*os.File, func(), error) {
	if err := ValidateName(name); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(p.Base, "state", name+".runtime-consumer.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, err
	}
	if err := checkSecretFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	var f *os.File
	var err error
	if inherited == 0 {
		f, err = lockedFileBy(path, deadline)
	} else {
		if inherited < 3 {
			return nil, nil, errors.New("invalid inherited consumer lock")
		}
		syscall.CloseOnExec(inherited)
		f = os.NewFile(uintptr(inherited), "binding-consumer")
		if f == nil {
			return nil, nil, errors.New("missing inherited consumer lock")
		}
		info, e1 := f.Stat()
		want, e2 := os.Lstat(path)
		if e1 != nil || e2 != nil || !info.Mode().IsRegular() || !os.SameFile(info, want) {
			f.Close()
			return nil, nil, errors.New("inherited consumer lock mismatch")
		}
		// Also refuses a descriptor that does not already hold this lock when
		// another consumer owns it. Normal exec adoption succeeds immediately.
		err = syscall.Flock(inherited, syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		return nil, nil, err
	}
	return f, unlocker(f), nil
}
