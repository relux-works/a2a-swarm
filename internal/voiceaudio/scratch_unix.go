//go:build linux || darwin

package voiceaudio

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"syscall"
)

// Check every physical component, not only EvalSymlinks' eventual target.
// A sticky writable ancestor is allowed; the caller root itself must be 0700.
func safePath(path string, private bool) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	for p := path; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			return false
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok || (st.Uid != uint32(os.Geteuid()) && st.Uid != 0) {
			return false
		}
		if fi.Mode().Perm()&0022 != 0 && fi.Mode()&os.ModeSticky == 0 {
			return false
		}
		if p == path && private && (st.Uid != uint32(os.Geteuid()) || fi.Mode().Perm() != 0700) {
			return false
		}
		if p == "/" {
			return true
		}
	}
}

func ownedRegular(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode().IsRegular() && fi.Mode().Perm()&0022 == 0 &&
		(st.Uid == uint32(os.Geteuid()) || st.Uid == 0)
}

type entry struct {
	name     string
	identity os.FileInfo
}
type scratch struct {
	parent, root *os.Root
	name, path   string
	identity     os.FileInfo
	entries      []entry
}

func newScratch(tmp string) (*scratch, Reason) {
	if !safePath(tmp, true) {
		return nil, UnsafeTMPDIR
	}
	parent, err := os.OpenRoot(tmp)
	if err != nil {
		return nil, UnsafeTMPDIR
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		parent.Close()
		return nil, UnsafeTMPDIR
	}
	name := "voiceaudio-" + hex.EncodeToString(random[:])
	if err = parent.Mkdir(name, 0700); err != nil {
		parent.Close()
		return nil, UnsafeTMPDIR
	}
	identity, err := parent.Lstat(name)
	if err != nil {
		parent.Close()
		return nil, UnsafeTMPDIR
	}
	root, err := parent.OpenRoot(name)
	if err != nil {
		parent.Remove(name)
		parent.Close()
		return nil, UnsafeTMPDIR
	}
	s := &scratch{parent: parent, root: root, name: name, path: filepath.Join(tmp, name), identity: identity}
	for _, dir := range []string{"home", "cache", "tmp"} {
		if err = s.root.Mkdir(dir, 0700); err != nil {
			s.cleanup()
			return nil, UnsafeTMPDIR
		}
		fi, err := s.root.Lstat(dir)
		if err != nil {
			s.cleanup()
			return nil, UnsafeTMPDIR
		}
		s.entries = append(s.entries, entry{dir, fi})
	}
	return s, ""
}

func (s *scratch) create(name string, mode os.FileMode) (*os.File, error) {
	f, err := s.root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	s.entries = append(s.entries, entry{name, fi})
	return f, nil
}

func (s *scratch) writeInput(data []byte) bool {
	f, err := s.create("input.ogg", 0600)
	if err != nil {
		return false
	}
	_, err = f.Write(data)
	closed := f.Close()
	return err == nil && closed == nil
}

// No recursive deletion: unexpected files and replaced identities survive.
// Cleanup failure invalidates success. Same-UID malicious mutation is outside
// this permission boundary, including the final snapshot-to-exec interval.
func (s *scratch) cleanup() bool {
	ok := true
	for i := len(s.entries) - 1; i >= 0; i-- {
		e := s.entries[i]
		fi, err := s.root.Lstat(e.name)
		if err != nil || !os.SameFile(fi, e.identity) {
			ok = false
			continue
		}
		if err = s.root.Remove(e.name); err != nil {
			ok = false
		}
	}
	fi, err := s.parent.Lstat(s.name)
	if err != nil || !os.SameFile(fi, s.identity) {
		ok = false
	} else if s.parent.Remove(s.name) != nil {
		ok = false
	}
	if s.root.Close() != nil {
		ok = false
	}
	if s.parent.Close() != nil {
		ok = false
	}
	return ok
}
