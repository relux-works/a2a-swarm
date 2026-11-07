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
)

func TestNativeConfiguredRootReplacement(t *testing.T) {
	for _, name := range []string{"valid", "root-directory", "root-symlink", "ancestor-directory", "ancestor-symlink"} {
		t.Run(name, func(t *testing.T) {
			base := t.TempDir()
			ancestor := filepath.Join(base, "container")
			root := filepath.Join(ancestor, "tmp")
			moved := filepath.Join(base, "moved")
			replacement := filepath.Join(base, "replacement")
			for _, dir := range []string{root, replacement} {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if strings.HasPrefix(name, "ancestor-") {
				if err := os.Mkdir(filepath.Join(replacement, "tmp"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("TMPDIR", root)
			started, release := make(chan struct{}), make(chan struct{})
			var releaseOnce, streamOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/config") {
					fmt.Fprint(w, `{"m.upload.size":64}`)
					return
				}
				w.(http.Flusher).Flush()
				streamOnce.Do(func() { close(started); <-release })
				fmt.Fprint(w, "valid")
			}))
			defer srv.Close()
			defer unblock()
			c := matrix.New(srv.URL, "synthetic-token")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			output := filepath.Join(root, "out")
			done := make(chan error, 1)
			go func() { _, err := Fetch(ctx, c, validEvent("m.file", 5), selection, output); done <- err }()
			select {
			case <-started:
			case err := <-done:
				t.Fatalf("fetch ended before stream start: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			oldRoot := root
			if name != "valid" {
				target := root
				if strings.HasPrefix(name, "ancestor-") {
					target = ancestor
				}
				if err := os.Rename(target, moved); err != nil {
					t.Fatal(err)
				}
				oldRoot = moved
				if target == ancestor {
					oldRoot = filepath.Join(moved, "tmp")
				}
				if strings.HasSuffix(name, "symlink") {
					if err := os.Symlink(replacement, target); err != nil {
						t.Fatal(err)
					}
				} else if err := os.MkdirAll(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			unblock()
			err := <-done
			if name == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if data, err := os.ReadFile(output); err != nil || string(data) != "valid" {
					t.Fatalf("returned path: %q %v", data, err)
				}
			} else {
				wantRefusal(t, err, UnsafeOutput)
				cleanRoot(t, oldRoot)
				cleanRoot(t, root)
				// Restore the fixture path, then repeat the same event/client.
				target := root
				if strings.HasPrefix(name, "ancestor-") {
					target = ancestor
				}
				if err := os.RemoveAll(target); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(moved, target); err != nil {
					t.Fatal(err)
				}
			}
			result, err := Fetch(context.Background(), c, validEvent("m.file", 5), selection, filepath.Join(root, "valid-neighbour"))
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(result.Path); err != nil || string(data) != "valid" {
				t.Fatalf("valid neighbour returned path: %q %v", data, err)
			}
		})
	}
}

func TestNativeOutputDirectorySpelling(t *testing.T) {
	for _, suffix := range []string{"/", "/.", "/./", "//"} {
		t.Run(suffix, func(t *testing.T) {
			f := newFixture(t, []byte("valid"), 64)
			path := filepath.Join(f.root, "chosen")
			_, err := Fetch(context.Background(), f.c, validEvent("m.file", 5), selection, path+suffix)
			wantRefusal(t, err, UnsafeOutput)
			if f.configs.Load() != 0 || f.downloads.Load() != 0 {
				t.Fatal("directory spelling reached media preflight/transfer")
			}
			cleanRoot(t, f.root)
			result, err := f.fetch(validEvent("m.file", 5), "ordinary-file")
			if err != nil {
				t.Fatal(err)
			}
			if data, err := os.ReadFile(result.Path); err != nil || string(data) != "valid" {
				t.Fatalf("ordinary file returned path: %q %v", data, err)
			}
		})
	}
}
