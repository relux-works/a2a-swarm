//go:build darwin && arm64 && cgo

package darwinrelease

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fixtureArm is one fresh fixture keychain for one release-path arm.
type fixtureArm struct {
	signer *signingKeychain
	dir    string
}

func newFixtureArm(t *testing.T, values map[string]string) fixtureArm {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	signer, err := openSigningKeychainWithPassword(filepath.Join(dir, "release-path.keychain-db"), values, hex.EncodeToString(random))
	if err != nil {
		t.Fatalf("SETUP: fixture import through the release path failed: %v", err)
	}
	t.Cleanup(func() {
		if err := signer.close(); err != nil {
			t.Errorf("CLEANUP: fixture keychain: %v", err)
		}
	})
	return fixtureArm{signer: signer, dir: dir}
}

func fixtureValues(t *testing.T, dir string) map[string]string {
	t.Helper()
	p12, err := os.ReadFile(filepath.Join(dir, "identity.p12"))
	if err != nil {
		t.Fatal(err)
	}
	pass, err := os.ReadFile(filepath.Join(dir, "identity.pass"))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]string{"MACOS_SIGN_P12": base64.StdEncoding.EncodeToString(p12), "MACOS_SIGN_P12_PASSWORD": strings.TrimSpace(string(pass))}
}

func fixtureExecutable(t *testing.T, dir, name string) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "main.c")
	if err := os.WriteFile(src, []byte("int main(void){return 0;}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, name)
	if s := run(t, 120*time.Second, "/usr/bin/xcrun", "clang", "-arch", "arm64", "-o", out, src); !s.ok() {
		t.Fatalf("SETUP: fixture executable %s: %s", name, s)
	}
	return out
}

func fixtureRequirement(name, selector string) string {
	return `identifier "works.relux.` + name + `" and certificate leaf = H"` + selector + `"`
}

func readList(t *testing.T) []string {
	t.Helper()
	l, err := nativeSearchList{}.Get()
	if err != nil {
		t.Fatalf("read the user search list: %v", err)
	}
	return l
}

// Proves the PRODUCTION signing boundary on a throwaway self-signed identity:
// signOne with the production nativeSearchList and signArgs.
//   - P1/P2: each name in Binaries signs and strictly verifies, with the
//     observed install equal to [keychain]+original and the original list
//     restored.
//   - P3: a missing target, after an observed install, is refused as exactly
//     target-missing with the list restored. The exit code is recorded, not
//     pinned (calibration).
//   - P4: the guard bypassed, with the keychain's absence observed, is refused
//     identity-not-found: the H6 witness on the same identity and argv.
//   - P5: an identity the keychain does not hold, after an observed install,
//     is refused identity-not-found with the list restored.
//
// RELEASE-PATH=PASS only if every arm holds. It is fixture-only evidence,
// separate from the H6/H1 discriminator; it says nothing about a Developer ID
// chain or a real release.
func TestReleasePathSigning(t *testing.T) {
	dir := os.Getenv(signingControlDirEnv)
	if dir == "" {
		t.Skip(signingControlDirEnv + " not set: no fixture identity")
	}
	values := fixtureValues(t, dir)
	ctx := context.Background()
	var failures []string
	fail := func(arm, why string) {
		failures = append(failures, arm+": "+why)
		t.Logf("RELEASE-PATH arm=%s FAIL %s", arm, why)
	}
	installedOK := func(arm string, seen []string, kc string, original []string) bool {
		if !sameList(seen, append([]string{kc}, original...)) {
			fail(arm, "install not observed as [keychain]+original")
			return false
		}
		return true
	}
	restoredOK := func(arm string, original []string) bool {
		if !sameList(readList(t), original) {
			fail(arm, "user search list not restored")
			return false
		}
		return true
	}

	for _, name := range Binaries { // P1, P2
		arm := "release-path/" + name
		f := newFixtureArm(t, values)
		target := fixtureExecutable(t, f.dir, name)
		expr := fixtureRequirement(name, f.signer.selector)
		original := readList(t)
		var seen []string
		st := signOne(ctx, nativeSearchList{}, f.signer.selector, f.signer.path, name, expr, target, f.dir, 90*time.Second, func(l []string) { seen = l })
		t.Logf("RELEASE-PATH arm=%s %s", arm, st.message("sign", name))
		if !st.ok() {
			fail(arm, "sign not ok")
		}
		installedOK(arm, seen, f.signer.path, original)
		if v := executeClassified(ctx, 60*time.Second, f.dir, "/usr/bin/codesign", "--verify", "--strict", "--test-requirement", "="+expr, target); v.State != "ran" || v.Exit != 0 {
			fail(arm, "verify "+v.message("verify", name))
		}
		restoredOK(arm, original)
	}

	{ // P3
		arm, name := "release-path/missing-target", Binaries[0]
		f := newFixtureArm(t, values)
		original := readList(t)
		var seen []string
		st := signOne(ctx, nativeSearchList{}, f.signer.selector, f.signer.path, name, fixtureRequirement(name, f.signer.selector), filepath.Join(f.dir, "absent"), f.dir, 90*time.Second, func(l []string) { seen = l })
		t.Logf("RELEASE-PATH arm=%s %s", arm, st.message("sign", name))
		if st.List != "ok" || st.State != "ran" || st.Exit == 0 || st.Class != "target-missing" {
			fail(arm, "not refused as exactly target-missing")
		}
		installedOK(arm, seen, f.signer.path, original)
		restoredOK(arm, original)
	}

	{ // P4
		arm, name := "bypass/direct", Binaries[0]
		f := newFixtureArm(t, values)
		target := fixtureExecutable(t, f.dir, name)
		before := readList(t)
		if slices.Contains(before, f.signer.path) {
			fail(arm, "keychain absence not observed")
		}
		st := executeClassified(ctx, 90*time.Second, f.dir, "/usr/bin/codesign", signArgs(f.signer.selector, f.signer.path, name, fixtureRequirement(name, f.signer.selector), target)...)
		t.Logf("RELEASE-PATH arm=%s %s", arm, st.message("sign", name))
		if st.State != "ran" || st.Exit == 0 || st.Class != "identity-not-found" {
			fail(arm, "not refused as identity-not-found")
		}
		restoredOK(arm, before)
	}

	{ // P5
		arm, name := "release-path/unavailable-identity", Binaries[0]
		f := newFixtureArm(t, values)
		target := fixtureExecutable(t, f.dir, name)
		other := make([]byte, 20)
		if _, err := rand.Read(other); err != nil {
			t.Fatal(err)
		}
		selector := hex.EncodeToString(other)
		original := readList(t)
		var seen []string
		st := signOne(ctx, nativeSearchList{}, selector, f.signer.path, name, fixtureRequirement(name, selector), target, f.dir, 90*time.Second, func(l []string) { seen = l })
		t.Logf("RELEASE-PATH arm=%s %s", arm, st.message("sign", name))
		if st.List != "ok" || st.State != "ran" || st.Exit == 0 || st.Class != "identity-not-found" {
			fail(arm, "not refused as identity-not-found")
		}
		installedOK(arm, seen, f.signer.path, original)
		restoredOK(arm, original)
	}

	if len(failures) > 0 {
		t.Fatalf("RELEASE-PATH=FAIL: %s", strings.Join(failures, "; "))
	}
	t.Logf("RELEASE-PATH=PASS")
}
