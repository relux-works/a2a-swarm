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
)

func bindingArguments(pid int) ([]string, []string, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	raw, e := os.ReadFile(filepath.Join(base, "cmdline"))
	if e != nil {
		return nil, nil, e
	}
	var args []string
	for _, v := range bytes.Split(bytes.TrimSuffix(raw, []byte{0}), []byte{0}) {
		args = append(args, string(v))
	}
	raw, e = os.ReadFile(filepath.Join(base, "environ"))
	if e != nil {
		return nil, nil, e
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
		return "", e
	}
	s := st.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}
