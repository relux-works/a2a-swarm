//go:build unix && !linux

package association

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestAssociationUnixDescriptorShapeBoundary(t *testing.T) {
	for _, writable := range []bool{false, true} {
		t.Run(map[bool]string{false: "read-only", true: "writable"}[writable], func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "synthetic-intent")
			raw := make([]byte, 6)
			binary.BigEndian.PutUint32(raw, 2)
			copy(raw[4:], "{}")
			if e := os.WriteFile(p, raw, 0600); e != nil {
				t.Fatal(e)
			}
			flag := os.O_RDONLY
			if writable {
				flag = os.O_RDWR
			}
			f, e := os.OpenFile(p, flag, 0)
			if e != nil {
				t.Fatal(e)
			}
			if e = os.Remove(p); e != nil {
				f.Close()
				t.Fatal(e)
			}
			body, reason := readIntent(f)
			if writable {
				if reason != "input_untrusted" {
					t.Fatal("writable descriptor admitted")
				}
			} else if reason != "" || string(body) != "{}" {
				t.Fatalf("read-only private unlinked descriptor refused: %s", reason)
			}
		})
	}
}
