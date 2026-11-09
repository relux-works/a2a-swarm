//go:build linux

package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// Two bounded namespace-local producers report waitable exit before each scan
// cohort. Confirmed zombies and their concurrent reaping must not block binding;
// asynchronous Kill alone does not establish this precondition.
func TestBindingProcessChurn(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "a2a-churn-fixture")
	if err = os.WriteFile(exe, raw, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	refused, admitted := 0, 0
	scan := func() {
		err := NewApp().guardBindingProcesses(ctx, "synthetic-guard", dir)
		if errors.Is(err, ErrBindingProcessUnknown) {
			refused++
			var detail *bindingInspectionError
			if errors.As(err, &detail) {
				t.Logf("inspection refusal: phase=%s operation=%s class=%s errno=%d exit=%d", detail.phase, detail.operation, detail.class, detail.errno, detail.exitCode)
			}
		} else if err == nil {
			admitted++
		} else {
			t.Errorf("unexpected churn error: %v", err)
		}
	}
	for round := range 32 {
		func() {
			var workers sync.WaitGroup
			ready := make(chan error, 2)
			reap := make(chan struct{})
			var release sync.Once
			defer workers.Wait()
			defer release.Do(func() { close(reap) })
			for range 2 {
				workers.Go(func() {
					cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
					cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "A2A_AGENT=churn-neighbor", "A2A_CONFIG_DIR="+dir)
					if err := cmd.Start(); err != nil {
						ready <- err
						return
					}
					defer cmd.Wait()
					if err := cmd.Process.Kill(); err != nil {
						ready <- err
						return
					}
					ready <- bindingWaitChildExited(cmd.Process.Pid)
					<-reap
				})
			}
			for range 2 {
				if err := <-ready; err != nil {
					t.Fatalf("churn producer failed: %v", err)
				}
			}
			// One scan must see unreaped, confirmed exits. The next scans may
			// race reaping, but cannot race creation/exit of the next cohort.
			scan()
			release.Do(func() { close(reap) })
			scan()
			if round < 16 {
				scan()
			}
		}()
	}
	t.Logf("two producers, at most 64 own children; scanner admitted=%d inspection-refused=%d", admitted, refused)
	if ctx.Err() != nil {
		t.Fatal("bounded churn deadline exceeded")
	}
	if refused != 0 {
		t.Fatalf("exited process churn blocked binding %d times", refused)
	}
}

// WNOWAIT is the fixture readiness signal: wait for the entire child thread
// group to exit while retaining the zombie for scanner coverage. EINTR does
// not change that state; no elapsed delay or signal-0 result proves exit.
func bindingWaitChildExited(pid int) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Signo != int32(unix.SIGCHLD) {
			return errors.New("child exit readiness missing")
		}
		return nil
	}
}

// Readiness must reject a non-child PID, then retain an actually exited child
// as a waitable zombie. The independent WNOHANG check catches false readiness;
// the live image is the nearby refusal control for the same PID and selectors.
func TestBindingProcessChurnExitReadiness(t *testing.T) {
	dir, child := bindingLinuxChild(t, false)
	if err := bindingWaitChildExited(os.Getpid()); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("non-child supplied exit readiness: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := NewApp().guardBindingProcesses(ctx, "synthetic-guard", dir); !errors.Is(err, ErrBindingOldImage) {
		t.Fatalf("live image not refused: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := bindingWaitChildExited(child.Process.Pid); err != nil {
		t.Fatal(err)
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, child.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil || info.Signo != int32(unix.SIGCHLD) {
		t.Fatalf("false exit readiness: signo=%d err=%v", info.Signo, err)
	}
	if processGone(child.Process.Pid) || !bindingCandidateExited(child.Process.Pid) {
		t.Fatal("readiness must retain the confirmed zombie")
	}
	if err := NewApp().guardBindingProcesses(ctx, "synthetic-guard", dir); err != nil {
		t.Fatalf("confirmed exit blocked binding: %v", err)
	}
}

// A same-image live candidate is allowed, but exec of a different image at
// that same PID and with unchanged selectors must still refuse binding. Both
// stages signal readiness through stdout; stdin releases the exec transition.
func TestBindingProcessChangedImage(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(dir, "a2a-current-fixture")
	if err := os.Symlink(self, current); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	next := filepath.Join(dir, "a2a-changed-fixture")
	if err := os.WriteFile(next, raw, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, current, "-test.run=^TestBindingProcessChangedImageChild$")
	child.Env = append(os.Environ(), "A2A_BIND_CHANGED=1", "A2A_BIND_CHANGED_NEXT="+next, "A2A_AGENT=synthetic-guard", "A2A_CONFIG_DIR="+dir)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	in, err := child.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = in.Close(); _ = child.Process.Kill(); _ = child.Wait() })
	ready := func() {
		t.Helper()
		var signal [1]byte
		if _, err := io.ReadFull(out, signal[:]); err != nil || signal[0] != 'R' {
			t.Fatalf("child readiness: signal=%q err=%v", signal, err)
		}
	}
	ready()
	pid := child.Process.Pid
	before, err := bindingImage(ctx, pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := NewApp().guardBindingProcesses(ctx, "synthetic-guard", dir); err != nil {
		t.Fatalf("same image refused: %v", err)
	}
	if _, err := in.Write([]byte{'E'}); err != nil {
		t.Fatal(err)
	}
	ready()
	after, err := bindingImage(ctx, pid)
	if err != nil || after == before {
		t.Fatalf("fixture image did not change: %v", err)
	}
	if err := NewApp().guardBindingProcesses(ctx, "synthetic-guard", dir); !errors.Is(err, ErrBindingOldImage) {
		t.Fatalf("changed live image not refused: %v", err)
	}
}

// The isolated neighbor fixture keeps one PID and the same argv/selectors
// across exec, and acknowledges only after the replacement image is running.
func TestBindingProcessChangedImageChild(t *testing.T) {
	if os.Getenv("A2A_BIND_CHANGED") != "1" {
		return
	}
	if _, err := os.Stdout.Write([]byte{'R'}); err != nil {
		os.Exit(2)
	}
	var signal [1]byte
	if _, err := io.ReadFull(os.Stdin, signal[:]); err != nil {
		os.Exit(2)
	}
	if next := os.Getenv("A2A_BIND_CHANGED_NEXT"); next != "" {
		if err := os.Unsetenv("A2A_BIND_CHANGED_NEXT"); err != nil {
			os.Exit(2)
		}
		if err := unix.Exec(next, os.Args, os.Environ()); err != nil {
			os.Exit(2)
		}
	}
	os.Exit(0)
}

// Keep the exited child unreaped: signal 0 still succeeds, but Linux denies
// environ and no executable image remains. Use the existing real child seam.
func TestBindingProcessExitedCandidate(t *testing.T) {
	dir, child := bindingLinuxChild(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	a := NewApp()
	if err := a.guardBindingProcesses(ctx, "synthetic-guard", dir); !errors.Is(err, ErrBindingOldImage) {
		t.Fatalf("live old image not refused: %v", err)
	}
	if err := a.guardBindingProcesses(ctx, "other-identity", dir); err != nil {
		t.Fatalf("live different identity blocked: %v", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitBindingLinuxZombie(t, child.Process.Pid)
	if processGone(child.Process.Pid) {
		t.Fatal("fixture must retain a PID visible to signal 0")
	}
	_, _, argsErr := bindingArguments(child.Process.Pid)
	if !errors.Is(argsErr, syscall.EACCES) {
		t.Fatalf("expected actual zombie environ EACCES, got %v", argsErr)
	}
	var detail *bindingInspectionError
	if !errors.As(argsErr, &detail) || detail.operation != "linux-environ" || detail.class != "permission" || detail.errno != syscall.EACCES {
		t.Fatal("actual zombie refusal lost its internal operation/errno/class")
	}
	if !bindingCandidateExited(child.Process.Pid) {
		t.Fatal("kernel exit observation did not recognize the unreaped child")
	}
	if err := a.guardBindingProcesses(ctx, "synthetic-guard", dir); err != nil {
		t.Fatalf("confirmed exited candidate blocked binding: %v", err)
	}
}

// An unreadable live same-UID process has unknown selectors, so even a
// requested different identity must refuse. Permission errors alone never
// establish that a candidate exited.
func TestBindingProcessUnreadableLiveCandidate(t *testing.T) {
	dir, child := bindingLinuxChild(t, true)
	if processGone(child.Process.Pid) {
		t.Fatal("unreadable fixture is not alive")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	err := NewApp().guardBindingProcesses(ctx, "other-identity", dir)
	if !errors.Is(err, ErrBindingProcessUnknown) {
		t.Fatalf("unreadable live ownership was skipped: %v", err)
	}
	var detail *bindingInspectionError
	if !errors.As(err, &detail) || detail.phase != "candidate-arguments" || detail.operation != "linux-environ" || detail.class != "permission" || detail.errno != syscall.EACCES {
		t.Fatal("live refusal lost its internal phase/operation/errno/class")
	}
	if bindingCandidateExited(child.Process.Pid) {
		t.Fatal("unreadable live process was classified as exited")
	}
}

// Exit evidence is obtained afresh for each current process, never cached by
// numeric PID. A stale exited-process observation cannot admit a live neighbor.
func TestBindingProcessExitObservationIsFresh(t *testing.T) {
	_, exited := bindingLinuxChild(t, false)
	if err := exited.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	waitBindingLinuxZombie(t, exited.Process.Pid)
	if !bindingCandidateExited(exited.Process.Pid) {
		t.Fatal("exited child not observed")
	}
	if err := exited.Wait(); err == nil {
		t.Fatal("killed child unexpectedly succeeded")
	}
	_, live := bindingLinuxChild(t, true)
	if bindingCandidateExited(live.Process.Pid) {
		t.Fatal("prior exit observation admitted live process")
	}
	if bindingCandidateExited(-1) {
		t.Fatal("invalid PID supplied exit proof")
	}
}

func bindingLinuxChild(t *testing.T, unreadable bool) (string, *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "a2a-linux-fixture")
	if err = os.WriteFile(exe, raw, 0700); err != nil {
		t.Fatal(err)
	}
	childTest := "TestBindingProcessWitnessChild"
	if unreadable {
		childTest = "TestBindingProcessUnreadableChild"
	}
	child := exec.Command(exe, "-test.run=^"+childTest+"$")
	child.Env = append(os.Environ(), "A2A_BIND_WITNESS=1", "A2A_AGENT=synthetic-guard", "A2A_CONFIG_DIR="+dir)
	out, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	ready := make([]byte, 1)
	if _, err = io.ReadFull(out, ready); err != nil {
		t.Fatal(err)
	}
	return dir, child
}

func waitBindingLinuxZombie(t *testing.T, pid int) {
	t.Helper()
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		// A thread-group leader may already show Z while other threads still
		// exit. Waitability confirms the whole child exited without reaping it.
		var info unix.Siginfo
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
		if err != nil {
			t.Fatal(err)
		}
		if _, tail, ok := strings.Cut(string(raw), ") "); ok && strings.HasPrefix(tail, "Z ") && info.Signo == int32(unix.SIGCHLD) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("child did not reach the actual zombie state")
}

func TestBindingProcessUnreadableChild(t *testing.T) {
	if os.Getenv("A2A_BIND_WITNESS") != "1" {
		return
	}
	if _, _, err := syscall.Syscall(syscall.SYS_PRCTL, 4 /* PR_SET_DUMPABLE */, 0, 0); err != 0 {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write([]byte{'R'})
	time.Sleep(20 * time.Second)
	os.Exit(0)
}
