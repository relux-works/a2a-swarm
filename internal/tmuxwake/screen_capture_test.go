package tmuxwake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two sanitized real Claude input/footer captures, and one explicitly
// synthetic empty-cursor variant, admit idle without swallowing real drafts.
// This tests parsing, not live session delivery or terminal atomicity.
func TestClaudeCapturedInputs(t *testing.T) {
	files, err := filepath.Glob("testdata/claude-idle-*.txt")
	if err != nil || len(files) != 3 {
		t.Fatalf("fixture inventory: %v %d", err, len(files))
	}
	for _, file := range files {
		t.Run(filepath.Base(file), func(t *testing.T) {
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			screen := string(raw)
			box, ok := Input(screen, "claude")
			if !ok || box.Text != "" {
				t.Fatalf("idle capture rejected: %+v/%v", box, ok)
			}
			lines := strings.Split(screen, "\n")
			prompt := -1
			for i, line := range lines {
				if strings.HasPrefix(plain(line), "❯") {
					prompt = i
					break
				}
			}
			if prompt < 1 {
				t.Fatal("missing captured input")
			}
			for _, draft := range []string{"❯\u00a0KEEP plain draft", "❯\u00a0\x1b[7mK\x1b[0mEEP plain draft", "❯\u00a0\x1b[7mKEEP\x1b[0;2msuggestion\x1b[0m"} {
				copy := append([]string(nil), lines...)
				copy[prompt] = draft
				got, ok := Input(strings.Join(copy, "\n"), "claude")
				if ok && got.Text == "" {
					t.Fatalf("human draft treated as empty: %q", draft)
				}
			}
			for _, dialog := range []string{"Do you want to allow this tool?", "Permission required", "Would you like to implement this plan?"} {
				copy := append([]string(nil), lines...)
				copy[prompt-2] = dialog
				if _, ok := Input(strings.Join(copy, "\n"), "claude"); ok {
					t.Fatalf("dialog admitted: %s", dialog)
				}
			}
		})
	}
}
