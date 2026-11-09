//go:build linux || darwin

package courier

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

// Pause an actual diagnostic between header and body, then deliver a tick to
// the real keepalive goroutine. An unlocked diagnostic must let that tick write
// in the gap; the serialized path keeps the frame intact. No timer schedules
// the collision. The no-tick refusal and a successful successor are neighbors.
func TestCourierDiagnosticKeepaliveSerialization(t *testing.T) {
	for _, mode := range []string{"refusal-tick", "panic-tick", "refusal-no-tick"} {
		t.Run(mode, func(t *testing.T) {
			ticks := make(chan time.Time, 1)
			attempt := make(chan struct{}, 1)
			written := make(chan struct{}, 1)
			serialized := make(chan bool, 1)
			var active atomic.Pointer[stream]
			r := executionServerOptions(t, func(_ context.Context, inv Invocation) int {
				active.Store(inv.Stdout.(output).s)
				if inv.Request.Argv[0] == "whoami" {
					return 0
				}
				if mode == "panic-tick" {
					panic("private diagnostic fixture")
				}
				return inv.Refuse(LeaseConflict)
			}, LockReleaseBound, func(s *server) {
				s.keepaliveTicks = ticks
				s.keepaliveAttempt = func() { attempt <- struct{}{} }
				s.frameWrite = func(w io.Writer, b []byte) error {
					var p packet
					if err := json.Unmarshal(b, &p); err != nil {
						return err
					}
					if p.Type == "keepalive" {
						err := writeFrame(w, b)
						written <- struct{}{}
						return err
					}
					if p.Type != "error" || mode == "refusal-no-tick" {
						return writeFrame(w, b)
					}
					st := active.Load()
					locked := !st.mu.TryLock()
					if !locked {
						st.mu.Unlock()
					}
					serialized <- locked
					var header [4]byte
					binary.BigEndian.PutUint32(header[:], uint32(len(b)))
					if err := writeAll(w, header[:]); err != nil {
						return err
					}
					ticks <- time.Now()
					select {
					case <-attempt:
					case <-st.ctx.Done():
						return st.ctx.Err()
					case <-time.After(LockReleaseBound):
						return context.DeadlineExceeded
					}
					if !locked {
						// On the broken path, force the complete keepalive into the
						// diagnostic's body gap, rather than hoping for a race.
						select {
						case <-written:
						case <-st.ctx.Done():
							return st.ctx.Err()
						case <-time.After(LockReleaseBound):
							return context.DeadlineExceeded
						}
					}
					return writeAll(w, b)
				}
			})
			code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"inbox"}}, io.Discard, io.Discard)
			if mode != "refusal-no-tick" {
				select {
				case locked := <-serialized:
					if !locked {
						t.Fatal("post-handler diagnostic bypassed stream writer; tick interleaved its frame")
					}
				case <-r.done:
					t.Fatal("daemon stopped")
				case <-time.After(LockReleaseBound):
					t.Fatal("diagnostic was not exercised")
				}
			}
			want := LeaseConflict
			if mode == "panic-tick" {
				want = Unavailable
			}
			if code != 1 || err != want {
				t.Fatal("diagnostic framing/class changed", code, err)
			}
			if code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard); code != 0 || err != nil {
				t.Fatal("successor refused", code, err)
			}
		})
	}
}
