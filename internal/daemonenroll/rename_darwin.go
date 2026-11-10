package daemonenroll

import "golang.org/x/sys/unix"

func publish(parent int, stage, target string, rotate bool) error {
	flags := uint32(unix.RENAME_EXCL)
	if rotate {
		flags = unix.RENAME_SWAP
	}
	return unix.RenameatxNp(parent, stage, parent, target, flags)
}
