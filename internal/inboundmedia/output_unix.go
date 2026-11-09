//go:build linux || darwin

package inboundmedia

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"golang.org/x/sys/unix"
)

// Every traversal and publication uses already-open directory descriptors.
// No path check followed by rename/truncate fallback is permitted here.
type outputTarget struct {
	root, parent, scratch int
	scratchName, leaf     string
	rootParts             []string
	configuredRoot        string
	outputRoot            string
	parentParts           []string
	file                  *os.File
	scratchStat           unix.Stat_t
}

func cleanPath(path string) bool {
	if path == "" || strings.Contains(path, "\\") {
		return false
	}
	for _, r := range path {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return false
		}
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
func walkDirectory(start int, parts []string) (int, error) {
	fd, err := unix.Dup(start)
	if err != nil {
		return -1, err
	}
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			// Linux reports ENOTDIR for O_DIRECTORY|O_NOFOLLOW on a link.
			// Inspect only that entry, without following it, for the diagnostic.
			var st unix.Stat_t
			if unix.Fstatat(fd, part, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Mode&unix.S_IFMT == unix.S_IFLNK {
				err = unix.ELOOP
			}
		}
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
	}
	return fd, nil
}
func directoryReason(err error) outputReason {
	if errors.Is(err, unix.ELOOP) {
		return symlinkInPath
	}
	return parentMissing
}

func openTarget(output string) (*outputTarget, Code, outputReason) {
	tmp := os.Getenv("TMPDIR")
	// Abs/Clean erase directory-only spellings, but the returned path must
	// still name the regular file we publish.
	if !filepath.IsAbs(tmp) || !cleanPath(tmp) {
		return nil, UnsafeOutput, tmpdirInvalid
	}
	if !cleanPath(output) || strings.HasSuffix(output, "/") || filepath.Base(output) == "." {
		return nil, UnsafeOutput, outputOutside
	}
	// TMPDIR is caller-selected trusted configuration. Resolve its system
	// aliases once (notably /var and /tmp on macOS); never resolve an
	// output descendant or reselect the root during transfer.
	configuredRoot := filepath.Clean(tmp)
	rootPath, err := filepath.EvalSymlinks(configuredRoot)
	if err != nil {
		return nil, UnsafeOutput, directoryReason(err)
	}
	abs, err := filepath.Abs(output)
	if err != nil {
		return nil, UnsafeOutput, outputOutside
	}
	relative, err := filepath.Rel(configuredRoot, abs)
	outputRoot := configuredRoot
	if err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		// Also accept the canonical spelling of the same trusted root.
		relative, err = filepath.Rel(rootPath, abs)
		outputRoot = rootPath
	}
	if err == nil && (relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		// The output may spell the trusted root through different ancestor
		// aliases (for example /var while TMPDIR is a separate symlink).
		// Resolve prefixes only until that root is reached. Never resolve a
		// descendant: the remainder must still pass the no-follow dirfd walk.
		prefix := string(filepath.Separator)
		parts := strings.Split(strings.TrimPrefix(abs, prefix), string(filepath.Separator))
		for i, part := range parts[:len(parts)-1] {
			prefix = filepath.Join(prefix, part)
			resolved, resolveErr := filepath.EvalSymlinks(prefix)
			if resolveErr != nil {
				break
			}
			if resolved == rootPath {
				relative = filepath.Join(parts[i+1:]...)
				outputRoot = prefix
				break
			}
		}
	}
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, UnsafeOutput, outputOutside
	}
	slash, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, UnsafeOutput, directoryReason(err)
	}
	root, err := walkDirectory(slash, strings.Split(rootPath, "/"))
	unix.Close(slash)
	if err != nil {
		return nil, UnsafeOutput, directoryReason(err)
	}
	t := &outputTarget{root: root, parent: -1, scratch: -1, leaf: filepath.Base(relative), rootParts: strings.Split(rootPath, "/"), configuredRoot: configuredRoot, outputRoot: outputRoot}
	fail := func(code Code, reason outputReason) (*outputTarget, Code, outputReason) {
		t.cleanup()
		return nil, code, reason
	}
	t.parentParts = strings.Split(filepath.Dir(relative), "/")
	t.parent, err = walkDirectory(root, t.parentParts)
	if err != nil {
		return fail(UnsafeOutput, directoryReason(err))
	}
	var st unix.Stat_t
	err = unix.Fstatat(t.parent, t.leaf, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err == nil {
		if st.Mode&unix.S_IFMT == unix.S_IFLNK {
			return fail(UnsafeOutput, symlinkInPath)
		}
		return fail(OutputExists, outputExists)
	}
	if !errors.Is(err, unix.ENOENT) {
		return fail(UnsafeOutput, directoryReason(err))
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return fail(TransferFailed, "")
	}
	t.scratchName = ".a2a-native-media-" + hex.EncodeToString(random[:])
	if err = unix.Mkdirat(t.root, t.scratchName, 0700); err != nil {
		t.scratchName = ""
		return fail(TransferFailed, "")
	}
	if err = unix.Fstatat(t.root, t.scratchName, &t.scratchStat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fail(UnsafeOutput, parentMissing)
	}
	t.scratch, err = unix.Openat(t.root, t.scratchName, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return fail(UnsafeOutput, directoryReason(err))
	}
	if err = unix.Fstat(t.scratch, &t.scratchStat); err != nil || t.scratchStat.Mode&0777 != 0700 {
		return fail(UnsafeOutput, parentMissing)
	}
	fd, err := unix.Openat(t.scratch, "bytes", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return fail(TransferFailed, "")
	}
	t.file = os.NewFile(uintptr(fd), "native-media-scratch")
	return t, "", ""
}
func sameDirectory(first, second int) bool {
	var a, b unix.Stat_t
	return unix.Fstat(first, &a) == nil && unix.Fstat(second, &b) == nil && a.Dev == b.Dev && a.Ino == b.Ino
}
func (t *outputTarget) pathCurrent() bool {
	// The configured alias must still select the retained inode. Its
	// canonical path and all descendants are rewalked without symlinks.
	var retained unix.Stat_t
	var configured unix.Stat_t
	if unix.Fstat(t.root, &retained) != nil || unix.Stat(t.configuredRoot, &configured) != nil || retained.Dev != configured.Dev || retained.Ino != configured.Ino {
		return false
	}
	// An alternate output spelling must still select the same trusted root,
	// independently of the configured TMPDIR alias.
	var output unix.Stat_t
	if unix.Stat(t.outputRoot, &output) != nil || retained.Dev != output.Dev || retained.Ino != output.Ino {
		return false
	}
	// Rewalk the canonical absolute path, not just the retained root FD.
	// Moving/replacing the root or any ancestor must invalidate publication.
	slash, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	root, err := walkDirectory(slash, t.rootParts)
	unix.Close(slash)
	if err != nil {
		return false
	}
	defer unix.Close(root)
	if !sameDirectory(t.root, root) {
		return false
	}
	current, err := walkDirectory(root, t.parentParts)
	if err != nil {
		return false
	}
	defer unix.Close(current)
	return sameDirectory(t.parent, current)
}
func (t *outputTarget) publish() Code {
	if !t.pathCurrent() {
		return UnsafeOutput
	}
	if err := unix.Linkat(t.scratch, "bytes", t.parent, t.leaf, 0); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return OutputExists
		}
		return PublicationFailed
	}
	if !t.pathCurrent() {
		// Remove only our published inode via the retained parent. Never
		// resolve the replaced absolute path during rollback or cleanup.
		var source, published unix.Stat_t
		if unix.Fstatat(t.scratch, "bytes", &source, unix.AT_SYMLINK_NOFOLLOW) == nil &&
			unix.Fstatat(t.parent, t.leaf, &published, unix.AT_SYMLINK_NOFOLLOW) == nil &&
			source.Dev == published.Dev && source.Ino == published.Ino {
			unix.Unlinkat(t.parent, t.leaf, 0)
		}
		return UnsafeOutput
	}
	return ""
}
func (t *outputTarget) cleanup() {
	if t.file != nil {
		t.file.Close()
	}
	if t.scratch >= 0 {
		unix.Unlinkat(t.scratch, "bytes", 0)
		unix.Close(t.scratch)
	}
	if t.root >= 0 && t.scratchName != "" {
		var st unix.Stat_t
		if unix.Fstatat(t.root, t.scratchName, &st, unix.AT_SYMLINK_NOFOLLOW) == nil && st.Dev == t.scratchStat.Dev && st.Ino == t.scratchStat.Ino {
			unix.Unlinkat(t.root, t.scratchName, unix.AT_REMOVEDIR)
		}
	}
	if t.parent >= 0 {
		unix.Close(t.parent)
	}
	if t.root >= 0 {
		unix.Close(t.root)
	}
}
