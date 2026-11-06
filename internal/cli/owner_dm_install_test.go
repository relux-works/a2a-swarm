package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Test 17: hook wiring and denials depend only on runtime/binary/identity,
// never feature flags; principal-dm ensure/unset are denied like owner-dm
// terminal verbs while status/say stay allowed. Fresh and configured installations match; malformed
// permissions or a foreign rules file refuse without overwriting user bytes.
func TestOwnerDMInstallDenials(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e := newEnv(t)
			e.exePath("/usr/local/bin/a2a")
			root := t.TempDir()
			file := filepath.Join(root, "settings.json")
			if runtime == "codex" {
				file = filepath.Join(root, ".codex", "hooks.json")
			}
			e.run("hook", "install", "--agent", "bob", "--runtime", runtime, "--settings", file).ok(t)
			fresh := readFile(t, file)
			dir := filepath.Join(e.dir, "owner-dm")
			os.MkdirAll(dir, 0700)
			os.WriteFile(filepath.Join(dir, "bob.json"), []byte(`{"v":1,"bad":"invalid does not select hook wiring"}`), 0600)
			e.run("hook", "install", "--agent", "bob", "--runtime", runtime, "--settings", file).ok(t)
			if readFile(t, file) != fresh {
				t.Fatal("configuration selected a hook file")
			}
			if runtime == "claude" {
				var data struct {
					Permissions struct {
						Deny []string `json:"deny"`
					} `json:"permissions"`
				}
				if json.Unmarshal([]byte(fresh), &data) != nil || len(data.Permissions.Deny) != 10 {
					t.Fatal("missing deny rules")
				}
				for _, verb := range []string{"ensure", "set", "unset"} {
					if !contains(data.Permissions.Deny, "Bash(*owner-dm "+verb+")") || !contains(data.Permissions.Deny, "Bash(*owner-dm "+verb+" *)") {
						t.Fatal("terminal verb not denied", verb)
					}
				}
				for _, verb := range []string{"ensure", "unset"} {
					if !contains(data.Permissions.Deny, "Bash(*principal-dm "+verb+")") || !contains(data.Permissions.Deny, "Bash(*principal-dm "+verb+" *)") {
						t.Fatal("principal terminal verb not denied", verb)
					}
				}
				if contains(data.Permissions.Deny, "Bash(*principal-dm say *)") || contains(data.Permissions.Deny, "Bash(*principal-dm status)") {
					t.Fatal("agent verbs denied")
				}
				bad := filepath.Join(root, "bad.json")
				original := `{"permissions":{"deny":7}}`
				os.WriteFile(bad, []byte(original), 0600)
				e.run("hook", "install", "--agent", "bob", "--runtime", runtime, "--settings", bad).failsWith(t, ExitError, "string array")
				if readFile(t, bad) != original {
					t.Fatal("overwrote bad permissions")
				}
			} else {
				rules := filepath.Join(filepath.Dir(file), "rules", "a2a-owner-dm.rules")
				text := readFile(t, rules)
				for _, verb := range []string{"ensure", "set", "unset"} {
					if !strings.Contains(text, `"owner-dm", "`+verb+`"`) {
						t.Fatal("terminal rule missing", verb)
					}
				}
				for _, verb := range []string{"ensure", "unset"} {
					if !strings.Contains(text, `"principal-dm", "`+verb+`"`) {
						t.Fatal("principal terminal rule missing", verb)
					}
				}
				if strings.Count(text, `decision = "forbidden"`) != 10 || strings.Contains(text, `"status"`) || strings.Contains(text, `"bind"`) || strings.Contains(text, `"say"`) {
					t.Fatal("wrong deny scope")
				}
				e.run("hook", "install", "--agent", "bob", "--runtime", runtime, "--settings", file).ok(t)
				if readFile(t, rules) != text {
					t.Fatal("rule install not idempotent")
				}
				os.WriteFile(rules, []byte("# user rules\n"), 0600)
				e.run("hook", "install", "--agent", "bob", "--runtime", runtime, "--settings", file).failsWith(t, ExitError, "refusing to replace")
				if readFile(t, file) != fresh || readFile(t, rules) != "# user rules\n" {
					t.Fatal("foreign rules or hook changed")
				}
			}
		})
	}
}
