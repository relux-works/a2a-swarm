//go:build linux

package devicelist

import "golang.org/x/sys/unix"

// snapshotNoACL reports that the opened object carries no POSIX ACL: no
// access ACL and, on a directory, no default ACL. The profile refuses every
// extended ACL, not only one that grants a write, because the reader does
// not interpret ACL entries. An ACL that is equivalent to the mode bits is
// not stored as an attribute, so a plain 0644 or 0755 object has none. A
// filesystem without ACL support has none either. Any other answer fails
// closed.
func snapshotNoACL(fd int, directory bool) bool {
	attrs := []string{"system.posix_acl_access"}
	if directory {
		attrs = append(attrs, "system.posix_acl_default")
	}
	for _, attr := range attrs {
		_, err := unix.Fgetxattr(fd, attr, nil)
		if err != unix.ENODATA && err != unix.ENOTSUP {
			return false
		}
	}
	return true
}
