//go:build unix

package config

import (
	"os"
	"syscall"
)

func bindPartCurrentUserOwns(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
