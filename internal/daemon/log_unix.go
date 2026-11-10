//go:build linux || darwin

package daemon

import (
	"bytes"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
)

// logSink admits at most 64 queued lines and one worker. File destinations use
// a private nonblocking pipe; the worker alone owns any blocking stderr write.
// No flags on stderr's shared open file description are changed.
type logSink struct {
	queue     chan string
	done      chan struct{}
	dropped   atomic.Uint64
	fd        int
	pipe      *os.File
	closeOnce sync.Once
}

// NonblockingLogWriter opts an embedding into the bounded sink. TryDaemonLog
// must return promptly. Unknown Writer implementations are dropped rather than
// invoked, because io.Writer alone supplies no cancellation contract.
type NonblockingLogWriter interface{ TryDaemonLog([]byte) (int, error) }
type nonblockingLogAdapter struct{ NonblockingLogWriter }

func (w nonblockingLogAdapter) Write(p []byte) (int, error) { return w.TryDaemonLog(p) }

type dropLogWriter struct{}

func (dropLogWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func newLogSink(w io.Writer) *logSink {
	s := &logSink{queue: make(chan string, LogQueueLines), done: make(chan struct{}), fd: -1}
	if w == nil {
		w = io.Discard
	}
	if f, ok := w.(*os.File); ok {
		raw, err := f.SyscallConn()
		duplicate := -1
		if err == nil {
			err = raw.Control(func(fd uintptr) { duplicate, _ = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0) })
		}
		if err == nil && duplicate >= 0 {
			reader, writer, e := os.Pipe()
			if e == nil {
				s.pipe = writer
				rawPipe, e := writer.SyscallConn()
				if e == nil {
					e = rawPipe.Control(func(fd uintptr) { s.fd = int(fd) })
				}
				if e == nil && s.fd >= 0 {
					// queue is the admission bound for the pipe as well. Every submitted
					// line is the fixed refusal diagnostic and fits an atomic pipe write.
					go func() {
						defer close(s.done)
						defer unix.Close(duplicate)
						defer reader.Close()
						buf := make([]byte, len(refusalLine))
						for {
							n, err := io.ReadFull(reader, buf)
							if err != nil {
								return
							}
							select {
							case <-s.queue:
							default:
							}
							if written, err := unix.Write(duplicate, buf[:n]); err != nil || written != n {
								s.dropped.Add(1)
							}
						}
					}()
					return s
				}
				reader.Close()
				writer.Close()
				s.pipe = nil
				s.fd = -1
			}
			unix.Close(duplicate)
		}
		w = dropLogWriter{}
	} else if n, ok := w.(NonblockingLogWriter); ok {
		w = nonblockingLogAdapter{n}
	} else if _, ok := w.(*bytes.Buffer); !ok && w != io.Discard {
		w = dropLogWriter{}
	}
	go func() {
		defer close(s.done)
		for line := range s.queue {
			if n, err := io.WriteString(w, line); err != nil || n != len(line) {
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
		if s.pipe != nil {
			if n, err := unix.Write(s.fd, []byte(line)); err != nil || n != len(line) {
				select {
				case <-s.queue:
				default:
				}
				s.dropped.Add(1)
			}
		}
	default:
		s.dropped.Add(1)
	}
}
func (s *logSink) close() {
	s.closeOnce.Do(func() {
		if s.pipe != nil {
			// Closing the private pipe lets a drained worker finish naturally. A
			// suspended destination may retain its one worker until the reader resumes.
			s.pipe.Close()
			return
		}
		close(s.queue)
		<-s.done
	})
}
