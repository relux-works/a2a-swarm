//go:build darwin || linux

package config

import (
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func postOperationPlatform() bool { return true }

// Root.OpenFile resolves in-root symlinks itself, even with O_NOFOLLOW.
// Journal entries are single components: use openat on the pinned directory
// so the kernel refuses a final symlink without opening or creating its target.
func openPostOperationFile(root *os.Root, name string, flags int, perm os.FileMode) (*os.File, error) {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return nil, ErrPostOperation
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	fd, err := unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, uint32(perm.Perm()))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func postOperationOwner(st os.FileInfo, strict bool) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	if strict {
		mode := os.FileMode(0600)
		if st.IsDir() {
			mode = 0700
		}
		return stat.Uid == uint32(os.Geteuid()) && st.Mode().Perm() == mode && st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0
	}
	// System-owned sticky temporary ancestors are safe anchors; journal-owned
	// directories themselves must always be private and owned by this user.
	return (stat.Uid == uint32(os.Geteuid()) || stat.Uid == 0) &&
		(st.Mode().Perm()&0022 == 0 || stat.Uid == 0 && st.IsDir() && st.Mode()&os.ModeSticky != 0)
}
func lockPostOperation(f *os.File) (func(), error) {
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }, nil
}
