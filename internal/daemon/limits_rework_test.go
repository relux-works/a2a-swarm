//go:build linux || darwin

package daemon

import (
	"bytes"
	"context"
	"golang.org/x/sys/unix"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The first shared holder stays inside its callback until the second arrives.
// An exclusive lookup or serialization cannot satisfy this overlap witness.
func TestDaemonRegistrySharedOverlap(t *testing.T) {
	r := start(t, nil, 0)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 2)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	for range 2 {
		go func() {
			done <- registryLocked(r.home, false, true, func(*os.File, registry) error {
				entered <- struct{}{}
				select {
				case <-release:
					return nil
				case <-r.s.ctx.Done():
					return context.Canceled
				}
			})
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case err := <-done:
			t.Fatal("lookup failed during overlap", err)
		case <-r.done:
			t.Fatal("server stopped")
		case <-time.After(LockReleaseBound):
			t.Fatal("registry reads serialized")
		}
	}
	// An exclusive mutation is refused while both shared locks remain held.
	if err := registryLocked(r.home, false, false, func(*os.File, registry) error { return nil }); err != errRefused {
		t.Fatal("mutation did not refuse shared holder", err)
	}
	// Unblock through a distinct channel to retain both done-aware cleanup paths.
	close(release)
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-r.done:
			t.Fatal("server stopped")
		case <-time.After(LockReleaseBound):
			t.Fatal("lookup retained")
		}
	}
	if err := registryLocked(r.home, false, false, func(*os.File, registry) error { return nil }); err != nil {
		t.Fatal("exclusive neighbor", err)
	}
}

// Panic containment is exercised in a real serve goroutine. An uncontained
// panic terminates this test process, rather than being hidden by a test recover.
func TestDaemonHandlerPanicIsolation(t *testing.T) {
	r := executionServer(t, func(ctx context.Context, inv Invocation) int {
		switch inv.Request.Argv[0] {
		case "inbox":
			panic("PRIVATE_MARKER")
		case "read":
			io.WriteString(inv.Stdout, "partial\n")
			panic("PRIVATE_MARKER")
		case "hook":
			io.WriteString(inv.Stdout, "doorbell\n")
			if inv.Delivered() != nil {
				return 1
			}
			panic("PRIVATE_MARKER")
		}
		io.WriteString(inv.Stdout, "alive\n")
		return 0
	}, time.Second)
	for _, args := range [][]string{{"inbox"}, {"read", "room"}, {"hook", "prompt"}} {
		code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: args}, io.Discard, io.Discard)
		if code != 1 || err != Unavailable {
			t.Fatal("panic class", code, err)
		}
		var out bytes.Buffer
		code, err = Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, &out, io.Discard)
		if err != nil || code != 0 || out.String() != "alive\n" {
			t.Fatal("daemon did not survive", code, err)
		}
	}
}

// Blocking stderr flags remain unchanged before, during, and after the sink.
// A drained reader receives every line; a suspended reader cannot pin close.
func TestDaemonLogFlagsAndStatus(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	fd := int(writer.Fd()) // deliberately switch this descriptor to blocking
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.O_NONBLOCK != 0 {
		t.Fatal("fixture must start blocking")
	}
	sink := newLogSink(writer)
	assertFlags := func() {
		got, e := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
		if e != nil || got&unix.O_NONBLOCK != flags&unix.O_NONBLOCK {
			t.Fatal("stderr flags changed", got, flags, e)
		}
	}
	assertFlags()
	for range 20000 {
		sink.write(refusalLine)
	}
	closed := make(chan struct{})
	go func() { sink.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(LockReleaseBound):
		t.Fatal("suspended stderr pinned shutdown")
	}
	assertFlags()
	if sink.dropped.Load() == 0 {
		t.Fatal("no overflow witness")
	}
	writer.Close()
	readDone := make(chan struct{})
	go func() { io.Copy(io.Discard, reader); close(readDone) }()
	select {
	case <-readDone:
	case <-time.After(LockReleaseBound):
		t.Fatal("forwarder retained")
	}
	r := executionServerOptions(t, func(context.Context, Invocation) int { return 0 }, time.Second, func(s *server) { s.log = closedOutput{} })
	for range 17 {
		r.s.sink.write(refusalLine)
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(LockReleaseBound)
	defer timer.Stop()
	for r.s.sink.dropped.Load() != 17 {
		select {
		case <-r.done:
			t.Fatal("daemon stopped")
		case <-timer.C:
			t.Fatal("dropped lines not counted")
		case <-tick.C:
		}
	}
	var out bytes.Buffer
	code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"daemon", "status"}}, &out, io.Discard)
	if code != 0 || err != nil || out.String() != "log_dropped: 17\n" {
		t.Fatal("dropped counter missing", code, err, out.String())
	}
}

// Fill all 32 authenticated invocation slots across eight real station keys;
// the next authenticated station sees the exact busy frame on the wire.
func TestDaemonGlobalBusyWire(t *testing.T) {
	entered := make(chan struct{}, 32)
	r := executionServer(t, func(ctx context.Context, inv Invocation) int { entered <- struct{}{}; <-ctx.Done(); return 1 }, time.Minute)
	keys := make([][]byte, 9)
	for i := range keys {
		key, err := Register(r.home, "s"+string(rune('a'+i)), "identity", uint32(os.Geteuid()), false)
		if err != nil {
			t.Fatal(err)
		}
		keys[i] = key
		defer clear(key)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 32)
	for i := range 32 {
		go func() {
			_, err := Client(ctx, r.path, "s"+string(rune('a'+i/4)), "identity", keys[i/4], Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
			done <- err
		}()
	}
	for range 32 {
		select {
		case <-entered:
		case err := <-done:
			t.Fatal("slot failed", err)
		case <-r.done:
			t.Fatal("daemon stopped")
		case <-time.After(LockReleaseBound):
			t.Fatal("slots not entered")
		}
	}
	code, err := Client(context.Background(), r.path, "si", "identity", keys[8], Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
	if code != 1 || err != Busy {
		t.Fatal("global busy class", code, err)
	}
	cancel()
	for range 32 {
		select {
		case <-done:
		case <-r.done:
			t.Fatal("daemon stopped")
		case <-time.After(LockReleaseBound):
			t.Fatal("slot retained")
		}
	}
	// Exercise the released slot with a bounded command: the handler observes
	// cancellation, but its admission is the valid neighbor after saturation.
	neighborCtx, stop := context.WithCancel(context.Background())
	neighbor := make(chan error, 1)
	go func() {
		_, err := Client(neighborCtx, r.path, "si", "identity", keys[8], Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
		neighbor <- err
	}()
	select {
	case <-entered:
	case err := <-neighbor:
		t.Fatal("released slot refused", err)
	case <-r.done:
		t.Fatal("daemon stopped")
	case <-time.After(LockReleaseBound):
		t.Fatal("released slot not admitted")
	}
	stop()
	select {
	case <-neighbor:
	case <-r.done:
		t.Fatal("daemon stopped")
	case <-time.After(LockReleaseBound):
		t.Fatal("neighbor retained")
	}
}

// Handler-mode accept saturation emits daemon_busy before authentication.
// A normal authenticated request before saturation is the valid neighbor.
func TestDaemonHandlerConnectionBusyWire(t *testing.T) {
	r := executionServerOptions(t, func(context.Context, Invocation) int { return 0 }, time.Second, func(s *server) { s.handshake = time.Minute })
	if code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard); code != 0 || err != nil {
		t.Fatal("valid neighbor", code, err)
	}
	// Wait until the successful connection is removed, avoiding a timing race.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(LockReleaseBound)
	defer timer.Stop()
	for {
		r.s.mu.Lock()
		n := len(r.s.conns)
		r.s.mu.Unlock()
		if n == 0 {
			break
		}
		select {
		case <-r.done:
			t.Fatal("daemon stopped")
		case <-timer.C:
			t.Fatal("neighbor connection retained")
		case <-tick.C:
		}
	}
	for range 64 {
		c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(LockReleaseBound))
		if b, err := readFrame(c); err != nil || !bytes.Contains(b, []byte(`"challenge"`)) {
			t.Fatal("connection not admitted", err)
		}
	}
	code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
	if code != 1 || err != Busy {
		t.Fatal("handler connection busy class", code, err)
	}
}

// Force the EEXIST race with a real concurrent creator's pre-chmod inode.
// An already existing wrong-mode directory remains refused and unchanged.
func TestDaemonDirectoryCreatorRace(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "new")
	calls := 0
	dir, err := openDirectoryModeMkdir(path, true, 0700, func(fd int, name string, mode uint32) error {
		calls++
		if err := unix.Mkdirat(fd, name, 0500); err != nil {
			return err
		}
		return unix.EEXIST
	})
	if err != nil {
		t.Fatal("concurrent creator refused", err)
	}
	dir.Close()
	if calls != 1 {
		t.Fatal("race not exercised")
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("explicit creator mode", err)
	}
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
	if dir, err := openDirectoryMode(path, true, 0700); err != errRefused {
		if dir != nil {
			dir.Close()
		}
		t.Fatal("existing wrong mode adopted", err)
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0500 {
		t.Fatal("existing mode repaired", err)
	}
	os.Chmod(path, 0700)
	dir, err = openDirectoryMode(path, false, 0700)
	if err != nil {
		t.Fatal("valid neighbor", err)
	}
	dir.Close()
}
