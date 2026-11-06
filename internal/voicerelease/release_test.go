package voicerelease

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// Every emitted native reference form is admitted, while module/build paths,
// alternate loaders, traversal and lookalike system roots cannot enter a bundle.
func TestAllowedLibraryCompatibility(t *testing.T) {
	for _, tt := range []struct {
		name string
		ok   bool
	}{
		{"@rpath/libonnxruntime.dylib", true},
		{"@rpath/libsherpa-onnx-c-api.dylib", true},
		{"/usr/lib/libSystem.B.dylib", true},
		{"/usr/lib/libc++.1.dylib", true},
		{"/usr/lib/libiconv.2.dylib", true},
		{"/usr/lib/libresolv.9.dylib", true},
		{"/usr/lib/libobjc.A.dylib", true},
		{"/System/Library/Frameworks/CoreML.framework/Versions/A/CoreML", true},
		{"/System/Library/Frameworks/Foundation.framework/Versions/C/Foundation", true},
		{"/System/Library/Frameworks/CoreFoundation.framework/Versions/A/CoreFoundation", true},
		{"/System/Library/Frameworks/Security.framework/Versions/A/Security", true},
		{"/Users/build/go/pkg/mod/libonnxruntime.dylib", false},
		{"@loader_path/libonnxruntime.dylib", false},
		{"@rpath/other.dylib", false},
		{"@rpath/../libonnxruntime.dylib", false},
		{"/usr/lib/../../tmp/evil.dylib", false},
		{"/usr/library/evil.dylib", false},
		{"/System/Library/Frameworks-other/evil", false},
		{"", false},
	} {
		if got := AllowedLibrary(tt.name); got != tt.ok {
			t.Errorf("%q: %v want %v", tt.name, got, tt.ok)
		}
	}
}

// A release never overwrites an existing installation, even an empty directory.
func TestBuildPreservesExistingDestination(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "preserve")
	if err := os.WriteFile(file, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), dir); err == nil {
		t.Fatal("accepted existing destination")
	}
	b, err := os.ReadFile(file)
	if err != nil || string(b) != "unchanged" {
		t.Fatalf("changed existing destination: %q %v", b, err)
	}
}
