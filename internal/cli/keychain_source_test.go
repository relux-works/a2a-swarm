package cli

import (
	"errors"
	"go/ast"
	"go/parser"
	gotoken "go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// securityToolLiterals lists the production Go files under root/internal and
// root/cmd whose string literals name the macOS security tool: its path, or the
// bare command name an exec call would use. Comments and test files are not
// scanned.
func securityToolLiterals(root string) ([]string, error) {
	var hits []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, err := parser.ParseFile(gotoken.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			ast.Inspect(f, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != gotoken.STRING {
					return true
				}
				if v, err := strconv.Unquote(lit.Value); err == nil && (strings.Contains(v, "/usr/bin/security") || v == "security") {
					hits = append(hits, path)
				}
				return true
			})
			return nil
		})
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return hits, nil
}

// Phase 2 (#138) reads and writes Keychain items only in process, so that the
// item ACL bound to the signed a2a binary is what grants access. No production
// source names the security tool. The planted control proves the scan finds a
// literal and ignores a comment. Limit: a name assembled at run time, or a
// helper reached without naming it, would evade this scan.
func TestKeychainProductionNeverNamesSecurityTool(t *testing.T) {
	control := t.TempDir()
	if err := os.MkdirAll(filepath.Join(control, "internal", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"planted.go": "package x\n\nvar helper = \"/usr/bin/security\"\n",
		"comment.go": "package x\n\n// Never run /usr/bin/security.\nvar other = \"securityd-free\"\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(control, "internal", "x", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := securityToolLiterals(control)
	if err != nil || len(hits) != 1 || filepath.Base(hits[0]) != "planted.go" {
		t.Fatalf("scan control failed: hits=%v err=%v", hits, err)
	}
	hits, err = securityToolLiterals("../..")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("production source names the security tool: %v", hits)
	}
}
