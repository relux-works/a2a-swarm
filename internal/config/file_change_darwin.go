//go:build darwin

package config

import (
	"os"
	"syscall"
)

func platformFileChangeTimeNS(info os.FileInfo) (int64, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return st.Ctimespec.Sec*1_000_000_000 + st.Ctimespec.Nsec, true
}
