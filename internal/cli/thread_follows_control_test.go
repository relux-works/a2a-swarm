package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Proves the post/no-follow checker detects an intentionally restored implicit
// participation grant while explicit delivery still works. A Go overlay confines
// the plant to a disposable source fixture; the real checkout is never mutated.
func TestExplicitThreadImplicitFollowControl(t *testing.T) {
	if os.Getenv("A2A_RUN_EXPLICIT_THREAD_CONTROL") != "1" {
		t.Skip("opt-in bounded source control")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(root, "internal", "config", "thread_follows.go")
	source, err := os.ReadFile(original)
	if err != nil {
		t.Fatal(err)
	}
	anchor := "func (s State) FollowsThread(room, root string) bool {"
	if strings.Count(string(source), anchor) != 1 {
		t.Fatal("implicit-follow plant anchor missing")
	}
	mutant := strings.Replace(string(source), anchor, anchor+"\n\tif s.HasThread(root) { return true } // bounded intentional violation", 1)
	dir := t.TempDir()
	fixture := filepath.Join(dir, "thread_follows.go")
	overlay := filepath.Join(dir, "overlay.json")
	mapping, _ := json.Marshal(map[string]any{"Replace": map[string]string{original: fixture}})
	if err := os.WriteFile(overlay, mapping, 0600); err != nil {
		t.Fatal(err)
	}
	const negative = "TestExplicitThreadParticipationNeverFollows/answer"
	const positive = "TestExplicitThreadFollowDelivery"
	for _, planted := range []bool{true, false} {
		code := source
		if planted {
			code = []byte(mutant)
		}
		if err := os.WriteFile(fixture, code, 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		cmd := exec.CommandContext(ctx, "go", "test", "-p", "2", "-overlay", overlay, "./internal/cli", "-json", "-count=1", "-run", "^(TestExplicitThreadParticipationNeverFollows|TestExplicitThreadFollowDelivery)$/^answer$")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "A2A_RUN_EXPLICIT_THREAD_CONTROL=", "A2A_MATRIX_IT=", "A2A_ALITE_SENTINEL_RUN=", "A2A_PHASE1_SENTINEL_RUN=")
		out, runErr := cmd.CombinedOutput()
		cancel()
		exitCode := 0
		if runErr != nil {
			ee, ok := runErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("control setup failed: %v\n%s", runErr, out)
			}
			exitCode = ee.ExitCode()
		}
		verdicts := map[string]string{}
		intended := false
		scan := bufio.NewScanner(bytes.NewReader(out))
		for scan.Scan() {
			var ev struct{ Action, Test, Output string }
			if json.Unmarshal(scan.Bytes(), &ev) != nil {
				continue
			}
			if ev.Action == "pass" || ev.Action == "fail" {
				verdicts[ev.Test] = ev.Action
			}
			if ev.Test == negative && strings.Contains(ev.Output, "implicit follow admitted thread-only mail") {
				intended = true
			}
		}
		if scan.Err() != nil {
			t.Fatal(scan.Err())
		}
		t.Logf("plant=%v command=go test -p 2 -overlay <isolated fixture> ./internal/cli -json -count=1 -run <negative|positive> exit=%d\n%s", planted, exitCode, out)
		if verdicts[positive] != "pass" {
			t.Fatalf("valid nearby delivery did not pass: %v", verdicts)
		}
		if planted {
			if exitCode != 1 || verdicts[negative] != "fail" || !intended {
				t.Fatalf("plant not detected for intended violation: %v", verdicts)
			}
		} else if exitCode != 0 || verdicts[negative] != "pass" {
			t.Fatalf("restored fixture failed: %v", verdicts)
		}
	}
	final, err := os.ReadFile(original)
	if err != nil || !bytes.Equal(final, source) {
		t.Fatal("real source changed during isolated plant")
	}
}
