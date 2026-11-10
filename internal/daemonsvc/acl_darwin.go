package daemonsvc

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func effectiveNoWrite(fd int, name string) bool {
	err := unix.Faccessat(fd, name, unix.W_OK, unix.AT_EACCESS)
	return err == unix.EACCES || err == unix.EPERM
}

// Descriptor-only fgetattrlist, from the SDK's getattrlist(2)/sys/kauth.h.
// This checks ACL grants even as root; effective access alone cannot do that.
func aclNoWriteGrants(fd int) bool {
	attrs := unix.Attrlist{Bitmapcount: 5, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	var buf [4096]byte // header + filesec + at most 128 24-byte ACEs
	_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)), 0, 0)
	runtime.KeepAlive(attrs)
	runtime.KeepAlive(buf)
	return errno == 0 && darwinACLNoWrite(buf[:])
}
