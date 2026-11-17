//go:build unix

package envelopeobserve

import (
	"bytes"
	"os"
	"testing"
)

type protectedFixture struct {
	*bytes.Reader
	info os.FileInfo
}

func (f protectedFixture) Stat() (os.FileInfo, error) { return f.info, nil }
func (f protectedFixture) Close() error               { return nil }

// Claim: the shared reader admits protected root-owned snapshots and rejects writable, symlink, foreign-owned, nonregular and oversized snapshots or ancestors.
func TestProtectedHostFileSnapshot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("requires a non-root reader")
	}
	good := fileInfo{mode: 0644, uid: 0}
	for _, tc := range []struct {
		name, path string
		info       fileInfo
		limit      int
	}{
		{"group-write", "/policy/map", fileInfo{mode: 0664, uid: 0}, 2},
		{"world-write", "/policy/map", fileInfo{mode: 0646, uid: 0}, 2},
		{"symlink", "/policy/map", fileInfo{mode: os.ModeSymlink | 0644, uid: 0}, 2},
		{"foreign", "/policy/map", fileInfo{mode: 0644, uid: uint32(os.Geteuid())}, 2},
		{"fifo", "/policy/map", fileInfo{mode: os.ModeNamedPipe | 0644, uid: 0}, 2},
		{"ancestor-write", "/policy", fileInfo{mode: os.ModeDir | 0775, uid: 0}, 2},
		{"ancestor-symlink", "/policy", fileInfo{mode: os.ModeSymlink | 0755, uid: 0}, 2},
		{"ancestor-owner", "/policy", fileInfo{mode: os.ModeDir | 0755, uid: uint32(os.Geteuid())}, 2},
		{"oversize", "", good, 1},
		{"opened-inode", "opened", fileInfo{mode: 0664, uid: 0}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			read := func(bad bool) ([]byte, error) {
				return readProtectedFile("/policy/map", func() int {
					if bad {
						return tc.limit
					}
					return 2
				}(), func(path string) (os.FileInfo, error) {
					if bad && path == tc.path {
						return tc.info, nil
					}
					if path == "/policy/map" {
						return good, nil
					}
					return fileInfo{mode: os.ModeDir | 0755, uid: 0}, nil
				}, func(string) (protectedFile, error) {
					info := good
					if bad && tc.path == "opened" {
						info = tc.info
					}
					return protectedFixture{bytes.NewReader([]byte("{}")), info}, nil
				})
			}
			if raw, err := read(false); err != nil || string(raw) != "{}" {
				t.Fatal("protected control unavailable")
			}
			if raw, err := read(true); err != ErrRoster || raw != nil {
				t.Fatal("unprotected snapshot admitted")
			}
			if raw, err := read(false); err != nil || string(raw) != "{}" {
				t.Fatal("restored control unavailable")
			}
		})
	}
}
