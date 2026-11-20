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
	return nativePairRuntime(t, variant, true)
}

func nativePairRuntime(t *testing.T, variant string, runtime bool) string {
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
		// "-" is ad-hoc signing, with no real identity or Keychain item access.
		args := []string{"/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "fixture." + name}
		if runtime {
			args = append(args, "--options", "runtime")
		}
		args = append(args, "--requirements", "=designated => identifier \"fixture."+name+"\"", path)
		nativeCommand(t, args...)
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
		len(findings(report, "ad-hoc-companion")) != 2 || len(findings(report, "ad-hoc-signature")) != 2 ||
		len(findings(report, "hardened-runtime-missing")) != 0 || len(findings(report, "requirements-differ")) != 0 {
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
	// Each direction has its own equal-requirement control before adding a
	// real clause to only that side. Report identities must retain side order.
	for side, dir := range []string{next, old} {
		t.Run("extra-clause-in-"+[]string{"NEXT", "N"}[side], func(t *testing.T) {
			control, err := compare(old, next, filepath.Join(t.TempDir(), "equal"), inspect)
			if err != nil || !control.Completed || len(findings(control, "requirements-differ")) != 0 {
				t.Fatalf("equal native DR control: %+v %v", control, err)
			}
			path := filepath.Join(dir, names[1])
			nativeCommand(t, "/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "fixture."+names[1], "--options", "runtime", "--requirements",
				"=designated => identifier \"fixture.a2a-keychain-status\" and true", path)
			nativeRepin(t, dir)
			report, err := compare(old, next, filepath.Join(t.TempDir(), "changed"), inspect)
			diff := findings(report, "requirements-differ")
			if err != nil || !report.Completed || len(diff) != 1 || diff[0].Name != names[1] || diff[0].Old == nil || diff[0].New == nil ||
				diff[0].Old.CompiledDR != report.Pairs[0][1].CompiledDR || diff[0].New.CompiledDR != report.Pairs[1][1].CompiledDR ||
				diff[0].Old.CompiledDR == diff[0].New.CompiledDR {
				t.Fatalf("native DR delta absent or sides reversed: %+v %v", report, err)
			}
			nativeCommand(t, "/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "fixture."+names[1], "--options", "runtime", "--requirements",
				"=designated => identifier \"fixture.a2a-keychain-status\"", path)
			nativeRepin(t, dir)
		})
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

func nativeRepin(t *testing.T, dir string) {
	t.Helper()
	var pins strings.Builder
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&pins, "%s  %s\n", hash(b), name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(pins.String()), 0600); err != nil {
		t.Fatal(err)
	}
}

// The specimen must distinguish default validation from strict validation on
// this platform before it can prove that the comparator enforces strictness.
func TestNativeStrictOnlyRefusal(t *testing.T) {
	dir := nativePair(t, "0")
	path := filepath.Join(dir, names[0])
	for _, strict := range []bool{false, true} {
		args := []string{"/usr/bin/codesign", "--verify"}
		if strict {
			args = append(args, "--strict")
		}
		nativeCommand(t, append(args, path)...)
	}
	positive, err := compare(dir, dir, filepath.Join(t.TempDir(), "valid"), inspect)
	if err != nil || !positive.Completed {
		t.Fatalf("valid strict signature control: %+v %v", positive, err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.Write([]byte("unsigned trailing bytes"))
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		t.Fatalf("append fixture: %v %v", writeErr, closeErr)
	}
	nativeRepin(t, dir)
	// This positive default check prevents a generic page-hash failure from
	// masquerading as coverage of the stricter structural signature predicate.
	nativeCommand(t, "/usr/bin/codesign", "--verify", path)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", path).CombinedOutput()
	if ctx.Err() != nil {
		t.Fatal(ctx.Err())
	}
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() == 0 {
		t.Fatalf("specimen did not separate strict from default validation: %v %s", err, b)
	}
	if _, err := inspect(path); err == nil || err.Error() != "fixture-strict-signature" {
		t.Fatal("strict-only defect admitted by native inspection", err)
	}
	report, err := compare(dir, dir, filepath.Join(t.TempDir(), "refused"), inspect)
	if err == nil || !strings.Contains(err.Error(), "fixture-strict-signature") || report.Completed {
		t.Fatalf("default-valid strict-invalid image admitted: %+v %v", report, err)
	}
}

// Actual native flag extraction must distinguish runtime-signed fixtures from
// otherwise-valid ad-hoc fixtures without runtime, retaining all four findings.
func TestNativeRuntimeFindings(t *testing.T) {
	for _, runtime := range []bool{true, false} {
		t.Run(fmt.Sprint(runtime), func(t *testing.T) {
			dir := nativePairRuntime(t, "0", runtime)
			report, err := compare(dir, dir, filepath.Join(t.TempDir(), "evidence"), inspect)
			want := 4
			if runtime {
				want = 0
			}
			if err != nil || !report.Completed || len(findings(report, "hardened-runtime-missing")) != want {
				t.Fatalf("native runtime mapping: %+v %v", report, err)
			}
		})
	}
}
