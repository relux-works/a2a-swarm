//go:build unix

package association

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// safeOpen refuses symlinks in every component, special files, foreign owners,
// and permissive final modes. No O_CREATE, writable open or runtime lock exists.
func safeOpen(path string, mode os.FileMode, directory bool) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, invalid
	}
	for p := path; p != "/"; p = filepath.Dir(p) {
		st, e := os.Lstat(p)
		if e != nil || st.Mode()&os.ModeSymlink != 0 {
			return nil, invalid
		}
	}
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if e != nil {
		return nil, invalid
	}
	st, e := f.Stat()
	expectedMode := mode
	if directory {
		expectedMode |= os.ModeDir
	}
	if e != nil || st.Mode() != expectedMode {
		f.Close()
		return nil, invalid
	}
	s, ok := st.Sys().(*syscall.Stat_t)
	if !ok || s.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, invalid
	}
	if !samePath(f, path) {
		f.Close()
		return nil, invalid
	}
	return f, nil
}
func samePath(f *os.File, path string) bool {
	a, e := f.Stat()
	b, e2 := os.Lstat(path)
	return e == nil && e2 == nil && b.Mode()&os.ModeSymlink == 0 && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}
func stable(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime() == b.ModTime()
}
func privateDescriptor(st os.FileInfo) bool {
	s, ok := st.Sys().(*syscall.Stat_t)
	return ok && st.Mode() == 0600 && s.Uid == uint32(os.Geteuid()) && s.Nlink == 0
}

// ReadMetadata is only for bounded, nonsecret routing and host labels. An
// absent path is distinguished without reading credential or matrix.env files.
func ReadMetadata(path string, limit int64) ([]byte, error) {
	f, e := safeOpen(path, 0600, false)
	if e != nil {
		if _, s := os.Lstat(path); errors.Is(s, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, invalid
	}
	defer f.Close()
	a, _ := f.Stat()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	z, e2 := f.Stat()
	if e != nil || e2 != nil || int64(len(b)) > limit || !stable(a, z) || !samePath(f, path) {
		return nil, invalid
	}
	return b, nil
}
func snapshot(c Context, ctx context.Context) (map[string]any, error) {
	if ctx.Err() != nil {
		return nil, invalid
	}
	base, e := safeOpen(c.Base, 0700, true)
	if e != nil {
		return nil, invalid
	}
	defer base.Close()
	dir := filepath.Join(c.Base, "state")
	d, e := safeOpen(dir, 0700, true)
	if e != nil {
		return nil, invalid
	}
	defer d.Close()
	lockPath := filepath.Join(dir, c.Name+".lock")
	lock, e := safeOpen(lockPath, 0600, false)
	if e != nil {
		return nil, invalid
	}
	defer lock.Close()
	until := time.Now().Add(100 * time.Millisecond)
	for {
		if ctx.Err() != nil || !time.Now().Before(until) {
			return nil, invalid
		}
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if !errors.Is(e, syscall.EWOULDBLOCK) && !errors.Is(e, syscall.EINTR) {
			return nil, invalid
		}
		time.Sleep(min(time.Millisecond, time.Until(until)))
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	start := time.Now()
	path := filepath.Join(dir, c.Name+".json")
	f, e := safeOpen(path, 0600, false)
	if e != nil {
		return nil, invalid
	}
	defer f.Close()
	a, _ := f.Stat()
	b, e := io.ReadAll(io.LimitReader(f, 1048577))
	z, e2 := f.Stat()
	if e != nil || e2 != nil || len(b) > 1048576 || !stable(a, z) || !samePath(f, path) {
		return nil, invalid
	}
	m, e := parse(b, 32, 65536)
	if e != nil || ctx.Err() != nil || time.Since(start) > 100*time.Millisecond || !samePath(lock, lockPath) || !samePath(d, dir) || !samePath(base, c.Base) {
		return nil, invalid
	}
	return m, nil
}

func readIntent(f *os.File) ([]byte, string) {
	if f == nil {
		return nil, "input_untrusted"
	}
	defer f.Close()
	a, e := f.Stat()
	if e != nil || !privateDescriptor(a) || !descriptorCustody(f) {
		return nil, "input_untrusted"
	}
	if a.Size() < 5 || a.Size() > 8196 {
		return nil, "input_invalid"
	}
	b := make([]byte, int(a.Size()))
	if _, e = f.ReadAt(b, 0); e != nil {
		return nil, "input_invalid"
	}
	z, e := f.Stat()
	if e != nil || !stable(a, z) || !privateDescriptor(z) || !descriptorCustody(f) {
		return nil, "input_untrusted"
	}
	n := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if n == 0 || n > 8192 || int(n)+4 != len(b) {
		return nil, "input_invalid"
	}
	return b[4:], ""
}
