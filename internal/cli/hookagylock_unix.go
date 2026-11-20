//go:build darwin || linux

package cli

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Serialize the read/merge/rename performed by concurrent a2a installers in
// this hooks directory. Refuse a symlink leaf or non-private writer ownership.
func lockAgySettingsDir(path string, install bool) (func(), error) {
	dir := filepath.Dir(path)
	if install {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	}
	fd, err := syscall.Open(dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if !install && errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st syscall.Stat_t
	if err = syscall.Fstat(fd, &st); err != nil || st.Uid != uint32(os.Geteuid()) || st.Mode&0022 != 0 {
		syscall.Close(fd)
		return nil, errors.New("unsafe agy settings directory")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		syscall.Close(fd)
		return nil, errors.New("agy settings directory is busy")
	}
	return func() { syscall.Close(fd) }, nil
}

// An inherited fd may be blocking even when represented by *os.File. Make a
// pollable duplicate so cancellation interrupts a read and a full output pipe
// respects the governor's deadline. Restore the original flags on handback.
func agyBridgePipe(f *os.File) (*os.File, func(), error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, nil, err
	}
	var dup, flags int
	var opErr error
	err = raw.Control(func(fd uintptr) {
		flags, opErr = unix.FcntlInt(fd, unix.F_GETFL, 0)
		if opErr != nil {
			return
		}
		dup, opErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
		if opErr != nil {
			return
		}
		if opErr = unix.SetNonblock(dup, true); opErr != nil {
			unix.Close(dup)
		}
	})
	if err != nil {
		return nil, nil, err
	}
	if opErr != nil {
		return nil, nil, opErr
	}
	pipe := os.NewFile(uintptr(dup), "agy provider pipe")
	restore := func() { pipe.Close(); raw.Control(func(fd uintptr) { unix.FcntlInt(fd, unix.F_SETFL, flags) }) }
	if err = pipe.SetDeadline(time.Time{}); err != nil {
		restore()
		return nil, nil, err
	}
	return pipe, restore, nil
}
