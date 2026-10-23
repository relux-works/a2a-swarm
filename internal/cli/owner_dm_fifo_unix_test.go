//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func ownerTranscriptSentinel(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f, err := os.OpenFile(path, os.O_RDWR|syscall.O_NONBLOCK, 0600)
		if err == nil {
			_, _ = f.Write([]byte("forbidden transcript witness\n"))
			_ = f.Close()
		}
	})
	return path
}
