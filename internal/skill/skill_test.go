package skill

import (
	"bytes"
	"encoding/json"
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
		// Every embedded package member, including the Curator manifest and
		// shipped references, must survive both install and replacement.
		if err := fs.WalkDir(Files(), ".", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			want, err := fs.ReadFile(Files(), path)
			if err != nil {
				return err
			}
			got, err := os.ReadFile(filepath.Join(r.Copied, filepath.FromSlash(path)))
			if err != nil {
				return err
			}
			if !bytes.Equal(got, want) {
				t.Errorf("installed %s differs from embedded package", path)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
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

// The installed package declares only current instructed contacts: Matrix,
// the public GitHub skill/build source, and the gh api documentation fetch.
// This checks declarations, not permission to provision accounts or run hooks.
func TestInstallNetworkCapabilities(t *testing.T) {
	r, err := Install(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(r.Copied, "agent-skill.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		SchemaVersion int `json:"schema_version"`
		Capabilities  struct {
			Network []string `json:"network"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 3 {
		t.Fatalf("schema = %d, want narrow capability schema 3", manifest.SchemaVersion)
	}
	want := []string{"matrix.relux.works", "github.com", "api.github.com"}
	if len(manifest.Capabilities.Network) != len(want) {
		t.Errorf("network = %v, want exactly %v", manifest.Capabilities.Network, want)
	}
	for _, host := range want {
		t.Run(host, func(t *testing.T) {
			count := 0
			for _, declared := range manifest.Capabilities.Network {
				if declared == host {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%s declared %d times, want once", host, count)
			}
		})
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
