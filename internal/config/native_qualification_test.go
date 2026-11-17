package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Proves qualification selection is explicit, bounded and scoped to the
// original config root, with missing/malformed/relative or non-clean inputs
// selecting nothing beside the valid absolute control.
func TestNativeQualificationPath(t *testing.T) {
	base := t.TempDir()
	p := Paths{Base: base}
	if got := NativeQualificationPath(p); got != "" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ raw, want string }{
		{`{"qualification_path":"/protected/native.json"}`, "/protected/native.json"},
		{`{"qualification_path":"relative.json"}`, ""},
		{`{"qualification_path":"/protected/../native.json"}`, ""},
		{`{"qualification_path":null}`, ""},
		{`{`, ""},
	} {
		if err := os.WriteFile(filepath.Join(base, "native-qualification.json"), []byte(tc.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := NativeQualificationPath(p); got != tc.want {
			t.Fatalf("got %q want %q", got, tc.want)
		}
	}
	p.Root = t.TempDir()
	if got := NativeQualificationPath(p); got != "" {
		t.Fatal("selected child config over original root")
	}
	if err := os.WriteFile(filepath.Join(p.Root, "native-qualification.json"), []byte(`{"qualification_path":"/original/native.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := NativeQualificationPath(p); got != "/original/native.json" {
		t.Fatal(got)
	}
	if err := os.Truncate(filepath.Join(p.Root, "native-qualification.json"), 4097); err != nil {
		t.Fatal(err)
	}
	if got := NativeQualificationPath(p); got != "" {
		t.Fatal("oversized config accepted")
	}
}
