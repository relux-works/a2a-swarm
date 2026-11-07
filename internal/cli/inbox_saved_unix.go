//go:build unix

package cli

import (
	"os"
	"syscall"
)

func openSavedInboxFile(root *os.Root, name string) (*os.File, error) {
	// A replaced symlink is refused by the kernel; a replaced FIFO cannot block
	// between the lstat and the post-open inode/permission check.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
