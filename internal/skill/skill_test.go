package skill

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Proves the embedded skill has valid frontmatter (first line ---, name a2a,
// a description) and stays under the 200-line budget.
func TestEmbeddedSkill(t *testing.T) {
	data, err := fs.ReadFile(Files(), "SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.HasPrefix(s, "---\nname: a2a\ndescription: >\n") || strings.Count(s, "\n---\n") < 1 {
		t.Fatalf("bad frontmatter:\n%s", s[:120])
	}
	if n := strings.Count(s, "\n"); n > 200 {
		t.Fatalf("SKILL.md has %d lines, budget 200", n)
	}
}

// Proves install copies the skill and links it from .claude and .codex, and
// that a re-install replaces its own copy and links (idempotent).
func TestInstall(t *testing.T) {
	home := t.TempDir()
	for i := 0; i < 2; i++ {
		r, err := Install(home, false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 && !r.Replaced {
			t.Fatal("second install did not report replacing its copy")
		}
		copied := filepath.Join(home, ".agents", "skills", "a2a", "SKILL.md")
		if st, err := os.Lstat(copied); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("copy missing: %v", err)
		}
		for _, d := range []string{".claude", ".codex"} {
			link := filepath.Join(home, d, "skills", "a2a")
			target, err := os.Readlink(link)
			if err != nil || target != filepath.Join(home, ".agents", "skills", "a2a") {
				t.Fatalf("%s -> %q, %v", link, target, err)
			}
			if _, err := os.Stat(filepath.Join(link, "SKILL.md")); err != nil {
				t.Fatalf("link does not resolve: %v", err)
			}
		}
	}
}

// Proves install never replaces a foreign skill of the same name without
// --force: a real directory at a link path, or a link elsewhere, is refused and
// left untouched; --force replaces it.
func TestInstallRefusesForeign(t *testing.T) {
	home := t.TempDir()
	foreign := filepath.Join(home, ".claude", "skills", "a2a")
	os.MkdirAll(foreign, 0o755)
	os.WriteFile(filepath.Join(foreign, "SKILL.md"), []byte("someone else's"), 0o644)
	if _, err := Install(home, false); err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(foreign, "SKILL.md")); string(data) != "someone else's" {
		t.Fatal("foreign skill was touched")
	}
	other := filepath.Join(home, ".codex", "skills", "a2a")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.Symlink("/elsewhere", other)
	os.RemoveAll(foreign)
	if _, err := Install(home, false); err == nil {
		t.Fatal("link elsewhere was replaced without --force")
	}
	if _, err := Install(home, true); err != nil {
		t.Fatalf("--force: %v", err)
	}
}
