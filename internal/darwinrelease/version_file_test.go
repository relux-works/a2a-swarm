package darwinrelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proves release/VERSION is one stable version that the release admission's
// CheckVersion accepts for its own tag, that a different tag is refused, and
// that the release docs and README state the same version, so the proposed
// version cannot drift between the file the pipeline reads and the docs.
func TestReleaseVersionFileIsConsistent(t *testing.T) {
	root := filepath.Join("..", "..")
	raw, err := os.ReadFile(filepath.Join(root, "release", "VERSION"))
	if err != nil {
		t.Fatal(err)
	}
	version := strings.TrimSpace(string(raw))
	if string(raw) != version+"\n" {
		t.Fatalf("release/VERSION must be one line with a trailing newline: %q", raw)
	}
	if err := CheckVersion("v"+version, version); err != nil {
		t.Fatalf("CheckVersion(v%s) = %v", version, err)
	}
	if CheckVersion("v0.0.0", version) == nil {
		t.Fatal("a different tag was accepted for release/VERSION")
	}
	for file, want := range map[string]string{
		filepath.Join("docs", "darwin-release.md"): "`release/VERSION` proposes **" + version + "**",
		"README.md": "The successor candidate version is " + version,
	} {
		doc, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(strings.Join(strings.Fields(string(doc)), " "), want) {
			t.Fatalf("%s does not state %q", file, want)
		}
	}
}
