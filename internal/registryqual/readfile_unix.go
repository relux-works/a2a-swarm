//go:build unix

package registryqual

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens path read-only. O_NOFOLLOW refuses a final symlink and
// O_NONBLOCK keeps a FIFO from blocking the open; the caller then checks the
// descriptor is a regular file.
func openNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, syscall.ELOOP) {
		return nil, errSymlink
	}
	return f, err
}
