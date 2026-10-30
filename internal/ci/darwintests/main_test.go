package main

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The macOS runner and the Linux test job, as the CI workflow has them.
var (
	macRunner = platform{"darwin", "arm64"}
	linuxJob  = platform{"linux", "amd64"}
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

func testFile(header, pkg, body string) string {
	return header + "package " + pkg + "\n" + testImport + body
}

// Proves that the selection holds exactly the tests of test files built for
// the macOS runner and not for the Linux job. That covers the _darwin suffix,
// darwin, darwin && cgo and !linux constraints, an arm64-only portable file, and
// the internal and the external test package. Portable, linux-only, unix and
// cgo files, helpers, methods, lower-case names, a TestMain hook and other
// packages stay out.
func TestSelectTestsDarwinOnly(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":           "package p\n",
		"p/common_test.go": testFile("", "p", "func TestCommon(t *testing.T) {}\n"),
		"p/suffix_darwin_test.go": testFile("", "p",
			"func TestBySuffix(t *testing.T) {}\n"+
				"func Testlower(t *testing.T) {}\n"+
				"func helperTest(t *testing.T) {}\n"+
				"type s struct{}\n"+
				"func (s) TestMethod(t *testing.T) {}\n"+
				"func BenchmarkX(b *testing.B) {}\n"+
				"func TestMain(m *testing.M) { m.Run() }\n"),
		"p/tag_test.go":        testFile("//go:build darwin\n\n", "p", "func TestByTag(t *testing.T) {}\nfunc Test(t *testing.T) {}\n"),
		"p/darwincgo_test.go":  testFile("//go:build darwin && cgo\n\n", "p", "func TestDarwinCgo(t *testing.T) {}\n"),
		"p/notlinux_test.go":   testFile("//go:build !linux\n\n", "p", "func TestByNotLinux(t *testing.T) {}\n"),
		"p/arch_arm64_test.go": testFile("", "p", "func TestArm64Only(t *testing.T) {}\n"),
		"p/linux_test.go":      testFile("//go:build linux\n\n", "p", "func TestLinuxOnly(t *testing.T) {}\n"),
		"p/unix_test.go":       testFile("//go:build unix\n\n", "p", "func TestUnix(t *testing.T) {}\n"),
		"p/cgo_test.go":        testFile("//go:build cgo\n\n", "p", "func TestCgo(t *testing.T) {}\n"),
		"p/ext_darwin_test.go": testFile("", "p_test", "func TestExternal(t *testing.T) {}\n"),
		"q/q.go":               "package q\n",
		"q/q_test.go":          testFile("", "q", "func TestQ(t *testing.T) {}\n"),
	})
	plan, err := selectTests(dir, macRunner, linuxJob, "./...")
	if err != nil {
		t.Fatal(err)
	}
	want := []pkgTests{{
		ImportPath: "example.com/fixture/p",
		Files:      []string{"arch_arm64_test.go", "darwincgo_test.go", "ext_darwin_test.go", "notlinux_test.go", "suffix_darwin_test.go", "tag_test.go"},
		Tests:      []string{"Test", "TestArm64Only", "TestByNotLinux", "TestBySuffix", "TestByTag", "TestDarwinCgo", "TestExternal"},
	}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %+v, want %+v", plan, want)
	}
}

// Proves that a selection with no Darwin-only test is refused, so a broken
// selector cannot pass a lane that ran nothing. Covers a Darwin-only file with
// only a helper and one with only a TestMain hook.
func TestSelectTestsRefusesEmpty(t *testing.T) {
	for name, file := range map[string]string{
		"helper only":   testFile("", "p", "func helper(t *testing.T) {}\n"),
		"TestMain only": testFile("", "p", "func TestMain(m *testing.M) { m.Run() }\n"),
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeModule(t, map[string]string{
				"p/p.go":                "package p\n",
				"p/common_test.go":      testFile("", "p", "func TestCommon(t *testing.T) {}\n"),
				"p/only_darwin_test.go": file,
			})
			plan, err := selectTests(dir, macRunner, linuxJob, "./...")
			if err == nil || !strings.Contains(err.Error(), "no Darwin-only tests") {
				t.Fatalf("selectTests = %+v, %v; want the empty-selection refusal", plan, err)
			}
		})
	}
}

// Proves that TestMain taking *testing.T is selected as an ordinary test, as
// the go tool runs it, while the *testing.M hook is not.
func TestSelectTestsTestMainAsTest(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":                "package p\n",
		"p/main_darwin_test.go": testFile("", "p", "func TestMain(t *testing.T) {}\n"),
	})
	plan, err := selectTests(dir, macRunner, linuxJob, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !reflect.DeepEqual(plan[0].Tests, []string{"TestMain"}) {
		t.Fatalf("plan = %+v, want TestMain selected", plan)
	}
}

// Proves that an invalid test declaration in a Darwin-only file fails the
// selection, as the go tool would fail that package, even when another package
// is valid. Covers a missing parameter, a wrong parameter type, a result, a
// type parameter and a TestMain with neither *testing.M nor *testing.T.
func TestSelectTestsRefusesInvalidSignature(t *testing.T) {
	for name, decl := range map[string]string{
		"no parameter":   "func TestBroken() {}\n",
		"wrong type":     "func TestBroken(b *testing.B) {}\n",
		"result":         "func TestBroken(t *testing.T) error { return nil }\n",
		"type parameter": "func TestBroken[T any](t *testing.T) {}\n",
		"TestMain":       "func TestMain(b *testing.B) {}\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeModule(t, map[string]string{
				"p/p.go":                  "package p\n",
				"p/broken_darwin_test.go": testFile("", "p", decl),
				"q/q.go":                  "package q\n",
				"q/q_darwin_test.go":      testFile("", "q", "func TestQ(t *testing.T) {}\n"),
			})
			plan, err := selectTests(dir, macRunner, linuxJob, "./...")
			if err == nil || !strings.Contains(err.Error(), "wrong signature") {
				t.Fatalf("selectTests = %+v, %v; want a wrong-signature refusal", plan, err)
			}
		})
	}
}

// Proves that the go test loader runs over every package with runner-only test
// files, so two TestMain hooks in one runner test binary fail the selection
// even when that package has no test to select and another package is valid.
// Covers hooks in the internal and external Darwin-only files, and a hook in a
// shared file plus one in a Darwin-only file. A single hook is the control.
func TestSelectTestsRefusesTestMainTwice(t *testing.T) {
	hook := "func TestMain(m *testing.M) { m.Run() }\n"
	valid := map[string]string{
		"q/q.go":             "package q\n",
		"q/q_darwin_test.go": testFile("", "q", "func TestQ(t *testing.T) {}\n"),
		"p/p.go":             "package p\n",
	}
	for name, files := range map[string]map[string]string{
		"internal and external": {
			"p/in_darwin_test.go":  testFile("", "p", hook),
			"p/ext_darwin_test.go": testFile("", "p_test", hook),
		},
		"shared and darwin-only": {
			"p/main_test.go":       testFile("", "p", hook),
			"p/ext_darwin_test.go": testFile("", "p_test", hook),
		},
	} {
		t.Run(name, func(t *testing.T) {
			all := map[string]string{}
			for k, v := range valid {
				all[k] = v
			}
			for k, v := range files {
				all[k] = v
			}
			dir := writeModule(t, all)
			plan, err := selectTests(dir, macRunner, linuxJob, "./...")
			if err == nil || !strings.Contains(err.Error(), "multiple definitions of TestMain") {
				t.Fatalf("selectTests = %+v, %v; want the loader's TestMain refusal", plan, err)
			}
		})
	}
	t.Run("single hook control", func(t *testing.T) {
		all := map[string]string{"p/in_darwin_test.go": testFile("", "p", hook)}
		for k, v := range valid {
			all[k] = v
		}
		dir := writeModule(t, all)
		plan, err := selectTests(dir, macRunner, linuxJob, "./...")
		if err != nil || len(plan) != 1 || plan[0].ImportPath != "example.com/fixture/q" {
			t.Fatalf("selectTests = %+v, %v; want only package q selected", plan, err)
		}
	})
}

// Proves that a selected name also defined in a test file the Linux job runs
// is refused, because -run would run that portable test in the macOS lane too.
// The collision is checked across the internal and the external test package.
func TestSelectTestsRefusesPortableCollision(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":                "package p\n",
		"p/same_darwin_test.go": testFile("", "p", "func TestSame(t *testing.T) {}\n"),
		"p/common_test.go":      testFile("", "p_test", "func TestSame(t *testing.T) {}\n"),
	})
	plan, err := selectTests(dir, macRunner, linuxJob, "./...")
	if err == nil || !strings.Contains(err.Error(), "TestSame is defined in a Darwin-only file and in common_test.go") {
		t.Fatalf("selectTests = %+v, %v; want the collision refusal", plan, err)
	}
}

// Proves that one name defined in two Darwin-only origins, the internal and
// the external test package, is selected once, and -run then runs both.
func TestSelectTestsTwoDarwinOrigins(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":               "package p\n",
		"p/in_darwin_test.go":  testFile("", "p", "func TestSame(t *testing.T) {}\n"),
		"p/ext_darwin_test.go": testFile("", "p_test", "func TestSame(t *testing.T) {}\n"),
	})
	plan, err := selectTests(dir, macRunner, linuxJob, "./...")
	if err != nil {
		t.Fatal(err)
	}
	want := []pkgTests{{ImportPath: "example.com/fixture/p", Files: []string{"ext_darwin_test.go", "in_darwin_test.go"}, Tests: []string{"TestSame"}}}
	if !reflect.DeepEqual(plan, want) {
		t.Fatalf("plan = %+v, want %+v", plan, want)
	}
}

// Proves that a package go list cannot load fails the selection instead of
// being dropped silently.
func TestSelectTestsRefusesLoadError(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"p/p.go":             "package p\n",
		"p/other.go":         "package other\n",
		"p/p_darwin_test.go": testFile("", "p", "func TestP(t *testing.T) {}\n"),
	})
	plan, err := selectTests(dir, macRunner, linuxJob, "./...")
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

// Proves that the opt-in guard reports every entry whose name starts with
// A2A_, including names that are not shell identifiers and empty values, and
// returns names only. A multi-line value of another variable and look-alike
// names are not reported.
func TestOptInVariables(t *testing.T) {
	got := optInVariables([]string{
		"PATH=/usr/bin",
		"A2A_OBSERVER_DRY_RUN=1",
		"A2A_FOO-BAR=secret-value",
		"A2A_EMPTY=",
		"NOT_A2A_X=1",
		"XA2A_Y=2",
		"A2A=3",
		"MULTI=line one\nA2A_FAKE=x",
	})
	want := []string{"A2A_EMPTY", "A2A_FOO-BAR", "A2A_OBSERVER_DRY_RUN"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("optInVariables = %q, want %q", got, want)
	}
	if got := optInVariables([]string{"PATH=/usr/bin", "HOME=/tmp"}); len(got) != 0 {
		t.Fatalf("optInVariables on a clean environment = %q, want none", got)
	}
}
