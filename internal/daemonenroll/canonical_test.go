//go:build linux || darwin

package daemonenroll

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCanonicalParentAliasFixedOnce(t *testing.T) {
	root := canonicalHome(t)
	home := filepath.Join(root, "home")
	other := filepath.Join(root, "other")
	alias := filepath.Join(root, "alias")
	for _, path := range []string{home, other} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(alias, ".a2a")
	// Retarget the original alias after staging. Publication must stay on the
	// canonical home pinned at entry, without silently resolving the alias again.
	err := install(dir, strings.NewReader(fixture("canonical", 61)), false, func(phase string, _ *os.File) error {
		if phase != "staged" {
			return nil
		}
		if err := os.Remove(alias); err != nil {
			return err
		}
		return os.Symlink(other, alias)
	})
	if err != nil {
		t.Fatal("canonicalized enrollment failed")
	}
	checkGeneration(t, filepath.Join(home, ".a2a"), "canonical", 61)
	if _, err := os.Lstat(filepath.Join(other, ".a2a")); !os.IsNotExist(err) {
		t.Fatal("alias was resolved again")
	}
	// Readers using the fixed canonical path refuse a changed canonical inode.
	chain, err := openParent(home)
	if err != nil {
		t.Fatal(err)
	}
	defer chain.close()
	if err := os.Rename(home, home+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if chain.valid() {
		t.Fatal("changed canonical home was accepted")
	}
}

func TestCanonicalUnsafeAncestor(t *testing.T) {
	for _, mode := range []os.FileMode{0770, 0702} {
		t.Run(mode.String(), func(t *testing.T) {
			root := canonicalHome(t)
			home := filepath.Join(root, "home")
			alias := filepath.Join(canonicalHome(t), "alias")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(home, alias); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(root, mode); err != nil {
				t.Fatal(err)
			}
			defer os.Chmod(root, 0700)
			if Install(filepath.Join(alias, ".a2a"), strings.NewReader(fixture("unsafe", 62)), false) == nil {
				t.Fatal("writable canonical ancestor accepted")
			}
			if _, err := os.Lstat(filepath.Join(home, ".a2a")); !os.IsNotExist(err) {
				t.Fatal("refusal published enrollment")
			}
		})
	}
}

func TestCanonicalEntrySymlinksRefused(t *testing.T) {
	for _, name := range fileNames {
		t.Run(name, func(t *testing.T) {
			dir := target(t)
			if Install(dir, strings.NewReader(fixture("old", 63)), false) != nil {
				t.Fatal("setup")
			}
			path := filepath.Join(dir, name)
			if err := os.Rename(path, filepath.Join(filepath.Dir(dir), "original")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("../original", path); err != nil {
				t.Fatal(err)
			}
			if Install(dir, strings.NewReader(fixture("new", 64)), true) == nil {
				t.Fatal("entry symlink accepted")
			}
		})
	}
}

func TestCanonicalAncestorOwnershipAndStickyPolicy(t *testing.T) {
	caller := uint32(os.Geteuid())
	foreign := caller + 1
	if foreign == 0 {
		foreign++
	}
	cases := []struct {
		name        string
		st          unix.Stat_t
		final, want bool
	}{
		{"caller", unix.Stat_t{Uid: caller, Mode: unix.S_IFDIR | 0755}, false, true},
		{"root", unix.Stat_t{Uid: 0, Mode: unix.S_IFDIR | 0755}, false, true},
		{"root-sticky", unix.Stat_t{Uid: 0, Mode: unix.S_IFDIR | 01777}, false, true},
		{"root-writable", unix.Stat_t{Uid: 0, Mode: unix.S_IFDIR | 0777}, false, false},
		{"foreign", unix.Stat_t{Uid: foreign, Mode: unix.S_IFDIR | 0700}, false, false},
		{"foreign-sticky", unix.Stat_t{Uid: foreign, Mode: unix.S_IFDIR | 01777}, false, false},
		{"home", unix.Stat_t{Uid: caller, Mode: unix.S_IFDIR | 0700}, true, true},
		{"foreign-home", unix.Stat_t{Uid: foreign, Mode: unix.S_IFDIR | 0700}, true, false},
		{"root-home", unix.Stat_t{Uid: 0, Mode: unix.S_IFDIR | 0700}, true, caller == 0},
		{"caller-sticky", unix.Stat_t{Uid: caller, Mode: unix.S_IFDIR | 01777}, false, caller == 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeAncestorStat(&tc.st, tc.final); got != tc.want {
				t.Fatalf("ancestor policy = %v, want %v", got, tc.want)
			}
		})
	}
	// Foreign ownership uses the actual stat predicate without chown or privilege
	// changes. This does not qualify native access under another kernel UID.
}

func TestCanonicalRotationReaderPath(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "alias", true: "changed-canonical"}[changed], func(t *testing.T) {
			root := canonicalHome(t)
			home := filepath.Join(root, "home")
			if err := os.Mkdir(home, 0700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(home, ".a2a")
			if Install(dir, strings.NewReader(fixture("old", 65)), false) != nil {
				t.Fatal("setup")
			}
			alias := filepath.Join(root, "alias")
			if changed {
				if err := os.Rename(home, alias); err != nil {
					t.Fatal(err)
				}
				home, alias = alias, home
			}
			if err := os.Symlink(home, alias); err != nil {
				t.Fatal(err)
			}
			noncanonical := filepath.Join(alias, ".a2a")
			canonical := filepath.Join(home, ".a2a")
			if canonicalReaderPath(noncanonical) || canonicalReaderPath("relative/.a2a") {
				t.Fatal("noncanonical reader path accepted")
			}
			if !canonicalReaderPath(canonical) {
				t.Fatal("canonical reader path refused")
			}
			if Install(noncanonical, strings.NewReader(fixture("new", 66)), true) == nil {
				t.Fatal("rotation read a noncanonical directory")
			}
			checkGeneration(t, canonical, "old", 65)
			if Install(canonical, strings.NewReader(fixture("new", 66)), true) != nil {
				t.Fatal("canonical rotation refused")
			}
			checkGeneration(t, canonical, "new", 66)
		})
	}
}
