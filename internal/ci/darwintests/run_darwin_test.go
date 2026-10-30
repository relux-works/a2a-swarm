package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

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
	plan, compileOnly, err := selectTests(dir, platform{"darwin", runtime.GOARCH}, linuxJob, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if failed := runTests(dir, plan, compileOnly, io.Discard, io.Discard); !reflect.DeepEqual(failed, []string{"example.com/fixture/a"}) {
		t.Fatalf("failed = %v, want only example.com/fixture/a", failed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("package b did not run after the red package: %v", err)
	}
	if failed := runTests(dir, plan[1:], nil, io.Discard, io.Discard); len(failed) != 0 {
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
			plan, compileOnly, err := selectTests(dir, platform{"darwin", runtime.GOARCH}, linuxJob, "./...")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(compileOnly, []string{"example.com/fixture/p"}) {
				t.Fatalf("compileOnly = %v, want package p", compileOnly)
			}
			if failed := runTests(dir, plan, compileOnly, io.Discard, io.Discard); !reflect.DeepEqual(failed, tc.want) {
				t.Fatalf("failed = %v, want %v", failed, tc.want)
			}
		})
	}
}
