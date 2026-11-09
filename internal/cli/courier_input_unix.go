//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/courier"
	"golang.org/x/sys/unix"
)

const stationInputPoll = 20 * time.Millisecond

func readStationInput(ctx context.Context, r io.Reader, hook bool) ([]byte, error) {
	if !hook {
		return io.ReadAll(io.LimitReader(r, courier.MaxFrameBytes+1))
	}
	// In-memory readers finish synchronously. Unknown blocking reader adapters
	// fail closed instead of leaving a detached goroutine behind the hook budget.
	switch r.(type) {
	case *bytes.Reader, *bytes.Buffer, *strings.Reader:
		return io.ReadAll(io.LimitReader(r, courier.MaxFrameBytes+1))
	}
	f, ok := r.(*os.File)
	if !ok {
		return nil, courier.NotAllowed
	}
	fd := int(f.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		return nil, courier.Unavailable
	}
	if unix.SetNonblock(fd, true) != nil {
		return nil, courier.Unavailable
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
			if len(data) > courier.MaxFrameBytes {
				return nil, courier.NotAllowed
			}
		}
		if err == nil && n == 0 {
			return data, nil
		}
		if err != nil && err != unix.EAGAIN && err != unix.EINTR {
			return nil, courier.Unavailable
		}
		if err == unix.EAGAIN {
			poll := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			if _, err := unix.Poll(poll, int(stationInputPoll/time.Millisecond)); err != nil && err != unix.EINTR {
				return nil, courier.Unavailable
			}
		}
	}
}
