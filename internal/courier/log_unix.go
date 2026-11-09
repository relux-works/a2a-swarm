//go:build linux || darwin

package courier

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// logSink has a fixed queue and never makes connection/shutdown wait for a
// writer. A private nonblocking pipe isolates stderr flags from queue writes;
// its forwarding worker owns any blocking destination write.
type logSink struct {
	queue       chan string
	done        chan struct{}
	dropped     atomic.Uint64
	fd          int
	pipe        *os.File
	forwardDone chan struct{}
	closeOnce   sync.Once
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
		// Only the private pipe description is made nonblocking. A separate worker
		// owns blocking writes to a duplicate of stderr; it never pins shutdown.
		// If stderr is suspended, at most one fixed line remains in that worker.
		raw, err := f.SyscallConn()
		duplicate := -1
		if err == nil {
			err = raw.Control(func(fd uintptr) { duplicate, _ = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0) })
		}
		if err == nil && duplicate >= 0 {
			reader, writer, e := os.Pipe()
			if e == nil {
				s.pipe = writer
				s.forwardDone = make(chan struct{})
				go func() {
					defer close(s.forwardDone)
					defer unix.Close(duplicate)
					defer reader.Close()
					buf := make([]byte, len(refusalLine))
					for {
						n, err := io.ReadFull(reader, buf)
						if err != nil {
							return
						}
						if written, err := unix.Write(duplicate, buf[:n]); err != nil || written != n {
							s.dropped.Add(1)
						}
					}
				}()
				rawPipe, e := writer.SyscallConn()
				if e == nil {
					_ = rawPipe.Control(func(fd uintptr) { s.fd = int(fd) })
				}
			} else {
				unix.Close(duplicate)
			}
		}
		if s.fd < 0 {
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
	s.closeOnce.Do(func() {
		close(s.queue)
		<-s.done
		if s.pipe != nil {
			s.pipe.Close()
			// A drained writer finishes naturally. Never wait on a suspended stderr.
			// The worker owns its duplicate until the external reader resumes.
		}
	})
}
