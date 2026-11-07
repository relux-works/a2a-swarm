//go:build linux

package cli

import (
	"encoding/binary"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"testing"
)

func associationDescriptor(t *testing.T, b []byte) *os.File {
	t.Helper()
	fd, e := unix.MemfdCreate("synthetic-association", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if e != nil {
		t.Fatal(e)
	}
	w := os.NewFile(uintptr(fd), "synthetic-writer")
	defer w.Close()
	if e = w.Chmod(0600); e != nil {
		t.Fatal(e)
	}
	raw := make([]byte, len(b)+4)
	binary.BigEndian.PutUint32(raw, uint32(len(b)))
	copy(raw[4:], b)
	if _, e = w.Write(raw); e != nil {
		t.Fatal(e)
	}
	if _, e = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SEAL|unix.F_SEAL_WRITE|unix.F_SEAL_GROW|unix.F_SEAL_SHRINK); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(fmt.Sprintf("/proc/self/fd/%d", fd))
	if e != nil {
		t.Fatal(e)
	}
	return f
}
