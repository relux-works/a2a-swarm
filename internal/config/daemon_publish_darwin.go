package config

import "golang.org/x/sys/unix"

const daemonPublishSupported = true

func daemonPublishDirectory(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
