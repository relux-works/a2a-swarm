//go:build unix

package config

import (
	"os"
	"syscall"
)

func fileIdentity(info os.FileInfo) (dev, ino uint64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(st.Dev), uint64(st.Ino), true
}
