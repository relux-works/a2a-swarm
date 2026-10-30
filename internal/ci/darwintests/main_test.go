package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// writeModule creates a throwaway module from relative path -> content and
// isolates go list from any workspace or flags of the caller.
func writeModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/fixture\n\ngo 1.25\n"
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	return dir
}

const testImport = "import \"testing\"\n"

// Proves that the selection holds exactly the tests of test files built for
// darwin and not for linux: by _darwin suffix and by build constraint, in the
// internal and the external test package. Portable, linux-only, unix and cgo
// files, helpers, methods, non-test names and other packages stay out.
func TestSelectTestsDarwinOnly(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":           "package p\n",
		"p/common_test.go": "package p\n" + testImport + "func TestCommon(t *testing.T) {}\n",
		"p/suffix_darwin_test.go": "package p\n" + testImport +
			"func TestBySuffix(t *testing.T) {}\n" +
			"func Testlower(t *testing.T) {}\n" +
			"func helperTest(t *testing.T) {}\n" +
			"func TestNoParam() {}\n" +
			"type s struct{}\n" +
			"func (s) TestMethod(t *testing.T) {}\n" +
			"func BenchmarkX(b *testing.B) {}\n",
		"p/tag_test.go":        "//go:build darwin\n\npackage p\n" + testImport + "func TestByTag(t *testing.T) {}\nfunc Test(t *testing.T) {}\n",
		"p/linux_test.go":      "//go:build linux\n\npackage p\n" + testImport + "func TestLinuxOnly(t *testing.T) {}\n",
		"p/unix_test.go":       "//go:build unix\n\npackage p\n" + testImport + "func TestUnix(t *testing.T) {}\n",
		"p/cgo_test.go":        "//go:build cgo\n\npackage p\n" + testImport + "func TestCgo(t *testing.T) {}\n",
		"p/ext_darwin_test.go": "package p_test\n" + testImport + "func TestExternal(t *testing.T) {}\n",
		"q/q.go":               "package q\n",
		"q/q_test.go":          "package q\n" + testImport + "func TestQ(t *testing.T) {}\n",
	})
	plan, err := selectTests(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	want := []pkgTests{{
		ImportPath: "example.com/fixture/p",
		Files:      []string{"ext_darwin_test.go", "suffix_darwin_test.go", "tag_test.go"},
		Tests:      []string{"Test", "TestBySuffix", "TestByTag", "TestExternal"},
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %+v, want %+v", plan, want)
	}
}

// Proves that a selection with no Darwin-only test is refused, so a broken
// selector cannot pass a lane that ran nothing. Includes a Darwin-only file
// without any test function.
func TestSelectTestsRefusesEmpty(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":                  "package p\n",
		"p/common_test.go":        "package p\n" + testImport + "func TestCommon(t *testing.T) {}\n",
		"p/linux_test.go":         "//go:build linux\n\npackage p\n" + testImport + "func TestLinuxOnly(t *testing.T) {}\n",
		"p/helper_darwin_test.go": "package p\n" + testImport + "func helper(t *testing.T) {}\n",
	})
	plan, err := selectTests(dir, "./...")
	if err == nil || !strings.Contains(err.Error(), "no Darwin-only tests") {
		t.Fatalf("selectTests = %+v, %v; want the empty-selection refusal", plan, err)
	}
}

// Proves that a package go list cannot load fails the selection instead of
// being dropped silently.
func TestSelectTestsRefusesLoadError(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":             "package p\n",
		"p/other.go":         "package other\n",
		"p/p_darwin_test.go": "package p\n" + testImport + "func TestP(t *testing.T) {}\n",
	})
	plan, err := selectTests(dir, "./...")
	if err == nil || !strings.Contains(err.Error(), "example.com/fixture/p") {
		t.Fatalf("selectTests = %+v, %v; want a load error naming the package", plan, err)
	}
}

// Proves that the -run pattern matches each selected name exactly: no prefix,
// suffix or unselected name.
func TestRunPatternAnchored(t *testing.T) {
	re := regexp.MustCompile(pkgTests{Tests: []string{"TestA", "TestB"}}.runPattern())
	for name, want := range map[string]bool{"TestA": true, "TestB": true, "TestAB": false, "XTestA": false, "TestC": false} {
		if got := re.MatchString(name); got != want {
			t.Errorf("match %q = %v, want %v", name, got, want)
		}
	}
}
