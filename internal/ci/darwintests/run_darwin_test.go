package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// thisRunner is the platform these tests run on, the macOS runner itself.
var thisRunner = platform{"darwin", runtime.GOARCH}

// Proves that the lane turns red for a planted failing Darwin-only test and
// still runs the packages after it, and that a passing plan reports no
// failure. Darwin-only on purpose: on Linux the planted files are not compiled,
// which is the gap this lane closes, so the macOS lane selects this test too.
func TestRunTestsRedForFailingDarwinTest(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "b-ran")
	t.Setenv("DARWINTESTS_MARKER", marker)
	dir := writeModule(t, map[string]string{
		"a/a.go":                "package a\n",
		"a/fail_darwin_test.go": "package a\n" + testImport + "func TestPlantedFailure(t *testing.T) { t.Fatal(\"planted\") }\n",
		"b/b.go":                "package b\n",
		"b/pass_darwin_test.go": "package b\nimport (\n\t\"os\"\n\t\"testing\"\n)\n" +
			"func TestMarker(t *testing.T) {\n\tif err := os.WriteFile(os.Getenv(\"DARWINTESTS_MARKER\"), nil, 0o644); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n",
	})
	plan, compileOnly, err := selectTests(dir, thisRunner, linuxJob, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if failed := runTests(dir, thisRunner, plan, compileOnly, io.Discard, io.Discard); !reflect.DeepEqual(failed, []string{"example.com/fixture/a"}) {
		t.Fatalf("failed = %v, want only example.com/fixture/a", failed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("package b did not run after the red package: %v", err)
	}
	if failed := runTests(dir, thisRunner, plan[1:], nil, io.Discard, io.Discard); len(failed) != 0 {
		t.Fatalf("passing plan reported failures %v", failed)
	}
}

// Proves that a package whose Darwin-only test files hold no test, but do not
// compile, fails the lane through the compile-only step, while a compiling one
// passes. The go list loader does not type-check, so only a build finds this.
func TestRunTestsCompileOnlyFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		helper string
		want   []string
	}{
		"type error":       {"package p\n\nvar _ int = \"not an int\"\n", []string{"example.com/fixture/p"}},
		"compiles control": {"package p\n\nvar _ int = 1\n", nil},
	} {
		t.Run(name, func(t *testing.T) {
			dir := writeModule(t, map[string]string{
				"p/p.go":                  "package p\n",
				"p/helper_darwin_test.go": tc.helper,
				"q/q.go":                  "package q\n",
				"q/q_darwin_test.go":      testFile("", "q", "func TestQ(t *testing.T) {}\n"),
			})
			plan, compileOnly, err := selectTests(dir, thisRunner, linuxJob, "./...")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(compileOnly, []string{"example.com/fixture/p"}) {
				t.Fatalf("compileOnly = %v, want package p", compileOnly)
			}
			if failed := runTests(dir, thisRunner, plan, compileOnly, io.Discard, io.Discard); !reflect.DeepEqual(failed, tc.want) {
				t.Fatalf("failed = %v, want %v", failed, tc.want)
			}
		})
	}
}

// Proves that a package passes only when every selected test actually ran:
// go test exits 0 with "no tests to run" for a name it never built, and the
// lane must still turn red. A plan of the tests that exist is the control.
func TestRunTestsSelectedTestMustRun(t *testing.T) {
	dir := writeModule(t, map[string]string{
		"q/q.go":             "package q\n",
		"q/q_darwin_test.go": testFile("", "q", "func TestQ(t *testing.T) {}\n"),
	})
	missing := []pkgTests{{ImportPath: "example.com/fixture/q", Tests: []string{"TestMissing", "TestQ"}}}
	if failed := runTests(dir, thisRunner, missing, nil, io.Discard, io.Discard); !reflect.DeepEqual(failed, []string{"example.com/fixture/q"}) {
		t.Fatalf("failed = %v, want example.com/fixture/q for the test that never ran", failed)
	}
	present := []pkgTests{{ImportPath: "example.com/fixture/q", Tests: []string{"TestQ"}}}
	if failed := runTests(dir, thisRunner, present, nil, io.Discard, io.Discard); len(failed) != 0 {
		t.Fatalf("failed = %v for a plan whose tests all ran", failed)
	}
}

// Proves that the selected tests are built with the selection's target and
// cgo enabled even when the caller's environment disables cgo: a test file
// constrained to darwin && cgo is selected, runs and passes. Without the
// forced setting go test would not compile that file at all.
func TestRunTestsUsesSelectionTarget(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "cgo-ran")
	t.Setenv("DARWINTESTS_MARKER", marker)
	t.Setenv("CGO_ENABLED", "0")
	dir := writeModule(t, map[string]string{
		"c/c.go": "package c\n",
		"c/c_cgo_test.go": "//go:build darwin && cgo\n\npackage c\nimport (\n\t\"os\"\n\t\"testing\"\n)\n" +
			"func TestCgoOnly(t *testing.T) {\n\tif err := os.WriteFile(os.Getenv(\"DARWINTESTS_MARKER\"), nil, 0o644); err != nil {\n\t\tt.Fatal(err)\n\t}\n}\n",
	})
	plan, compileOnly, err := selectTests(dir, thisRunner, linuxJob, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if failed := runTests(dir, thisRunner, plan, compileOnly, io.Discard, io.Discard); len(failed) != 0 {
		t.Fatalf("failed = %v, want the cgo-constrained test to run and pass", failed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the cgo-constrained test did not run: %v", err)
	}
}
