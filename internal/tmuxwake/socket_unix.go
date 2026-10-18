//go:build darwin || linux || freebsd || openbsd || netbsd

package tmuxwake

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func socketID(st os.FileInfo) (string, error) {
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(s.Uid) != os.Getuid() {
		return "", errors.New("tmux socket not owned by this user")
	}
	return fmt.Sprintf("%d:%d", s.Dev, s.Ino), nil
}
