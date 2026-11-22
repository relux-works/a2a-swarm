package config

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestBindMigrationLockWaitIsBounded is the migration's row of
// TestBindOperationLockWaitIsBounded; it has its own fixture because the
// identity is not marked yet. With the State lock held elsewhere the
// migration answers ErrLockTimeout at its deadline and its context's error
// when cancelled, State is byte for byte as it was, and no sidecar appears.
// Control: with the lock free the same call migrates.
//
// Regression witness: the migration had no context and waited in a blocking
// flock.
func TestBindMigrationLockWaitIsBounded(t *testing.T) {
	p := testPaths(t)
	if err := SaveState(p, bindTestName, State{NextBatch: "s0", Threads: []string{"$keep"}}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil {
		t.Fatal(err)
	}
	unlock := holdBindTestLock(t, p)
	released := false
	defer func() {
		if !released {
			unlock()
		}
	}()
	migrate := func(ctx context.Context) error {
		done := make(chan error, 1)
		go func() {
			_, err := EnsureBindPartContext(ctx, p, bindTestName, BindPartOpenOptions{Holder: "test-holder", Now: time.Now()})
			done <- err
		}()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			if !released {
				released = true
				unlock()
			}
			<-done
			t.Fatal("the migration was still waiting for the State lock ten seconds after its context ended")
			return nil
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	err = migrate(ctx)
	cancel()
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("deadline while the lock is held: %v, want ErrLockTimeout", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := migrate(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation while the lock is held: %v, want context.Canceled", err)
	}
	cancel()
	after, err := os.ReadFile(p.StateFile(bindTestName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("a refused migration changed State: err=%v", err)
	}
	if _, err := os.Lstat(p.BindPartFile(bindTestName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refused migration left a sidecar: %v", err)
	}

	released = true
	unlock()
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := migrate(ctx); err != nil {
		t.Fatalf("control with the lock free: %v", err)
	}
	if st, err := LoadState(p, bindTestName); err != nil || !BindPartActive(st) {
		t.Fatalf("control: the identity is not migrated: active=%t err=%v", BindPartActive(st), err)
	}
}

// bindLockFreeOperations are the exported functions of bind_part_ops.go that
// take neither a context nor the State lock. Every other exported function
// there without a context waits for the State lock without limit.
var bindLockFreeOperations = map[string]bool{
	"NewBindToken": true, "BindPartActive": true, "BindPartMarked": true, "ReadBindPartView": true,
}

// bindLockNames are the identifiers through which bind_part_ops.go reaches the
// State lock.
var bindLockNames = map[string]bool{
	"lockFile": true, "lockFileBy": true, "lockFileContext": true, "withStateLock": true,
	"withBindPartOperationLockBy": true, "withBindRollbackLock": true, "bindOperationLock": true, "bindOperationEndLock": true,
}

// TestNoCommandWaitsForTheStateLockWithoutAContext proves, from the sources,
// that no file of the product calls a sidecar operation that waits for the
// State lock without limit. The operations are read from bind_part_ops.go:
// an exported function there either takes a context first, or is one of the
// four named lock-free ones, or waits without limit. The last kind is left for
// tests only. The test also refuses a literal nil in place of the context,
// which would select the unlimited wait.
//
// Controls: the derived set holds the operations known to wait without limit;
// each lock-free function is checked not to mention the lock; and a planted
// call in a source text is found by the same scan.
//
// It reads sources: it shows which function a command calls, not what the
// context it passes is worth. The bounded wait itself is shown by
// TestBindOperationLockWaitIsBounded for every operation, and by the tests of
// the commands for the paths they take.
func TestNoCommandWaitsForTheStateLockWithoutAContext(t *testing.T) {
	fset := token.NewFileSet()
	ops, err := parser.ParseFile(fset, "bind_part_ops.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	unlimited, bounded := map[string]bool{}, map[string]bool{}
	for _, decl := range ops.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !fn.Name.IsExported() {
			continue
		}
		name := fn.Name.Name
		if takesContextFirst(fn) {
			bounded[name] = true
			continue
		}
		if bindLockFreeOperations[name] {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && bindLockNames[id.Name] {
					t.Errorf("%s is listed as lock-free and mentions %s", name, id.Name)
				}
				return true
			})
			continue
		}
		unlimited[name] = true
	}
	for _, name := range []string{"EnsureBindPart", "BeginBindCursor", "CommitBindCursor", "AbortBindCursor", "SetBindControl", "RecordBindNotice", "AcquireOrRenewBindLease", "FinishBindOwnStatus", "ExportBindPartForRollback"} {
		if !unlimited[name] {
			t.Fatalf("control: %s is not in the derived set of unlimited waits %v", name, sortedNames(unlimited))
		}
	}
	for _, name := range []string{"TryCommitEmptyBindCursorContext", "ReleaseBindLeaseContext", "RenewBindMonitorLeaseContext", "TransferBindSessionAndLeaseContext", "EnsureBindPartContext", "SetBindControlContext"} {
		if !bounded[name] {
			t.Fatalf("control: %s is not in the derived set of bounded operations %v", name, sortedNames(bounded))
		}
	}

	scan := func(path string, src any) []string {
		file, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		var found []string
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			switch {
			case unlimited[name]:
				found = append(found, fset.Position(call.Pos()).String()+": "+name+" waits for the State lock without limit")
			case bounded[name] && len(call.Args) > 0:
				if id, ok := call.Args[0].(*ast.Ident); ok && id.Name == "nil" {
					found = append(found, fset.Position(call.Pos()).String()+": "+name+" is given a nil context")
				}
			}
			return true
		})
		return found
	}
	if got := scan("planted.go", "package planted\nfunc f() { config.BeginBindCursor(); config.CommitBindCursorContext(nil) }\n"); len(got) != 2 {
		t.Fatalf("control: the scan found %d of the two planted calls: %v", len(got), got)
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("the module root is not at %s: %v", root, err)
	}
	files := 0
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
		found = append(found, scan(path, nil)...)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("control: only %d product files were scanned under %s", files, root)
	}
	if len(found) != 0 {
		t.Fatalf("the product waits for the State lock without limit:\n%s", strings.Join(found, "\n"))
	}
}

func takesContextFirst(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil || len(fn.Type.Params.List) == 0 {
		return false
	}
	sel, ok := fn.Type.Params.List[0].Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "context" && sel.Sel.Name == "Context"
}

func sortedNames(set map[string]bool) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
