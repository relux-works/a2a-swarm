package config

import "golang.org/x/sys/unix"

const courierPublishSupported = true

func courierPublishDirectory(from, to string) error {
	return unix.RenamexNp(from, to, unix.RENAME_EXCL)
}
