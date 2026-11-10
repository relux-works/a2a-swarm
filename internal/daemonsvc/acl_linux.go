package daemonsvc

import "golang.org/x/sys/unix"

func effectiveNoWrite(fd int, name string) bool {
	// Do not use x/sys's mode-only faccessat emulation on older kernels.
	// An unavailable faccessat2 fails closed rather than ignoring ACLs.
	return unix.Faccessat2(fd, name, unix.W_OK, unix.AT_EACCESS) == unix.EACCES
}

func aclNoWriteGrants(fd int) bool {
	// Installed Linux trust paths have no extended/default POSIX ACL. Refuse
	// any such ACL conservatively (including read-only ACLs), or an unknown read.
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return false
	}
	names := []string{"system.posix_acl_access"}
	if st.Mode&unix.S_IFMT == unix.S_IFDIR {
		names = append(names, "system.posix_acl_default")
	}
	for _, name := range names {
		n, err := unix.Fgetxattr(fd, name, nil)
		if err == unix.ENODATA || err == unix.EOPNOTSUPP {
			continue
		}
		if err != nil || n != 0 {
			return false
		}
	}
	return true
}
