//go:build unix

package voice

import (
	"golang.org/x/sys/unix"
	"os"
)

func openReleaseFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
}
