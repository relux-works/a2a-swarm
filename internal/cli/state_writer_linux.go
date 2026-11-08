//go:build linux

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func oldLoadedLibexecStateWriter(_ context.Context, pid int, image string) bool {
	return oldProcLibexecStateWriter(filepath.Join("/proc", strconv.Itoa(pid), "exe"), image)
}

func oldProcLibexecStateWriter(procPath, image string) bool {
	if image == "" {
		return false
	}
	// Read the kernel's loaded-image name, not argv[0] or its replacement.
	path, err := os.Readlink(procPath)
	if err != nil {
		return false
	}
	if strings.HasSuffix(path, " (deleted)") {
		// A live filename can itself end in the kernel marker. If that exact
		// name still identifies the loaded inode, keep it literal for the
		// strict layout gate. No pathname supplies the classified bytes.
		atName, statErr := os.Stat(path)
		if statErr != nil && !os.IsNotExist(statErr) {
			return false
		}
		if statErr != nil || executableMetadata(atName).Image != image {
			path = strings.TrimSuffix(path, " (deleted)")
		}
	}
	if !filepath.IsAbs(path) {
		return false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	resolved := filepath.Join(parent, filepath.Base(path))
	if err != nil || !stateWriterLibexecPath(resolved) || hookExecutableParents(path, resolved) != nil {
		return false
	}
	// Following this kernel proc link is intentional. O_NOFOLLOW remains
	// mandatory for ordinary paths. An unlinked loaded inode is still readable.
	fd, err := unix.Open(procPath, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), "state-writer-loaded-image")
	defer f.Close()
	return oldStateWriterImage(f, image, func(stamp terminalExecutable) bool {
		return stateWriterImageAtPath(procPath, stamp, true)
	})
}
