//go:build unix

package cli

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func boundSecurityProcess(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killSecurityGroup(cmd) }
	cmd.WaitDelay = 100 * time.Millisecond
	return nil
}

func killSecurityGroup(cmd *exec.Cmd) error {
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// Own the copy loops: Go 1.25.5 Cmd.Wait masks awaitGoroutines errors when
// the leader exits nonzero. Passing files to Cmd keeps pipe completion separate
// from the leader status, including ordinary missing/duplicate statuses.
func runBoundSecurityProcess(cmd *exec.Cmd) error {
	type pipeCopy struct {
		child, parent *os.File
		copy          func() error
	}
	var pipes []pipeCopy
	defer func() {
		for _, p := range pipes {
			_ = p.child.Close()
			_ = p.parent.Close()
		}
	}()
	output := func(dst io.Writer) (*os.File, error) {
		r, w, err := os.Pipe()
		if err != nil {
			return nil, err
		}
		if dst == nil {
			dst = io.Discard
		}
		pipes = append(pipes, pipeCopy{w, r, func() error { _, err := io.Copy(dst, r); return err }})
		return w, nil
	}
	var err error
	cmd.Stdout, err = output(cmd.Stdout)
	if err != nil {
		return errSecurityProcessIO
	}
	cmd.Stderr, err = output(cmd.Stderr)
	if err != nil {
		return errSecurityProcessIO
	}
	if src := cmd.Stdin; src != nil {
		r, w, err := os.Pipe()
		if err != nil {
			return errSecurityProcessIO
		}
		pipes = append(pipes, pipeCopy{r, w, func() error { _, err := io.Copy(w, src); _ = w.Close(); return err }})
		cmd.Stdin = r
	}
	if err := cmd.Start(); err != nil {
		return err // No child/group was started; do not signal a PID.
	}
	results := make(chan error, len(pipes))
	for _, p := range pipes {
		_ = p.child.Close() // Parent must not keep a child endpoint alive.
		go func(p pipeCopy) { results <- p.copy() }(p)
	}
	waitErr := cmd.Wait()
	// The same 100ms drain bound applies after ANY leader result. Never infer
	// completion from its status or duration. All copies must actually finish.
	timer := time.NewTimer(cmd.WaitDelay)
	defer timer.Stop()
	remaining := len(pipes)
	var pipeErr error
draining:
	for remaining > 0 {
		select {
		case err := <-results:
			remaining--
			if err != nil {
				pipeErr = errSecurityProcessIO
			}
		case <-timer.C:
			pipeErr = exec.ErrWaitDelay
			break draining
		}
	}
	// Cancel only watches the leader. Always synchronously clean the invocation's
	// group, even after successful exit with closed descendant stdio. No timer or
	// goroutine may retain a reusable PGID after return.
	cleanupErr := killSecurityGroup(cmd)
	cleanupErr = completedSecurityGroupCleanup(cmd.Process.Pid, cmd.ProcessState != nil, cmd.WaitDelay, cleanupErr)
	for _, p := range pipes {
		_ = p.parent.Close()
	}
	for remaining > 0 {
		<-results
		remaining--
	} // Close unblocks file I/O; join copies.
	if cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) {
		waitErr = errors.Join(waitErr, errSecurityProcessCleanup)
	}
	if pipeErr != nil {
		return errors.Join(waitErr, pipeErr)
	}
	return waitErr
}
func openKeychainState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		f.Close()
		return nil, errors.New("invalid private state")
	}
	return f, nil
}
func keychainLock(path string, deadline time.Time) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		f.Close()
		return nil, errors.New("invalid lock")
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errors.New("keychain lock timeout")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
