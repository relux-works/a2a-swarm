//go:build linux || darwin

package wakemute

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPinCanonicalRootAdmission covers canonical admission and two distinct symlink refusals.
func TestPinCanonicalRootAdmission(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	child := filepath.Join(real, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	ancestorLink := filepath.Join(root, "ancestor-alias")
	if err := os.Symlink(real, ancestorLink); err != nil {
		t.Fatal(err)
	}
	ancestorPath := filepath.Join(ancestorLink, "child")
	final, err := os.Lstat(ancestorPath)
	if err != nil || final.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("ancestor fixture final component = (%v, %v), want a non-symlink directory", final, err)
	}

	for _, test := range []struct {
		name    string
		claim   string
		path    string
		wantErr bool
	}{
		{name: "canonical root admitted", claim: "a canonical root is admitted", path: root},
		{name: "final-component symlink refused", claim: "a final-component symlink is refused", path: link, wantErr: true},
		{name: "ancestor symlink refused", claim: "a symlinked ancestor is refused even when the final component is a directory", path: ancestorPath, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pinned, err := pin(test.path)
			if test.wantErr {
				if pinned != nil || !errors.Is(err, ErrUnsafe) {
					t.Fatalf("%s: pin(%q) = (%v, %v), want an unsafe refusal", test.claim, test.path, pinned, err)
				}
				return
			}
			if err != nil || pinned == nil {
				t.Fatalf("%s: pin(%q) = (%v, %v), want admission", test.claim, test.path, pinned, err)
			}
			pinned.close()
		})
	}
}
