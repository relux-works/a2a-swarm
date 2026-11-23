package config

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPlantedWriteFaultsOnlyFailAStep proves what PlantBindPartWriteFaults
// gives a test of another package, and that it gives nothing more:
//   - a check that returns an error fails its step with that error. A failed
//     rename leaves the sidecar byte for byte as it was; a failed directory
//     sync is the uncertain write;
//   - a check that returns nil does not stand in for its step: the write then
//     succeeds as usual and the document is in place;
//   - a nil argument leaves its step alone, and the returned function puts
//     both steps back: the checks are not called afterwards.
//
// It shows the seam, not a real failing disk.
func TestPlantedWriteFaultsOnlyFailAStep(t *testing.T) {
	p, _, view := bindFixture(t, "s0", true)
	owner := BindOperationOwner{Holder: "holder-1", Host: "host-1", PID: os.Getpid(), Birth: "birth-1"}
	alive := func(BindOperationOwner) bool { return false }
	reserve := func(token string) error {
		_, err := BeginBindCursor(p, bindTestName, token, "delivery", view.NextBatch, "", owner, alive)
		return err
	}
	sidecar := func() string {
		t.Helper()
		data, err := os.ReadFile(p.BindPartFile(bindTestName))
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	before := sidecar()
	planted := errors.New("planted")

	renames := 0
	restore := PlantBindPartWriteFaults(func() error { renames++; return planted }, nil)
	t.Cleanup(restore)
	if err := reserve(strings.Repeat("1", 32)); !errors.Is(err, planted) || errors.Is(err, ErrBindPartWrite) {
		t.Fatalf("a write whose rename is failed: %v, want the planted error and no uncertain write", err)
	}
	if renames != 1 || sidecar() != before {
		t.Fatalf("a failed rename: %d checks, sidecar unchanged=%t", renames, sidecar() == before)
	}
	restore()

	syncs := 0
	restore = PlantBindPartWriteFaults(nil, func() error { syncs++; return planted })
	t.Cleanup(restore)
	if err := reserve(strings.Repeat("2", 32)); !errors.Is(err, ErrBindPartWrite) {
		t.Fatalf("a write whose directory sync is failed: %v, want ErrBindPartWrite", err)
	}
	// The reservation and its take-back each tried the sync.
	if syncs != 2 || renames != 1 {
		t.Fatalf("a failed directory sync: %d sync checks, %d rename checks, want 2 and 1", syncs, renames)
	}
	restore()

	renames, syncs = 0, 0
	restore = PlantBindPartWriteFaults(func() error { renames++; return nil }, func() error { syncs++; return nil })
	t.Cleanup(restore)
	token := strings.Repeat("3", 32)
	if err := reserve(token); err != nil {
		t.Fatalf("a write whose checks pass: %v", err)
	}
	part, _, err := readBindPart(p, bindTestName)
	if err != nil || part.CursorOp == nil || part.CursorOp.Token != token {
		t.Fatalf("checks that pass stood in for the write: %+v err=%v", part.CursorOp, err)
	}
	if renames != 1 || syncs != 1 {
		t.Fatalf("checks that pass: %d rename and %d sync checks, want 1 and 1", renames, syncs)
	}
	restore()
	if err := AbortBindCursor(p, bindTestName, token); err != nil {
		t.Fatalf("control: a write after the steps were put back: %v", err)
	}
	if renames != 1 || syncs != 1 {
		t.Fatalf("the checks were called after the steps were put back: %d and %d", renames, syncs)
	}
}

// TestNoProductFilePlantsWriteFaults proves, from the sources, that no file of
// the product calls PlantBindPartWriteFaults: it is there for tests alone.
//
// Control: a planted call in a source text is found by the same scan, and the
// scan covers the product's files.
//
// It reads call expressions by name. It does not follow a function value that
// is passed around; no product file takes one.
func TestNoProductFilePlantsWriteFaults(t *testing.T) {
	const name = "PlantBindPartWriteFaults"
	fset := token.NewFileSet()
	scan := func(path string, src any) []string {
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var found []string
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.FuncDecl:
				// The declaration itself is not a use.
				if node.Name.Name == name {
					return false
				}
			case *ast.Ident:
				if node.Name == name {
					found = append(found, fset.Position(node.Pos()).String())
				}
			}
			return true
		})
		return found
	}
	if got := scan("planted.go", "package planted\nfunc f() { config.PlantBindPartWriteFaults(nil, nil); g := config.PlantBindPartWriteFaults; _ = g }\n"); len(got) != 2 {
		t.Fatalf("control: the scan found %d of the two planted uses: %v", len(got), got)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not at %s: %v", root, err)
	}
	files, declared := 0, false
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		if data, err := os.ReadFile(path); err == nil && strings.Contains(string(data), "func "+name+"(") {
			declared = true
		}
		found = append(found, scan(path, nil)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 || !declared {
		t.Fatalf("control: %d product files were scanned under %s; the declaration was seen: %t", files, root, declared)
	}
	if len(found) != 0 {
		t.Fatalf("the product uses the test seam:\n%s", strings.Join(found, "\n"))
	}
}
