//go:build unix

package darwinrelease

import (
	"os/exec"
	"syscall"
)

// ownProcessGroup starts the command in a new process group of its own and
// makes cancellation kill that whole group: the command and every descendant
// that stayed in it. Only this group is ever signalled.
func ownProcessGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		if c.Process == nil {
			return nil
		}
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
}

// reapOwnGroup kills descendants left in the command's own process group
// after the command itself has exited. It reports whether any were found.
// Residual: a descendant that left the group (setsid/setpgid) is not covered;
// WaitDelay still bounds the wait for its output.
func reapOwnGroup(c *exec.Cmd) bool {
	if c.Process == nil {
		return false
	}
	pgid := c.Process.Pid
	if syscall.Kill(-pgid, 0) != nil {
		return false
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return true
}
