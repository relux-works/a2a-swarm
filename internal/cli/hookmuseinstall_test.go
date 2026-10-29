package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func museInstall(e *env, path, name string) result {
	return e.run("hook", "install", "--runtime", "muse", "--agent", name, "--settings", path)
}

// Proves the native project shape, absolute literal identity, one row/event,
// unchanged non-owned config/env/handlers, mode and byte-idempotent reinstall.
// No plugin, native trust storage or legacy owner rules are written.
func TestMuseInstallMergeAndUninstall(t *testing.T) {
	e := newEnv(t)
	e.exePath("/usr/local/with space/a2a")
	path := filepath.Join(t.TempDir(), DefaultMuseHooks)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"env":{"UNRELATED":"value","A2A_AGENT":"unrelated"},"unrelated":{"x":true},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo retained"}]}],"PreToolUse":[]}}`
	if err := os.WriteFile(path, []byte(original), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	r := museInstall(e, path, "alice").ok(t)
	first := readFile(t, path)
	if !strings.Contains(r.stdout, "native human trust") || strings.Contains(first, "permissions") || !strings.Contains(first, `--as alice hook prompt --runtime muse`) || !strings.Contains(first, "'/usr/local/with space/a2a'") {
		t.Fatalf("wrong install: %s %+v", first, r)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(first), &got); err != nil {
		t.Fatal(err)
	}
	if compactJSON(t, string(got["env"])) != `{"UNRELATED":"value","A2A_AGENT":"unrelated"}` {
		t.Fatal("env changed")
	}
	rt, _, err := newEnvApp(e).ensureRuntime("muse")
	if err != nil || rt.settings != DefaultMuseHooks {
		t.Fatal("explicit runtime not supported")
	}
	var hooks map[string][]json.RawMessage
	json.Unmarshal(got["hooks"], &hooks)
	for _, ev := range hookEvents {
		owned := 0
		for _, raw := range hooks[ev.name] {
			if n, ok := ourMuseHookGroup(raw, ev); ok && n == "alice" {
				owned++
			}
		}
		if owned != 1 {
			t.Fatalf("%s owned rows %d", ev.name, owned)
		}
	}
	if !strings.Contains(museInstall(e, path, "alice").ok(t).stdout, "unchanged") || readFile(t, path) != first {
		t.Fatal("not idempotent")
	}
	// Duplicate only an exactly owned row: the installer must collapse it.
	root, _ := parseJSONObject([]byte(first))
	h, _ := root.childObject("hooks")
	h.set("SessionStart", rawArray(append(hooks["SessionStart"], hooks["SessionStart"][0])))
	root.set("hooks", h.marshal())
	os.WriteFile(path, root.marshal(), 0640)
	museInstall(e, path, "alice").ok(t)
	if readFile(t, path) != first {
		t.Fatal("duplicate owned row retained")
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0640 {
		t.Fatal("mode changed")
	}
	e.as("alice", "hook", "uninstall", "--runtime", "muse", "--settings", path).ok(t)
	var after map[string]any
	var before map[string]any
	json.Unmarshal([]byte(readFile(t, path)), &after)
	json.Unmarshal([]byte(original), &before)
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("removed foreign config: %s", readFile(t, path))
	}
	second := readFile(t, path)
	e.as("alice", "hook", "uninstall", "--runtime", "muse", "--settings", path).ok(t)
	if readFile(t, path) != second {
		t.Fatal("second uninstall changed config")
	}
	for _, p := range []string{"trust.json", "owner-rules.json", "../.codex/AGENTS.md"} {
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), p)); !os.IsNotExist(err) {
			t.Fatalf("unexpected native/legacy file %s", p)
		}
	}
}

func newEnvApp(e *env) *App { return &App{Getenv: func(k string) string { return e.extra[k] }} }

// Malformed/duplicate/non-array config and symlinks refuse without writes.
// Adjacent valid config installation is exercised above and after every case.
func TestMuseInstallRefusals(t *testing.T) {
	for _, bad := range []string{`null`, `[]`, `{`, `{} garbage`, `{"hooks":null}`, `{"hooks":{"Stop":null}}`, `{"hooks":{"Stop":{}}}`, `{"hooks":{},"hooks":{}}`, `{"hooks":{"Stop":[]},"env":{"x":1,"x":2}}`} {
		t.Run(bad, func(t *testing.T) {
			e := newEnv(t)
			e.exePath("/abs/a2a")
			path := filepath.Join(t.TempDir(), "hooks.json")
			os.WriteFile(path, []byte(bad), 0600)
			museInstall(e, path, "alice").failsWith(t, ExitError, "refusing")
			if readFile(t, path) != bad {
				t.Fatal("failed install wrote")
			}
			os.WriteFile(path, []byte(`{}`), 0600)
			museInstall(e, path, "alice").ok(t)
		})
	}
	e := newEnv(t)
	e.exePath("/abs/a2a")
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	os.WriteFile(target, []byte(`{}`), 0600)
	link := filepath.Join(dir, "link")
	os.Symlink(target, link)
	museInstall(e, link, "alice").failsWith(t, ExitError, "not a regular file")
	if readFile(t, target) != `{}` {
		t.Fatal("symlink target changed")
	}
}

// Established ownership guard: bound alice refuses bob and foreign owned
// rows; unbound explicit --agent bob is valid. Never reject all explicit IDs.
func TestMuseInstallIdentityOwnership(t *testing.T) {
	e := newEnv(t)
	e.exePath("/abs/a2a")
	path := filepath.Join(t.TempDir(), "hooks.json")
	e.extra[config.EnvAgent] = "alice"
	museInstall(e, path, "bob").failsWith(t, ExitUsage, "never act as another identity")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("foreign install wrote")
	}
	delete(e.extra, config.EnvAgent)
	museInstall(e, path, "bob").ok(t)
	original := readFile(t, path)
	museInstall(e, path, "alice").failsWith(t, ExitError, "another identity")
	e.as("alice", "hook", "uninstall", "--runtime", "muse", "--settings", path).failsWith(t, ExitError, "another identity")
	if readFile(t, path) != original {
		t.Fatal("foreign hook ownership changed")
	}
	e.extra[config.EnvAgent] = "bob"
	e.as("alice", "hook", "uninstall", "--runtime", "muse", "--settings", path).failsWith(t, ExitUsage, "never act as another identity")
	e.as("bob", "hook", "uninstall", "--runtime", "muse", "--settings", path).ok(t)
}

// Installer only reads the live lease and pause state, warns and preserves
// both byte-for-byte. No consumer, forced release or silent resume is created.
func TestMuseInstallLeaseAndPause(t *testing.T) {
	e, _ := world(t)
	e.exePath("/abs/a2a")
	path := filepath.Join(t.TempDir(), DefaultMuseHooks)
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Lease = &config.Lease{Holder: "foreign", Host: machineLabel(), Expires: time.Now().Add(time.Hour)}
		st.HooksPaused = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := e.state("alice")
	for i := 0; i < 2; i++ {
		r := museInstall(e, path, "alice").ok(t)
		if !strings.Contains(r.stdout, "lease is held") || !strings.Contains(r.stdout, "remain paused") || !reflect.DeepEqual(before, e.state("alice")) {
			t.Fatalf("changed owner/pause: %+v", r)
		}
	}
}

// The existing identity ensure dispatch selects Muse and installs its project
// config idempotently. Guidance explicitly leaves idle/context/session gates
// unqualified instead of advertising a nonexistent binding controller.
func TestMuseEnsure(t *testing.T) {
	e := newEnv(t)
	r := e.ensure("--role", "dev", "--project", "sample", "--runtime", "muse").ok(t)
	if !strings.Contains(r.stdout, "does not arm idle wake") || !strings.Contains(r.stdout, "same-session") {
		t.Fatalf("missing limits: %+v", r)
	}
	path := filepath.Join(e.projDir, DefaultMuseHooks)
	first := readFile(t, path)
	e.ensure("--role", "dev", "--project", "sample", "--runtime", "muse").ok(t)
	if readFile(t, path) != first {
		t.Fatal("ensure not idempotent")
	}
}
