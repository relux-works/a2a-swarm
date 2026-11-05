package tuitestkit

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// UpdateSnapshots controls whether snapshot functions overwrite golden files
// instead of comparing against them. Set via UPDATE_SNAPSHOTS=1 environment
// variable, or directly in test code.
var UpdateSnapshots bool

// snapshotBaseDir overrides the automatic path resolution for tests.
// When empty (default), snapshot functions use runtime.Caller to determine
// the test file's directory and place golden files in testdata/snapshots/.
// Tests set this to t.TempDir() so no golden files leak into the repo.
var snapshotBaseDir string

func init() {
	if os.Getenv("UPDATE_SNAPSHOTS") == "1" {
		UpdateSnapshots = true
	}
}

// snapshotPath returns the full path for a golden file named `name`.
// If snapshotBaseDir is set, it uses that directly.
// Otherwise it walks up the call stack (skip frames) to find the caller's
// source file directory and appends testdata/snapshots/.
func snapshotPath(name string, callerSkip int) string {
	base := snapshotBaseDir
	if base == "" {
		_, file, _, ok := runtime.Caller(callerSkip)
		if !ok {
			panic("tuitestkit: cannot determine caller file for snapshot path")
		}
		base = filepath.Join(filepath.Dir(file), "testdata", "snapshots")
	}
	return filepath.Join(base, name+".golden")
}

// SnapshotView captures model.View(), strips ANSI escape codes, and compares
// (or updates) the golden file named `name`.
func SnapshotView(t *testing.T, model tea.Model, name string) {
	t.Helper()
	view := StripANSI(model.View())
	snapshot(t, view, name, 3)
}

// SnapshotViewRaw captures model.View() with raw ANSI codes intact and
// compares (or updates) the golden file named `name`.
func SnapshotViewRaw(t *testing.T, model tea.Model, name string) {
	t.Helper()
	snapshot(t, model.View(), name, 3)
}

// SnapshotStr compares a pre-rendered view string (after ANSI stripping)
// against the golden file named `name`.
func SnapshotStr(t *testing.T, view string, name string) {
	t.Helper()
	snapshot(t, StripANSI(view), name, 3)
}

// SnapshotStrRaw compares a pre-rendered view string (raw, with ANSI codes)
// against the golden file named `name`.
func SnapshotStrRaw(t *testing.T, view string, name string) {
	t.Helper()
	snapshot(t, view, name, 3)
}

// snapshotT is the subset of testing.T used by the snapshot implementation.
// Extracted as an interface so tests can intercept failure calls.
type snapshotT interface {
	Helper()
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
}

// snapshot is the core implementation shared by all Snapshot* functions.
// callerSkip controls how many stack frames to skip when resolving the
// snapshot path (only used when snapshotBaseDir is empty).
func snapshot(t snapshotT, content string, name string, callerSkip int) {
	t.Helper()

	path := snapshotPath(name, callerSkip)

	if UpdateSnapshots {
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("snapshot: cannot create directory %s: %v", dir, err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("snapshot: cannot write golden file %s: %v", path, err)
		}
		return
	}

	expected, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("snapshot %q: golden file not found at %s\nRun with UPDATE_SNAPSHOTS=1 to create it.", name, path)
		}
		t.Fatalf("snapshot %q: cannot read golden file: %v", name, err)
	}

	expectedStr := string(expected)
	if expectedStr == content {
		return
	}

	diff := unifiedDiff(expectedStr, content)
	t.Errorf("snapshot %q mismatch:\n%s", name, diff)
}

// unifiedDiff produces a simple line-by-line diff between expected and actual.
// Lines prefixed with "-" are only in expected; "+" only in actual.
// Unchanged lines are prefixed with " ". Every line includes a line number.
func unifiedDiff(expected, actual string) string {
	expLines := strings.Split(expected, "\n")
	actLines := strings.Split(actual, "\n")

	// LCS-based diff for accurate results.
	lcs := lcsTable(expLines, actLines)
	var b strings.Builder

	b.WriteString("--- expected\n")
	b.WriteString("+++ actual\n")

	i, j := len(expLines), len(actLines)
	// Build diff lines in reverse, then reverse the slice.
	var lines []string
	for i > 0 || j > 0 {
		if i > 0 && j > 0 && expLines[i-1] == actLines[j-1] {
			lines = append(lines, fmt.Sprintf(" %4d  %s", i, expLines[i-1]))
			i--
			j--
		} else if j > 0 && (i == 0 || lcs[i][j-1] >= lcs[i-1][j]) {
			lines = append(lines, fmt.Sprintf("+%4d  %s", j, actLines[j-1]))
			j--
		} else {
			lines = append(lines, fmt.Sprintf("-%4d  %s", i, expLines[i-1]))
			i--
		}
	}

	// Reverse to get top-to-bottom order.
	for k := len(lines) - 1; k >= 0; k-- {
		b.WriteString(lines[k])
		b.WriteByte('\n')
	}

	return b.String()
}

// lcsTable builds the classic LCS (longest common subsequence) DP table.
func lcsTable(a, b []string) [][]int {
	m, n := len(a), len(b)
	table := make([][]int, m+1)
	for i := range table {
		table[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				table[i][j] = table[i-1][j-1] + 1
			} else if table[i-1][j] >= table[i][j-1] {
				table[i][j] = table[i-1][j]
			} else {
				table[i][j] = table[i][j-1]
			}
		}
	}
	return table
}
