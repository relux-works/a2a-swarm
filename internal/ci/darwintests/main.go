// Command darwintests selects, and with -run executes, every test that the CI
// test job never runs: tests in a _test.go file the go tool builds for this
// macOS runner but not for the Linux test job (linux/amd64), whether by a
// _darwin or architecture suffix or by a build constraint. The macOS CI lane
// runs them through this command. It refuses an empty selection, an invalid
// test declaration, a selected name that a portable test file shares, and a
// fuzz target or an example with output in a Darwin-only file, which go test
// would run but a -run of Test names would not. A selected test that never
// starts fails its package. So a broken selector cannot report a green lane
// that ran nothing or ran the wrong tests.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/doc"
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
	runner := platform{"darwin", runtime.GOARCH}
	plan, compileOnly, err := selectTests(".", runner, platform{"linux", *linuxArch}, patterns...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "darwintests:", err)
		os.Exit(2)
	}
	printPlan(os.Stdout, plan)
	if !*run {
		return
	}
	if failed := runTests(".", runner, plan, compileOnly, os.Stdout, os.Stderr); len(failed) > 0 {
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

// runTests first compiles, without running, the test binary of every package in
// compileOnly: packages with runner-only test files but no test to select,
// whose loader or compile errors go test would otherwise never report here.
// Then it runs every package of the plan. Both use the selection's target, cgo
// enabled, whatever the caller's environment says, so the files that were
// selected are the files that are built. It continues after a failure and
// returns the import paths that failed to compile, whose go test exited
// non-zero, or in which a selected test never ran.
func runTests(dir string, target platform, plan []pkgTests, compileOnly []string, stdout, stderr io.Writer) []string {
	env := append(os.Environ(), "GOOS="+target.goos, "GOARCH="+target.goarch, "CGO_ENABLED=1")
	var failed []string
	if len(compileOnly) > 0 {
		out, err := os.MkdirTemp("", "darwintests-")
		if err != nil {
			fmt.Fprintln(stderr, "darwintests:", err)
			return compileOnly
		}
		defer os.RemoveAll(out)
		for i, path := range compileOnly {
			cmd := exec.Command("go", "test", "-race", "-c", "-o", filepath.Join(out, fmt.Sprintf("%d.test", i)), path)
			cmd.Dir = dir
			cmd.Env = env
			cmd.Stdout, cmd.Stderr = stdout, stderr
			if err := cmd.Run(); err != nil {
				failed = append(failed, path)
			}
		}
	}
	for _, p := range plan {
		if !runPackage(dir, env, p, stdout, stderr) {
			failed = append(failed, p.ImportPath)
		}
	}
	return failed
}

// runPackage runs the selected tests of p with go test -json, writes their
// ordinary output to stdout and reports whether go test passed and every
// selected test started. go test exits 0 with "no tests to run" when a
// selected test was not built, so the exit status alone cannot tell.
func runPackage(dir string, env []string, p pkgTests, stdout, stderr io.Writer) bool {
	cmd := exec.Command("go", "test", "-race", "-count=1", "-json", "-run", p.runPattern(), p.ImportPath)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stderr = stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintln(stderr, "darwintests:", err)
		return false
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(stderr, "darwintests:", err)
		return false
	}
	ran, readErr := readTestEvents(pipe, stdout)
	if readErr != nil {
		// Drain whatever the scanner left so go test can exit and Wait returns.
		io.Copy(io.Discard, pipe)
		fmt.Fprintf(stderr, "darwintests: %s: reading go test output: %v\n", p.ImportPath, readErr)
	}
	ok := cmd.Wait() == nil && readErr == nil
	for _, name := range p.Tests {
		if !ran[name] {
			fmt.Fprintf(stderr, "darwintests: %s: selected test %s did not run\n", p.ImportPath, name)
			ok = false
		}
	}
	return ok
}

// readTestEvents reads a go test -json stream from r, writes each event's
// output to stdout and returns the top-level tests that started. Every line of
// that stream is a JSON event with an Action, so a line that is not one (see
// decodeTestEvent) is an error: the stream can no longer prove which tests
// ran. Such a line is still echoed, and reading continues to the end so that
// the output stays complete; the first such error is returned. A line longer
// than the scanner's limit stops reading and is returned as the error; the
// caller drains the rest.
func readTestEvents(r io.Reader, stdout io.Writer) (map[string]bool, error) {
	ran := map[string]bool{}
	var first error
	lines := bufio.NewScanner(r)
	lines.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for n := 1; lines.Scan(); n++ {
		action, test, output, err := decodeTestEvent(lines.Bytes())
		if err != nil {
			fmt.Fprintln(stdout, lines.Text())
			if first == nil {
				first = fmt.Errorf("line %d is not a go test -json event: %v", n, err)
			}
			continue
		}
		io.WriteString(stdout, output)
		if action == "run" && test != "" && !strings.Contains(test, "/") {
			ran[test] = true
		}
	}
	if err := lines.Err(); err != nil {
		return ran, err
	}
	return ran, first
}

// testEventStrings are the string fields of a go test -json event, from
// cmd/internal/test2json's event and go's BuildEvent; Elapsed is its one number.
var testEventStrings = []string{"Time", "Action", "Package", "Test", "Output", "FailedBuild", "Key", "Value", "ImportPath"}

// decodeTestEvent decodes one line of go test -json output. The line must be a
// JSON object with a non-empty Action (null decodes as an object without one), and every known field it has must have
// that field's JSON type: null or another type is an error, also for a field
// this lane does not use. Unknown fields are ignored, so a newer go may add
// some.
func decodeTestEvent(line []byte) (action, test, output string, err error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(line, &fields); err != nil {
		return "", "", "", err
	}
	values := map[string]string{}
	for _, name := range testEventStrings {
		raw, ok := fields[name]
		if !ok {
			continue
		}
		var v *string
		if err := json.Unmarshal(raw, &v); err != nil || v == nil {
			return "", "", "", fmt.Errorf("field %s is not a string", name)
		}
		values[name] = *v
	}
	if raw, ok := fields["Elapsed"]; ok {
		var v *float64
		if err := json.Unmarshal(raw, &v); err != nil || v == nil {
			return "", "", "", errors.New("field Elapsed is not a number")
		}
	}
	if values["Action"] == "" {
		return "", "", "", errors.New("no Action")
	}
	return values["Action"], values["Test"], values["Output"], nil
}

// selectTests lists the packages matched by patterns for the runner and for
// the Linux job, and returns the tests of the test files present only in the
// runner's listing. A selected name that also appears in a test file compiled
// into the same runner test binary is refused: -run selects by name, so it
// would run that test too.
func selectTests(dir string, runner, linux platform, patterns ...string) ([]pkgTests, []string, error) {
	onRunner, err := listTestFiles(dir, runner, patterns)
	if err != nil {
		return nil, nil, err
	}
	onLinux, err := listTestFiles(dir, linux, patterns)
	if err != nil {
		return nil, nil, err
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
			names, others, err := testFuncs(filepath.Join(pkg.dir, name))
			if err != nil {
				return nil, nil, err
			}
			if len(others) > 0 {
				return nil, nil, fmt.Errorf("%s: %s declares %s, which go test runs but this lane does not select; move it to a portable test file or make it a Test function", path, name, strings.Join(others, ", "))
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
			names, _, err := testFuncs(filepath.Join(pkg.dir, name))
			if err != nil {
				return nil, nil, err
			}
			for _, test := range names {
				if selected[test] {
					return nil, nil, fmt.Errorf("%s: %s is defined in a Darwin-only file and in %s, which the Linux job also runs; rename one", path, test, name)
				}
			}
		}
		sort.Strings(tests)
		tests = dedupe(tests)
		plan = append(plan, pkgTests{ImportPath: path, Files: only, Tests: tests})
	}
	if err := validateTestLoad(dir, runner, withOnly); err != nil {
		return nil, nil, err
	}
	if len(plan) == 0 {
		return nil, nil, errors.New("no Darwin-only tests found")
	}
	inPlan := map[string]bool{}
	for _, p := range plan {
		inPlan[p.ImportPath] = true
	}
	var compileOnly []string
	for _, path := range withOnly {
		if !inPlan[path] {
			compileOnly = append(compileOnly, path)
		}
	}
	return plan, compileOnly, nil
}

// validateTestLoad runs the go tool's test loader (go list -test) for target
// over every package with runner-only test files, including packages with no
// test to select, so that a loader error such as two TestMain hooks in one
// test binary, or an error in a package a test file imports, fails the
// selection instead of going unnoticed.
func validateTestLoad(dir string, target platform, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	// Requesting the embed fields turns off go list's embed suppression, so a
	// missing embedded file is reported as the loader of go test reports it.
	args := append([]string{"list", "-race", "-e", "-test", "-json=ImportPath,Error,DepsErrors,Incomplete,EmbedFiles,TestEmbedFiles,XTestEmbedFiles"}, paths...)
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
			DepsErrors []struct{ Err string }
			Incomplete bool
		}
		if err := dec.Decode(&p); err == io.EOF {
			return nil
		} else if err != nil {
			return fmt.Errorf("go list -test for %s/%s: %v", target.goos, target.goarch, err)
		}
		if p.Error != nil {
			return fmt.Errorf("go test loader for %s/%s: %s: %s", target.goos, target.goarch, p.ImportPath, p.Error.Err)
		}
		if len(p.DepsErrors) > 0 {
			return fmt.Errorf("go test loader for %s/%s: %s: dependency: %s", target.goos, target.goarch, p.ImportPath, p.DepsErrors[0].Err)
		}
		if p.Incomplete {
			return fmt.Errorf("go test loader for %s/%s: %s: incomplete package", target.goos, target.goarch, p.ImportPath)
		}
	}
}

type listedPkg struct {
	dir   string
	files map[string]bool
}

// listTestFiles runs go list for target with cgo enabled and the race build tag
// on both sides, as both CI jobs run go test -race, so that neither a cgo nor a
// race constraint makes a file look Darwin-only or hides it.
func listTestFiles(dir string, target platform, patterns []string) (map[string]listedPkg, error) {
	args := append([]string{"list", "-race", "-e", "-json=ImportPath,Dir,TestGoFiles,XTestGoFiles,Error"}, patterns...)
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
// others holds what go test also runs under other names: fuzz targets, whose
// seed corpus runs as a test, and examples with an output comment.
func testFuncs(file string) (names, others []string, err error) {
	f, err := parser.ParseFile(token.NewFileSet(), file, nil, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, nil, err
	}
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
				return nil, nil, fmt.Errorf("%s: wrong signature for TestMain, must be: func TestMain(m *testing.M)", file)
			}
		case isTest(name, "Test"):
			if !isTestFunc(fn, "T") {
				return nil, nil, fmt.Errorf("%s: wrong signature for %s, must be: func %s(t *testing.T)", file, name, name)
			}
			names = append(names, name)
		case isTest(name, "Fuzz"):
			others = append(others, name)
		}
	}
	for _, ex := range doc.Examples(f) {
		if ex.Output != "" || ex.EmptyOutput {
			others = append(others, "Example"+ex.Name)
		}
	}
	return names, others, nil
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

// isTest reports whether name is prefix followed by nothing or by a rune that
// is not a lower-case letter, the go tool's rule for test function names.
func isTest(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
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
