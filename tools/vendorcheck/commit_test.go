package main

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type fixtureFile struct {
	data string
	mode string
}

func fixtureGit(t *testing.T, repo, input string, args ...string) string {
	t.Helper()
	out, err := git(context.Background(), repo, []byte(input), args...)
	if err != nil {
		t.Fatalf("fixture git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// Synthetic Git objects are fixture data, never published commits or tags.
func fixtureCommit(t *testing.T, repo string, files map[string]fixtureFile, parent, label string) string {
	t.Helper()
	entries := map[string]entry{}
	for name, file := range files {
		entries[name] = entry{file.mode, fixtureGit(t, repo, file.data, "hash-object", "-w", "--stdin")}
	}
	var tree func(string) string
	tree = func(prefix string) string {
		direct := map[string]entry{}
		dirs := map[string]bool{}
		for path, item := range entries {
			if !strings.HasPrefix(path, prefix) {
				continue
			}
			suffix := strings.TrimPrefix(path, prefix)
			first, _, nested := strings.Cut(suffix, "/")
			if nested {
				dirs[first] = true
			} else {
				direct[first] = item
			}
		}
		for dir := range dirs {
			direct[dir] = entry{"040000", tree(prefix + dir + "/")}
		}
		var input strings.Builder
		for _, name := range sortedKeys(direct) {
			kind := "blob"
			if direct[name].mode == "040000" {
				kind = "tree"
			}
			fmt.Fprintf(&input, "%s %s %s\t%s\n", direct[name].mode, kind, direct[name].oid, name)
		}
		return fixtureGit(t, repo, input.String(), "mktree")
	}
	object := "tree " + tree("") + "\n"
	if parent != "" {
		object += "parent " + parent + "\n"
	}
	object += "author Fixture <fixture@example.invalid> 1700000000 +0000\ncommitter Fixture <fixture@example.invalid> 1700000000 +0000\n\n" + label + "\n"
	return fixtureGit(t, repo, object, "hash-object", "-t", "commit", "-w", "--stdin")
}

func fixtureFiles() map[string]fixtureFile {
	return map[string]fixtureFile{
		".gitignore":         {"*.lock\nbin/\n", "100644"},
		".gitattributes":     {"vendor/** export-ignore\n", "100644"},
		"go.mod":             {"module fixture.invalid/program\n\ngo 1.25.5\n\nrequire fixture.invalid/dep v0.0.0\nreplace fixture.invalid/dep => ./dep\n", "100644"},
		"cmd/a2a/main.go":    {"package main\nimport (\"fmt\"; \"fixture.invalid/dep\")\nfunc main(){fmt.Println(dep.Value)}\n", "100644"},
		"dep/go.mod":         {"module fixture.invalid/dep\n\ngo 1.25.5\n", "100644"},
		"dep/value.go":       {"package dep\nimport _ \"embed\"\n//go:embed receipt.lock\nvar Value string\n", "100644"},
		"dep/receipt.lock":   {"original\n", "100644"},
		"alternate/go.mod":   {"module fixture.invalid/dep\n\ngo 1.25.5\n", "100644"},
		"alternate/value.go": {"package dep\nconst Value = \"alternate\"\n", "100644"},
	}
}

func cloneFiles(files map[string]fixtureFile) map[string]fixtureFile {
	copy := map[string]fixtureFile{}
	for k, v := range files {
		copy[k] = v
	}
	return copy
}

func withVendor(t *testing.T, source map[string]fixtureFile) map[string]fixtureFile {
	t.Helper()
	dir := t.TempDir()
	for name, file := range source {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(file.data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("go", "mod", "vendor")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOENV=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOFLAGS=")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture generator: %v: %s", err, output)
	}
	files := cloneFiles(source)
	err := filepath.WalkDir(filepath.Join(dir, "vendor"), func(path string, item fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if item.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(dir, path)
		files[filepath.ToSlash(relative)] = fixtureFile{string(data), "100644"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	files["skill-build.json"] = fixtureFile{`{"schema_version":1,"targets":{"a2a":{"driver":"go-repository-v1","build_root":".","source_dir":"cmd/a2a"}}}` + "\n", "100644"}
	return files
}

// Exercises the complete CLI path: raw committed bytes, main binding, branch
// membership and real vendoring with a local dependency. No remote or model.
func TestRunCommitGate(t *testing.T) {
	repo := t.TempDir()
	fixtureGit(t, repo, "", "init", "--bare", "--quiet", "--template=")
	source := fixtureFiles()
	base := fixtureCommit(t, repo, source, "", "main")
	clean := withVendor(t, source)
	pin := fixtureCommit(t, repo, clean, base, "build")
	fixtureGit(t, repo, "", "update-ref", mainRef, base)
	fixtureGit(t, repo, "", "update-ref", buildRef, pin)
	check := func(commit, main, want string) {
		t.Helper()
		var out bytes.Buffer
		err := run([]string{"--repository", repo, "--commit", commit, "--main-commit", main}, &out)
		t.Logf("gate output=%q refusal=%v", strings.TrimSpace(out.String()), err)
		if want == "" {
			if err != nil || !strings.Contains(out.String(), "vendor_tree_equal commit="+commit+" main="+main) {
				t.Fatalf("positive: %v %s", err, &out)
			}
		} else if err == nil || err.Error() != want {
			t.Fatalf("want %q, got %v", want, err)
		}
	}
	check(pin, base, "")
	// Export-ignore and *.lock cannot omit the committed embedded lock file.
	if _, ok := clean["vendor/fixture.invalid/dep/receipt.lock"]; !ok {
		t.Fatal("ignored-path positive control absent")
	}
	sourceEdit := cloneFiles(clean)
	sourceEdit["cmd/a2a/main.go"] = fixtureFile{"package main\nfunc main(){}\n", "100644"}
	check(fixtureCommit(t, repo, sourceEdit, base, "source plant"), base, "source_tree_drift: cmd/a2a/main.go")
	replace := cloneFiles(source)
	replace["go.mod"] = fixtureFile{strings.ReplaceAll(source["go.mod"].data, "=> ./dep", "=> ./alternate"), "100644"}
	regenerated := withVendor(t, replace)
	if regenerated["vendor/fixture.invalid/dep/value.go"] == clean["vendor/fixture.invalid/dep/value.go"] {
		t.Fatal("replace did not change generated vendor")
	}
	check(fixtureCommit(t, repo, regenerated, base, "replace plant"), base, "source_tree_drift: go.mod")
	sibling := fixtureCommit(t, repo, clean, base, "unretained sibling")
	check(sibling, base, "build_pin_not_on_branch")
	detachedMain := fixtureCommit(t, repo, source, "", "unreachable baseline")
	check(pin, detachedMain, "main_commit_not_reachable")
	// A clean-looking filesystem cannot repair a bad committed vendor file.
	bad := cloneFiles(clean)
	item := bad["vendor/modules.txt"]
	item.data += "# lie\n"
	bad["vendor/modules.txt"] = item
	badPin := fixtureCommit(t, repo, bad, pin, "committed vendor plant")
	fixtureGit(t, repo, "", "update-ref", buildRef, badPin)
	if err := os.MkdirAll(filepath.Join(repo, "vendor"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "vendor/modules.txt"), []byte(clean["vendor/modules.txt"].data), 0o644); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, repo, "", "update-ref", "refs/heads/visible", badPin)
	fixtureGit(t, repo, "", "symbolic-ref", "HEAD", "refs/heads/visible")
	check(badPin, base, "vendor_tree_drift: modules.txt")
	check(pin, base, "") // An older retained pin works even when HEAD differs.
	for _, kind := range []string{"missing", "executable"} {
		planted := cloneFiles(clean)
		path := "vendor/fixture.invalid/dep/receipt.lock"
		if kind == "missing" {
			delete(planted, path)
		} else {
			v := planted[path]
			v.mode = "100755"
			planted[path] = v
		}
		selected := fixtureCommit(t, repo, planted, pin, kind)
		fixtureGit(t, repo, "", "update-ref", buildRef, selected)
		check(selected, base, "vendor_tree_drift: fixture.invalid/dep/receipt.lock")
	}
	fixtureGit(t, repo, "", "update-ref", buildRef, pin)
	check(pin, base, "")
}

// Missing/ambiguous CLI selectors cannot accidentally verify the current tree.
func TestRunRefusesSelectors(t *testing.T) {
	for _, args := range [][]string{nil, {"--repository", "relative"}, {"--repository", t.TempDir(), "--commit", "HEAD", "--main-commit", "main"}} {
		if err := run(args, ioDiscard{}); err == nil {
			t.Fatal("accepted ambiguous selectors")
		}
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(p []byte) (int, error) { return len(p), nil }
