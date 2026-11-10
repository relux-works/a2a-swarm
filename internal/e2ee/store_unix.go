//go:build darwin || linux

package e2ee

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// FileStore is a directory capability, not a path subsequently resolved again.
// Its explicit root must be owned by the effective UID, mode exactly 0700. U1
// wires it to no production path and has no home/env/local-base fallback. U2a
// may wire only a Courier identity home whose daemon runs as the dedicated
// service user (_a2a), excluding the agent user's access to all custody. A local
// non-Courier identity gets no custody. This library never changes UID or repairs
// permissions; effective-UID checking alone does not prove the service-user role.
type FileStore struct {
	dir   *os.File
	fault func(string) error
	// writeStage is an internal IO seam; nil uses the actual file writer. A
	// partial failure leaves a private stage for next-lock recovery, never publish.
	writeStage func(*os.File, []byte) (int, error)
}

// LockedStore holds the identity-wide crypto lock. All store operations require
// it. It is not goroutine-safe: one coordinator owns a lease and its savepoints.
type LockedStore struct {
	store      *FileStore
	lock       *os.File
	closed     bool
	localID    lockIdentity
	recovering bool
}

type lockIdentity struct{ device, inode uint64 }

var processLocks = struct {
	sync.Mutex
	held map[lockIdentity]bool
}{held: map[lockIdentity]bool{}}

func reserveProcessLock(dir *os.File) (lockIdentity, error) {
	var st unix.Stat_t
	if unix.Fstat(int(dir.Fd()), &st) != nil {
		return lockIdentity{}, ErrIO
	}
	key := lockIdentity{uint64(st.Dev), uint64(st.Ino)}
	processLocks.Lock()
	defer processLocks.Unlock()
	if processLocks.held[key] {
		return key, ErrLock
	}
	processLocks.held[key] = true
	return key, nil
}
func releaseProcessLock(key lockIdentity) {
	processLocks.Lock()
	defer processLocks.Unlock()
	delete(processLocks.held, key)
}

func safeStat(f *os.File, directory bool) error {
	var st unix.Stat_t
	if unix.Fstat(int(f.Fd()), &st) != nil {
		return ErrIO
	}
	return validateStat(&st, os.Geteuid(), directory)
}

func validateStat(st *unix.Stat_t, uid int, directory bool) error {
	want := uint32(unix.S_IFREG)
	mode := uint32(0600)
	if directory {
		want = unix.S_IFDIR
		mode = 0700
	}
	if uint32(st.Mode)&unix.S_IFMT != want || uint32(st.Mode)&07777 != mode || int(st.Uid) != uid {
		return ErrUnsafe
	}
	return nil
}

// OpenFileStore rejects every symlink component, including platform aliases
// such as macOS /var. Callers must supply a host-resolved base (tests use
// filepath.EvalSymlinks(t.TempDir())). Only the final directory may be created.
func OpenFileStore(path string) (*FileStore, error) {
	return openFileStore(path, nil)
}

func openFileStore(path string, fault func(string) error) (*FileStore, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || len(path) > 4096 || path == "/" {
		return nil, ErrUnsafe
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrIO
	}
	current := os.NewFile(uintptr(fd), "e2ee-parent")
	for i, part := range parts {
		var ancestor unix.Stat_t
		if unix.Fstat(int(current.Fd()), &ancestor) != nil {
			current.Close()
			return nil, ErrIO
		}
		// Root-owned sticky temp roots are safe for a private owned child.
		// Other writable ancestors allow a different UID to replace the path.
		if (int(ancestor.Uid) != os.Geteuid() && ancestor.Uid != 0) || (uint32(ancestor.Mode)&0022 != 0 && !(ancestor.Uid == 0 && uint32(ancestor.Mode)&unix.S_ISVTX != 0)) {
			current.Close()
			return nil, ErrUnsafe
		}
		next, err := unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && i == len(parts)-1 {
			if safeStat(current, true) != nil {
				current.Close()
				return nil, ErrUnsafe
			}
			if err = unix.Mkdirat(int(current.Fd()), part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
				current.Close()
				return nil, ErrIO
			}
			next, err = unix.Openat(int(current.Fd()), part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err == nil && i == len(parts)-1 {
			// A retry also syncs an existing final entry: a previous mkdir may
			// have succeeded while its parent-directory sync was interrupted.
			child := os.NewFile(uintptr(next), "e2ee-dir-sync")
			check := &FileStore{fault: fault}
			if safeStat(child, true) != nil {
				child.Close()
				current.Close()
				return nil, ErrUnsafe
			}
			if check.step("mkdir:child-sync") != nil || child.Sync() != nil || check.step("mkdir:parent-sync") != nil || current.Sync() != nil {
				child.Close()
				current.Close()
				return nil, ErrIO
			}
			current.Close()
			return &FileStore{dir: child, fault: fault}, nil
		}
		current.Close()
		if err != nil {
			return nil, ErrUnsafe
		}
		current = os.NewFile(uintptr(next), "e2ee-dir")
	}
	if err = safeStat(current, true); err != nil {
		current.Close()
		return nil, err
	}
	return &FileStore{dir: current, fault: fault}, nil
}
func (s *FileStore) Close() error {
	if s.dir.Close() != nil {
		return ErrIO
	}
	return nil
}
func (s *FileStore) step(label string) error {
	if s.fault != nil && s.fault(label) != nil {
		return ErrIO
	}
	return nil
}
func (s *FileStore) syncDir(label string) error {
	if s.step(label) != nil || s.dir.Sync() != nil {
		return ErrIO
	}
	return nil
}
func (s *FileStore) open(name string) (*os.File, error) {
	if safeStat(s.dir, true) != nil {
		return nil, ErrUnsafe
	}
	fd, err := unix.Openat(int(s.dir.Fd()), name, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, ErrUnavailable
	}
	if err != nil {
		return nil, ErrUnsafe
	}
	f := os.NewFile(uintptr(fd), "e2ee-item")
	if err = safeStat(f, false); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
func (s *FileStore) Lock(ctx context.Context) (*LockedStore, error) {
	if safeStat(s.dir, true) != nil {
		return nil, ErrUnsafe
	}
	// Fail closed immediately on same-process reentry, even through a different
	// FileStore handle for this inode. Cross-process contention alone may wait.
	localID, err := reserveProcessLock(s.dir)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			releaseProcessLock(localID)
		}
	}()
	// O_EXCL distinguishes creation, whose directory entry must be durable.
	fd, err := unix.Openat(int(s.dir.Fd()), "crypto.lock", unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(int(s.dir.Fd()), "crypto.lock", unix.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, ErrUnsafe
	}
	f := os.NewFile(uintptr(fd), "e2ee-lock")
	if err = safeStat(f, false); err != nil {
		f.Close()
		return nil, err
	}
	if created && (s.step("lock-file-sync") != nil || f.Sync() != nil || s.syncDir("lock-dir-sync") != nil) {
		f.Close()
		return nil, ErrIO
	}
	for {
		if ctx.Err() != nil {
			f.Close()
			return nil, ErrLock
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			if err = s.cleanStages(); err != nil {
				_ = unix.Flock(fd, unix.LOCK_UN)
				f.Close()
				return nil, err
			}
			transferred = true
			return &LockedStore{store: s, lock: f, localID: localID}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			f.Close()
			return nil, ErrLock
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ErrLock
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// A crash may leave a staging link beside the durable published item. Cleanup
// runs only under the OS lock and removes only our exact random staging names,
// never a root, pointer or state. This prevents repeated interrupted commits
// from retaining unbounded obsolete sealed copies.
func (s *FileStore) cleanStages() error {
	fd, err := unix.Openat(int(s.dir.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return ErrIO
	}
	dir := os.NewFile(uintptr(fd), "e2ee-stage-scan")
	defer dir.Close()
	removed := false
	for {
		entries, err := dir.ReadDir(128)
		if err != nil && err != io.EOF {
			return ErrIO
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasPrefix(name, ".stage-") || !hexSize(strings.TrimPrefix(name, ".stage-"), 16) {
				continue
			}
			f, err := s.open(name)
			if err != nil {
				return err
			}
			f.Close()
			if unix.Unlinkat(int(s.dir.Fd()), name, 0) != nil {
				return ErrIO
			}
			removed = true
		}
		if err == io.EOF {
			break
		}
	}
	if removed {
		return s.syncDir("recovery-stage-sync")
	}
	return nil
}
func (l *LockedStore) Close() error {
	if l.closed || l.recovering {
		return ErrLock
	}
	l.closed = true
	err := unix.Flock(int(l.lock.Fd()), unix.LOCK_UN)
	closeErr := l.lock.Close()
	releaseProcessLock(l.localID)
	if err != nil || closeErr != nil {
		return ErrLock
	}
	return nil
}
func (l *LockedStore) read(name string, max int) ([]byte, error) {
	if l.closed {
		return nil, ErrLock
	}
	f, err := l.store.open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, int64(max)+1))
	if err != nil {
		return nil, ErrIO
	}
	if len(b) > max {
		return nil, ErrCapacity
	}
	return b, nil
}

// publish stages, fsyncs, publishes, fsyncs the directory, then durably removes
// the staging link. An error after publication is an uncertain commit: recovery
// must read back under the same lock, never assume that the old file survived.
func (l *LockedStore) publish(name string, data []byte, createOnly bool) error {
	if l.closed {
		return ErrLock
	}
	s := l.store
	if safeStat(s.dir, true) != nil {
		return ErrUnsafe
	}
	existing, err := s.open(name)
	if err == nil {
		existing.Close()
		if createOnly {
			return ErrExists
		}
	} else if err != ErrUnavailable {
		return err
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return ErrIO
	}
	tmp := ".stage-" + hex.EncodeToString(random[:])
	fd, err := unix.Openat(int(s.dir.Fd()), tmp, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return ErrIO
	}
	f := os.NewFile(uintptr(fd), "e2ee-stage")
	removeStage := true
	defer func() {
		f.Close()
		if removeStage {
			_ = unix.Unlinkat(int(s.dir.Fd()), tmp, 0)
		}
	}()
	if err = safeStat(f, false); err != nil {
		return err
	}
	if s.step(name+":write") != nil {
		return ErrIO
	}
	write := s.writeStage
	if write == nil {
		write = func(file *os.File, raw []byte) (int, error) { return file.Write(raw) }
	}
	n, err := write(f, data)
	if err != nil || n != len(data) {
		removeStage = false
		return ErrIO
	}
	if s.step(name+":file-sync") != nil || f.Sync() != nil {
		return ErrIO
	}
	if f.Close() != nil {
		return ErrIO
	}
	if s.step(name+":publish") != nil {
		return ErrIO
	}
	if createOnly {
		err = unix.Linkat(int(s.dir.Fd()), tmp, int(s.dir.Fd()), name, 0)
	} else {
		err = unix.Renameat(int(s.dir.Fd()), tmp, int(s.dir.Fd()), name)
	}
	if errors.Is(err, unix.EEXIST) {
		return ErrExists
	}
	if err != nil {
		return ErrIO
	}
	if s.syncDir(name+":dir-sync") != nil {
		return ErrIO
	}
	if createOnly {
		if s.step(name+":unlink") != nil {
			return ErrIO
		}
		if unix.Unlinkat(int(s.dir.Fd()), tmp, 0) != nil {
			return ErrIO
		}
		if s.syncDir(name+":cleanup-sync") != nil {
			return ErrIO
		}
	}
	return nil
}
