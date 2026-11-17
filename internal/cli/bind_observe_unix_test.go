//go:build darwin || linux

package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// Claim: on the launch alias means argv0 is a custody-admitted symlink
// alias of the running image. Beside that positive control, a direct image
// launch, a bare name, and an alias in an other-writable directory are all
// off the alias, so an alias retarget cannot reach those consumers.
func TestOnLaunchAliasVerdict(t *testing.T) {
	root := launchAliasCustodyRoot(t)
	images, links, open := filepath.Join(root, "images"), filepath.Join(root, "links"), filepath.Join(root, "open")
	mkdirs(t, images, links, open)
	if err := os.Chmod(open, 0777); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(images, "a2a-image")
	if err := os.WriteFile(image, []byte("image"), 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(links, "a2a")
	writable := filepath.Join(open, "a2a")
	for link, target := range map[string]string{alias: image, writable: image} {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, argv0 string
		want        bool
	}{
		{"admitted symlink alias", alias, true},
		{"direct image", image, false},
		{"bare name", "a2a", false},
		{"alias in other-writable directory", writable, false},
		{"empty argv0", "", false},
	} {
		if got := onLaunchAlias(tc.argv0, image); got != tc.want {
			t.Errorf("%s: onLaunchAlias=%v, want %v", tc.name, got, tc.want)
		}
	}
}
