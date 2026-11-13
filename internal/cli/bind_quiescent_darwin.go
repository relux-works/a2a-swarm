//go:build darwin

package cli

import (
	"errors"
	"strconv"

	"golang.org/x/sys/unix"
)

// processStart returns pid's kernel start time from kern.proc.pid without
// forking. Together with a canonical Birth check at registration it
// identifies one process instance; a reused PID has another start time.
func processStart(pid int) (string, error) {
	if pid <= 1 {
		return "", errors.New("invalid pid")
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if int(k.Proc.P_pid) != pid {
		return "", errors.New("process not found")
	}
	t := k.Proc.P_starttime
	return strconv.FormatInt(int64(t.Sec), 10) + "." + strconv.FormatInt(int64(t.Usec), 10), nil
}
