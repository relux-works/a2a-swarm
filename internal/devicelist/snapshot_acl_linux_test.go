//go:build linux

package devicelist

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// posixACL is an access ACL in the kernel's attribute encoding (version 2):
// the three base entries for the given permission bits, one named-user entry
// for the given UID with all permissions, and a mask equal to the group
// bits. The mask keeps the mode bits as they were, so nothing but the ACL
// itself differs from the control.
func posixACL(uid uint32, owner, group, other uint16) []byte {
	const undefined = 0xffffffff
	entries := []struct {
		tag  uint16
		perm uint16
		id   uint32
	}{
		{0x01, owner, undefined}, // ACL_USER_OBJ
		{0x02, 7, uid},           // ACL_USER
		{0x04, group, undefined}, // ACL_GROUP_OBJ
		{0x10, group, undefined}, // ACL_MASK
		{0x20, other, undefined}, // ACL_OTHER
	}
	blob := binary.LittleEndian.AppendUint32(nil, 2)
	for _, e := range entries {
		blob = binary.LittleEndian.AppendUint16(blob, e.tag)
		blob = binary.LittleEndian.AppendUint16(blob, e.perm)
		blob = binary.LittleEndian.AppendUint32(blob, e.id)
	}
	return blob
}

// Claim (contract section 5, "no ACLs"): a snapshot file, or a directory on
// its path, that carries a POSIX ACL is ErrSnapshotUnsafe although its mode
// bits and owner are the profile's. Control: the same objects without the
// ACL are read, before the ACL is set and after it is removed. The test is
// skipped where the filesystem of the test directory has no ACL support.
// Claim: access ACLs on file, parent and ancestor, and a parent default ACL, refuse beside ACL-free controls.
func TestReadSnapshotRefusesACL(t *testing.T) {
	skipAsRoot(t)
	profile, config, valid := snapshotFixture(t)
	writeSnapshot(t, config.Path, valid, 0644)
	read := func() error {
		_, _, err := readSnapshot(profile, config, at)
		return err
	}
	if err := read(); err != nil {
		t.Fatal("control before the ACL:", err)
	}
	uid := uint32(os.Getuid())
	targets := []struct {
		path      string
		mode      uint32
		acl       []byte
		attribute string
	}{
		{config.Path, 0644, posixACL(uid, 6, 4, 4), "system.posix_acl_access"},
		{profile.root, 0755, posixACL(uid, 7, 5, 5), "system.posix_acl_access"},
		{filepath.Dir(config.Path), 0755, posixACL(uid, 7, 5, 5), "system.posix_acl_access"},
		{filepath.Dir(config.Path), 0755, posixACL(uid, 7, 5, 5), "system.posix_acl_default"},
	}
	for _, target := range targets {
		if err := read(); err != nil {
			t.Fatal("control before ACL:", err)
		}
		err := unix.Setxattr(target.path, target.attribute, target.acl, 0)
		if err == unix.ENOTSUP {
			t.Skip("the filesystem of the test directory has no POSIX ACL support")
		}
		if err != nil {
			t.Fatal("setting the ACL:", err)
		}
		var st unix.Stat_t
		if err := unix.Stat(target.path, &st); err != nil {
			t.Fatal(err)
		}
		if uint32(st.Mode)&07777 != target.mode {
			t.Fatalf("fixture: the ACL changed the mode bits of %s to %o", target.path, uint32(st.Mode)&07777)
		}
		if err := read(); err != ErrSnapshotUnsafe {
			t.Fatalf("%s with an ACL: %v", target.path, err)
		}
		if err := unix.Removexattr(target.path, target.attribute); err != nil {
			t.Fatal(err)
		}
		if err := read(); err != nil {
			t.Fatal("control after the ACL was removed:", err)
		}
	}
}
