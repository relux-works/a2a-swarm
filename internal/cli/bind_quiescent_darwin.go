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
		// kern.proc.pid answers an absent PID with a short record (EIO in
		// x/sys); only a signal-0 ESRCH proves absence, anything else stays
		// unknown.
		if errors.Is(err, unix.ESRCH) || errors.Is(unix.Kill(pid, 0), unix.ESRCH) {
			return "", errProcessGone
		}
		return "", err
	}
	// kern.proc.pid answers an absent PID with an empty record.
	if k.Proc.P_pid == 0 {
		return "", errProcessGone
	}
	if int(k.Proc.P_pid) != pid {
		return "", errors.New("process record mismatch")
	}
	t := k.Proc.P_starttime
	return strconv.FormatInt(int64(t.Sec), 10) + "." + strconv.FormatInt(int64(t.Usec), 10), nil
}
