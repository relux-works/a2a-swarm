//go:build unix

package voice

import (
	"context"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FIFO and symlink opens cannot hang even if substituted after an Lstat;
// regular-file descriptor/path identity checks still admit an intact positive.
func TestReleaseOpenSpecialFiles(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, []byte("intact"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := HashReleaseFile(context.Background(), regular); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := unix.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for _, p := range []string{fifo, link} {
		f, err := openReleaseFile(p)
		if err == nil {
			st, e := f.Stat()
			f.Close()
			if e != nil || st.Mode().IsRegular() {
				t.Fatal("special file masqueraded as regular")
			}
		}
		if _, err := HashReleaseFile(context.Background(), p); err == nil {
			t.Fatal("hashed special file")
		}
	}
	if time.Since(start) > time.Second {
		t.Fatal("special-file refusal blocked")
	}
}
