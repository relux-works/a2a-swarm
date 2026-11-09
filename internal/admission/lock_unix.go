//go:build darwin || linux

package admission

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

func lock(ctx context.Context, r *os.Root) (func(), error) {
	if err := privateFile(r, "queue.lock"); err != nil {
		return nil, err
	}
	f, err := r.OpenFile("queue.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, ErrStorage
	}
	for {
		if ctx.Err() != nil {
			f.Close()
			return nil, ErrStorage
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, ErrStorage
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, ErrStorage
		case <-timer.C:
		}
	}
}
