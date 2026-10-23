package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Test 1: no config, including a pin alone, leaves these concrete command
// outputs identical. This test also runs unchanged on pre-F1 0fd5f36c; logged
// fingerprints compare that base with the candidate, including archive bytes.
func TestOwnerDMLegacyCompatibility(t *testing.T) {
	for _, command := range []string{"inbox", "wait", "watch", "read", "files", "archive", "hook"} {
		t.Run(command, func(t *testing.T) {
			fingerprints := []string{}
			normalized := []string{}
			for _, pin := range []bool{false, true} {
				e, room := world(t)
				e.sequentialSessions()
				fixed := time.Date(2026, 9, 29, 10, 0, 20, 0, time.UTC)
				started := time.Now()
				e.now = func() time.Time { return fixed.Add(time.Since(started)) }
				if pin {
					dir := filepath.Join(e.dir, "owner-dm")
					os.MkdirAll(dir, 0700)
					os.WriteFile(filepath.Join(dir, "bob.pin"), []byte(`{"mode":"owner-channel","owner":"@alice:test.local"}`), 0600)
				}
				e.as("bob", "subscribe", "--room", room, "--kind", "notice").ok(t)
				e.as("bob", "wait", "--timeout", "1ms").ok(t)
				e.hs.inject(room, "@alice:"+testServer, map[string]any{"msgtype": "m.text", "body": "legacy control", "ai.relux.a2a": map[string]any{"v": 1, "type": "message", "kind": "notice", "to": []string{"bob"}}})
				var r result
				archiveDir := t.TempDir()
				archiveBytes := ""
				switch command {
				case "inbox":
					r = e.as("bob", "inbox", "--json")
				case "wait":
					r = e.as("bob", "wait", "--timeout", "2s", "--json")
				case "watch":
					ctx, cancel := context.WithTimeout(context.Background(), time.Second)
					e.configure = func(a *App) { a.afterPrint = func() error { cancel(); return nil } }
					r = e.runCtx(ctx, "--as", "bob", "watch", "--json")
					cancel()
				case "read":
					r = e.as("bob", "read", room, "--json")
				case "files":
					r = e.as("bob", "files", room, "--json")
				case "archive":
					r = e.as("bob", "archive", room, "--dir", archiveDir, "--json-index")
					filepath.WalkDir(archiveDir, func(path string, d os.DirEntry, err error) error {
						if err != nil {
							return err
						}
						if !d.IsDir() {
							b, err := os.ReadFile(path)
							if err != nil {
								return err
							}
							rel, _ := filepath.Rel(archiveDir, path)
							archiveBytes += rel + "\n" + string(b)
						}
						return nil
					})
				case "hook":
					e.stdin = `{"session_id":"legacy-control","stop_hook_active":false,"hook_event_name":"Stop"}`
					r = e.as("bob", "hook", "stop", "--runtime", "claude")
				}
				if r.code != 0 {
					t.Fatal(command, r)
				}
				if (command == "inbox" || command == "wait" || command == "watch" || command == "read") && !strings.Contains(r.stdout, "legacy control") {
					t.Fatal("compatibility control did not exercise a message", command, r)
				}
				// Subscription IDs are random presentation data; normalize just their
				// values, keeping every other output byte and ordering unchanged.
				var list struct {
					Subs []struct {
						ID string `json:"id"`
					} `json:"subs"`
				}
				json.Unmarshal(e.hs.accountDataOf("@bob:"+testServer, SubsType), &list)
				output := fmt.Sprintf("%d\n%s\n%s\n%s", r.code, r.stdout, r.stderr, archiveBytes)
				output = strings.ReplaceAll(output, archiveDir, "<archive>")
				output = strings.ReplaceAll(output, e.dir, "<config>")
				for _, sub := range list.Subs {
					output = strings.ReplaceAll(output, sub.ID, "<sub>")
				}
				normalized = append(normalized, output)
				fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(output)))
				fingerprints = append(fingerprints, fingerprint)
				if len(e.hs.createdRooms()) != 0 {
					t.Fatal("legacy command created owner room")
				}
				items, _ := os.ReadDir(filepath.Join(e.dir, "owner-dm"))
				want := 0
				if pin {
					want = 1
				}
				if len(items) != want {
					t.Fatal("never-configured command provisioned files")
				}
			}
			if fingerprints[0] != fingerprints[1] {
				t.Fatalf("pin changed legacy output: %v\n%q\n%q", fingerprints, normalized[0], normalized[1])
			}
			t.Logf("legacy %s sha256 %s", command, fingerprints[0])
		})
	}
}
