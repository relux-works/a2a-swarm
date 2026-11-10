//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/daemon"
	"golang.org/x/sys/unix"
)

const stationInputPoll = 20 * time.Millisecond

func readStationInput(ctx context.Context, r io.Reader, hook bool) ([]byte, error) {

	// In-memory readers finish synchronously. Unknown blocking reader adapters
	// fail closed instead of leaving a detached goroutine behind the hook budget.
	switch r.(type) {
	case *bytes.Reader, *bytes.Buffer, *strings.Reader:
		return io.ReadAll(io.LimitReader(r, daemon.MaxFrameBytes+1))
	}
	f, ok := r.(*os.File)
	if !ok {
		if hook {
			return nil, daemon.NotAllowed
		}
		type inputResult struct {
			data []byte
			err  error
		}
		done := make(chan inputResult, 1)
		go func() {
			data, err := io.ReadAll(io.LimitReader(r, daemon.MaxFrameBytes+1))
			done <- inputResult{data, err}
		}()
		select {
		case result := <-done:
			return result.data, result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	fd := int(f.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, daemon.Unavailable
	}
	if unix.SetNonblock(fd, true) != nil {
		return nil, daemon.Unavailable
	}
	defer unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
	var data []byte
	buf := make([]byte, 4096)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := unix.Read(fd, buf)
		if n > 0 {
			data = append(data, buf[:n]...)
			if len(data) > daemon.MaxFrameBytes {
				return nil, daemon.NotAllowed
			}
		}
		if err == nil && n == 0 {
			return data, nil
		}
		if err != nil && err != unix.EAGAIN && err != unix.EINTR {
			return nil, daemon.Unavailable
		}
		if err == unix.EAGAIN {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, int(stationInputPoll/time.Millisecond)); err != nil && err != unix.EINTR {
				return nil, daemon.Unavailable
			}
		}
	}
}
