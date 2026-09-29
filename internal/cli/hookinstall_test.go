package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// installedFresh is the exact file `a2a hook install --agent alice` writes
// into a new settings file (documented in docs/cli.md).
const installedFresh = `{
  "env": {
    "A2A_AGENT": "alice"
  },
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/a2a --as alice hook session-start --runtime claude"
          }
        ]
      }
    ],
    "UserPromptSubmit": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/a2a --as alice hook prompt --runtime claude"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "/usr/local/bin/a2a --as alice hook stop --runtime claude"
          }
        ]
      }
    ]
  }
}
`

// userSettings is a pre-existing settings file with unrelated keys and hooks.
const userSettings = `{
  "permissions": {"allow": ["Bash(ls)"]},
  "env": {"OTHER": "1"},
  "hooks": {
    "Stop": [{"hooks": [{"type": "command", "command": "echo done && true"}]}],
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "x <y>"}]}],
    "UserPromptSubmit": [{"hooks": [
      {"type": "command", "command": "/usr/local/bin/a2a --as alice hook prompt"},
      {"type": "command", "command": "mine too"}]}]
  },
  "model": "opus"
}
`

func (e *env) exePath(p string) {
	e.configure = func(a *App) { a.exe = func() (string, error) { return p, nil } }
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func compactJSON(t *testing.T, s string) string {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, []byte(s)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// Proves install writes exactly the documented settings into a new file with
// mode 0600 (creating the directory), without contacting the homeserver, and
// that re-running it changes nothing (byte-identical, "unchanged").
func TestHookInstallFresh(t *testing.T) {
	e := newEnv(t)
	e.exePath("/usr/local/bin/a2a")
	path := filepath.Join(t.TempDir(), ".claude", "settings.local.json")
	tuitestkit.ContainsStr(t, e.run("hook", "install", "--runtime", "claude", "--agent", "alice", "--settings", path).ok(t).stdout, "installed")
	if got := readFile(t, path); got != installedFresh {
		t.Fatalf("installed:\n%s", got)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	tuitestkit.ContainsStr(t, e.run("hook", "install", "--runtime", "claude", "--agent", "alice", "--settings", path).ok(t).stdout, "unchanged")
	if got := readFile(t, path); got != installedFresh {
		t.Fatal("re-run changed the file")
	}
	if n := len(e.hs.Calls()); n != 0 {
		t.Fatalf("install reached the homeserver: %d requests", n)
	}
}

// Proves install merges into an existing file: unrelated keys, their order,
// the file mode, other hooks (including a group that merely contains our
// command next to another one) are kept; env.A2A_AGENT and one entry per event
// are added; re-running is byte-identical; a moved binary replaces our entry
// in place instead of adding a second one.
func TestHookInstallMergesIdempotently(t *testing.T) {
	e := newEnv(t)
	e.exePath("/opt/a2a tools/a2a")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(userSettings), 0o640); err != nil {
		t.Fatal(err)
	}
	os.Chmod(path, 0o640)
	e.run("hook", "install", "--runtime", "claude", "--settings", path, "--agent", "alice").ok(t)
	first := readFile(t, path)
	var got struct {
		Permissions json.RawMessage              `json:"permissions"`
		Env         map[string]string            `json:"env"`
		Model       string                       `json:"model"`
		Hooks       map[string][]json.RawMessage `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(first), &got); err != nil {
		t.Fatal(err)
	}
	if got.Env["OTHER"] != "1" || got.Env["A2A_AGENT"] != "alice" || got.Model != "opus" || compactJSON(t, string(got.Permissions)) != `{"allow":["Bash(ls)"]}` {
		t.Fatalf("unrelated keys changed: %s", first)
	}
	stop := `{"hooks":[{"type":"command","command":"'/opt/a2a tools/a2a' --as alice hook stop --runtime claude"}]}`
	if len(got.Hooks["Stop"]) != 2 || compactJSON(t, string(got.Hooks["Stop"][0])) != `{"hooks":[{"type":"command","command":"echo done && true"}]}` ||
		compactJSON(t, string(got.Hooks["Stop"][1])) != stop || len(got.Hooks["PreToolUse"]) != 1 ||
		len(got.Hooks["UserPromptSubmit"]) != 2 || len(got.Hooks["SessionStart"]) != 1 {
		t.Fatalf("hooks = %s", first)
	}
	keys := []string{`"permissions"`, `"env"`, `"hooks"`, `"model"`}
	for i := 1; i < len(keys); i++ {
		if bytes.Index([]byte(first), []byte(keys[i-1])) > bytes.Index([]byte(first), []byte(keys[i])) {
			t.Fatalf("key order changed: %s", first)
		}
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", st.Mode())
	}
	e.run("hook", "install", "--runtime", "claude", "--settings", path, "--agent", "alice").ok(t)
	if readFile(t, path) != first {
		t.Fatal("re-run is not byte-identical")
	}
	e.exePath("/usr/local/bin/a2a")
	e.run("hook", "install", "--runtime", "claude", "--settings", path, "--agent", "alice").ok(t)
	if err := json.Unmarshal([]byte(readFile(t, path)), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Hooks["Stop"]) != 2 || compactJSON(t, string(got.Hooks["Stop"][1])) != `{"hooks":[{"type":"command","command":"/usr/local/bin/a2a --as alice hook stop --runtime claude"}]}` {
		t.Fatalf("moved binary: %s", got.Hooks["Stop"])
	}
}

// Proves uninstall removes exactly what install added (our three entries and
// our env.A2A_AGENT): the result equals the original settings; a second
// uninstall and an uninstall of a missing file change nothing (no file is created).
func TestHookUninstall(t *testing.T) {
	e := newEnv(t)
	e.exePath("/usr/local/bin/a2a")
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(userSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	e.run("hook", "install", "--runtime", "claude", "--settings", path, "--agent", "alice").ok(t)
	tuitestkit.ContainsStr(t, e.run("hook", "uninstall", "--runtime", "claude", "--settings", path).ok(t).stdout, "removed 3 a2a hook entries")
	if compactJSON(t, readFile(t, path)) != compactJSON(t, userSettings) {
		t.Fatalf("uninstall left:\n%s", readFile(t, path))
	}
	before := readFile(t, path)
	tuitestkit.ContainsStr(t, e.run("hook", "uninstall", "--runtime", "claude", "--settings", path).ok(t).stdout, "unchanged")
	if readFile(t, path) != before {
		t.Fatal("second uninstall changed the file")
	}
	fresh := filepath.Join(t.TempDir(), "settings.local.json")
	e.run("hook", "install", "--runtime", "claude", "--settings", fresh, "--agent", "alice").ok(t)
	e.run("hook", "uninstall", "--runtime", "claude", "--settings", fresh).ok(t)
	if got := readFile(t, fresh); got != "{}\n" {
		t.Fatalf("fresh uninstall left %q", got)
	}
	missing := filepath.Join(t.TempDir(), "none.json")
	tuitestkit.ContainsStr(t, e.run("hook", "uninstall", "--runtime", "claude", "--settings", missing).ok(t).stdout, "no such file")
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("uninstall created a file")
	}
}

// Proves install refuses bad input and leaves the file untouched: an invalid
// or missing agent name and stray arguments (usage, exit 2), a settings file
// that is not a JSON object, a "hooks" that is not an object, and a symlink
// (exit 1); a valid install next to them succeeds (positive control).
func TestHookInstallNegative(t *testing.T) {
	e := newEnv(t)
	e.exePath("/usr/local/bin/a2a")
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	e.run("hook", "install", "--runtime", "claude", "--agent", "Bad Name", "--settings", path).failsWith(t, ExitUsage, "invalid agent name")
	e.run("hook", "install", "--runtime", "claude", "--settings", path).failsWith(t, ExitUsage, "--agent is required")
	e.run("hook", "install", "--runtime", "claude", "--agent", "alice", "--settings", path, "extra").failsWith(t, ExitUsage, "usage")
	e.run("hook", "uninstall", "--runtime", "claude", "--agent", "alice").failsWith(t, ExitUsage, "flag provided but not defined")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused install wrote the file")
	}
	for name, content := range map[string]string{"array.json": `[]`, "hooks.json": `{"hooks": []}`, "broken.json": `{"a":`} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o600)
		e.run("hook", "install", "--runtime", "claude", "--agent", "alice", "--settings", p).failsWith(t, ExitError, "refusing to change it")
		if readFile(t, p) != content {
			t.Fatalf("%s changed", name)
		}
	}
	link := filepath.Join(dir, "link.json")
	os.Symlink(filepath.Join(dir, "array.json"), link)
	e.run("hook", "install", "--runtime", "claude", "--agent", "alice", "--settings", link).failsWith(t, ExitError, "not a regular file")
	e.run("hook", "install", "--runtime", "claude", "--agent", "alice", "--settings", path).ok(t)
}
