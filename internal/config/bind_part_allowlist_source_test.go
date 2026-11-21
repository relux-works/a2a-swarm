package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestBindPartPublicationAllowlistCoversEveryWriter proves every publication
// result that the binding code assigns as a string literal is accepted by the
// sidecar's validation, so a result added to a writer cannot be refused by
// the sidecar unnoticed. It scans the non-test sources of internal/cli for
// assignments to NoticePublication. Limits: a result built at run time would
// not be seen; none exists today. The scan fails when it finds no literal at
// all, so a renamed field cannot make it pass empty.
func TestBindPartPublicationAllowlistCoversEveryWriter(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "cli", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("binding sources not found: %v", err)
	}
	assigned := regexp.MustCompile(`NoticePublication\s*(?:=|:)\s*"([^"\\]*)"`)
	seen := map[string]string{}
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range assigned.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = file
		}
	}
	if len(seen) == 0 {
		t.Fatal("no publication result literal found in the binding sources")
	}
	for result, file := range seen {
		if !validBindNoticePublication(result) {
			t.Errorf("%s assigns publication result %q, which the sidecar refuses", file, result)
		}
	}
}
