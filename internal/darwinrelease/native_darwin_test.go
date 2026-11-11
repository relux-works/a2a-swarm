//go:build darwin && arm64 && cgo

package darwinrelease

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Native Security.framework compilation compares the COMPLETE approved AST.
// These controls prove syntax and sensitivity to removed/replaced terms, never
// Developer ID validity, native Keychain access or cross-version no-UI behavior.
func TestNativeRequirementControls(t *testing.T) {
	for _, name := range Binaries {
		t.Run(name, func(t *testing.T) {
			expr, e := Requirement(name)
			if e != nil {
				t.Fatal(e)
			}
			valid, e := CompileRequirement(expr)
			if e != nil || len(valid) == 0 {
				t.Fatal("approved syntax", e)
			}
			again, e := CompileRequirement(expr)
			if e != nil || !bytes.Equal(valid, again) {
				t.Fatal("compiled requirement not stable", e)
			}
			variants := []string{
				strings.Replace(expr, "anchor apple generic", "true", 1),
				strings.Replace(expr, `identifier "works.relux.`+name+`"`, `identifier "works.relux.other"`, 1),
				strings.Replace(expr, ` and certificate 1[field.1.2.840.113635.100.6.2.6] exists`, "", 1),
				strings.Replace(expr, ` and certificate leaf[field.1.2.840.113635.100.6.1.13] exists`, "", 1),
				strings.Replace(expr, `262RZ595FP`, `0000000000`, 1),
				expr + ` and cdhash H"0000000000000000000000000000000000000000"`,
			}
			for _, bad := range variants {
				compiled, e := CompileRequirement(bad)
				if e != nil {
					t.Fatal("control must be valid requirement syntax", e)
				}
				if e := matchCompiledRequirement(valid, compiled); e == nil || e.Error() != "full-designated-requirement-mismatch" {
					t.Fatal("removed/replaced full-DR term undetected", e)
				}
			}
			if _, e = CompileRequirement("anchor ("); e == nil || e.Error() != "requirement-compile" {
				t.Fatal("invalid requirement accepted", e)
			}
		})
	}
}

// Real self-created Mach-O/ad-hoc images are refused by the native policy;
// there is no fixture Developer ID certificate and no Keychain/notary operation.
func TestNativeAdHocAndArchitectureRefusal(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.c")
	if e := os.WriteFile(src, []byte("int main(void) { return 0; }\n"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, arch := range []string{"arm64", "x86_64"} {
		t.Run(arch, func(t *testing.T) {
			bin := filepath.Join(dir, "control-"+arch)
			fixtureCommand(t, dir, "/usr/bin/xcrun", "clang", "-target", arch+"-apple-macos13.0", src, "-o", bin)
			fixtureCommand(t, dir, "/usr/bin/codesign", "--force", "--sign", "-", "--identifier", "works.relux.a2a", "--options", "runtime", bin)
			fixtureCommand(t, dir, "/usr/bin/codesign", "--verify", "--strict", bin)
			_, e := VerifyBinary(bin, "a2a", t.TempDir())
			want := "ad-hoc-signature"
			if arch != "arm64" {
				want = "darwin-arm64-executable-required"
			}
			if e == nil || e.Error() != want {
				t.Fatalf("wanted %s, got %v", want, e)
			}
		})
	}
	if _, e := VerifyBinary(filepath.Join(dir, "missing"), "a2a-keychain-status", t.TempDir()); e == nil || e.Error() != "companion-or-cli-missing" {
		t.Fatal("missing companion admitted", e)
	}
}
