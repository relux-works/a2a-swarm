// Package skill carries the a2a agent skill inside the binary, so the installed
// skill always matches the CLI version that installed it.
package skill

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Name is the skill's directory name.
const Name = "a2a"

//go:embed files/a2a
var files embed.FS

// Files returns the embedded skill tree rooted at the skill directory.
func Files() fs.FS {
	sub, err := fs.Sub(files, "files/"+Name)
	if err != nil {
		panic(err)
	}
	return sub
}

// Result reports what Install did.
type Result struct {
	Copied   string   // ~/.agents/skills/a2a
	Linked   []string // ~/.claude/skills/a2a, ~/.codex/skills/a2a
	Replaced bool     // an older installed copy was replaced
}

// Install copies the skill into <home>/.agents/skills/a2a (a copy, not a
// link, so a moved or rebased checkout never breaks agents) and links it from
// <home>/.claude/skills and <home>/.codex/skills. It refuses to replace
// anything it did not create: a non-directory at the copy path, or a link
// path that is not a symlink to the copy (unless force).
func Install(home string, force bool) (Result, error) {
	var r Result
	dst := filepath.Join(home, ".agents", "skills", Name)
	r.Copied = dst
	if st, err := os.Lstat(dst); err == nil {
		if st.Mode()&fs.ModeSymlink != 0 || !st.IsDir() {
			if !force {
				return r, fmt.Errorf("%s exists and is not a directory; refusing (use --force)", dst)
			}
		}
		if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil && !force {
			return r, fmt.Errorf("%s exists but is not an a2a skill copy; refusing (use --force)", dst)
		}
		if err := os.RemoveAll(dst); err != nil {
			return r, err
		}
		r.Replaced = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return r, err
	}
	src := Files()
	if err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	}); err != nil {
		return r, err
	}
	for _, dir := range []string{".claude", ".codex"} {
		link := filepath.Join(home, dir, "skills", Name)
		if st, err := os.Lstat(link); err == nil {
			cur, lerr := os.Readlink(link)
			if st.Mode()&fs.ModeSymlink == 0 || (lerr == nil && cur != dst) {
				if !force {
					return r, fmt.Errorf("%s exists and is not a link to %s; refusing (use --force)", link, dst)
				}
			}
			if err := os.RemoveAll(link); err != nil {
				return r, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return r, err
		}
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			return r, err
		}
		if err := os.Symlink(dst, link); err != nil {
			return r, err
		}
		r.Linked = append(r.Linked, link)
	}
	return r, nil
}
