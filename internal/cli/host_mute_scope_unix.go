//go:build unix

package cli

import (
	"os"
	"syscall"
)

// Scope inputs are descriptor-validated regular files. Nonblocking open keeps
// a FIFO replacement from waiting for a peer before that validation can run.
func openWakeScope(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
