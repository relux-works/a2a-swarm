//go:build darwin

package devicelist

// snapshotNoACL is NOT IMPLEMENTED on darwin and reports no ACL for every
// object. A darwin ACL is not an extended attribute; reading it needs
// acl_get_fd_np of libc or getattrlist with the extended-security attribute,
// and neither is wired here. Until it is, the "no ACL" rule of the snapshot
// profile is unchecked on darwin and therefore weaker than the contract.
// Native ACL qualification is required before any darwin installation;
// docs/device-list.md records this limit.
func snapshotNoACL(fd int, directory bool) bool { return true }
