package wakemute

import (
	"context"
	"io"
	"os"
	"time"
)

// WriteOutput applies the invocation budget to pollable production descriptors
// (pipes/sockets). Regular filesystem syscalls and custom embedded writers are
// not preemptible by Go; those writers must themselves honor their I/O contract.
// A late failure never means an already committed operation was rolled back.
func WriteOutput(ctx context.Context, w io.Writer, p []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return 0, ErrAuthority
	}
	if f, ok := w.(*os.File); ok {
		if err := f.SetWriteDeadline(deadline); err == nil {
			defer f.SetWriteDeadline(time.Time{})
		} else if n, e, handled := ttyWrite(ctx, f, p); handled {
			return n, e
		}
	}
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	if err == nil {
		err = ctx.Err()
	}
	return n, err
}
