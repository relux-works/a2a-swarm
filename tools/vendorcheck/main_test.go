package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Proves exact fixture equality, intended drift refusal, then restored success;
// no generator or external module is run by these comparison tests.
func TestCompareTreesDriftAndRestore(t *testing.T) {
	expected, actual := t.TempDir(), t.TempDir()
	for _, root := range []string{expected, actual} {
		if err := os.WriteFile(filepath.Join(root, "module.go"), []byte("package module\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := compareTrees(expected, actual); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(actual, "module.go")
	if err := os.WriteFile(file, []byte("package module\n// drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareTrees(expected, actual); !errors.Is(err, errDrift) {
		t.Fatalf("want named drift, got %v", err)
	}
	if err := os.WriteFile(file, []byte("package module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := compareTrees(expected, actual); err != nil {
		t.Fatal(err)
	}
}

// Proves a byte comparison cannot silently omit extras, missing files, type
// changes or symlinks, even when a link would resolve to matching bytes.
func TestCompareTreesRefusesStructuralChanges(t *testing.T) {
	for _, kind := range []string{"extra", "missing", "directory", "symlink", "executable"} {
		t.Run(kind, func(t *testing.T) {
			expected, actual := t.TempDir(), t.TempDir()
			for _, root := range []string{expected, actual} {
				if err := os.WriteFile(filepath.Join(root, "module.go"), []byte("package module\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			file := filepath.Join(actual, "module.go")
			if kind == "extra" {
				if err := os.WriteFile(filepath.Join(actual, "extra"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			} else if kind == "executable" {
				if err := os.Chmod(file, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if kind == "directory" {
					if err := os.Mkdir(file, 0o755); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "symlink" {
					if err := os.Symlink(filepath.Join(expected, "module.go"), file); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := compareTrees(expected, actual); !errors.Is(err, errDrift) {
				t.Fatalf("want named drift, got %v", err)
			}
		})
	}
}
