package darwinrelease

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Build only ordinary Git-written caches in isolated fixture execution. Split
// fixtures require two actual layers; alternate fixtures borrow the donor cache.
func releaseCacheFixture(t *testing.T, mode string, unsigned bool) (gitFixture, string, map[string][]byte) {
	t.Helper()
	f := newGitFixture(t)
	fixtureCommand(t, f.root, "git", "config", "core.commitGraph", "true")
	args := []string{"commit-graph", "write", "--reachable"}
	if mode == "split" {
		args = append(args, "--split=no-merge")
		fixtureCommand(t, f.root, "git", args...)
	}
	if unsigned {
		fixtureCommand(t, f.root, "git", "commit", "-q", "--no-gpg-sign", "--allow-empty", "-m", "unsigned intermediate")
	}
	fixtureCommand(t, f.root, "git", "commit", "-q", "-S", "--allow-empty", "-m", "approved descendant")
	fixtureCommand(t, f.root, "git", "tag", "-d", "v0.1.0")
	fixtureCommand(t, f.root, "git", "tag", "-s", "v0.1.0", "-m", "bot fixture tag")
	fixtureCommand(t, f.root, "git", args...)
	fixtureCommand(t, f.root, "git", "commit-graph", "verify")
	objects := fixtureCommand(t, f.root, "git", "rev-parse", "--path-format=absolute", "--git-path", "objects")
	cache := map[string][]byte{}
	remember := func(path string) []byte {
		t.Helper()
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() || st.Size() == 0 {
			t.Fatal("required ordinary cache file missing or invalid", path, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cache[path] = b
		return b
	}
	if mode == "split" {
		dir := filepath.Join(objects, "info", "commit-graphs")
		chain := remember(filepath.Join(dir, "commit-graph-chain"))
		layers := strings.Fields(string(chain))
		if len(layers) < 2 {
			t.Fatal("split fixture did not create multiple layers")
		}
		for _, hash := range layers {
			remember(filepath.Join(dir, "graph-"+hash+".graph"))
		}
	} else {
		remember(filepath.Join(objects, "info", "commit-graph"))
	}
	root := f.root
	switch mode {
	case "linked-common":
		root = filepath.Join(t.TempDir(), "linked")
		fixtureCommand(t, f.root, "git", "worktree", "add", "--detach", root, "HEAD")
		common := fixtureCommand(t, root, "git", "rev-parse", "--path-format=absolute", "--git-common-dir")
		if !releaseCacheStoresMatch(common, filepath.Dir(objects)) {
			t.Fatalf("linked fixture did not use the shared Git directory: %q versus %q", common, filepath.Dir(objects))
		}
	case "alternate":
		root = filepath.Join(t.TempDir(), "borrower")
		fixtureCommand(t, f.root, "git", "clone", "--shared", f.root, root)
		fixtureCommand(t, root, "git", "config", "core.commitGraph", "true")
		own := fixtureCommand(t, root, "git", "rev-parse", "--path-format=absolute", "--git-path", "objects")
		alternate := remember(filepath.Join(own, "info", "alternates"))
		referenced := releaseAlternateStorePath(own, string(alternate))
		if !releaseCacheStoresMatch(referenced, objects) {
			t.Fatalf("alternate fixture did not borrow the cache object store: %q versus %q", referenced, objects)
		}
		for _, path := range []string{filepath.Join(own, "info", "commit-graph"), filepath.Join(own, "info", "commit-graphs")} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Fatal("alternate fixture unexpectedly has a local graph", path, err)
			}
		}
	}
	if got := fixtureCommand(t, root, "git", "config", "--type=bool", "--get", "core.commitGraph"); got != "true" {
		t.Fatal("repository cache-reading request missing", got)
	}
	return f, root, cache
}

func assertReleaseCacheUnchanged(t *testing.T, cache map[string][]byte) {
	t.Helper()
	for path, before := range cache {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("admission removed or changed required cache bytes", path, err)
		}
	}
}

// Actual controlled Git reports its effective reader policy despite repository
// and inherited overrides. Ordinary valid signed cache-bearing histories ACCEPT;
// unsigned intermediates refuse. These controls do not reproduce a forged-cache
// bypass: configuration isolation and ordinary-cache admission are distinct claims.
func TestReleaseGitCacheIsolation(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.commitGraph")
	t.Setenv("GIT_CONFIG_VALUE_0", "true")
	t.Setenv("GIT_CONFIG_PARAMETERS", `'core.commitGraph'='true'`)
	t.Setenv("GIT_TEST_COMMIT_GRAPH", "1")
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "not-a-repository"))
	for _, mode := range []string{"ordinary", "split", "linked-common", "alternate"} {
		t.Run(mode, func(t *testing.T) {
			f, root, cache := releaseCacheFixture(t, mode, false)
			t.Run("valid-signed", func(t *testing.T) {
				if _, err := verifyGit(root, "v0.1.0", "0.1.0", "", f.p, localGit(root)); err != nil {
					t.Fatal("valid signed cache-bearing history must ACCEPT", err)
				}
				assertReleaseCacheUnchanged(t, cache)
			})
			t.Run("reader-policy", func(t *testing.T) {
				g := localGit(root)
				got, err := g("config", "--type=bool", "--get", "core.commitGraph")
				if err != nil {
					t.Fatal("effective cache policy query failed", err)
				}
				if got != "false" {
					t.Fatalf("cache reader policy wanted false, got %s", got)
				}
				// Explicitly check the existing environment filter for the test override,
				// which Git can otherwise use independently of core.commitGraph.
				for _, entry := range cleanEnvironment() {
					if strings.HasPrefix(entry, "GIT_") {
						t.Fatal("inherited Git override survived filtering", entry)
					}
				}
				// A malformed inherited test override must not reach real history reads.
				t.Setenv("GIT_TEST_COMMIT_GRAPH", "invalid-boolean")
				got, err = g("rev-list", "--count", "HEAD")
				if err != nil || got != "2" {
					t.Fatal("controlled history read retained an inherited override", got, err)
				}
				assertReleaseCacheUnchanged(t, cache)
			})
			t.Run("unsigned-intermediate", func(t *testing.T) {
				negative, negativeRoot, negativeCache := releaseCacheFixture(t, mode, true)
				if _, err := verifyGit(negativeRoot, "v0.1.0", "0.1.0", "", negative.p, localGit(negativeRoot)); err == nil || err.Error() != "commit-signer" {
					t.Fatal("ordinary cache hid an unsigned intermediate", err)
				}
				assertReleaseCacheUnchanged(t, negativeCache)
			})
			assertReleaseCacheUnchanged(t, cache)
		})
	}
}

// Real directory identity accepts symlink aliases and refuses stat errors,
// non-directories and different existing stores. This protects fixture
// portability, not a malicious cache or admission-bypass demonstration.
func TestReleaseCacheStoreIdentity(t *testing.T) {
	root := t.TempDir()
	expected := filepath.Join(root, "store")
	other := filepath.Join(root, "other-store")
	for _, path := range []string{expected, other} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(expected, alias); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(root, "missing")
	dangling := filepath.Join(root, "dangling")
	if err := os.Symlink(missing, dangling); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "regular-file")
	if err := os.WriteFile(file, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, left, right string
		want              bool
	}{
		{"ordinary", expected, expected, true},
		{"symlink-alias", alias, expected, true},
		{"symlink-expected", expected, alias, true},
		{"different-existing", other, expected, false},
		{"missing", missing, expected, false},
		{"dangling", dangling, expected, false},
		{"missing-expected", expected, missing, false},
		{"regular-file", file, expected, false},
		{"regular-file-expected", expected, file, false},
		{"same-regular-file", file, file, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := releaseCacheStoresMatch(c.left, c.right); got != c.want {
				t.Fatalf("directory identity for %q and %q: wanted %t, got %t", c.left, c.right, c.want, got)
			}
		})
	}
	// Git resolves relative alternates from the object directory, not the cwd
	// or the info directory containing the alternates file.
	objects := filepath.Join(root, "objects")
	if err := os.Mkdir(objects, 0700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, record string
		want         bool
	}{
		{"absolute-record", expected, true},
		{"relative-record", "../store", true},
		{"relative-alias", "../alias", true},
		{"relative-different", "../other-store", false},
		{"relative-missing", "../missing", false},
		{"relative-file", "../regular-file", false},
		{"empty-record", "", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			resolved := releaseAlternateStorePath(objects, c.record)
			if got := releaseCacheStoresMatch(resolved, expected); got != c.want {
				t.Fatalf("alternate record %q resolved to %q: wanted %t, got %t", c.record, resolved, c.want, got)
			}
		})
	}
}

// Fixture paths must identify the same existing directory, not merely share
// spelling or a platform-specific prefix. Stat follows ordinary symlink aliases.
func releaseCacheStoresMatch(left, right string) bool {
	a, err := os.Stat(left)
	if err != nil || !a.IsDir() {
		return false
	}
	b, err := os.Stat(right)
	return err == nil && b.IsDir() && os.SameFile(a, b)
}

func releaseAlternateStorePath(objects, record string) string {
	path := strings.TrimSpace(record)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(objects, path)
}
