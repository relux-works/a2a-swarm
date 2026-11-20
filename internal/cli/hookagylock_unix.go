//go:build darwin || linux

package cli

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
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
