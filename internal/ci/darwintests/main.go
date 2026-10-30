// Command darwintests selects, and with -run executes, every test defined in a
// Darwin-only test file: a _test.go file the go tool builds for darwin but not
// for linux, whether by its _darwin suffix or by a build constraint. The CI
// test job runs on Linux and never compiles these files, so the macOS CI lane
// runs them through this command. It refuses an empty selection, so a broken
// selector cannot report a green lane that ran nothing.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pkgTests is one package and the tests from its Darwin-only test files.
type pkgTests struct {
	ImportPath string
	Files      []string
	Tests      []string
}

// runPattern is the anchored go test -run pattern that selects exactly Tests.
func (p pkgTests) runPattern() string {
	return "^(" + strings.Join(p.Tests, "|") + ")$"
}

func main() {
	run := flag.Bool("run", false, "run the selected tests with go test -race -count=1")
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	plan, err := selectTests(".", patterns...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "darwintests:", err)
		os.Exit(2)
	}
	printPlan(os.Stdout, plan)
	if !*run {
		return
	}
	if failed := runTests(".", plan, os.Stdout, os.Stderr); len(failed) > 0 {
		fmt.Fprintf(os.Stderr, "darwintests: failed packages: %s\n", strings.Join(failed, " "))
		os.Exit(1)
	}
}

func printPlan(w io.Writer, plan []pkgTests) {
	for _, p := range plan {
		fmt.Fprintf(w, "%s (%s): %s\n", p.ImportPath, strings.Join(p.Files, ", "), strings.Join(p.Tests, " "))
	}
}

// runTests runs every package of the plan, even after a failure, and returns
// the import paths whose go test exited non-zero.
func runTests(dir string, plan []pkgTests, stdout, stderr io.Writer) []string {
	var failed []string
	for _, p := range plan {
		cmd := exec.Command("go", "test", "-race", "-count=1", "-run", p.runPattern(), p.ImportPath)
		cmd.Dir = dir
		cmd.Stdout, cmd.Stderr = stdout, stderr
		if err := cmd.Run(); err != nil {
			failed = append(failed, p.ImportPath)
		}
	}
	return failed
}

// selectTests lists the packages matched by patterns for darwin and for linux,
// and returns the tests of the test files present only in the darwin listing.
func selectTests(dir string, patterns ...string) ([]pkgTests, error) {
	darwin, err := listTestFiles(dir, "darwin", patterns)
	if err != nil {
		return nil, err
	}
	linux, err := listTestFiles(dir, "linux", patterns)
	if err != nil {
		return nil, err
	}
	var plan []pkgTests
	for _, path := range sortedKeys(darwin) {
		pkg := darwin[path]
		var both map[string]bool
		if other, ok := linux[path]; ok {
			both = other.files
		}
		var only []string
		for name := range pkg.files {
			if !both[name] {
				only = append(only, name)
			}
		}
		if len(only) == 0 {
			continue
		}
		sort.Strings(only)
		var tests []string
		for _, name := range only {
			names, err := testFuncs(filepath.Join(pkg.dir, name))
			if err != nil {
				return nil, err
			}
			tests = append(tests, names...)
		}
		if len(tests) == 0 {
			continue
		}
		sort.Strings(tests)
		plan = append(plan, pkgTests{ImportPath: path, Files: only, Tests: tests})
	}
	if len(plan) == 0 {
		return nil, errors.New("no Darwin-only tests found")
	}
	return plan, nil
}

type listedPkg struct {
	dir   string
	files map[string]bool
}

// listTestFiles runs go list for goos with cgo enabled on both sides, so that a
// cgo constraint never makes a file look Darwin-only.
func listTestFiles(dir, goos string, patterns []string) (map[string]listedPkg, error) {
	args := append([]string{"list", "-e", "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles,Error"}, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+goos, "CGO_ENABLED=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list for %s: %v: %s", goos, err, strings.TrimSpace(stderr.String()))
	}
	pkgs := map[string]listedPkg{}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath   string
			Dir          string
			TestGoFiles  []string
			XTestGoFiles []string
			Error        *struct{ Err string }
		}
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			return nil, fmt.Errorf("go list for %s: %v", goos, err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list for %s: %s: %s", goos, p.ImportPath, p.Error.Err)
		}
		files := map[string]bool{}
		for _, f := range append(p.TestGoFiles, p.XTestGoFiles...) {
			files[f] = true
		}
		pkgs[p.ImportPath] = listedPkg{dir: p.Dir, files: files}
	}
	return pkgs, nil
}

// testFuncs returns the top-level functions in file that go test runs as tests:
// TestXxx with one parameter, where Xxx does not start with a lower-case letter.
func testFuncs(file string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !isTestName(fn.Name.Name) || fn.Type.Params.NumFields() != 1 {
			continue
		}
		names = append(names, fn.Name.Name)
	}
	return names, nil
}

func isTestName(name string) bool {
	rest, ok := strings.CutPrefix(name, "Test")
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

func sortedKeys(m map[string]listedPkg) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
