package cli

import (
	"encoding/json"
	"fmt"
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
			root := principalDMTestRoot(t)
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
				assertDMCanonicalCodexSpellings(t, text, "/usr/local/bin/a2a", "a2a", "bin/a2a", "./bin/a2a")
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
				if strings.Count(text, `decision = "forbidden"`) != 20 || strings.Contains(text, `"--as"`) || strings.Contains(text, `"status"`) || strings.Contains(text, `"bind"`) || strings.Contains(text, `"say"`) {
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

// Every spelling must have every canonical terminal prefix exactly once;
// counting occurrences alone could accept duplicates while missing a verb.
func assertDMCanonicalCodexSpellings(t *testing.T, rules string, aliases ...string) {
	t.Helper()
	for _, alias := range aliases {
		for _, command := range [][2]string{{"owner-dm", "ensure"}, {"owner-dm", "set"}, {"owner-dm", "unset"}, {"principal-dm", "ensure"}, {"principal-dm", "unset"}} {
			prefix := fmt.Sprintf("prefix_rule(pattern = [%s, %q, %q], decision = \"forbidden\",", marshalNoEscape(alias), command[0], command[1])
			if strings.Count(rules, prefix) != 1 {
				t.Fatalf("known executable spelling requires exactly one canonical denial for %s %s: %q", command[0], command[1], alias)
			}
		}
		if strings.Count(rules, string(marshalNoEscape(alias))) != 5 {
			t.Fatalf("unexpected extra spelling coverage outside canonical DM prefixes: %q", alias)
		}
	}
	for _, forbidden := range []string{`"--as"`, `"status"`, `"say"`, `"bind"`, `"unbind"`} {
		if strings.Contains(rules, forbidden) {
			t.Fatalf("rules contain noncanonical or permitted action %s", forbidden)
		}
	}
}

// Duplicate/empty aliases add no rules; spaces and both quote characters stay
// a single executable token. Reinstallation must preserve the exact bytes.
func TestOwnerDMCodexExecutableSpellings(t *testing.T) {
	file := filepath.Join(t.TempDir(), ".codex", "hooks.json")
	exe := "/opt/images/a2a"
	alias := "/opt/installed/a2a space'and\"quote"
	install := func() {
		t.Helper()
		if err := ownerCodexRules(file, exe, "bob", true, alias, exe, alias, "a2a", ""); err != nil {
			t.Fatal(err)
		}
	}
	install()
	rules := filepath.Join(filepath.Dir(file), "rules", "a2a-owner-dm.rules")
	before := readFile(t, rules)
	assertDMCanonicalCodexSpellings(t, before, exe, alias, "a2a", "bin/a2a", "./bin/a2a")
	if strings.Count(before, `decision = "forbidden"`) != 25 {
		t.Fatal("unexpected rule count")
	}
	install()
	if readFile(t, rules) != before {
		t.Fatal("alias rule install is not byte-idempotent")
	}
}
