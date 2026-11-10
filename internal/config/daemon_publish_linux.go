package config

import "golang.org/x/sys/unix"

const daemonPublishSupported = true

func daemonPublishDirectory(from, to string) error {
	return unix.Renameat2(unix.AT_FDCWD, from, unix.AT_FDCWD, to, unix.RENAME_NOREPLACE)
}
