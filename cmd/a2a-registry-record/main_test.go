package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runTool(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String(), errb.String()
}

const marker = "PRIVATE-MARKER-c41d"

// Bad usage exits 2 without reading anything.
func TestUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"--fixture", "x.json"}, {"--record", "a", "extra"}, {"--bogus"}} {
		if code, _, errOut := runTool(args...); code != 2 || !strings.Contains(errOut, "usage:") {
			t.Errorf("%v: exit %d %q", args, code, errOut)
		}
	}
}

// --record prints keys and types without values; --fixture writes a new file
// with synthetic identity values and refuses to overwrite an existing one;
// a refused fixture names the field, never its value, and creates no file.
func TestRecordAndFixture(t *testing.T) {
	dir := t.TempDir()
	record := filepath.Join(dir, "4321.json")
	data := `{"pid":4321,"sessionId":"` + marker + `","cwd":"/Users/` + marker + `","version":"2.1.312","status":"idle"}`
	if err := os.WriteFile(record, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runTool("--record", record)
	if code != 0 || !strings.Contains(out, "sessionId\tstring") || strings.Contains(out+errOut, marker) || strings.Contains(out, "4321\n") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	fixture := filepath.Join(dir, "fixture.json")
	if code, out, errOut := runTool("--record", record, "--fixture", fixture); code != 0 {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	written, err := os.ReadFile(fixture)
	if err != nil || strings.Contains(string(written), marker) || !strings.Contains(string(written), `"cwd": "/synthetic"`) {
		t.Fatalf("fixture %v: %s", err, written)
	}
	if code, _, errOut := runTool("--record", record, "--fixture", fixture); code != 2 || !strings.Contains(errOut, "exists") {
		t.Fatalf("existing fixture overwritten: exit %d %s", code, errOut)
	}
	if again, _ := os.ReadFile(fixture); !bytes.Equal(again, written) {
		t.Fatal("existing fixture changed")
	}
	unknown := filepath.Join(dir, "unknown.json")
	os.WriteFile(unknown, []byte(`{"pid":1,"extra":"`+marker+`"}`), 0o600)
	if code, _, errOut := runTool("--record", unknown, "--fixture", filepath.Join(dir, "new.json")); code != 2 || strings.Contains(errOut, marker) {
		t.Fatalf("unknown key: exit %d %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.json")); !os.IsNotExist(err) {
		t.Fatal("refused fixture created a file")
	}
}
