package matrix

import (
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

// Proves the exact-ID, full-sender and raw-content assertions detect known
// violations. Each isolated overlay must fail the named behavioral assertion;
// compilation errors, missing tests and timeouts do not qualify as detection.
func TestVerificationEventInstrumentControls(t *testing.T) {
	if os.Getenv("A2A_RUN_VERIFICATION_CONTROLS") != "1" {
		t.Skip("opt-in isolated source controls")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "internal/matrix/verification.go")
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []struct{ name, old, next, mask, assertion string }{
		{"omit-returned-id", `id != eventID`, `false`, `^TestVerificationEventRefusals$/^event-mismatch$`, `accepted invalid response (event-mismatch)`},
		{"shorten-sender", `Sender: sender,`, `Sender: strings.SplitN(strings.TrimPrefix(sender, "@"), ":", 2)[0],`, `^TestVerificationEventExactObservation$`, `incorrect observation`},
		{"normalize-body", `result.Content = append(json.RawMessage(nil), spans["content"]...)`, `result.Content = json.RawMessage(strings.ReplaceAll(string(spans["content"]), ` + "`\\r\\n`" + `, ` + "`\\n`" + `))`, `^TestVerificationEventExactObservation$`, `incorrect observation`},
	} {
		t.Run(p.name, func(t *testing.T) {
			if strings.Count(string(original), p.old) != 1 {
				t.Fatal("plant anchor absent or ambiguous")
			}
			dir := t.TempDir()
			replacement := filepath.Join(dir, "verification.go")
			if err := os.WriteFile(replacement, []byte(strings.Replace(string(original), p.old, p.next, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(dir, "overlay.json")
			data, _ := json.Marshal(map[string]any{"Replace": map[string]string{source: replacement}})
			if err := os.WriteFile(overlay, data, 0600); err != nil {
				t.Fatal(err)
			}
			run := func(overlay, mask string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				defer cancel()
				args := []string{"test", "-p", "1", "-count=1", "-v"}
				if overlay != "" {
					args = append(args, "-overlay", overlay)
				}
				args = append(args, "./internal/matrix", "-run", mask)
				cmd := exec.CommandContext(ctx, "go", args...)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "GOMAXPROCS=2")
				var out bytes.Buffer
				cmd.Stdout = &out
				cmd.Stderr = &out
				if err := cmd.Start(); err != nil {
					return nil, err
				}
				t.Logf("owned child pid=%d overlay=%t", cmd.Process.Pid, overlay != "")
				err := cmd.Wait()
				if ctx.Err() != nil {
					t.Fatalf("control timed out: %s", out.String())
				}
				return out.Bytes(), err
			}
			positive, err := run("", p.mask)
			if err != nil || !bytes.Contains(positive, []byte("--- PASS:")) {
				t.Fatalf("pre-plant positive did not pass: %v\n%s", err, positive)
			}
			negative, err := run(overlay, p.mask)
			if err == nil || !bytes.Contains(negative, []byte("--- FAIL:")) || !bytes.Contains(negative, []byte(p.assertion)) {
				t.Fatalf("intended control assertion not reached: %v\n%s", err, negative)
			}
			restored, err := run("", p.mask)
			if err != nil || !bytes.Contains(restored, []byte("--- PASS:")) {
				t.Fatalf("restored positive did not pass: %v\n%s", err, restored)
			}
			t.Logf("intended FAIL observed, original source unchanged, restored positive PASS\n%s", negative)
		})
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("source changed during isolated controls")
	}
}
