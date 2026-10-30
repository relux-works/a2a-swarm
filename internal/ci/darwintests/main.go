// Command darwintests selects, and with -run executes, every test that the CI
// test job never runs: tests in a _test.go file the go tool builds for this
// macOS runner but not for the Linux test job (linux/amd64), whether by a
// _darwin or architecture suffix or by a build constraint. The macOS CI lane
// runs them through this command. It refuses an empty selection, an invalid
// test declaration and a selected name that a portable test file shares, so a
// broken selector cannot report a green lane that ran nothing or ran the wrong
// tests.
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
	"runtime"
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

// platform is one go list target.
type platform struct{ goos, goarch string }

func main() {
	run := flag.Bool("run", false, "run the selected tests with go test -race -count=1")
	linuxArch := flag.String("linux-goarch", "amd64", "GOARCH of the Linux CI test job")
	flag.Parse()
	patterns := flag.Args()
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}
	if *run {
		// Opt-in real-host tests are gated by A2A_* variables; none may reach CI.
		if names := optInVariables(os.Environ()); len(names) > 0 {
			fmt.Fprintf(os.Stderr, "darwintests: A2A_* variables must be unset: %s\n", strings.Join(names, " "))
			os.Exit(2)
		}
	}
	plan, err := selectTests(".", platform{"darwin", runtime.GOARCH}, platform{"linux", *linuxArch}, patterns...)
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

// optInVariables returns the sorted names of environment entries whose name,
// the part before the first '=', starts with A2A_. Values are never returned.
func optInVariables(environ []string) []string {
	var names []string
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "A2A_") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
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

// selectTests lists the packages matched by patterns for the runner and for
// the Linux job, and returns the tests of the test files present only in the
// runner's listing. A selected name that also appears in a test file compiled
// into the same runner test binary is refused: -run selects by name, so it
// would run that test too.
func selectTests(dir string, runner, linux platform, patterns ...string) ([]pkgTests, error) {
	onRunner, err := listTestFiles(dir, runner, patterns)
	if err != nil {
		return nil, err
	}
	onLinux, err := listTestFiles(dir, linux, patterns)
	if err != nil {
		return nil, err
	}
	var plan []pkgTests
	var withOnly []string
	for _, path := range sortedKeys(onRunner) {
		pkg := onRunner[path]
		var both map[string]bool
		if other, ok := onLinux[path]; ok {
			both = other.files
		}
		var only, shared []string
		for name := range pkg.files {
			if both[name] {
				shared = append(shared, name)
			} else {
				only = append(only, name)
			}
		}
		if len(only) == 0 {
			continue
		}
		withOnly = append(withOnly, path)
		sort.Strings(only)
		sort.Strings(shared)
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
		selected := map[string]bool{}
		for _, name := range tests {
			selected[name] = true
		}
		for _, name := range shared {
			names, err := testFuncs(filepath.Join(pkg.dir, name))
			if err != nil {
				return nil, err
			}
			for _, test := range names {
				if selected[test] {
					return nil, fmt.Errorf("%s: %s is defined in a Darwin-only file and in %s, which the Linux job also runs; rename one", path, test, name)
				}
			}
		}
		sort.Strings(tests)
		tests = dedupe(tests)
		plan = append(plan, pkgTests{ImportPath: path, Files: only, Tests: tests})
	}
	if err := validateTestLoad(dir, runner, withOnly); err != nil {
		return nil, err
	}
	if len(plan) == 0 {
		return nil, errors.New("no Darwin-only tests found")
	}
	return plan, nil
}

// validateTestLoad runs the go tool's test loader (go list -test) for target
// over every package with runner-only test files, including packages with no
// test to select, so that a loader error such as two TestMain hooks in one
// test binary fails the selection instead of going unnoticed.
func validateTestLoad(dir string, target platform, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	args := append([]string{"list", "-e", "-test", "-json=ImportPath,Error"}, paths...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("go list -test for %s/%s: %v: %s", target.goos, target.goarch, err, strings.TrimSpace(stderr.String()))
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p struct {
			ImportPath string
			Error      *struct{ Err string }
		}
		if err := dec.Decode(&p); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("go list -test for %s/%s: %v", target.goos, target.goarch, err)
		}
		if p.Error != nil {
			return fmt.Errorf("go test loader for %s/%s: %s: %s", target.goos, target.goarch, p.ImportPath, p.Error.Err)
		}
	}
}

type listedPkg struct {
	dir   string
	files map[string]bool
}

// listTestFiles runs go list for target with cgo enabled on both sides, so
// that a cgo constraint never makes a file look Darwin-only.
func listTestFiles(dir string, target platform, patterns []string) (map[string]listedPkg, error) {
	args := append([]string{"list", "-e", "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles,Error"}, patterns...)
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list for %s/%s: %v: %s", target.goos, target.goarch, err, strings.TrimSpace(stderr.String()))
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
			return nil, fmt.Errorf("go list for %s/%s: %v", target.goos, target.goarch, err)
		}
		if p.Error != nil {
			return nil, fmt.Errorf("go list for %s/%s: %s: %s", target.goos, target.goarch, p.ImportPath, p.Error.Err)
		}
		files := map[string]bool{}
		for _, f := range append(p.TestGoFiles, p.XTestGoFiles...) {
			files[f] = true
		}
		pkgs[p.ImportPath] = listedPkg{dir: p.Dir, files: files}
	}
	return pkgs, nil
}

// testFuncs returns the top-level functions in file that go test runs as
// tests, with the go tool's rules: a TestXxx function, where Xxx does not start
// with a lower-case letter, must take exactly one *testing.T and return
// nothing, and is an error otherwise. TestMain taking *testing.M is the test
// binary's hook and is not a test; TestMain taking *testing.T is a test.
func testFuncs(file string) ([]string, error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}
		name := fn.Name.Name
		switch {
		case name == "TestMain":
			if isTestFunc(fn, "T") {
				names = append(names, name)
			} else if !isTestFunc(fn, "M") {
				return nil, fmt.Errorf("%s: wrong signature for TestMain, must be: func TestMain(m *testing.M)", file)
			}
		case isTestName(name):
			if !isTestFunc(fn, "T") {
				return nil, fmt.Errorf("%s: wrong signature for %s, must be: func %s(t *testing.T)", file, name, name)
			}
			names = append(names, name)
		}
	}
	return names, nil
}

// isTestFunc reports whether fn has no type parameters, no results and exactly
// one parameter of type *M or *x.M, where M is arg; like the go tool, it cannot
// see how testing was imported.
func isTestFunc(fn *ast.FuncDecl, arg string) bool {
	if fn.Type.TypeParams.NumFields() > 0 || fn.Type.Results.NumFields() > 0 || fn.Type.Params.NumFields() != 1 {
		return false
	}
	ptr, ok := fn.Type.Params.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	if id, ok := ptr.X.(*ast.Ident); ok {
		return id.Name == arg
	}
	if sel, ok := ptr.X.(*ast.SelectorExpr); ok {
		return sel.Sel.Name == arg
	}
	return false
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

// dedupe drops adjacent duplicates from a sorted slice: the internal and the
// external test package may both define a selected name, and both run.
func dedupe(sorted []string) []string {
	out := sorted[:0]
	for i, s := range sorted {
		if i == 0 || s != sorted[i-1] {
			out = append(out, s)
		}
	}
	return out
}

func sortedKeys(m map[string]listedPkg) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
