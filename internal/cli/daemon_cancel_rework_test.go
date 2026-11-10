//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"golang.org/x/sys/unix"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// A real open file description owns the contender until after cancellation.
// No stub can make acquisition appear bounded by returning early.
func TestDaemonStateFlockCancellation(t *testing.T) {
	f := newDaemonFixture(t)
	for _, contended := range []bool{true, false} {
		p := f.paths["alice"]
		lock, err := os.OpenFile(filepath.Join(filepath.Dir(p.StateFile("alice")), "alice.lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal(err)
		}
		if contended {
			if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan result, 1)
		go func() {
			var out, diag bytes.Buffer
			code := f.app("alice", "", &out, &diag).Run(ctx, []string{"wait", "--timeout", "1ms"})
			done <- result{code: code, stdout: out.String(), stderr: diag.String()}
		}()
		if contended {
			ticker := time.NewTicker(time.Millisecond)
			timer := time.NewTimer(daemon.LockReleaseBound)
			held := false
			for !held {
				release, err := config.AcquireRawConsumerLock(p, "alice", time.Now().Add(time.Millisecond))
				if err != nil {
					held = true
					break
				}
				release()
				select {
				case r := <-done:
					t.Fatal("wait ended before contention", r)
				case <-f.done:
					t.Fatal("daemon stopped")
				case <-timer.C:
					t.Fatal("consumer lock not held")
				case <-ticker.C:
				}
			}
			ticker.Stop()
			timer.Stop()
			cancel()
		}
		select {
		case r := <-done:
			if contended && r.code != 130 {
				t.Fatal("cancel exit", r)
			}
			if !contended && r.code != 0 {
				t.Fatal("uncontended neighbor", r)
			}
		case <-f.done:
			t.Fatal("daemon stopped")
		case <-time.After(daemon.LockReleaseBound):
			t.Fatal("station retained")
		}
		// The contender remains held while another open description probes release.
		release, err := config.AcquireRawConsumerLock(p, "alice", time.Now().Add(daemon.LockReleaseBound))
		if err != nil {
			t.Fatal("consumer retained behind state flock", err)
		}
		release()
		unix.Flock(int(lock.Fd()), unix.LOCK_UN)
		lock.Close()
		cancel()
	}
}

// Build the shipped entry point: a test-only signal handler cannot satisfy
// this control. A completed pipe write proves main has reached stdin reading.
func TestDaemonStdinSIGINTBinary(t *testing.T) {
	binary := filepath.Join(canonicalRoot(t), "a2a")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/a2a")
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=readonly")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	entered := make(chan int, 2)
	s := newC5Station(t, func(ctx context.Context, inv daemon.Invocation) int { entered <- len(inv.Request.Stdin); return 0 })
	for _, complete := range []bool{false, true} {
		cmd := exec.Command(binary, "post", "room", "-")
		cmd.Env = []string{"HOME=" + s.home, config.EnvConfigDir + "=" + s.home, "PATH=" + os.Getenv("PATH")}
		inputRead, input, err := os.Pipe()
		cmd.Stdin = inputRead
		defer inputRead.Close()
		if err != nil {
			t.Fatal(err)
		}
		var out, diag bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diag
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		inputDone := make(chan error, 1)
		go func() { _, err := input.Write(bytes.Repeat([]byte("x"), 1024)); inputDone <- err }()
		select {
		case err := <-inputDone:
			if err != nil {
				t.Fatal(err)
			}
		case err := <-done:
			t.Fatal("child exited before reading stdin", err)
		case <-s.done:
			t.Fatal("daemon stopped")
		case <-time.After(daemon.LockReleaseBound):
			cmd.Process.Signal(os.Interrupt)
			input.Close()
			t.Fatal("stdin never consumed")
		}
		// Kernel pending-byte observation proves the shipped main is reading
		// stdin, even on Darwin where the pipe can buffer the entire test input.
		request := uint(0x541b)
		if runtime.GOOS == "darwin" {
			request = 0x4004667f
		}
		ticker := time.NewTicker(time.Millisecond)
		timer := time.NewTimer(daemon.LockReleaseBound)
		for {
			n, err := unix.IoctlGetInt(int(inputRead.Fd()), request)
			if err != nil {
				t.Fatal(err)
			}
			if n == 0 {
				break
			}
			select {
			case err := <-done:
				t.Fatal("child exited before stdin drain", err)
			case <-s.done:
				t.Fatal("daemon stopped")
			case <-timer.C:
				t.Fatal("stdin not read")
			case <-ticker.C:
			}
		}
		ticker.Stop()
		timer.Stop()
		if complete {
			input.Close()
		} else {
			if err := cmd.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case err := <-done:
			if complete {
				if err != nil {
					t.Fatal("EOF neighbor", err, diag.String())
				}
			} else {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 130 {
					t.Fatal("SIGINT exit", err)
				}
			}
		case <-s.done:
			t.Fatal("daemon stopped")
		case <-time.After(daemon.LockReleaseBound):
			input.Close()
			cmd.Process.Signal(os.Interrupt)
			t.Fatal("SIGINT/EOF retained child")
		}
		input.Close()
		if out.Len() != 0 || diag.Len() != 0 {
			t.Fatal("unexpected child output", diag.String())
		}
		if complete {
			select {
			case n := <-entered:
				if n != 1024 {
					t.Fatal("stdin changed", n)
				}
			case <-s.done:
				t.Fatal("daemon stopped")
			case <-time.After(daemon.LockReleaseBound):
				t.Fatal("EOF not posted")
			}
		} else {
			select {
			case <-entered:
				t.Fatal("cancelled stdin posted")
			default:
			}
		}
	}
}

type slowDisarmTransport struct {
	next             http.RoundTripper
	entered, release chan struct{}
}

func (s slowDisarmTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && r.Body != nil {
		data, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		if bytes.Contains(data, []byte(`"doorbell":"none"`)) {
			close(s.entered)
			<-s.release
		}
	}
	return s.next.RoundTrip(r)
}

// Even a disarm transport that ignores cancellation cannot retain the consumer
// lock: the real wait releases before entering that transport. Normal disarm is
// the neighbor after the held transport is released.
func TestDaemonWaitSlowDisarmRelease(t *testing.T) {
	f := newDaemonFixture(t)
	root := filepath.Join(f.home, "identities", "alice")
	var initOut, initErr bytes.Buffer
	init := &App{Stdout: &initOut, Stderr: &initErr, HTTP: f.daemon.HTTP, Now: time.Now, connectionSecretStore: f.store, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return root
		}
		return ""
	}}
	if code := init.Run(context.Background(), []string{"--as", "alice", "status", "init"}); code != 0 {
		t.Fatal("status fixture", code, initErr.String())
	}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	next := f.daemon.HTTP.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	f.daemon.HTTP = &http.Client{Transport: slowDisarmTransport{next, entered, release}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan int, 1)
	go func() {
		var out, diag bytes.Buffer
		done <- f.app("alice", "", &out, &diag).Run(ctx, []string{"wait", "--timeout", "10m", "--status", "idle", "--heartbeat", "1h", "--runtime", "codex", "--hooks", "none"})
	}()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	deadline := time.NewTimer(daemon.LockReleaseBound)
	defer deadline.Stop()
	for f.envs["alice"].hs.SyncCount("@alice:"+testServer) == 0 {
		select {
		case code := <-done:
			t.Fatal("wait exited", code)
		case <-f.done:
			t.Fatal("daemon stopped")
		case <-deadline.C:
			t.Fatal("wait never polled")
		case <-tick.C:
		}
	}
	cancel()
	select {
	case <-entered:
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(daemon.LockReleaseBound):
		t.Fatal("disarm not reached")
	}
	unlock, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", time.Now().Add(daemon.LockReleaseBound))
	if err != nil {
		t.Fatal("disarm retained consumer lock", err)
	}
	unlock()
	close(release)
	select {
	case code := <-done:
		if code != 130 {
			t.Fatal("cancel exit", code)
		}
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(daemon.LockReleaseBound):
		t.Fatal("wait retained")
	}
	f.run("alice", "", "wait", "--timeout", "1ms").ok(t)
}

// Real inbox output waits for its station's output ACK under the state lock.
// Cancellation of a concurrent wait must release its consumer while that ACK
// remains stalled, and the inbox must leave the mail eligible after disconnect.
func TestDaemonWaitInboxContender(t *testing.T) {
	f := newDaemonFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waitDone := make(chan int, 1)
	go func() {
		var out, diag bytes.Buffer
		waitDone <- f.app("alice", "", &out, &diag).Run(ctx, []string{"wait", "--timeout", "10m"})
	}()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	timer := time.NewTimer(daemon.LockReleaseBound)
	defer timer.Stop()
	for f.envs["alice"].hs.SyncCount("@alice:"+testServer) == 0 {
		select {
		case code := <-waitDone:
			t.Fatal("wait exited", code)
		case <-f.done:
			t.Fatal("daemon stopped")
		case <-timer.C:
			t.Fatal("wait never polled")
		case <-tick.C:
		}
	}
	// Persist mail directly using the fixture mailbox, so no sync completion is
	// needed before inbox reaches the state-held output/ACK barrier.
	mb, err := config.OpenMailbox(f.paths["alice"], "alice")
	if err != nil {
		t.Fatal(err)
	}
	_ = mb
	// A pending invite exercises the same state-held output path without peer text.
	if _, err := mb.Append([]config.MailEntry{{ID: "invite:contender", Kind: config.MailInvite, Room: f.rooms["alice"], Label: "room"}}); err != nil {
		t.Fatal(err)
	}
	held := &heldHookFlush{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-held.release:
		default:
			close(held.release)
		}
	}()
	inboxCtx, stopInbox := context.WithCancel(context.Background())
	defer stopInbox()
	inboxDone := make(chan int, 1)
	go func() {
		var diag bytes.Buffer
		inboxDone <- f.app("alice", "", held, &diag).Run(inboxCtx, []string{"inbox"})
	}()
	select {
	case <-held.entered:
	case code := <-inboxDone:
		t.Fatal("inbox exited", code)
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(daemon.LockReleaseBound):
		t.Fatal("inbox never stalled")
	}
	if _, err := config.UpdateStateBy(f.paths["alice"], "alice", time.Now().Add(time.Millisecond), func(*config.State) error { return nil }); err != config.ErrLockTimeout {
		t.Fatal("inbox not holding real state lock", err)
	}
	cancel()
	select {
	case code := <-waitDone:
		if code != 130 {
			t.Fatal("cancel exit", code)
		}
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(daemon.LockReleaseBound):
		t.Fatal("wait not cancelled")
	}
	unlock, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", time.Now().Add(daemon.LockReleaseBound))
	if err != nil {
		t.Fatal("inbox stalled consumer release", err)
	}
	unlock()
	stopInbox()
	close(held.release)
	select {
	case code := <-inboxDone:
		if code != 130 {
			t.Fatal("inbox cancel exit", code)
		}
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(daemon.LockReleaseBound):
		t.Fatal("inbox retained")
	}
	f.run("alice", "", "wait", "--timeout", "1ms").ok(t)
}

type heldHookFlush struct {
	entered, release chan struct{}
	bytes.Buffer
}

func (w *heldHookFlush) Flush() error { close(w.entered); <-w.release; return nil }
