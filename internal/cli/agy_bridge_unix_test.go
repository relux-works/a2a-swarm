//go:build darwin || linux

package cli

import (
	"context"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"golang.org/x/sys/unix"
	"os"
	"strings"
	"testing"
	"time"
)

// A real inherited blocking pipe stays open at its upstream end. A refusal
// must still join the reader promptly and restore the descriptor flags.
func TestAgyBridgeBlockingPipeRefusal(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := int(r.Fd())
	if err = unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	in, restore, err := agyBridgePipe(r)
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		finished <- NewApp().runAgyBridge(context.Background(), in, agyTestID, agyQualifiedModel, time.Hour)
	}()
	if _, err = w.WriteString("{\"event\":\"init\"}\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if err == nil || !strings.Contains(err.Error(), "invalid agy frame") {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		w.Close()
		<-finished
		t.Fatal("refusal hung behind the upstream writer")
	}
	restore()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_NONBLOCK != 0 {
		t.Fatal("inherited flags not restored")
	}
}

// A stalled native receiver cannot hold the notification transaction indefinitely.
func TestAgyBridgeBlockingPipeOutput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := int(w.Fd())
	if err = unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range [][]byte{make([]byte, 4096), {1}} {
		for {
			_, err = unix.Write(fd, chunk)
			if err == unix.EAGAIN {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err = unix.SetNonblock(fd, false); err != nil {
		t.Fatal(err)
	}
	output, restore, err := agyBridgePipe(w)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := wakemute.WriteOutput(ctx, output, []byte("notice")); finished <- err }()
	select {
	case err := <-finished:
		if err == nil {
			t.Fatal("full pipe accepted a notice")
		}
	case <-time.After(2 * time.Second):
		r.Close()
		<-finished
		t.Fatal("native output ignored its deadline")
	}
}
