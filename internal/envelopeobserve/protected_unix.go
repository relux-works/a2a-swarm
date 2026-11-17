//go:build unix

package envelopeobserve

import (
	"os"
	"syscall"
)

func protectedInfo(st os.FileInfo, directory bool) bool {
	// The reader must not be able to chmod/replace the roster or any ancestor.
	return os.Geteuid() != 0 && protectedStorageInfo(st, directory)
}

// protectedStorageInfo is the shared storage rule for reader and host writer.
func protectedStorageInfo(st os.FileInfo, directory bool) bool {
	stat, ok := st.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && st.Mode().Perm()&0o022 == 0 &&
		(directory && st.IsDir() || !directory && st.Mode().IsRegular())
}
