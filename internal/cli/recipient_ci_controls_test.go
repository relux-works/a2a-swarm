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

// Source-mutation controls run only on the existing GitHub Actions test route.
// A compiler failure cannot count as a killed violation: the intended assertion
// must fail. No producer/local execution is needed to author these controls.
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
	if err := os.CopyFS(filepath.Join(dir, "internal"), os.DirFS(filepath.Join(root, "internal"))); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"go.mod", "go.sum"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
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
