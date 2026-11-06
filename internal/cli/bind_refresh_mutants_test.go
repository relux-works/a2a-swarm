//go:build darwin || linux

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The opt-in source plant removes the actual safe-point verdict at poll's exec
// admission. Only the unsafe-attempt assertion kills it; crashes, skips, absent
// tests and setup failures do not count. A settled positive must still pass.
func TestTerminalRefreshSafePointPlant(t *testing.T) {
	isolateBindTest(t)
	if os.Getenv("A2A_RUN_BIND_REFRESH_MUTANTS") != "1" {
		t.Skip("opt-in isolated re-exec safe-point control")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum", "internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			target := filepath.Join(dst, rel)
			if d.IsDir() {
				return os.MkdirAll(target, 0700)
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(target, b, 0600)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dst, "internal/cli/bind_refresh.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	old := "safe = terminalRefreshSafe(st, generation, birth, a.Now())"
	if strings.Count(string(raw), old) != 1 {
		t.Fatal("setup: nonunique safe-point plant anchor")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), old, old+" || true", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, run := range []struct {
		name string
		red  bool
	}{{"attempt", true}, {"settled", false}} {
		target := "TestTerminalRefreshSafePoint/" + run.name
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "./internal/cli", "-run", "^TestTerminalRefreshSafePoint$/^"+run.name+"$")
		cmd.Dir = dst
		cmd.Env = append(os.Environ(), "A2A_RUN_BIND_REFRESH_MUTANTS=", "A2A_MATRIX_IT=")
		out, err := cmd.CombinedOutput()
		timeout := ctx.Err() != nil
		cancel()
		reached, asserted, bad := false, false, false
		decoder := json.NewDecoder(bytes.NewReader(out))
		for decoder.More() {
			var event struct{ Action, Test, Output string }
			if err := decoder.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event.Test == target && (run.red && event.Action == "fail" || !run.red && event.Action == "pass") {
				reached = true
			}
			if event.Test == target && strings.Contains(event.Output, "unsafe re-exec gate") {
				asserted = true
			}
			if strings.Contains(event.Output, "panic:") || strings.Contains(event.Output, "setup:") || event.Action == "skip" {
				bad = true
			}
		}
		exit := 0
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if err != nil {
			exit = -1
		}
		t.Logf("safe-point plant test=%s exit=%d\n%s", target, exit, out)
		if timeout || bad || !reached || run.red && (exit != 1 || !asserted) || !run.red && exit != 0 {
			t.Fatalf("SURVIVOR/unproved safe-point plant: %s exit=%d intended=%t", target, exit, asserted)
		}
	}
}
