package transport

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// Proves grammar, duplicate atomicity, lookup and sorted enumeration; factories are not opened.
func TestRegistry(t *testing.T) {
	registry.Lock()
	saved := registry.entries
	registry.entries = map[string]Factory{}
	registry.Unlock()
	t.Cleanup(func() { registry.Lock(); registry.entries = saved; registry.Unlock() })
	f := func(Config) (Transport, error) { return nil, nil }
	for _, n := range []string{"z", "a-1", strings.Repeat("a", 32)} {
		if e := Register(n, f); e != nil {
			t.Fatal(e)
		}
		if _, ok := Lookup(n); !ok {
			t.Fatal("missing valid name")
		}
	}
	before := Names()
	if e := Register("z", f); Code(e) != Refused {
		t.Fatal("duplicate accepted")
	}
	for _, n := range []string{"", "A", "-a", "a_b", strings.Repeat("a", 33), "a\n"} {
		if e := Register(n, f); Code(e) != InvalidReference {
			t.Fatalf("invalid grammar accepted %q", n)
		}
	}
	if e := Register("nil", nil); e == nil {
		t.Fatal("nil factory accepted")
	}
	if !reflect.DeepEqual(before, Names()) || !sort.StringsAreSorted(Names()) {
		t.Fatal("registry changed on refusal or not sorted")
	}
	done := make(chan struct{}, 10)
	for i := 0; i < 10; i++ {
		go func() { defer func() { done <- struct{}{} }(); _ = Names(); _, _ = Lookup("z"); _ = Register("z", f) }()
	}
	for i := 0; i < 10; i++ {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("concurrent registry access did not finish")
		}
	}
}

// Proves external importability by inspecting the actual dependency graph, not source text.
func TestImportBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "./...")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=readonly")
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("go list failed: %s", out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if internalImport(line) {
			t.Fatalf("transport import boundary violated: %s", line)
		}
	}
}

// Proves every code and unknown constructor input has fixed diagnostics, including wrapped errors.
func TestErrorsAndGapReasons(t *testing.T) {
	for _, c := range []ErrorCode{UnsupportedByTransport, ContentTooLarge, InvalidReference, InvalidCursor, Unauthenticated, Unavailable, Refused} {
		e := NewError(c)
		if Code(fmt.Errorf("wrapper: %w", e)) != c || e.Error() != "transport: "+string(c) {
			t.Fatal("closed error changed")
		}
	}
	if strings.Contains(NewError("secret-marker").Error(), "secret-marker") {
		t.Fatal("unknown code leaks")
	}
	for _, r := range []GapReason{TimelineLimited, RelayUnreachable, RelaySetChanged, WindowExceeded} {
		if !r.Valid() {
			t.Fatal("legitimate Gap rejected")
		}
	}
	for _, r := range []GapReason{"", "secret-marker", "timeline-limited"} {
		if r.Valid() {
			t.Fatal("unknown Gap accepted")
		}
	}
}

func internalImport(path string) bool {
	const root = "github.com/relux-works/a2a-swarm/internal"
	// go list may append a test variant in brackets; match the actual import path.
	fields := strings.Fields(path)
	if len(fields) == 0 {
		return false
	}
	return fields[0] == root || strings.HasPrefix(fields[0], root+"/")
}

// Proves test-only imports into both internal itself and a child fail the named
// boundary test, beside the clean dependency graph. Copies never alter this module.
func TestImportBoundaryControls(t *testing.T) {
	for _, suffix := range []string{"", "/probe"} {
		t.Run("test_import"+strings.ReplaceAll(suffix, "/", "_"), func(t *testing.T) {
			root, e := filepath.EvalSymlinks(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			write := func(path string, b []byte) {
				t.Helper()
				if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(path, b, 0600); e != nil {
					t.Fatal(e)
				}
			}
			write(filepath.Join(root, "go.mod"), []byte("module github.com/relux-works/a2a-swarm\n\ngo 1.25.12\n"))
			entries, e := os.ReadDir(".")
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".go") {
					b, e := os.ReadFile(entry.Name())
					if e != nil {
						t.Fatal(e)
					}
					write(filepath.Join(root, "transport", entry.Name()), b)
				}
			}
			run := func(wantFail bool) {
				t.Helper()
				c := exec.Command("go", "test", "-json", "-count=1", "-run=^TestImportBoundary$", "./transport")
				c.Dir = root
				c.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=readonly")
				out, e := c.CombinedOutput()
				action := "pass"
				if wantFail {
					action = "fail"
				}
				found, reason := false, false
				dec := json.NewDecoder(bytes.NewReader(out))
				for dec.More() {
					var event struct{ Action, Test, Output string }
					if err := dec.Decode(&event); err != nil {
						t.Fatalf("invalid go test JSON: %s", out)
					}
					if event.Test == "TestImportBoundary" {
						if event.Action == action {
							found = true
						}
						if strings.Contains(event.Output, "transport import boundary violated:") {
							reason = true
						}
					}
				}
				if (e != nil) != wantFail || !found || wantFail && !reason {
					t.Fatalf("expected %s TestImportBoundary: %s", action, out)
				}
				t.Logf("plant=_test.go import internal%s observed=%s TestImportBoundary", suffix, action)
			}
			run(false)
			write(filepath.Join(root, "internal"+suffix, "probe.go"), []byte("package probe\n"))
			write(filepath.Join(root, "transport", "boundary_plant_test.go"), []byte("package transport\nimport _ \"github.com/relux-works/a2a-swarm/internal"+suffix+"\"\n"))
			run(true)
		})
	}
}

// Proves nil/zero errors and refusal normalization use only closed diagnostics.
func TestNilErrorsAndRefusals(t *testing.T) {
	var typedNil *Error
	for _, e := range []*Error{typedNil, {}, NewError(Unavailable)} {
		if e.Error() != "transport: unavailable" || e.Code() != Unavailable || Code(e) != Unavailable || Code(fmt.Errorf("wrapped: %w", e)) != Unavailable {
			t.Fatal("nil/zero Error is not unavailable")
		}
	}
	if Code(nil) != "" {
		t.Fatal("nil error has a code")
	}
	for _, c := range []ErrorCode{UnsupportedByTransport, ContentTooLarge, InvalidReference, Unauthenticated, Refused} {
		r := NewRefusal(c)
		if !r.Valid() || r.Code() != c {
			t.Fatal("valid refusal rejected")
		}
	}
	for _, c := range []ErrorCode{"", Unavailable, InvalidCursor, "received-secret"} {
		r := NewRefusal(c)
		if !r.Valid() || r.Code() != Refused || strings.Contains(fmt.Sprint(r), "received-secret") {
			t.Fatal("unknown/transient refusal leaked")
		}
	}
	if (Refusal{}).Valid() {
		t.Fatal("absent refusal accepted")
	}
	for _, p := range []string{"github.com/relux-works/a2a-swarm/internal", "github.com/relux-works/a2a-swarm/internal/probe"} {
		if !internalImport(p) {
			t.Fatal("internal boundary missed")
		}
	}
	if internalImport("github.com/relux-works/a2a-swarm/internals") {
		t.Fatal("valid neighbour rejected")
	}
}
