//go:build linux || darwin

package courier

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Proves keepalives cross the real framed connection, stay invisible, and keep
// a silent command alive past its former execution idle budget. Compressed
// intervals witness the mechanism; the constants assert the production bounds.
func TestCourierKeepaliveNoIdleCutoff(t *testing.T) {
	if KeepaliveInterval != 20*time.Second || StationReadDeadline != 3*KeepaliveInterval {
		t.Fatal("contract timing changed")
	}
	r := executionServerOptions(t, func(ctx context.Context, inv Invocation) int {
		timer := time.NewTimer(250 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
			return 0
		case <-ctx.Done():
			return 1
		}
	}, 50*time.Millisecond, func(s *server) { s.keepalive = 10 * time.Millisecond })
	var out, diag bytes.Buffer
	code, err := client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"wait"}}, &out, &diag, 50*time.Millisecond)
	if err != nil || code != 0 || out.Len() != 0 || diag.Len() != 0 {
		t.Fatal("silent live command expired or keepalive printed", err)
	}
}

// Proves missing keepalives yield exactly the idle class, beside live streaming
// and shutdown's unavailable class. Injected short deadlines avoid a minute
// per negative control; no host idle-wake reliability is claimed.
func TestCourierIdleAndShutdownDiagnostics(t *testing.T) {
	for _, mode := range []string{"idle", "shutdown", "valid"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			r := executionServerOptions(t, func(ctx context.Context, inv Invocation) int {
				close(entered)
				select {
				case <-ctx.Done():
					return 1
				case <-release:
					return 0
				}
			}, time.Second, func(s *server) {
				s.keepalive = 10 * time.Millisecond
				if mode == "idle" {
					s.keepalive = time.Hour
				}
			})
			done := make(chan error, 1)
			go func() {
				_, err := client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"wait"}}, io.Discard, io.Discard, 40*time.Millisecond)
				done <- err
			}()
			select {
			case <-entered:
			case err := <-done:
				t.Fatal("client ended early", err)
			case <-r.done:
				t.Fatal("daemon ended")
			case <-time.After(time.Second):
				t.Fatal("no invocation")
			}
			if mode == "shutdown" {
				r.cancel()
			}
			if mode == "valid" {
				close(release)
			}
			select {
			case err := <-done:
				want := error(IdleTimeout)
				if mode == "shutdown" {
					want = Unavailable
				}
				if mode == "valid" {
					want = nil
				}
				if !errors.Is(err, want) {
					t.Fatalf("class = %v, want %v", err, want)
				}
			case <-time.After(LockReleaseBound):
				t.Fatal("client stuck")
			}
		})
	}
}

// Proves both caps refuse immediately without queuing; releasing one slot
// admits the valid neighbor. The wire test separately checks the closed class.
func TestCourierInvocationCaps(t *testing.T) {
	if MaxStationInvocations != 4 || MaxInvocations != 32 || MaxConnections != 64 {
		t.Fatal("contract caps changed")
	}
	s := &server{}
	for station := 0; station < MaxInvocations/MaxStationInvocations; station++ {
		name := fmt.Sprint(station)
		for range MaxStationInvocations {
			if !s.acquire(name) {
				t.Fatal("valid slot refused")
			}
		}
		if s.acquire(name) {
			t.Fatal("station cap bypassed")
		}
	}
	if s.acquire("extra") {
		t.Fatal("global cap bypassed")
	}
	s.release("0")
	if !s.acquire("extra") {
		t.Fatal("released global slot not reused")
	}
}

// Proves authenticated cap, selection and allowlist diagnostics never echo
// request operands; every refusal is followed by a successful neighbor.
func TestCourierClosedWireDiagnostics(t *testing.T) {
	entered := make(chan struct{}, MaxStationInvocations)
	r := executionServer(t, func(ctx context.Context, inv Invocation) int {
		if inv.Request.Argv[0] == "inbox" {
			entered <- struct{}{}
			<-ctx.Done()
		}
		return 0
	}, time.Second)
	for range MaxStationInvocations {
		rawInvocation(t, r)
	}
	for range MaxStationInvocations {
		select {
		case <-entered:
		case <-r.done:
			t.Fatal("server ended")
		case <-time.After(time.Second):
			t.Fatal("not entered")
		}
	}
	_, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
	if err != Busy {
		t.Fatal("capacity diagnostic", err)
	}
	for _, tc := range []struct {
		args  []string
		class Diagnostic
	}{
		{[]string{"post", "room", "TOKEN_BODY_MARKER", "--id-file", "PRIVATE_PATH"}, NotAllowed},
		{[]string{"whoami", "--profile", "a", "--profile", "b"}, NotAllowed},
		{[]string{"whoami", "--as", "other"}, IdentityMismatch},
	} {
		var out, diag bytes.Buffer
		_, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: tc.args}, &out, &diag)
		if err != tc.class || out.Len() != 0 || diag.Len() != 0 || strings.Contains(err.Error(), "MARKER") {
			t.Fatal("open diagnostic")
		}
	}
	// A distinct valid station has capacity even while the first is full.
	key, err := Register(r.home, "neighbor", "identity", uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal(err)
	}
	if code, err := Client(context.Background(), r.path, "neighbor", "identity", key, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard); err != nil || code != 0 {
		t.Fatal("valid neighbor refused", err)
	}
	_, err = Client(context.Background(), r.path, "neighbor", "identity", bytes.Repeat([]byte{0}, StationKeyLen), Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
	if err != EnrollmentInvalid {
		t.Fatal("bad key class", err)
	}
	_, err = Client(context.Background(), filepath.Join(filepath.Dir(r.path), "absent"), "neighbor", "identity", key, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard)
	if err != Unavailable {
		t.Fatal("missing endpoint class", err)
	}
}

// Proves simultaneous valid hello lookups share the registry lock; the bad-MAC
// neighbor still refuses. Synchronization carries failures, not testing calls
// from worker goroutines. It does not qualify multiple kernel UIDs.
func TestCourierEightConcurrentHellos(t *testing.T) {
	r := start(t, nil, 0)
	ready := make(chan struct{})
	done := make(chan error, 8)
	for range 8 {
		go func() {
			select {
			case <-ready:
			case <-r.s.ctx.Done():
				done <- context.Canceled
				return
			}
			c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: r.path, Net: "unix"})
			if err != nil {
				done <- err
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(time.Second))
			b, err := readFrame(c)
			var challenge struct {
				Nonce string `json:"nonce"`
			}
			if err == nil {
				err = json.Unmarshal(b, &challenge)
			}
			if err != nil {
				done <- err
				return
			}
			// Decode through the same strict hex helper as the protocol fixture.
			var nonce []byte
			nonce, err = hex.DecodeString(challenge.Nonce)
			if err != nil {
				done <- err
				return
			}
			if err = writeFrame(c, hello(r.secret, nonce, "station", "identity")); err != nil {
				done <- err
				return
			}
			b, err = readFrame(c)
			if err == nil && string(b) != string(successFrame) {
				err = errors.New("valid hello refused")
			}
			done <- err
		}()
	}
	close(ready)
	for range 8 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-r.done:
			t.Fatal("daemon ended")
		case <-time.After(3 * time.Second):
			t.Fatal("hello stuck")
		}
	}
	c, n := connect(t, r)
	if writeFrame(c, hello(bytes.Repeat([]byte{0}, 32), n, "station", "identity")) != nil {
		t.Fatal("write")
	}
	assertRefused(t, c)
}

// Proves transient Accept failures back off and preserve the endpoint; a
// successful neighbor executes after both injected kernel error classes.
func TestCourierTransientAccept(t *testing.T) {
	var calls atomic.Int32
	var first, second atomic.Int64
	r := executionServerOptions(t, func(context.Context, Invocation) int { return 0 }, time.Second, func(s *server) {
		s.accept = func(l *net.UnixListener) (*net.UnixConn, error) {
			switch calls.Add(1) {
			case 1:
				first.Store(time.Now().UnixNano())
				return nil, unix.EMFILE
			case 2:
				second.Store(time.Now().UnixNano())
				return nil, unix.ECONNABORTED
			}
			return l.AcceptUnix()
		}
	})
	if code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard); err != nil || code != 0 {
		t.Fatal("transient accept killed listener", err)
	}
	if time.Duration(second.Load()-first.Load()) < AcceptBackoff {
		t.Fatal("accept retried without backoff")
	}
	if calls.Load() < 3 {
		t.Fatal("injection not exercised")
	}
}

// Proves register, socket lock and registry files get explicit modes under a
// hostile umask. Existing unsafe files remain refused, never chmod-repaired.
func TestCourierExplicitModes(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socketRoot := privateTemp(t)
	old := unix.Umask(0277)
	defer unix.Umask(old)
	home := filepath.Join(root, "home")
	key, err := Register(home, "station", "identity", uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal(err)
	}
	r := &running{home: home, path: filepath.Join(socketRoot, "s", "c.sock"), secret: key, done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	ready := make(chan struct{})
	r.s = &server{home: home, log: io.Discard, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: time.Second, idle: time.Second, handler: func(context.Context, Invocation) int { return 0 }}
	go func() { r.done <- r.s.serve(ctx, r.path, func() { close(ready) }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-r.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(LockReleaseBound):
			t.Error("shutdown timeout")
		}
	})
	select {
	case <-ready:
	case err := <-r.done:
		t.Fatal(err)
	case <-time.After(LockReleaseBound):
		t.Fatal("startup timeout")
	}

	for _, p := range []string{filepath.Join(r.home, ".stations.lock"), filepath.Join(r.home, "stations.json"), filepath.Join(filepath.Dir(r.path), ".courier.lock")} {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal("umask changed explicit file mode")
		}
	}
	for path, mode := range map[string]os.FileMode{r.home: 0700, filepath.Dir(r.path): 0711, r.path: 0666} {
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != mode {
			t.Fatal("explicit directory/socket mode", path, err)
		}
	}
	p := filepath.Join(r.home, "stations.json")
	if err := os.Chmod(p, 0644); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookup(r.home, "station"); ok {
		t.Fatal("unsafe file repaired")
	}
	if err := os.Chmod(p, 0600); err != nil {
		t.Fatal(err)
	}
	if code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard); err != nil || code != 0 {
		t.Fatal("valid mode neighbor refused", err)
	}
}

// Proves a stopped reader backpressures a writer using only one bounded chunk,
// then times out and cancels. The adjacent reading client preserves all bytes.
func TestCourierOutputBackpressure(t *testing.T) {
	for _, reading := range []bool{false, true} {
		t.Run(fmt.Sprint(reading), func(t *testing.T) {
			finished := make(chan error, 1)
			r := executionServer(t, func(ctx context.Context, inv Invocation) int {
				block := make([]byte, OutputChunkBytes)
				for range 128 {
					if _, err := inv.Stdout.Write(block); err != nil {
						finished <- err
						return 1
					}
				}
				finished <- nil
				return 0
			}, 100*time.Millisecond)
			if reading {
				if code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"inbox"}}, io.Discard, io.Discard); err != nil || code != 0 {
					t.Fatal("reading neighbor failed", err)
				}
			} else {
				c, _ := rawInvocation(t, r)
				defer c.Close()
				// Let the deadline elapse with no reads at all.
				select {
				case <-finished:
					finished <- errRefused
				case <-r.done:
					t.Fatal("daemon ended")
				case <-time.After(LockReleaseBound):
					t.Fatal("reader did not time out")
				}
				b, err := readFrame(c)
				var p packet
				if err != nil || json.Unmarshal(b, &p) != nil || p.Type != "stdout" || len(p.Data) > OutputChunkBytes {
					t.Fatal("bounded output missing")
				}
				b, err = readFrame(c)
				if err != nil || diagnosticFrame(b, p.ID) != IdleTimeout {
					t.Fatal("stalled reader diagnostic missing", err)
				}
			}
			select {
			case err := <-finished:
				if reading && err != nil || !reading && err == nil {
					t.Fatal("wrong backpressure result", err)
				}
			case <-r.done:
				t.Fatal("daemon ended")
			case <-time.After(LockReleaseBound):
				t.Fatal("blocked output did not stop")
			}
		})
	}
}

// Proves invocation helpers and the bounded file-log worker return to a measured
// baseline after cancellation/shutdown, even with an undrained stderr pipe.
// A tolerance permits runtime service goroutines; this is not a heap soak.
func TestCourierGoroutineLeakBlockedLogShutdown(t *testing.T) {
	baseline := runtime.NumGoroutine()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	for range 4 {
		r := executionServerOptions(t, func(context.Context, Invocation) int { return 0 }, time.Second, func(s *server) { s.log = writer })
		for range 20000 {
			r.s.sink.write(refusalLine)
		}
		for range 8 {
			if code, err := Client(context.Background(), r.path, "station", "identity", r.secret, Request{Argv: []string{"whoami"}}, io.Discard, io.Discard); err != nil || code != 0 {
				t.Fatal("blocked logging blocked hello", err)
			}
		}
		r.cancel()
		select {
		case err := <-r.done:
			r.done <- err
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(LockReleaseBound):
			t.Fatal("blocked logging blocked shutdown")
		}
		if r.s.sink.dropped.Load() == 0 {
			t.Fatal("drop witness absent")
		}
	}
	// Resume the external log reader after verifying bounded shutdown. This
	// also releases the one destination worker owned by each stopped serve.
	writer.Close()
	drained := make(chan struct{})
	go func() { io.Copy(io.Discard, reader); close(drained) }()
	select {
	case <-drained:
	case <-time.After(LockReleaseBound):
		t.Fatal("log workers did not drain")
	}
	deadline := time.NewTimer(LockReleaseBound)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for runtime.NumGoroutine() > baseline+3 {
		select {
		case <-deadline.C:
			t.Fatal("goroutines retained")
		case <-tick.C:
		}
	}
	// Drained stderr neighbor: every submitted line arrives through a pipe.
	drainedRead, drainedWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer drainedRead.Close()
	defer drainedWrite.Close()
	logDone := make(chan []byte, 1)
	go func() { data, _ := io.ReadAll(drainedRead); logDone <- data }()
	sink := newLogSink(drainedWrite)
	for range LogQueueLines {
		sink.write(refusalLine)
	}
	sink.close()
	drainedWrite.Close()
	select {
	case data := <-logDone:
		if string(data) != strings.Repeat(refusalLine, LogQueueLines) || sink.dropped.Load() != 0 {
			t.Fatal("drained log lost lines")
		}
	case <-time.After(LockReleaseBound):
		t.Fatal("drained logger stuck")
	}
}

// Proves output flow ACKs cannot substitute for receipt ACKs, and shutdown
// releases an unacknowledged delivery without receipting. An already accepted
// receipt ACK is the valid neighbor and stays completed across shutdown.
func TestCourierShutdownReceiptBarrier(t *testing.T) {
	for _, acknowledged := range []bool{false, true} {
		t.Run(fmt.Sprint(acknowledged), func(t *testing.T) {
			var receipts atomic.Int32
			finished := make(chan struct{})
			r := executionServer(t, func(ctx context.Context, inv Invocation) int {
				defer close(finished)
				if _, err := io.WriteString(inv.Stdout, "mail"); err != nil {
					return 1
				}
				if inv.Delivered() != nil {
					return 1
				}
				receipts.Add(1)
				return 0
			}, time.Second)
			c, id := rawInvocation(t, r)
			b, err := readFrame(c)
			var out packet
			if err != nil || json.Unmarshal(b, &out) != nil || out.Type != "stdout" {
				t.Fatal("output missing")
			}
			acknowledgeOutput(t, c, out)
			b, err = readFrame(c)
			var delivery packet
			if err != nil || json.Unmarshal(b, &delivery) != nil || delivery.Type != "delivery" {
				t.Fatal("delivery missing")
			}
			if receipts.Load() != 0 {
				t.Fatal("flow ACK completed receipt")
			}
			if acknowledged {
				b, err = json.Marshal(packet{Type: "ack", ID: id, Batch: delivery.Batch, Identity: "identity"})
				if err != nil || writeFrame(c, b) != nil {
					t.Fatal("ack write")
				}
				select {
				case <-finished:
				case <-r.done:
					t.Fatal("daemon stopped")
				case <-time.After(LockReleaseBound):
					t.Fatal("ACK not accepted")
				}
			}
			r.cancel()
			select {
			case <-finished:
			case <-time.After(LockReleaseBound):
				t.Fatal("shutdown retained delivery")
			}
			want := int32(0)
			if acknowledged {
				want = 1
			}
			if receipts.Load() != want {
				t.Fatal("shutdown receipt boundary")
			}
		})
	}
}

// Proves forged and repeated flow ACKs cannot advance an output writer, with
// the valid ACK as neighbor. These ACKs never exercise the receipt callback.
func TestCourierOutputACKBinding(t *testing.T) {
	for _, mode := range []string{"valid", "identity", "invocation", "batch", "receipt-kind"} {
		t.Run(mode, func(t *testing.T) {
			finished := make(chan bool, 1)
			r := executionServer(t, func(ctx context.Context, inv Invocation) int {
				_, err := io.WriteString(inv.Stdout, "data")
				finished <- err == nil
				return 0
			}, time.Second)
			c, _ := rawInvocation(t, r)
			b, err := readFrame(c)
			var p packet
			if err != nil || json.Unmarshal(b, &p) != nil {
				t.Fatal("output missing")
			}
			ack := packet{Type: "output_ack", ID: p.ID, Batch: p.Batch, Identity: p.Identity}
			switch mode {
			case "identity":
				ack.Identity = "other"
			case "invocation":
				ack.ID = strings.Repeat("0", 32)
			case "batch":
				ack.Batch = strings.Repeat("0", 32)
			case "receipt-kind":
				ack.Type = "ack"
			}
			b, err = json.Marshal(ack)
			if err != nil || writeFrame(c, b) != nil {
				t.Fatal("ack write")
			}
			select {
			case ok := <-finished:
				if ok != (mode == "valid") {
					t.Fatal("output ACK binding violated")
				}
			case <-r.done:
				t.Fatal("daemon stopped")
			case <-time.After(LockReleaseBound):
				t.Fatal("writer stuck")
			}
		})
	}
}

// Proves a post-bind startup refusal unlinks exactly its own socket and releases
// the serve lock. The immediate successful start on the same path is neighbor.
func TestCourierRefusedStartupUnlinksSocket(t *testing.T) {
	home, _ := registered(t)
	socket := filepath.Join(privateTemp(t), "s", "c.sock")
	for _, mode := range []string{"permissions", "path-lookup"} {
		s := &server{home: home, log: io.Discard, conns: map[*net.UnixConn]bool{}}
		if mode == "permissions" {
			s.permissions = func(*os.File, string, os.FileInfo) error { return errRefused }
		} else {
			s.socketInfo = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
		}
		if err := s.serve(context.Background(), socket, nil); err == nil {
			t.Fatal("startup refusal not exercised", mode)
		}
		if _, err := os.Lstat(socket); !os.IsNotExist(err) {
			t.Fatal("refused startup left socket", mode)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ready := make(chan struct{})
	s := &server{home: home, log: io.Discard, conns: map[*net.UnixConn]bool{}, peer: peerUID, handshake: time.Second, idle: time.Second}
	go func() { done <- s.serve(ctx, socket, func() { close(ready) }) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal("neighbor refused", err)
	case <-time.After(LockReleaseBound):
		t.Fatal("neighbor stuck")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(LockReleaseBound):
		t.Fatal("shutdown stuck")
	}
}

type blockingLog struct{ called chan struct{} }

func (w blockingLog) Write([]byte) (int, error) { close(w.called); select {} }

// Proves unclassified blocking writers are never called, with a supported
// in-memory writer as neighbor. No detached blocked writer goroutine is created.
func TestCourierUnclassifiedLogWriterDrops(t *testing.T) {
	called := make(chan struct{})
	sink := newLogSink(blockingLog{called})
	sink.write(refusalLine)
	sink.close()
	select {
	case <-called:
		t.Fatal("uncancellable writer called")
	default:
	}
	if sink.dropped.Load() != 1 {
		t.Fatal("unsupported writer drop not counted")
	}
	var out bytes.Buffer
	sink = newLogSink(&out)
	sink.write(refusalLine)
	sink.close()
	if out.String() != refusalLine || sink.dropped.Load() != 0 {
		t.Fatal("supported writer lost line")
	}
}
