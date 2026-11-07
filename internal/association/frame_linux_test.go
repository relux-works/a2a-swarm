//go:build linux

package association

import (
	"encoding/binary"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"testing"
	"unsafe"
)

func frame(t *testing.T, b []byte) *os.File {
	t.Helper()
	nr := uintptr(0)
	switch runtime.GOARCH {
	case "amd64":
		nr = 319
	case "arm64":
		nr = 279
	default:
		t.Skip("synthetic memfd helper unavailable on this architecture")
	}
	name := append([]byte("synthetic-association"), 0)
	fd, _, err := syscall.Syscall(nr, uintptr(unsafe.Pointer(&name[0])), 3, 0)
	if err != 0 {
		t.Fatal(err)
	}
	f := os.NewFile(fd, "synthetic-writer")
	defer f.Close()
	if e := f.Chmod(0600); e != nil {
		t.Fatal(e)
	}
	raw := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(raw, uint32(len(b)))
	copy(raw[4:], b)
	if _, e := f.Write(raw); e != nil {
		t.Fatal(e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_FCNTL, fd, 1033, 15); e != 0 {
		t.Fatal(e)
	}
	ro, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		t.Fatal(e)
	}
	// The comparator must use positional reads, ignoring this inherited offset.
	if _, e = ro.Seek(2, 0); e != nil {
		t.Fatal(e)
	}
	return ro
}
