//go:build linux || darwin

package courier

import (
	"bytes"
	"io"
	"os"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// logSink has a fixed queue and never makes connection/shutdown wait for a
// writer. File stderr uses nonblocking kernel writes, so an undrained pipe
// cannot pin the worker. Its original flags are restored before returning.
type logSink struct {
	queue   chan string
	done    chan struct{}
	dropped atomic.Uint64
	fd      int
	flags   int
}

// NonblockingLogWriter opts an embedding into the bounded sink. TryCourierLog
// must return promptly. Unknown Writer implementations are dropped rather than
// invoked, because io.Writer alone supplies no cancellation contract.
type NonblockingLogWriter interface{ TryCourierLog([]byte) (int, error) }
type nonblockingLogAdapter struct{ NonblockingLogWriter }

func (w nonblockingLogAdapter) Write(p []byte) (int, error) { return w.TryCourierLog(p) }

type dropLogWriter struct{}

func (dropLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func newLogSink(w io.Writer) *logSink {
	s := &logSink{queue: make(chan string, LogQueueLines), done: make(chan struct{}), fd: -1}
	if w == nil {
		w = io.Discard
	}
	if f, ok := w.(*os.File); ok {
		// Fd() can switch a Go-managed pipe back to blocking mode. Capture
		// through SyscallConn and own a duplicate; never call Fd in the worker.
		raw, err := f.SyscallConn()
		if err == nil {
			err = raw.Control(func(fd uintptr) {
				flags, e := unix.FcntlInt(fd, unix.F_GETFL, 0)
				if e != nil {
					return
				}
				duplicate, e := unix.Dup(int(fd))
				if e != nil {
					return
				}
				unix.CloseOnExec(duplicate)
				if unix.SetNonblock(duplicate, true) != nil {
					unix.Close(duplicate)
					return
				}
				s.fd = duplicate
				s.flags = flags
			})
		}
		if err != nil || s.fd < 0 {
			w = dropLogWriter{}
		}
	} else if n, ok := w.(NonblockingLogWriter); ok {
		w = nonblockingLogAdapter{n}
	} else if _, ok := w.(*bytes.Buffer); !ok && w != io.Discard {
		w = dropLogWriter{}
	}
	go func() {
		defer close(s.done)
		for line := range s.queue {
			var n int
			var err error
			if s.fd >= 0 {
				n, err = unix.Write(s.fd, []byte(line))
			} else {
				n, err = io.WriteString(w, line)
			}
			if err != nil || n != len(line) {
				s.dropped.Add(1)
			}
		}
	}()
	return s
}
func (s *logSink) write(line string) {
	if s == nil {
		return
	}
	select {
	case s.queue <- line:
	default:
		s.dropped.Add(1)
	}
}
func (s *logSink) close() {
	close(s.queue)
	<-s.done
	if s.fd >= 0 {
		_, _ = unix.FcntlInt(uintptr(s.fd), unix.F_SETFL, s.flags)
		_ = unix.Close(s.fd)
	}
}
