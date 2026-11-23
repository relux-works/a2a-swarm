package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Topology fence: production release packages cannot import the diagnostic
// package or call its dispatcher. Scripts and workflows cannot invoke its mode.
// The runtime environment guard additionally covers dynamic CI command values.
func TestFixtureReleaseTopology(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == ".temp" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			tree, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			for _, imp := range tree.Imports {
				name, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if strings.HasSuffix(name, "/internal/fixturecompare") && rel != "cmd/a2a-release/fixture.go" {
					t.Errorf("production fixture import outside dispatcher: %s", rel)
				}
			}
			if !strings.HasPrefix(rel, "internal/fixturecompare/") {
				ast.Inspect(tree, func(node ast.Node) bool {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						return true
					}
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "fixtureCompare" && rel != "cmd/a2a-release/main.go" {
						t.Errorf("fixture dispatcher reachable from %s", rel)
					}
					return true
				})
			}
			if rel == "cmd/a2a-release/main.go" {
				total, standalone := 0, 0
				ast.Inspect(tree, func(node ast.Node) bool {
					if id, ok := node.(*ast.Ident); ok && id.Name == "fixtureCompare" {
						total++
					}
					clause, ok := node.(*ast.CaseClause)
					if !ok || len(clause.List) != 1 {
						return true
					}
					literal, ok := clause.List[0].(*ast.BasicLit)
					if !ok || literal.Value != `"fixture-compare"` {
						return true
					}
					ast.Inspect(clause, func(child ast.Node) bool {
						if id, ok := child.(*ast.Ident); ok && id.Name == "fixtureCompare" {
							standalone++
						}
						return true
					})
					return true
				})
				if total != 1 || standalone != 1 {
					t.Errorf("fixture dispatcher escaped its distinct literal command: total=%d standalone=%d", total, standalone)
				}
			}
		}
		if strings.HasPrefix(rel, ".scripts/") || strings.HasPrefix(rel, ".github/workflows/") || strings.HasPrefix(rel, ".github/release/") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(string(b), "\n") {
				for _, token := range []string{"fixture-compare", "fixturecompare", "fixtureCompare"} {
					if strings.Contains(line, token) && !fixtureNativeTestLine(rel, line) {
						t.Errorf("release automation invokes diagnostic token %q in %s", token, rel)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The sole allowed automation reference tests the packages; it does not invoke
// the command. Extra shell operations or another path cannot use this allowance.
func fixtureNativeTestLine(path, line string) bool {
	return path == ".github/workflows/release-controls.yml" &&
		line == "      - run: go test -p 1 -parallel 2 -race -count=1 -timeout=3m ./internal/darwinrelease ./internal/fixturecompare ./cmd/a2a-release"
}

func TestFixtureNativeTestAllowance(t *testing.T) {
	const path = ".github/workflows/release-controls.yml"
	const line = "      - run: go test -p 1 -parallel 2 -race -count=1 -timeout=3m ./internal/darwinrelease ./internal/fixturecompare ./cmd/a2a-release"
	for _, tc := range []struct {
		path, line string
		want       bool
	}{
		{path, line, true},
		{path, line + "; a2a-release fixture-compare N NEXT OUT", false},
		{path, "      - run: a2a-release fixture-compare N NEXT OUT", false},
		{path, "      - run: fixtureCompare N NEXT OUT", false},
		{path, "      - run: fixturecompare N NEXT OUT", false},
		{".scripts/release.sh", line, false},
	} {
		if got := fixtureNativeTestLine(tc.path, tc.line); got != tc.want {
			t.Errorf("test allowance for %q in %s: got %t want %t", tc.line, tc.path, got, tc.want)
		}
	}
}
