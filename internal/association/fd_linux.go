//go:build linux

package association

import (
	"os"
	"syscall"
)

func descriptorCustody(f *os.File) bool {
	flags, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_GETFL, 0)
	if e != 0 || flags&syscall.O_ACCMODE != syscall.O_RDONLY {
		return false
	}
	// Linux uapi F_GET_SEALS and F_SEAL_{SEAL,SHRINK,GROW,WRITE}.
	seals, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), 1034, 0)
	return e == 0 && seals&15 == 15
}
