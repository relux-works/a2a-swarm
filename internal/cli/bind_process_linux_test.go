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

// Two bounded namespace-local producers repeatedly exit without immediate
// reaping while the real scanner runs. Live foreign selectors are a neighbor;
// actual exited candidates must not turn ordinary churn into a refusal.
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
	var workers sync.WaitGroup
	workerErrors := make(chan error, 2)
	for range 2 {
		workers.Go(func() {
			for range 32 {
				if ctx.Err() != nil {
					return
				}
				cmd := exec.CommandContext(ctx, exe, "-test.run=^$")
				cmd.Env = append(os.Environ(), "GORACE=atexit_sleep_ms=0", "A2A_AGENT=churn-neighbor", "A2A_CONFIG_DIR="+dir)
				if err := cmd.Start(); err != nil {
					workerErrors <- err
					return
				}
				_ = cmd.Process.Kill()
				time.Sleep(3 * time.Millisecond)
				_ = cmd.Wait()
			}
		})
	}
	refused, admitted := 0, 0
	for range 80 {
		err := NewApp().guardBindingProcesses(ctx, "synthetic-guard", dir)
		if errors.Is(err, ErrBindingProcessUnknown) {
			refused++
		} else if err == nil {
			admitted++
		} else {
			t.Errorf("unexpected churn error: %v", err)
		}
	}
	workers.Wait()
	close(workerErrors)
	for err := range workerErrors {
		t.Errorf("churn producer failed: %v", err)
	}
	t.Logf("two producers, at most 64 own children; scanner admitted=%d inspection-refused=%d", admitted, refused)
	if ctx.Err() != nil {
		t.Fatal("bounded churn deadline exceeded")
	}
	if refused != 0 {
		t.Fatalf("exited process churn blocked binding %d times", refused)
	}
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
