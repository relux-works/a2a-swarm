package hostnotification

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func socketPair(t *testing.T, kind int) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, kind, 0)
	if err != nil {
		t.Fatal(err)
	}
	var pair [2]*net.UnixConn
	for i, fd := range fds {
		f := os.NewFile(uintptr(fd), "isolated-host-stream")
		conn, err := net.FileConn(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		pair[i] = conn.(*net.UnixConn)
		t.Cleanup(func() { _ = conn.Close() })
	}
	return pair[0], pair[1]
}

// Proves sequential exchanges use the actual inherited STREAM endpoint and
// original correlation, with no pathname dial or additional native operation.
func TestClientSequential(t *testing.T) {
	a, b := socketPair(t, syscall.SOCK_STREAM)
	c, err := New(a, fixtureRequest("trigger").Binding, 1000)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_ = b.SetDeadline(time.Now().Add(3 * time.Second))
		reader := bufio.NewReader(b)
		for i := 0; i < 2; i++ {
			frame, err := reader.ReadBytes('\n')
			if err != nil {
				done <- err
				return
			}
			var r Request
			if err = json.Unmarshal(frame, &r); err != nil {
				done <- err
				return
			}
			response := acceptedResponse(r)
			if r.Method == "lookup_attempt" {
				response.Result = "existing-attempt"
			}
			data, _ := json.Marshal(response)
			_, err = b.Write(append(data, '\n'))
			if err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for _, method := range []string{"trigger", "lookup_attempt"} {
		id, err := NewRequestID()
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Call(context.Background(), id, method, "a2a-"+strings.Repeat("b", 32))
		if err != nil || r.Attempt == nil {
			t.Fatalf("exchange: %v", err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// Proves malformed/lost replies retain unresolved and close this endpoint;
// a second call sends no retry. Peer receipt proves only one request arrived.
func TestClientUncertainReply(t *testing.T) {
	for _, mode := range []string{"lost", "wrong-id", "partial", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			a, b := socketPair(t, syscall.SOCK_STREAM)
			c, err := New(a, fixtureRequest("trigger").Binding, 1000)
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				defer b.Close()
				_ = b.SetDeadline(time.Now().Add(3 * time.Second))
				frame, err := bufio.NewReader(b).ReadBytes('\n')
				if err != nil {
					done <- err
					return
				}
				var r Request
				if err = json.Unmarshal(frame, &r); err != nil {
					done <- err
					return
				}
				var response []byte
				switch mode {
				case "lost":
				case "partial":
					response = []byte(`{"contract":`)
				case "oversize":
					response = []byte(strings.Repeat("x", MaxFrame))
				case "wrong-id":
					v := acceptedResponse(r)
					v.RequestID = "foreign"
					response, _ = json.Marshal(v)
					response = append(response, '\n')
				}
				if len(response) > 0 {
					_, _ = b.Write(response)
				}
				done <- nil
			}()
			if _, err := c.Call(context.Background(), "original-request", "trigger", "a2a-"+strings.Repeat("b", 32)); !errors.Is(err, ErrUnresolved) || !c.closed.Load() {
				t.Fatalf("uncertainty lost: %v", err)
			}
			if _, err := c.Call(context.Background(), "new-request", "trigger", "a2a-"+strings.Repeat("b", 32)); !errors.Is(err, ErrUnresolved) {
				t.Fatalf("closed stream retried: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Proves invalid bounds and datagram endpoints refuse construction, beside a
// real stream; busy use refuses locally without corrupting that stream.
func TestClientAdmission(t *testing.T) {
	a, b := socketPair(t, syscall.SOCK_DGRAM)
	if _, err := New(a, fixtureRequest("trigger").Binding, 1); !errors.Is(err, ErrProtocol) {
		t.Fatalf("datagram admitted: %v", err)
	}
	_ = b.Close()
	a, _ = socketPair(t, syscall.SOCK_STREAM)
	for _, ms := range []int{0, 60001} {
		if _, err := New(a, fixtureRequest("trigger").Binding, ms); err == nil {
			t.Fatal("invalid timeout admitted")
		}
	}
	for _, ms := range []int{1, 60000} {
		c, err := New(a, fixtureRequest("trigger").Binding, ms)
		if err != nil {
			t.Fatal(err)
		}
		c.mu.Lock()
		_, err = c.Call(context.Background(), "request", "trigger", "a2a-"+strings.Repeat("b", 32))
		c.mu.Unlock()
		if !errors.Is(err, ErrBusy) || c.closed.Load() {
			t.Fatalf("busy stream corrupted: %v", err)
		}
	}
}

// Proves explicit timeout and cancellation close an unresolved exchange; the
// isolated peer is closed and joined so no fixture goroutine remains blocked.
func TestClientDeadline(t *testing.T) {
	for _, cancel := range []bool{false, true} {
		a, b := socketPair(t, syscall.SOCK_STREAM)
		c, err := New(a, fixtureRequest("trigger").Binding, 20)
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithCancel(context.Background())
		if cancel {
			stop()
		}
		if _, err := c.Call(ctx, "request", "trigger", "a2a-"+strings.Repeat("b", 32)); !errors.Is(err, ErrUnresolved) || !c.closed.Load() {
			t.Fatalf("deadline not unresolved: %v", err)
		}
		stop()
		_ = b.Close()
	}
}
