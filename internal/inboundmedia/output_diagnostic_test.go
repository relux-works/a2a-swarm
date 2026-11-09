//go:build linux || darwin

package inboundmedia

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeOutputDiagnosticAndValidNeighbour(t *testing.T) {
	for _, kind := range []string{"outside", "unset", "relative", "exists", "missing", "parent-link", "leaf-link"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, []byte("valid"), 64)
			root, err := filepath.EvalSymlinks(f.root)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("TMPDIR", root)
			output := filepath.Join(root, "private-output")
			reason := "output_outside_tmpdir"
			switch kind {
			case "outside":
				output = filepath.Join(filepath.Dir(root), "private-output")
			case "unset":
				t.Setenv("TMPDIR", "")
				reason = "tmpdir_unset_or_relative"
			case "relative":
				t.Setenv("TMPDIR", "relative")
				reason = "tmpdir_unset_or_relative"
			case "exists":
				if err := os.WriteFile(output, []byte("competitor"), 0600); err != nil {
					t.Fatal(err)
				}
				reason = "output_exists"
			case "missing":
				output = filepath.Join(root, "missing", "private-output")
				reason = "parent_missing"
			case "parent-link", "leaf-link":
				if err := os.Symlink(root, output); err != nil {
					t.Fatal(err)
				}
				if kind == "parent-link" {
					output = filepath.Join(output, "private-output")
				}
				reason = "symlink_in_path"
			}
			_, err = Fetch(context.Background(), f.c, validEvent("m.file", 5), selection, output)
			wantRefusal(t, err, UnsafeOutput)
			message := err.Error()
			if !strings.Contains(message, "subreason="+reason) || !strings.Contains(message, `-o "$TMPDIR/<name>"`) || strings.Contains(message, root) || strings.Contains(message, "private-output") {
				t.Fatalf("unsafe diagnostic: %s", message)
			}
			if f.downloads.Load() != 0 || f.configs.Load() != 0 {
				t.Fatal("unsafe output reached media endpoints")
			}
			t.Setenv("TMPDIR", root)
			if _, err := f.fetch(validEvent("m.file", 5), "fresh"); err != nil {
				t.Fatalf("valid neighbour: %v", err)
			}
		})
	}
}
