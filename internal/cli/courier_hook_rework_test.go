//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"errors"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Every selector position accepted by Authorize must retain hook treatment.
func TestCourierHookGlobalPositions(t *testing.T) {
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		if string(inv.Request.Stdin) == "ok" {
			io.WriteString(inv.Stdout, "doorbell\n")
			if inv.Delivered() != nil {
				return 1
			}
			return 0
		}
		return 1
	})
	for _, selector := range [][]string{{"--as", "identity"}, {"--as=identity"}, {"-as", "identity"}, {"--profile", "p"}, {"--connection-fingerprint=f"}} {
		for _, position := range []int{0, 1, 2, 3, 4} {
			args := []string{"hook", "prompt", "--runtime", "claude"}
			args = append(append(append([]string{}, args[:position]...), selector...), args[position:]...)
			if stationHookEvent(args) != "UserPromptSubmit" {
				t.Fatalf("event missing: %v", args)
			}
			r := s.run(context.Background(), "", args...)
			if r.code != 0 || r.stdout != "" {
				t.Fatalf("hook did not fail open: %v", r)
			}
			data, err := os.ReadFile(filepath.Join(s.home, "state", hookFailureLog))
			if err != nil || !bytes.HasSuffix(data, []byte(" UserPromptSubmit courier_unavailable\n")) {
				t.Fatal("wrong hook failure class", err)
			}
			neighbor := s.run(context.Background(), "ok", args...)
			if neighbor.code != 0 || neighbor.stdout != "doorbell\n" {
				t.Fatal("global selector neighbor", neighbor)
			}
		}
	}
	r := s.run(context.Background(), "", "hook", "session-end")
	if r.code != 0 || r.stdout != "" {
		t.Fatal("session-end did not fail open", r)
	}
	// A selector after -- is literal and cannot manufacture a hook event.
	if stationHookEvent([]string{"hook", "--", "--as", "identity", "prompt"}) != "Hook" {
		t.Fatal("parsed past --")
	}
}

type receiptOutput struct {
	bytes.Buffer
	receipts *atomic.Int32
	flushes  int
	fail     bool
}

func (w *receiptOutput) Write(p []byte) (int, error) {
	if w.receipts.Load() != 0 {
		return 0, errors.New("ACK before stdout write")
	}
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return w.Buffer.Write(p)
}
func (w *receiptOutput) Flush() error {
	if w.receipts.Load() != 0 {
		return errors.New("ACK before stdout flush")
	}
	w.flushes++
	return nil
}

// Real station, socket, Delivered callback and flushable stdout. The failing
// writer must leave the same pending doorbell available for the next invocation.
func TestCourierHookReceiptStdoutBarrier(t *testing.T) {
	var receipts atomic.Int32
	finished := make(chan struct{}, 8)
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		defer func() { finished <- struct{}{} }()
		if receipts.Load() == 0 {
			if _, err := io.WriteString(inv.Stdout, "doorbell\n"); err != nil {
				return 1
			}
			if inv.Delivered() != nil {
				return 1
			}
			receipts.Add(1)
		}
		return 0
	})
	for _, fail := range []bool{true, false, false} {
		out := &receiptOutput{receipts: &receipts, fail: fail}
		var diag bytes.Buffer
		a := &App{Stdout: out, Stderr: &diag, Stdin: strings.NewReader(""), Getenv: func(k string) string {
			if k == config.EnvConfigDir {
				return s.home
			}
			return ""
		}}
		if code := a.Run(context.Background(), []string{"hook", "prompt"}); code != 0 {
			t.Fatal("hook exit", code)
		}
		select {
		case <-finished:
		case <-s.done:
			t.Fatal("daemon stopped")
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("handler stuck")
		}
		if fail {
			if out.Len() != 0 || receipts.Load() != 0 {
				t.Fatal("failed stdout consumed rung")
			}
			data, err := os.ReadFile(filepath.Join(s.home, "state", hookFailureLog))
			if err != nil || bytes.Count(data, []byte("\n")) != 1 || !bytes.HasSuffix(data, []byte(" UserPromptSubmit courier_unavailable\n")) {
				t.Fatal("missing exact failure log", err)
			}
		} else if receipts.Load() != 1 {
			t.Fatal("success not receipted once")
		}
		if !fail && out.Len() > 0 && (out.String() != "doorbell\n" || out.flushes == 0) {
			t.Fatal("doorbell not flushed")
		}
	}
}

// Failure line and counter are updated under the same real inode lock.
// Two concurrent successes may report the accumulated gap at most once.
func TestCourierHookConcurrentAccounting(t *testing.T) {
	home := canonicalRoot(t)
	var diag bytes.Buffer
	if err := updateHookFailures(home, "Stop", courier.Unavailable, &diag); err != nil {
		t.Fatal(err)
	}
	const count = 24
	done := make(chan error, count)
	for range count {
		go func() { done <- updateHookFailures(home, "Stop", courier.Unavailable, io.Discard) }()
	}
	for range count {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("append stuck")
		}
	}
	f, err := hookFailureFile(home, false)
	if err != nil {
		t.Fatal(err)
	}
	gap, err := readHookGap(f)
	f.Close()
	data, e := os.ReadFile(filepath.Join(home, "state", hookFailureLog))
	if err != nil || e != nil || gap.Count != count+1 || bytes.Count(data, []byte("\n")) != count+1 {
		t.Fatal("counter/append mismatch", gap, err, e)
	}
	outputs := make(chan string, 2)
	for range 2 {
		go func() {
			var out bytes.Buffer
			err := updateHookFailures(home, "Stop", "", &out)
			done <- err
			outputs <- out.String()
		}()
	}
	lines := 0
	for range 2 {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("gap stuck")
		}
		lines += strings.Count(<-outputs, "\n")
	}
	if lines != 1 {
		t.Fatal("duplicated gap", lines)
	}
}

// Observe the actual daemon receipt event while stdout Flush is held open.
// Checking a counter only at Write time could miss an ACK racing another path.
func TestCourierHookFlushACKOrdering(t *testing.T) {
	receipt := make(chan struct{})
	finished := make(chan struct{})
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		defer close(finished)
		if _, err := io.WriteString(inv.Stdout, "doorbell\n"); err != nil {
			return 1
		}
		if inv.Delivered() != nil {
			return 1
		}
		close(receipt)
		return 0
	})
	if err := updateHookFailures(s.home, "Stop", courier.Unavailable, io.Discard); err != nil {
		t.Fatal(err)
	}
	w := &heldHookFlush{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-w.release:
		default:
			close(w.release)
		}
	}()
	done := make(chan int, 1)
	var diag bytes.Buffer
	a := &App{Stdout: w, Stderr: &diag, Stdin: strings.NewReader(""), Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return s.home
		}
		return ""
	}}
	go func() { done <- a.Run(context.Background(), []string{"hook", "prompt"}) }()
	select {
	case <-w.entered:
	case <-s.done:
		t.Fatal("daemon stopped")
	case <-done:
		t.Fatal("hook exited before flush")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("flush not reached")
	}
	select {
	case <-receipt:
		t.Fatal("ACK arrived before real stdout flush")
	case <-s.done:
		t.Fatal("daemon stopped")
	case <-done:
		t.Fatal("hook exited during flush")
	case <-time.After(100 * time.Millisecond):
	}
	if diag.Len() != 0 {
		t.Fatal("gap preceded doorbell flush")
	}
	close(w.release)
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("success exit", code)
		}
	case <-s.done:
		t.Fatal("daemon stopped")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("hook stuck")
	}
	select {
	case <-finished:
	case <-s.done:
		t.Fatal("daemon stopped")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("handler stuck")
	}
	select {
	case <-receipt:
	default:
		t.Fatal("flushed neighbor not receipted")
	}
	if strings.Count(diag.String(), "hook deliveries failed open since") != 1 {
		t.Fatal("missing/duplicate gap", diag.String())
	}
	if w.String() != "doorbell\n" {
		t.Fatal("doorbell changed")
	}
}

// Discovery is inside the caller's deadline, and discovery failures retain
// hook exit-0 semantics. The completed discovery/writer are valid neighbors.
func TestCourierHookDiscoveryAndOutputBudget(t *testing.T) {
	for _, mode := range []string{"nil", "resolve-fails"} {
		t.Run(mode, func(t *testing.T) {
			var out, diag bytes.Buffer
			a := &App{Stdout: &out, Stderr: &diag}
			if mode == "resolve-fails" {
				t.Setenv("HOME", "")
				a.Getenv = func(string) string { return "" }
			}
			handled, code := a.stationInvocation(context.Background(), []string{"hook", "prompt"})
			if !handled || code != 0 || out.Len() != 0 || diag.String() != "a2a: courier: enrollment_invalid; re-run enroll\n" {
				t.Fatal("hook discovery did not fail open", handled, code, diag.String())
			}
		})
	}
	home := canonicalRoot(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var out, diag bytes.Buffer
	a := &App{Stdout: &out, Stderr: &diag, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			close(entered)
			<-release
			return home
		}
		return ""
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan int, 1)
	go func() { _, code := a.stationInvocation(ctx, []string{"hook", "prompt"}); done <- code }()
	select {
	case <-entered:
	case <-done:
		t.Fatal("discovery not entered")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("discovery stuck")
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatal("budget exit", code)
		}
	case <-time.After(time.Second):
		t.Fatal("path discovery escaped caller budget")
	}
	close(release)
	// An expired output wrapper must refuse even an in-memory writer.
	expired, cancelExpired := context.WithCancel(context.Background())
	cancelExpired()
	if n, err := (hookContextWriter{expired, &out}).Write([]byte("late")); n != 0 || err != context.Canceled || out.Len() != 0 {
		t.Fatal("post-expiry output", n, err)
	}
	live := hookContextWriter{context.Background(), &out}
	if n, err := live.Write([]byte("ok")); n != 2 || err != nil || out.String() != "ok" {
		t.Fatal("live output neighbor", n, err)
	}
}

// Fill an actual blocking pipe, then prove the stdout writer returns on its
// context without a reader. The same pipe drains and accepts the next write.
func TestCourierHookBlockedStdoutBudget(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	fd := int(w.Fd())
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	for {
		_, err := unix.Write(fd, make([]byte, 1024))
		if err == unix.EAGAIN {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	unix.FcntlInt(uintptr(fd), unix.F_SETFL, flags)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		n, err := writeHookFile(ctx, w, []byte("x"))
		if n != 0 {
			done <- errors.New("wrote into full pipe")
			return
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err != context.DeadlineExceeded {
			t.Fatal("blocked stdout reason", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked stdout ignored context")
	}
	got, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || got&unix.O_NONBLOCK != flags&unix.O_NONBLOCK {
		t.Fatal("stdout flags not restored")
	}
	drained := make(chan struct{})
	go func() { io.Copy(io.Discard, r); close(drained) }()
	if n, err := writeHookFile(context.Background(), w, []byte("neighbor")); n != 8 || err != nil {
		t.Fatal("drained neighbor", n, err)
	}
	w.Close()
	select {
	case <-drained:
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("drain stuck")
	}
}
