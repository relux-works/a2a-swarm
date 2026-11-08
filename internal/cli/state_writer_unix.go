//go:build darwin || linux

package cli

import (
	"debug/buildinfo"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"golang.org/x/sys/unix"
)

// The documented mktemp image.XXXXXX recipe emits six ASCII alphanumerics.
var stateWriterImageDirRE = regexp.MustCompile(`^image\.[A-Za-z0-9]{6}$`)

const stateWriterImageMax = 64 << 20

func stateWriterLibexecPath(path string) bool {
	parent := filepath.Dir(path)
	if filepath.Base(parent) == "a2a" && filepath.Base(filepath.Dir(parent)) == "libexec" {
		return true // legacy flat .../libexec/a2a/<file>
	}
	return filepath.Base(path) == "a2a" && stateWriterImageDirRE.MatchString(filepath.Base(parent)) && filepath.Base(filepath.Dir(parent)) == "a2a" && filepath.Base(filepath.Dir(filepath.Dir(parent))) == "libexec"
}

func oldLibexecStateWriter(path, runningImage string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !stateWriterLibexecPath(resolved) || hookExecutableParents(path, resolved) != nil {
		return false
	}
	fd, err := unix.Open(resolved, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	f := os.NewFile(uintptr(fd), "state-writer-image")
	defer f.Close()
	return oldStateWriterImage(f, runningImage, func(stamp terminalExecutable) bool {
		current, err := filepath.EvalSymlinks(path)
		return err == nil && current == resolved && stateWriterImageAtPath(resolved, stamp, false)
	})
}

func stateWriterImageAtPath(path string, stamp terminalExecutable, follow bool) bool {
	stat := os.Lstat
	if follow {
		stat = os.Stat // only the kernel's /proc/PID/exe link, never a hook path
	}
	atPath, err := stat(path)
	return err == nil && executableMetadata(atPath) == stamp
}

func oldStateWriterImage(f *os.File, runningImage string, sameImage func(terminalExecutable) bool) bool {
	before, err := f.Stat()
	if err != nil {
		return false
	}
	stamp := executableMetadata(before)
	if !before.Mode().IsRegular() || stamp.UID != uint32(os.Getuid()) || before.Mode().Perm()&0100 == 0 || before.Mode().Perm()&0022 != 0 || before.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 || before.Size() <= 0 || before.Size() > stateWriterImageMax {
		return false
	}
	if runningImage != "" && runningImage != stamp.Image || !sameImage(stamp) {
		return false
	}
	// Both parsers see a bounded ReaderAt even if the file grows after Stat.
	bounded := io.NewSectionReader(f, 0, before.Size())
	if !completeTerminalExecutable(bounded, before.Size()) {
		return false
	}
	info, err := buildinfo.Read(bounded)
	after, statErr := f.Stat()
	return err == nil && statErr == nil && executableMetadata(after) == stamp && sameImage(stamp) && opaqueUnawareBuild(info)
}
