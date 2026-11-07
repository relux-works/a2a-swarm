//go:build unix && !linux

package association

import (
	"os"
	"syscall"
)

// This only checks shape. Closing every writer is the admitted launcher's
// cooperative obligation on macOS; there is no claim of kernel immutability.
func descriptorCustody(f *os.File) bool {
	flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFL, 0)
	return e == 0 && flags&syscall.O_ACCMODE == syscall.O_RDONLY
}
