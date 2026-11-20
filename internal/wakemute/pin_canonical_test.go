//go:build linux || darwin

package wakemute

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestPinCanonicalRootAdmission proves a canonical root is admitted while a symlinked root is refused.
func TestPinCanonicalRootAdmission(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		path    string
		wantErr bool
	}{
		{name: "canonical root admitted", path: root},
		{name: "symlinked root refused", path: link, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			pinned, err := pin(test.path)
			if test.wantErr {
				if pinned != nil || !errors.Is(err, ErrUnsafe) {
					t.Fatalf("pin(%q) = (%v, %v), want an unsafe refusal", test.path, pinned, err)
				}
				return
			}
			if err != nil || pinned == nil {
				t.Fatalf("pin(%q) = (%v, %v), want admission", test.path, pinned, err)
			}
			pinned.close()
		})
	}
}
