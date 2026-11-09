package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These controls prove each recipient mutation fails its intended assertion,
// between passing original and restored checks, on the GitHub Actions route.
// They do not qualify live Matrix delivery; compiler failures cannot kill mutants.
func TestRecipientCIControls(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("CI") != "true" {
		t.Skip("recipient source-mutation controls require GitHub Actions CI")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(cwd))
	dir := t.TempDir()
	copyRecipientCISandbox(t, root, dir)
	run := func(t *testing.T, pattern string) (string, error) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "test", "-mod=readonly", "./internal/cli", "-run", pattern, "-count=1", "-timeout=3m")
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
		out, err := cmd.CombinedOutput()
		if ctx.Err() != nil {
			t.Fatalf("recipient CI control timed out: %v", ctx.Err())
		}
		return string(out), err
	}
	for _, tc := range []struct {
		name, file, old, replacement, pattern, assertion string
	}{
		{
			name:        "silent-lowercase",
			file:        "recipient_identity.go",
			old:         "return \"\", true, usagef(\"invalid user %q: explicit targets must be lowercase; use %q\", target, lower)",
			replacement: "return lower, true, nil",
			pattern:     "^TestRecipientExactRefusal$/^post$/^uppercase-local$/^0$",
			assertion:   "recipient refusal:",
		},
		{
			name:        "empty-server-autofill",
			file:        "recipient_identity.go",
			old:         "if !recipientExactTarget(target) {",
			replacement: "if strings.HasSuffix(target, \":\") { target += selectedServer }\nif !recipientExactTarget(target) {",
			pattern:     "^TestRecipientExactRefusal$/^post$/^empty-server$/^0$",
			assertion:   "recipient refusal:",
		},
		{
			name:        "short-local-header",
			file:        "topics.go",
			old:         "if explicit {\n\t\t\tnames = append(names, uid)",
			replacement: "if explicit {\n\t\t\tnames = append(names, localpartIfLocal(uid, server))",
			pattern:     "^TestRecipientExactWire$/^alpha[.]example$/^post$/^explicit-local$",
			assertion:   "recipient identity mismatch:",
		},
		{
			name:        "ignore-trailing-invalid",
			file:        "topics.go",
			old:         "uid, explicit, err := messageRecipient(t, server)\n\t\tif err != nil {\n\t\t\treturn nil, nil, err",
			replacement: "uid, explicit, err := messageRecipient(t, server)\n\t\tif err != nil {\n\t\t\tif len(names) > 0 { return names, mentions, nil }; return nil, nil, err",
			pattern:     "^TestRecipientExactRefusal$/^post$/^empty-server$/^2$",
			assertion:   "recipient refusal:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "internal", "cli", tc.file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(original), tc.old) != 1 {
				t.Fatal("control source anchor must occur exactly once")
			}
			// Keep baseline, mutant and restored checks bounded to one subtest.
			if out, err := run(t, tc.pattern); err != nil {
				t.Fatalf("original targeted recipient positives: %v\n%s", err, out)
			}
			mutant := strings.Replace(string(original), tc.old, tc.replacement, 1)
			if err := os.WriteFile(path, []byte(mutant), 0600); err != nil {
				t.Fatal(err)
			}
			// Restore even if an assertion aborts this subtest.
			defer func() {
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Error(err)
				}
			}()
			out, err := run(t, tc.pattern)
			if err == nil || !strings.Contains(out, "--- FAIL: TestRecipientExact") || !strings.Contains(out, tc.assertion) {
				t.Fatalf("violation did not fail its intended assertion: %v\n%s", err, out)
			}
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := run(t, tc.pattern); err != nil {
				t.Fatalf("restored targeted recipient positives: %v\n%s", err, out)
			}
		})
	}
}

// Copy the module rather than a fixed package list: test-only dependencies can
// add new top-level packages. Git metadata and local scratch are not build inputs.
func copyRecipientCISandbox(t *testing.T, root, dir string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || name == ".temp" {
			continue
		}
		if entry.IsDir() {
			if err := os.CopyFS(filepath.Join(dir, name), os.DirFS(filepath.Join(root, name))); err != nil {
				t.Fatal(err)
			}
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

// The actual CLI test dependency graph must have every in-module top-level
// directory in the sandbox, even outside CI. This guards copy omissions, not
// compilation or dependency completeness on platforms other than the current one.
func TestRecipientCISandboxDependencies(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filepath.Dir(cwd))
	dir := t.TempDir()
	copyRecipientCISandbox(t, root, dir)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "list", "-mod=readonly", "-deps", "-test", "-f", "{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}", "./internal/cli")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("list CLI test dependencies: %v\n%s", err, out)
	}
	seen := map[string]bool{}
	for _, path := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path == "" {
			continue
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || !filepath.IsLocal(rel) {
			t.Fatalf("dependency directory outside module: %q (%v)", path, err)
		}
		top := strings.Split(rel, string(filepath.Separator))[0]
		if seen[top] {
			continue
		}
		seen[top] = true
		info, err := os.Stat(filepath.Join(dir, top))
		if err != nil {
			t.Errorf("recipient CI sandbox missing imported top-level directory %q: %v", top, err)
		} else if !info.IsDir() {
			t.Errorf("recipient CI sandbox imported top-level directory %q is not a directory", top)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no in-module CLI test dependency directories found")
	}
}

// Adjacent build inputs, including new package trees and dot directories, are
// copied byte-for-byte; Git metadata and scratch are excluded. This fixture
// checks copy policy, not a build of the real CLI (covered by the controls).
func TestRecipientCISandboxCopy(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	for _, name := range []string{
		"go.mod", "go.sum", "README.md", ".github/workflows/go.yml",
		"internal/cli/cli.go", "transport/transport.go", "cmd/a2a/main.go",
		"tools/check/main.go", ".git/config", ".temp/local.go",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	copyRecipientCISandbox(t, root, dir)
	for _, name := range []string{
		"go.mod", "go.sum", "README.md", ".github/workflows/go.yml",
		"internal/cli/cli.go", "transport/transport.go", "cmd/a2a/main.go",
		"tools/check/main.go",
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != name {
			t.Errorf("sandbox input %q: got %q, %v", name, data, err)
		}
	}
	for _, name := range []string{".git", ".temp"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("sandbox must exclude %q: %v", name, err)
		}
	}
}
