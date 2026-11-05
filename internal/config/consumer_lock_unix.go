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
	if err := ValidateName(name); err != nil {
		return err
	}
	path := filepath.Join(p.Base, "state", name+".runtime-consumer.lock")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := checkSecretFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var f *os.File
	var err error
	if inherited == 0 {
		f, err = lockedFileBy(path, deadline)
	} else {
		if inherited < 3 {
			return errors.New("invalid inherited consumer lock")
		}
		syscall.CloseOnExec(inherited)
		f = os.NewFile(uintptr(inherited), "binding-consumer")
		if f == nil {
			return errors.New("missing inherited consumer lock")
		}
		info, e1 := f.Stat()
		want, e2 := os.Lstat(path)
		if e1 != nil || e2 != nil || !info.Mode().IsRegular() || !os.SameFile(info, want) {
			f.Close()
			return errors.New("inherited consumer lock mismatch")
		}
		// Also refuses a descriptor that does not already hold this lock when
		// another consumer owns it. Normal exec adoption succeeds immediately.
		err = syscall.Flock(inherited, syscall.LOCK_EX|syscall.LOCK_NB)
	}
	if err != nil {
		if f != nil {
			f.Close()
		}
		return err
	}
	defer unlocker(f)()
	return fn(f)
}
