//go:build darwin

package cli

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// KERN_PROCARGS2 is read only for same-UID a2a candidates. Raw argv/env never
// leave memory and are never included in an error, log, or room message.
func bindingArguments(pid int) ([]string, []string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return nil, nil, bindingInspectionFailure("darwin-sysctl-arguments", err)
	}
	if len(raw) < 4 {
		return nil, nil, bindingInspectionFailure("darwin-arguments-length", nil)
	}
	argc := int(binary.NativeEndian.Uint32(raw[:4]))
	raw = raw[4:]
	if argc < 1 || argc > 65536 {
		return nil, nil, bindingInspectionFailure("darwin-arguments-count", nil)
	}
	end := bytes.IndexByte(raw, 0)
	if end < 0 {
		return nil, nil, bindingInspectionFailure("darwin-arguments-executable", nil)
	}
	raw = bytes.TrimLeft(raw[end+1:], "\x00")
	var args []string
	for i := 0; i < argc; i++ {
		end = bytes.IndexByte(raw, 0)
		if end < 0 {
			return nil, nil, bindingInspectionFailure("darwin-arguments-record", nil)
		}
		args = append(args, string(raw[:end]))
		raw = raw[end+1:]
	}
	var env []string
	for _, v := range bytes.Split(raw, []byte{0}) {
		if bytes.HasPrefix(v, []byte("A2A_AGENT=")) || bytes.HasPrefix(v, []byte("A2A_CONFIG_DIR=")) {
			env = append(env, string(v))
		}
	}
	return args, env, nil
}

// lsof reports the vnode of the RUNNING executable, including an unlinked old
// image after same-path replacement. Stat(executable path) cannot do that.
func bindingImage(ctx context.Context, pid int) (string, error) {
	raw, err := bindingImageOutput(ctx, pid)
	if err != nil {
		return "", bindingInspectionFailure("darwin-image-lsof", err)
	}
	return parseBindingImage(string(raw))
}
func bindingImageOutput(ctx context.Context, pid int) ([]byte, error) {
	return exec.CommandContext(ctx, "/usr/sbin/lsof", "-a", "-p", strconv.Itoa(pid), "-d", "txt", "-F", "finD").Output()
}
func parseBindingImage(raw string) (string, error) {
	dev, ino := "", ""
	text := false
	for _, line := range strings.Split(raw, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'f':
			if text {
				if dev != "" && ino != "" {
					return fmt.Sprintf("%s:%s", dev, ino), nil
				}
				return "", bindingInspectionFailure("darwin-image-record", nil)
			}
			text = line == "ftxt"
		case 'D':
			if text {
				dev = line[1:]
			}
		case 'i':
			if text {
				ino = line[1:]
			}
		}
	}
	if text && dev != "" && ino != "" {
		return dev + ":" + ino, nil
	}
	return "", bindingInspectionFailure("darwin-image-record", nil)
}

// No Darwin exit-state proof is established here. Keep its existing ESRCH gate.
func bindingCandidateExited(int) bool { return false }
