//go:build unix

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

func (f ownerDMFiles) lock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(f.base), 0700); err != nil {
		return nil, err
	}
	path := f.path("lock")
	if _, _, err := ownerRead(path); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN); _ = file.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			file.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, fmt.Errorf("owner-dm lock: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
