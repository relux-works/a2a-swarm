package courierenroll

import "golang.org/x/sys/unix"

func publish(parent int, stage, target string, rotate bool) error {
	flags := uint(unix.RENAME_NOREPLACE)
	if rotate {
		flags = unix.RENAME_EXCHANGE
	}
	return unix.Renameat2(parent, stage, parent, target, flags)
}
