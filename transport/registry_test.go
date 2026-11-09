package transport

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
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
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = Names(); _, _ = Lookup("z"); _ = Register("z", f) }()
	}
	wg.Wait()
}

// Proves external importability by inspecting the actual dependency graph, not source text.
func TestImportBoundary(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "./...")
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOFLAGS=-mod=readonly")
	out, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("go list failed: %s", out)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "github.com/relux-works/a2a-swarm/internal/") {
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
