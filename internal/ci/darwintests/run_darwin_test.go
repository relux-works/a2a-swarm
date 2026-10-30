package main

import (
	"io"
	"os"
	"path/filepath"
	"reflect"
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
	plan, err := selectTests(dir, "./...")
	if err != nil {
		t.Fatal(err)
	}
	if failed := runTests(dir, plan, io.Discard, io.Discard); !reflect.DeepEqual(failed, []string{"example.com/fixture/a"}) {
		t.Fatalf("failed = %v, want only example.com/fixture/a", failed)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("package b did not run after the red package: %v", err)
	}
	if failed := runTests(dir, plan[1:], io.Discard, io.Discard); len(failed) != 0 {
		t.Fatalf("passing plan reported failures %v", failed)
	}
}
