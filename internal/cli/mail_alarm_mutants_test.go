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

// CI-only opt-in control removes the actual episode gate. Credit requires the
// specific dedupe assertion to fail and the independent destination control to
// pass; setup errors, missing plants, skips and crashes never kill the mutant.
func TestMailAlarmDedupeMutation(t *testing.T) {
	if os.Getenv("A2A_RUN_MAIL_ALARM_MUTANTS") != "1" {
		t.Skip("CI opt-in mutation control")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum", "internal", "cmd"} {
		err = filepath.WalkDir(filepath.Join(root, name), func(path string, d fs.DirEntry, err error) error {
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
	path := filepath.Join(dst, "internal/cli/mail_alarm.go")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	anchor := "state.Invalid || state.Active ||"
	if strings.Count(string(b), anchor) != 1 {
		t.Fatal("setup: dedupe plant anchor not unique")
	}
	if err = os.WriteFile(path, []byte(strings.Replace(string(b), anchor, "state.Invalid ||", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, control := range []struct {
		name string
		red  bool
	}{{"TestMailAlarmDedupeAndRearm", true}, {"TestMailAlarmDestinations", false}} {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, "go", "test", "-json", "-count=1", "./internal/cli", "-run", "^"+control.name+"$")
		cmd.Dir = dst
		cmd.Env = append(os.Environ(), "A2A_RUN_MAIL_ALARM_MUTANTS=")
		out, runErr := cmd.CombinedOutput()
		timedOut := ctx.Err() != nil
		cancel()
		reached, intended, bad := false, false, false
		dec := json.NewDecoder(bytes.NewReader(out))
		for dec.More() {
			var event struct{ Action, Test, Output string }
			if err = dec.Decode(&event); err != nil {
				t.Fatal(err)
			}
			if event.Test == control.name && (control.red && event.Action == "fail" || !control.red && event.Action == "pass") {
				reached = true
			}
			if event.Test == control.name && strings.Contains(event.Output, "dedupe gate allowed repeat") {
				intended = true
			}
			if strings.Contains(event.Output, "panic:") || strings.Contains(event.Output, "setup:") || strings.Contains(event.Output, "cleanup:") {
				bad = true
			}
		}
		exit := 0
		if ee, ok := runErr.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else if runErr != nil {
			exit = -1
		}
		if timedOut || bad || !reached || control.red && (exit != 1 || !intended) || !control.red && exit != 0 {
			t.Fatalf("unproved dedupe plant: test=%s exit=%d\n%s", control.name, exit, out)
		}
	}
}
