//go:build unix && !linux

package cli

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func associationDescriptor(t *testing.T, b []byte) *os.File {
	t.Helper()
	p := filepath.Join(t.TempDir(), "synthetic-intent")
	raw := make([]byte, len(b)+4)
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
	return f
}
