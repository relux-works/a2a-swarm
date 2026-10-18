package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"
)

type sentinelObserver struct {
	cmd    *exec.Cmd
	input  io.WriteCloser
	log    *os.File
	cancel context.CancelFunc
}

func startSentinelObserver(executable, logPath string) (*sentinelObserver, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	cmd := exec.CommandContext(ctx, executable, "--controlled")
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C", "LC_ALL=C"}
	cmd.Stdout, cmd.Stderr = f, f
	input, err := cmd.StdinPipe()
	if err == nil {
		err = boundSecurityProcess(cmd)
	}
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		if input != nil {
			_ = input.Close()
		}
		cancel()
		_ = f.Close()
		return nil, err
	}
	return &sentinelObserver{cmd: cmd, input: input, log: f, cancel: cancel}, nil
}

func (o *sentinelObserver) stop() error {
	// EOF is the observer's explicit stop protocol, never a UI action.
	_ = o.input.Close()
	// Bound even an observer that stopped reading its control pipe.
	timer := time.AfterFunc(2*time.Second, o.cancel)
	err := o.cmd.Wait()
	timer.Stop()
	o.cancel()
	_ = o.log.Close()
	if !errors.Is(syscall.Kill(-o.cmd.Process.Pid, 0), syscall.ESRCH) {
		return errors.New("observer own process group survives")
	}
	return err
}

// This opt-in gate is observations only: no security, probe, fixture, password,
// Keychain list, UI focus, titles, text, screenshot or permission operations.
func TestKeychainObserverTenDryRuns(t *testing.T) {
	if os.Getenv("A2A_OBSERVER_DRY_RUN") != "1" {
		t.Skip("observer-only local readiness evidence")
	}
	run := os.Getenv("TASK_BOARD_RUN_ID")
	if !regexp.MustCompile(`^RUN-[0-9]{6}-[a-z0-9]{6}$`).MatchString(run) {
		t.Fatal("missing run binding")
	}
	bin, err := filepath.Abs("../../.temp/phase1-" + run)
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(bin, "observer-only-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("observations_only=%s readiness_cap=%s keychain_security_probe_list_fixture_calls=0", root, observerReadinessDeadline)
	for i := 1; i <= 10; i++ {
		start := time.Now()
		o, err := startSentinelObserver(filepath.Join(bin, "keychain-observer"), filepath.Join(root, time.Now().UTC().Format("150405.000000000")+".log"))
		if err != nil {
			t.Fatal("observer unavailable", err)
		}
		readyErr := waitObserverReady(context.Background(), o.log.Name())
		readyAt := time.Now()
		stopErr := o.stop()
		data, readErr := os.ReadFile(o.log.Name())
		t.Logf("cycle=%d start=%s ready_elapsed=%s end=%s observer_exit=%d stop_error=%v own_group_reaped=%t observations=%q", i, start.UTC().Format(time.RFC3339Nano), readyAt.Sub(start), time.Now().UTC().Format(time.RFC3339Nano), o.cmd.ProcessState.ExitCode(), stopErr, errors.Is(syscall.Kill(-o.cmd.Process.Pid, 0), syscall.ESRCH), data)
		if readyErr != nil || stopErr != nil || readErr != nil || readyAt.Sub(start) > observerReadinessDeadline || bytes.Contains(data, []byte("observer=unavailable")) {
			t.Fatalf("observer-only cycle %d failed ready=%v stop=%v read=%v; platform sentinel remains forbidden", i, readyErr, stopErr, readErr)
		}
	}
}
