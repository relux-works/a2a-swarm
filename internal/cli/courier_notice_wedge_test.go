//go:build linux || darwin

package cli

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/relux-works/a2a-swarm/internal/courier"
)

// Exercise the production C5 deadline without changing C5: a transparent
// Unix-frame proxy drops only keepalives after a successful request. It never
// records or prints authentication material. All sockets are fixture-owned.
func TestCMSilentStreamDeadline(t *testing.T) {
	admitted := make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, _ courier.Invocation) int {
		close(admitted)
		<-ctx.Done()
		return 0
	})
	path := filepath.Join(shortCourierBase(t), "w.sock")
	proxy, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal("proxy listen failed")
	}
	defer proxy.Close()
	if err := os.WriteFile(filepath.Join(s.home, ".a2a", "courier.sock"), []byte(path+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	stop := make(chan struct{})
	go func() {
		defer close(done)
		station, err := proxy.Accept()
		if err != nil {
			return
		}
		defer station.Close()
		backend, err := net.Dial("unix", s.socket)
		if err != nil {
			return
		}
		defer backend.Close()
		copying := make(chan struct{})
		go func() { defer close(copying); _, _ = io.Copy(backend, station); _ = backend.Close() }()
		stopped := make(chan struct{})
		go func() {
			defer close(stopped)
			select {
			case <-stop:
			case <-copying:
			}
			_ = station.Close()
			_ = backend.Close()
		}()
		defer func() { _ = station.Close(); _ = backend.Close(); <-copying; <-stopped }()
		for {
			var header [4]byte
			if _, err := io.ReadFull(backend, header[:]); err != nil {
				return
			}
			n := binary.BigEndian.Uint32(header[:])
			if n > courier.MaxFrameBytes {
				return
			}
			body := make([]byte, n)
			if _, err := io.ReadFull(backend, body); err != nil {
				return
			}
			var frame struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(body, &frame) != nil {
				return
			}
			if frame.Type == "keepalive" {
				continue
			}
			if _, err := station.Write(header[:]); err != nil {
				return
			}
			if _, err := station.Write(body); err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = proxy.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("proxy did not stop")
		}
	})
	l := cmStartC5(t, s)
	c := cmConnect(t, l)
	select {
	case <-admitted:
	case <-time.After(5 * time.Second):
		t.Fatal("healthy request not admitted")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), courier.StationReadDeadline+time.Second)
	defer cancel()
	_, _, err = c.Read(ctx)
	elapsed := time.Since(start)
	var closed websocket.CloseError
	// Scheduling allowance is measured and reported, not a longer protocol
	// deadline. The source of the bound remains the production 60 s deadline.
	if !errors.As(err, &closed) || closed.Reason != "courier_idle_timeout" || elapsed < 59*time.Second || elapsed > courier.StationReadDeadline+time.Second {
		t.Fatal("silent stream lacked bounded exact idle reason", elapsed)
	}
	t.Logf("observed silent-stream closure: %s (production deadline 60s)", elapsed)
}
