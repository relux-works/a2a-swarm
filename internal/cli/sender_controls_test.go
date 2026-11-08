package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CI-only known-violation controls, executed by the existing Go workflow. Each
// isolated overlay must fail the named identity assertion between passing
// original/restored runs. Build failures, missing tests and timeouts do not
// qualify, and the checkout must remain byte-identical. No live service is used.
func TestSenderInstrumentControls(t *testing.T) {
	if os.Getenv("A2A_RUN_SENDER_CONTROLS") != "1" {
		t.Skip("CI-only isolated sender controls")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, plant := range []struct{ name, file, old, next, mask, assertion string }{
		{"invite-local-shortening", "wait.go", `inviter = ev.Sender`, `inviter = localpartIfLocal(ev.Sender, server)`,
			`^TestSenderInviteConsumers$/^alpha.example$`, "invite sender changed:"},
		{"invite-lossy-record", "delivery.go", `Sender: e.Sender`, `Sender: sanitizeLine(e.Sender)`,
			`^TestSenderInviteConsumers$/^alpha.example$`, "invite sender changed:"},
		{"invite-inbox-clipping", "inbox.go", `b.WriteString(it.line + "\n")`, `b.WriteString(inboxField(it.line) + "\n")`,
			`^TestSenderInviteConsumers$/^alpha.example$`, "invite inbox sender clipped or frame changed"},
		{"local-shortening", "json.go", `Sender:   ev.Sender,`, `Sender:   localpartIfLocal(ev.Sender, server),`,
			`^TestSenderReadConsumers$/^alpha.example$`, "event sender changed:"},
		{"claimed-header-origin", "json.go", `Sender:   ev.Sender,`, `Sender:   func() string { if h, ok := decodeHeader(m.A2A); ok { return h.From }; return ev.Sender }(),`,
			`^TestSenderReadConsumers$/^alpha.example$`, "event sender changed:"},
		{"inbox-sender-clipping", "inbox.go", `"from=" + senderText(rec.Sender)`, `"from=" + inboxField(rec.Sender)`,
			`^TestSenderLongInboxConsumers$/^inbox$`, "long inbox sender changed"},
		{"show-sender-clipping", "inbox_saved.go", `senderText(r.Sender), inboxField(r.Room)`, `inboxField(r.Sender), inboxField(r.Room)`,
			`^TestSenderLongInboxConsumers$/^saved$`, "long saved show sender changed"},
	} {
		t.Run(plant.name, func(t *testing.T) {
			path := filepath.Join(root, "internal", "cli", plant.file)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			originalHash := sha256.Sum256(original)
			t.Cleanup(func() {
				current, err := os.ReadFile(path)
				if err != nil || sha256.Sum256(current) != originalHash {
					t.Error("sender control changed original source")
				}
			})
			if strings.Count(string(original), plant.old) != 1 {
				t.Fatal("sender control anchor absent or ambiguous")
			}
			dir := t.TempDir()
			replacement := filepath.Join(dir, plant.file)
			if err := os.WriteFile(replacement, []byte(strings.Replace(string(original), plant.old, plant.next, 1)), 0600); err != nil {
				t.Fatal(err)
			}
			overlay := filepath.Join(dir, "overlay.json")
			data, err := json.Marshal(map[string]any{"Replace": map[string]string{path: replacement}})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(overlay, data, 0600); err != nil {
				t.Fatal(err)
			}
			run := func(overlay string) ([]byte, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
				defer cancel()
				args := []string{"test", "-p", "1", "-count=1", "-v", "-timeout", "2m"}
				if overlay != "" {
					args = append(args, "-overlay", overlay)
				}
				args = append(args, "./internal/cli", "-run", plant.mask)
				cmd := exec.CommandContext(ctx, "go", args...)
				cmd.Dir = root
				cmd.Env = append(os.Environ(), "GOMAXPROCS=2", "A2A_RUN_SENDER_CONTROLS=0")
				output, err := cmd.CombinedOutput()
				if ctx.Err() != nil {
					t.Fatal("sender control timed out")
				}
				return output, err
			}
			positive, err := run("")
			if err != nil || !bytes.Contains(positive, []byte("--- PASS: TestSender")) {
				t.Fatalf("sender control original positive failed: %v\n%s", err, positive)
			}
			negative, err := run(overlay)
			if err == nil || !bytes.Contains(negative, []byte("--- FAIL: TestSender")) || !bytes.Contains(negative, []byte(plant.assertion)) || bytes.Contains(negative, []byte("[build failed]")) || bytes.Contains(negative, []byte("test timed out")) {
				t.Fatalf("sender control missed intended assertion %q: %v\n%s", plant.assertion, err, negative)
			}
			restored, err := run("")
			if err != nil || !bytes.Contains(restored, []byte("--- PASS: TestSender")) {
				t.Fatalf("sender control restored positive failed: %v\n%s", err, restored)
			}
			t.Logf("intended FAIL observed (%s); original/restored PASS; source unchanged", plant.assertion)
		})
	}
}
