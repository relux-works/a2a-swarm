//go:build linux || darwin

package inboundmedia

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"golang.org/x/sys/unix"
)

func TestNativeExplicitOutputAndUnsafePaths(t *testing.T) {
	for _, kind := range []string{"missing", "outside", "traversal", "parent-symlink", "leaf-symlink", "dangling", "exists", "directory", "fifo", "missing-parent", "newline", "no-tmp", "write-error"} {
		t.Run(kind, func(t *testing.T) {
			outside := t.TempDir()
			f := newFixture(t, []byte("valid"), 64)
			output := filepath.Join(f.root, "out")
			code := UnsafeOutput
			switch kind {
			case "missing":
				output = ""
				code = OutputRequired
			case "outside":
				output = filepath.Join(outside, "out")
			case "traversal":
				output = f.root + "/../" + filepath.Base(f.root) + "/out"
			case "parent-symlink":
				if err := os.Symlink(outside, filepath.Join(f.root, "link")); err != nil {
					t.Fatal(err)
				}
				output = filepath.Join(f.root, "link", "out")
			case "leaf-symlink":
				if err := os.Symlink(filepath.Join(outside, "victim"), output); err != nil {
					t.Fatal(err)
				}
				code = OutputExists
			case "dangling":
				if err := os.Symlink("absent", output); err != nil {
					t.Fatal(err)
				}
				code = OutputExists
			case "exists":
				if err := os.WriteFile(output, []byte("competitor"), 0600); err != nil {
					t.Fatal(err)
				}
				code = OutputExists
			case "directory":
				if err := os.Mkdir(output, 0700); err != nil {
					t.Fatal(err)
				}
				code = OutputExists
			case "fifo":
				if err := unix.Mkfifo(output, 0600); err != nil {
					t.Fatal(err)
				}
				code = OutputExists
			case "missing-parent":
				output = filepath.Join(f.root, "absent", "out")
			case "newline":
				output = filepath.Join(f.root, "out\nspoof")
			case "no-tmp":
				t.Setenv("TMPDIR", "")
			case "write-error":
				if err := os.Chmod(f.root, 0500); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(f.root, 0700)
				code = TransferFailed
			}
			_, err := Fetch(context.Background(), f.c, validEvent("m.file", 5), selection, output)
			wantRefusal(t, err, code)
			if f.downloads.Load() != 0 {
				t.Fatal("unsafe output downloaded")
			}
			entries, _ := os.ReadDir(f.root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".a2a-native-media-") {
					t.Fatal("scratch leftover")
				}
			}
			if kind == "exists" {
				data, _ := os.ReadFile(output)
				if string(data) != "competitor" {
					t.Fatal("clobbered competitor")
				}
			}
			if kind == "write-error" {
				if err := os.Chmod(f.root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			cleanRoot(t, outside)
			// The same bytes and explicit fresh output pass after only the bad path
			// is replaced; malicious declared names never choose this path.
			t.Setenv("TMPDIR", f.root)
			if _, err := f.fetch(validEvent("m.file", 5), "fresh"); err != nil {
				t.Fatalf("valid neighbour: %v", err)
			}
		})
	}
}

// Pause one real synthetic stream, mutate the final path, then release it.
// Publication is atomic no-replace and cleanup never deletes the competitor.
// Parent substitution is refused while the retained FD prevents redirection.
func TestNativePublicationRacesAndPrivateModes(t *testing.T) {
	for _, kind := range []string{"fresh", "competitor", "symlink", "parent-replacement", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			outside := t.TempDir()
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			parent := filepath.Join(root, "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			started := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseStream := func() { releaseOnce.Do(func() { close(release) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/config") {
					fmt.Fprint(w, `{"m.upload.size":64}`)
					return
				}
				w.(http.Flusher).Flush()
				close(started)
				<-release
				fmt.Fprint(w, "valid")
			}))
			defer srv.Close()
			defer releaseStream()
			c := matrix.New(srv.URL, "synthetic")
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			done := make(chan error, 1)
			path := filepath.Join(parent, "out")
			go func() { _, err := Fetch(ctx, c, validEvent("m.file", 5), selection, path); done <- err }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("fetch ended before stream start: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			entries, _ := os.ReadDir(root)
			found := false
			for _, entry := range entries {
				if !strings.HasPrefix(entry.Name(), ".a2a-native-media-") {
					continue
				}
				found = true
				info, err := entry.Info()
				if err != nil || info.Mode().Perm() != 0700 {
					t.Errorf("scratch directory mode: %v %v", info, err)
				}
				stat, err := os.Stat(filepath.Join(root, entry.Name(), "bytes"))
				if err != nil || stat.Mode().Perm() != 0600 {
					t.Errorf("scratch file mode: %v %v", stat, err)
				}
			}
			if !found {
				t.Error("no private scratch during stream")
			}
			switch kind {
			case "competitor":
				if err := os.WriteFile(path, []byte("competitor"), 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(outside, "victim"), path); err != nil {
					t.Fatal(err)
				}
			case "parent-replacement":
				if err := os.Rename(parent, filepath.Join(root, "moved")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, parent); err != nil {
					t.Fatal(err)
				}
			case "cancel":
				cancel()
			}
			releaseStream()
			var err error
			select {
			case err = <-done:
			case <-time.After(15 * time.Second):
				t.Fatal("fetch did not stop after release")
			}
			switch kind {
			case "fresh":
				if err != nil {
					t.Fatal(err)
				}
				data, _ := os.ReadFile(path)
				if string(data) != "valid" {
					t.Fatal("valid neighbour bytes")
				}
			case "competitor":
				wantRefusal(t, err, OutputExists)
				data, _ := os.ReadFile(path)
				if string(data) != "competitor" {
					t.Fatal("competitor replaced/deleted")
				}
			case "symlink":
				wantRefusal(t, err, OutputExists)
				if _, err := os.Readlink(path); err != nil {
					t.Fatal("competing symlink deleted")
				}
			case "parent-replacement":
				wantRefusal(t, err, UnsafeOutput)
				cleanRoot(t, filepath.Join(root, "moved"))
			case "cancel":
				wantRefusal(t, err, Cancelled)
				cleanRoot(t, parent)
			}
			cleanRoot(t, outside)
			entries, _ = os.ReadDir(root)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".a2a-native-media-") {
					t.Fatal("private scratch leaked")
				}
			}
		})
	}
}

func TestNativeNoRenameFallbackOnLinkFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	target, code := openTarget(filepath.Join(root, "out"))
	if code != "" {
		t.Fatal(code)
	}
	defer target.cleanup()
	if _, err := target.file.WriteString("verified"); err != nil {
		t.Fatal(err)
	}
	if err := target.file.Close(); err != nil {
		t.Fatal(err)
	}
	// A real link error (missing source) must not create/truncate a target.
	if err := unix.Unlinkat(target.scratch, "bytes", 0); err != nil {
		t.Fatal(err)
	}
	if code := target.publish(); code != PublicationFailed {
		t.Fatalf("missing source publication=%s", code)
	}
	if _, err := os.Lstat(filepath.Join(root, "out")); !os.IsNotExist(err) {
		t.Fatal("failed link created target")
	}
}
