//go:build unix && !linux

package association

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// On non-Linux Unix the admitted launcher closes the writer before passing an
// unlinked read-only descriptor. This deliberately does not claim immutability.
func frame(t *testing.T, b []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "synthetic-intent")
	raw := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(raw, uint32(len(b)))
	copy(raw[4:], b)
	if e := os.WriteFile(p, raw, 0600); e != nil {
		t.Fatal(e)
	}
	f, e := os.Open(p)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Remove(p); e != nil {
		f.Close()
		t.Fatal(e)
	}
	if _, e = f.Seek(2, 0); e != nil {
		f.Close()
		t.Fatal(e)
	}
	return f
}
