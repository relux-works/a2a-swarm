//go:build linux || darwin

package courierenroll

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/courier"
	"golang.org/x/sys/unix"
)

var fileNames = []string{"courier.sock", "identity", "station.key"}

// Install consumes a pipe enrollment. Publication replaces a whole directory,
// never individual files. A rotation retains the old generation as a private
// sibling so readers holding its directory fd can finish consistently.
func Install(dir string, r io.Reader, rotate bool) error {
	return install(dir, r, rotate, nil)
}

// fault is an in-process test seam, never controlled by the process environment.
type fault func(phase string, stage *os.File) error

func install(dir string, r io.Reader, rotate bool, hook fault) error {
	e, err := readInput(r)
	if err != nil {
		return err
	}
	defer clear(e.key)
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir || filepath.Base(dir) != ".a2a" {
		return errFiles
	}
	// Resolve the home once. All subsequent opens and inode checks use only
	// this canonical namespace; the supplied alias is never followed again.
	requestedDir := dir
	parent, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return errFiles
	}
	dir = filepath.Join(parent, ".a2a")
	chain, err := openParent(filepath.Dir(dir))
	if err != nil {
		return errFiles
	}
	defer chain.close()
	p := chain.fds[len(chain.fds)-1]
	// A stable directory inode is the writer lock. No lock or temporary entry
	// is placed inside .a2a. Contention fails promptly, without a wait loop.
	if unix.Flock(p, unix.LOCK_EX|unix.LOCK_NB) != nil {
		return errFiles
	}
	defer unix.Flock(p, unix.LOCK_UN)
	if !chain.valid() {
		return errFiles
	}
	old, err := openDir(p, ".a2a")
	exists := err == nil
	if err != nil && err != unix.ENOENT {
		return errFiles
	}
	if exists {
		defer old.Close()
		if !rotate || !canonicalReaderPath(requestedDir) || verify(old, nil) != nil {
			return errFiles
		}
	}
	// --rotate also permits an absent enrollment (the first install).
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return errWrite
	}
	stageName := ".a2a-enroll-" + hex.EncodeToString(nonce[:])
	if unix.Mkdirat(p, stageName, 0700) != nil {
		return errWrite
	}
	stage, err := openDir(p, stageName)
	if err != nil {
		return errRecovery
	}
	defer stage.Close()
	keep := false
	defer func() {
		if !keep {
			removeStage(p, stageName, stage)
		}
	}()
	if !ownedMode(int(stage.Fd()), unix.S_IFDIR, 0700) {
		return errFiles
	}
	pointer, err := courier.PointerFile(e.socket)
	if err != nil {
		return errInput
	}
	identity, err := courier.IdentityFile(e.identity)
	if err != nil {
		return errInput
	}
	contents := [][]byte{pointer, identity, e.key}
	for i, name := range fileNames {
		fd, err := unix.Openat(int(stage.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			return errWrite
		}
		f := os.NewFile(uintptr(fd), name)
		_, err = f.Write(contents[i])
		if err == nil && !ownedMode(fd, unix.S_IFREG, 0600) {
			err = errFiles
		}
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return errWrite
		}
		if hook != nil && hook("wrote-"+name, stage) != nil {
			return errWrite
		}
	}
	if hook != nil && hook("staged", stage) != nil {
		return errWrite
	}
	if verify(stage, contents) != nil || stage.Sync() != nil || !chain.valid() {
		return errFiles
	}
	if exists {
		if !sameEntry(p, ".a2a", old) || verify(old, nil) != nil {
			return errFiles
		}
	} else {
		var st unix.Stat_t
		if unix.Fstatat(p, ".a2a", &st, unix.AT_SYMLINK_NOFOLLOW) != unix.ENOENT {
			return errFiles
		}
	}
	if !sameEntry(p, stageName, stage) {
		return errFiles
	}
	if err = publish(p, stageName, ".a2a", exists); err != nil {
		return errWrite
	}
	// A crash after this atomic operation sees a complete new generation. A
	// crash before it leaves the old directory unchanged (or absent).
	keep = exists
	postErr := error(nil)
	if hook != nil {
		postErr = hook("published", stage)
	}
	if postErr == nil && (!chain.valid() || !sameEntry(p, ".a2a", stage) || verify(stage, contents) != nil) {
		postErr = errFiles
	}
	if postErr == nil {
		postErr = unix.Fsync(p)
	}
	if postErr == nil {
		return nil
	}
	// On an ordinary post-publication failure, restore the previous namespace.
	// An exceptional failed rollback is explicitly uncertain, never success.
	if !sameEntry(p, ".a2a", stage) {
		keep = true
		return errRecovery
	}
	if exists {
		if !sameEntry(p, stageName, old) || publish(p, stageName, ".a2a", true) != nil {
			keep = true
			return errRecovery
		}
	} else if publish(p, ".a2a", stageName, false) != nil {
		keep = true
		return errRecovery
	}
	// This generation was briefly visible. A reader may have pinned it before
	// rollback, so retain it just like an old generation after successful swap.
	keep = true
	if unix.Fsync(p) != nil {
		keep = true
		return errRecovery
	}
	return errWrite
}

// Existing enrollment is a reader input: validate the supplied namespace,
// without adopting a newly resolved path or storing a station-home record.
func canonicalReaderPath(dir string) bool {
	if !filepath.IsAbs(dir) {
		return false
	}
	canonical, err := filepath.EvalSymlinks(dir)
	return err == nil && canonical == dir
}

func openDir(parent int, name string) (*os.File, error) {
	fd, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func safeStat(st *unix.Stat_t, kind uint32, mode uint32) bool {
	return st.Uid == uint32(os.Geteuid()) && uint32(st.Mode)&unix.S_IFMT == kind && uint32(st.Mode)&07777 == mode && (kind != unix.S_IFREG || st.Nlink == 1)
}

func ownedMode(fd int, kind uint32, mode uint32) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && safeStat(&st, kind, mode)
}

func sameEntry(parent int, name string, f *os.File) bool {
	var a, b unix.Stat_t
	return unix.Fstatat(parent, name, &a, unix.AT_SYMLINK_NOFOLLOW) == nil && unix.Fstat(int(f.Fd()), &b) == nil && a.Dev == b.Dev && a.Ino == b.Ino && uint32(a.Mode)&unix.S_IFMT == uint32(b.Mode)&unix.S_IFMT
}

// verify checks exactly three single-link regular files without following any
// symlink. Existing data must itself be a complete valid enrollment.
func verify(dir *os.File, want [][]byte) error {
	if !ownedMode(int(dir.Fd()), unix.S_IFDIR, 0700) {
		return errFiles
	}
	fd, err := unix.Openat(int(dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return errFiles
	}
	listing := os.NewFile(uintptr(fd), "enrollment")
	names, err := listing.Readdirnames(4)
	listing.Close()
	if err != nil && err != io.EOF || len(names) != 3 {
		return errFiles
	}
	allowed := map[string]bool{"courier.sock": true, "identity": true, "station.key": true}
	for _, name := range names {
		if !allowed[name] {
			return errFiles
		}
	}
	data := make([][]byte, 3)
	for i, name := range fileNames {
		fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return errFiles
		}
		f := os.NewFile(uintptr(fd), name)
		ok := ownedMode(fd, unix.S_IFREG, 0600)
		if ok {
			data[i], err = io.ReadAll(io.LimitReader(f, 1025))
		}
		ok = ok && ownedMode(fd, unix.S_IFREG, 0600) && sameEntry(int(dir.Fd()), name, f)
		f.Close()
		if !ok || err != nil {
			return errFiles
		}
		if want != nil && !bytes.Equal(data[i], want[i]) {
			return errFiles
		}
	}
	defer clear(data[2])
	_, pointerErr := courier.ParsePointerFile(data[0])
	_, identityErr := courier.ParseIdentityFile(data[1])
	if pointerErr != nil || identityErr != nil || len(data[2]) != courier.StationKeyLen {
		return errFiles
	}
	return nil
}

// Cleanup is confined to our newly allocated, still-linked staging inode. Old
// published generations are retained, never unlinked beneath active readers.
func removeStage(parent int, name string, stage *os.File) {
	if !sameEntry(parent, name, stage) {
		return
	}
	for _, file := range fileNames {
		_ = unix.Unlinkat(int(stage.Fd()), file, 0)
	}
	_ = unix.Unlinkat(parent, name, unix.AT_REMOVEDIR)
}

type parentChain struct {
	fds   []int
	names []string
}

func (c *parentChain) close() {
	for _, fd := range c.fds {
		unix.Close(fd)
	}
}

func safeAncestor(fd int, final bool) bool {
	var st unix.Stat_t
	return unix.Fstat(fd, &st) == nil && safeAncestorStat(&st, final)
}

func safeAncestorStat(st *unix.Stat_t, final bool) bool {
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR ||
		(st.Uid != uint32(os.Geteuid()) && st.Uid != 0) ||
		(final && st.Uid != uint32(os.Geteuid())) {
		return false
	}
	// Shared root-owned sticky ancestors (for example /tmp) are allowed.
	return uint32(st.Mode)&0022 == 0 || st.Uid == 0 && uint32(st.Mode)&unix.S_ISVTX != 0
}

func openParent(path string) (*parentChain, error) {
	c := &parentChain{}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errFiles
	}
	c.fds = append(c.fds, fd)
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name == "" {
			continue
		}
		if !safeAncestor(fd, false) {
			c.close()
			return nil, errFiles
		}
		f, err := openDir(fd, name)
		if err != nil {
			c.close()
			return nil, errFiles
		}
		// Transfer ownership from os.File without a finalizer closing the fd.
		next, err := unix.Dup(int(f.Fd()))
		f.Close()
		if err != nil {
			c.close()
			return nil, errFiles
		}
		unix.CloseOnExec(next)
		fd = next
		c.fds = append(c.fds, fd)
		c.names = append(c.names, name)
	}
	if !c.valid() {
		c.close()
		return nil, errFiles
	}
	return c, nil
}

func (c *parentChain) valid() bool {
	for i, fd := range c.fds {
		if !safeAncestor(fd, i == len(c.fds)-1) {
			return false
		}
		if i == 0 {
			continue
		}
		var a, b unix.Stat_t
		if unix.Fstatat(c.fds[i-1], c.names[i-1], &a, unix.AT_SYMLINK_NOFOLLOW) != nil || unix.Fstat(fd, &b) != nil || a.Dev != b.Dev || a.Ino != b.Ino || uint32(a.Mode)&unix.S_IFMT != unix.S_IFDIR {
			return false
		}
	}
	return true
}
