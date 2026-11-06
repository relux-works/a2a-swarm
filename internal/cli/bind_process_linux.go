//go:build linux

package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func bindingArguments(pid int) ([]string, []string, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	raw, e := os.ReadFile(filepath.Join(base, "cmdline"))
	if e != nil {
		return nil, nil, bindingInspectionFailure("linux-cmdline", e)
	}
	var args []string
	for _, v := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		args = append(args, string(v))
	}
	raw, e = os.ReadFile(filepath.Join(base, "environ"))
	if e != nil {
		return nil, nil, bindingInspectionFailure("linux-environ", e)
	}
	var env []string
	for _, v := range bytes.Split(raw, []byte{0}) {
		s := string(v)
		if strings.HasPrefix(s, "A2A_AGENT=") || strings.HasPrefix(s, "A2A_CONFIG_DIR=") {
			env = append(env, s)
		}
	}
	return args, env, nil
}
func bindingImage(_ context.Context, pid int) (string, error) {
	st, e := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if e != nil {
		return "", bindingInspectionFailure("linux-image-stat", e)
	}
	s := st.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}

// Signal 0 still succeeds for a zombie, although its environ/image can no
// longer be read. A fresh pidfd pins the current process instance (no cached
// PID/birth or errno-based ownership inference); POLLIN confirms it exited.
// Unsupported/denied probes and live or otherwise uncertain processes refuse.
func bindingCandidateExited(pid int) bool {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return false
	}
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	n, err := unix.Poll(fds, 0)
	return err == nil && n == 1 && fds[0].Revents&unix.POLLIN != 0
}
