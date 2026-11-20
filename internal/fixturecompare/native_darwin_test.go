//go:build darwin && arm64 && cgo

package fixturecompare

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func nativeCommand(t *testing.T, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if b, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput(); err != nil {
		t.Fatalf("fixture command %s: %v: %s", args[0], err, b)
	}
}

func nativePair(t *testing.T, variant string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(t.TempDir(), "main.c")
	if err := os.WriteFile(src, []byte("int main(void) { return "+variant+"; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var pins strings.Builder
	for _, name := range names {
		path := filepath.Join(dir, name)
		nativeCommand(t, "/usr/bin/xcrun", "clang", "-target", "arm64-apple-macos13.0", src, "-o", path)
		// "-" is ad-hoc signing, with no real identity or Keychain operation.
		nativeCommand(t, "/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "fixture."+name, "--options", "runtime", "--requirements", "=designated => identifier \"fixture."+name+"\"", path)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&pins, "%s  %s\n", hash(b), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(pins.String()), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// Native strict checking accepts real ad-hoc fixture images, retains findings
// and compares complete compiled requirements. No Developer ID, release,
// Keychain or installed-access claim follows from these self-created images.
func TestNativeFixtureComparison(t *testing.T) {
	old, next := nativePair(t, "0"), nativePair(t, "1")
	before := [2]map[string]fileWitness{witness(t, old), witness(t, next)}
	out := filepath.Join(t.TempDir(), "evidence")
	report, err := compare(old, next, out, inspect)
	if err != nil || !report.Completed || report.ReleaseVerified || len(findings(report, "timestamp-missing")) != 4 ||
		len(findings(report, "ad-hoc-companion")) != 2 || len(findings(report, "requirements-differ")) != 0 {
		t.Fatalf("native comparison: %+v %v", report, err)
	}
	for side, dir := range []string{old, next} {
		assertWitness(t, dir, before[side])
	}
	for _, pair := range report.Pairs {
		for _, id := range pair {
			if !strings.Contains(id.DR, "identifier") || !id.StrictSignatureVerified {
				t.Fatalf("actual DR missing: %+v", id)
			}
		}
	}
	path := filepath.Join(next, names[1])
	nativeCommand(t, "/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "fixture."+names[1], "--options", "runtime", "--requirements",
		"=designated => identifier \"fixture.a2a-keychain-status\" and true", path)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pins, _ := os.ReadFile(filepath.Join(next, "SHA256SUMS"))
	lines := strings.Split(string(pins), "\n")
	lines[1] = hash(b) + "  " + names[1]
	if err := os.WriteFile(filepath.Join(next, "SHA256SUMS"), []byte(strings.Join(lines, "\n")), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = compare(old, next, filepath.Join(t.TempDir(), "changed"), inspect)
	diff := findings(report, "requirements-differ")
	if err != nil || len(diff) != 1 || diff[0].Old == nil || diff[0].New == nil || diff[0].Old.CompiledDR == diff[0].New.CompiledDR {
		t.Fatalf("native DR delta absent: %+v %v", report, err)
	}
}

// Changing a signed instruction with updated pins must still fail native strict
// validation. A hash pass is not a signature pass.
func TestNativeStrictCorruption(t *testing.T) {
	dir := nativePair(t, "0")
	path := filepath.Join(dir, names[0])
	if _, err := inspect(path); err != nil {
		t.Fatal("positive signature control", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte{0xc0, 0x03, 0x5f, 0xd6} // arm64 RET in signed __TEXT
	offset := bytes.Index(b, needle)
	if offset < 0 {
		t.Fatal("fixture RET instruction not found")
	}
	b[offset] ^= 1
	if err := os.WriteFile(path, b, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := inspect(path); err == nil || err.Error() != "fixture-strict-signature" {
		t.Fatal("corrupt signature admitted", err)
	}
	var pins strings.Builder
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&pins, "%s  %s\n", hash(data), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(pins.String()), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := compare(dir, dir, filepath.Join(t.TempDir(), "evidence"), inspect)
	if err == nil || !strings.Contains(err.Error(), "fixture-strict-signature") || report.Completed {
		t.Fatalf("re-pinned corrupt image admitted: %+v %v", report, err)
	}
}

// An unsigned image is not a timestamp finding: strict validation must fail.
// Unsupported architectures are refused before signature inspection.
func TestNativeUnsignedAndArchitecture(t *testing.T) {
	dir := nativePair(t, "0")
	path := filepath.Join(dir, names[0])
	nativeCommand(t, "/usr/bin/codesign", "--remove-signature", path)
	if _, err := inspect(path); err == nil || err.Error() != "fixture-strict-signature" {
		t.Fatal("unsigned image admitted", err)
	}
	src := filepath.Join(t.TempDir(), "main.c")
	if err := os.WriteFile(src, []byte("int main(void) { return 0; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(t.TempDir(), "wrong-arch")
	nativeCommand(t, "/usr/bin/xcrun", "clang", "-target", "x86_64-apple-macos13.0", src, "-o", wrong)
	if _, err := inspect(wrong); err == nil || err.Error() != "fixture-darwin-arm64-executable-required" {
		t.Fatal("wrong architecture admitted", err)
	}
}
