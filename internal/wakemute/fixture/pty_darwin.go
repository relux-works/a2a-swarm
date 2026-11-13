//go:build darwin

package fixture

import (
	"bytes"
	"golang.org/x/sys/unix"
	"os"
	"unsafe"
)

func openPTY() (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	for _, request := range []uintptr{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, master.Fd(), request, 0); e != 0 {
			master.Close()
			return nil, nil, e
		}
	}
	var name [128]byte
	if _, _, e := unix.Syscall(unix.SYS_IOCTL, master.Fd(), unix.TIOCPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		master.Close()
		return nil, nil, e
	}
	end := bytes.IndexByte(name[:], 0)
	if end < 0 {
		master.Close()
		return nil, nil, unix.EINVAL
	}
	slave, err := os.OpenFile(string(name[:end]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, err
	}
	return master, slave, nil
}
