package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// Bind fixtures launch subprocesses as well as in-process Apps. Keep both
// paths in private stores and let each fixture opt into its own terminal.
func isolateBindTest(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CLAUDE_CODE_") ||
			(strings.HasPrefix(key, "A2A_") && !strings.HasPrefix(key, "A2A_RUN_")) ||
			key == "TMUX" || key == "TMUX_PANE" || key == "CLAUDECODE" ||
			key == "CLAUDE_ENV_FILE" || key == "CLAUDE_SESSION_ID" ||
			key == "CLAUDE_PROJECT_DIR" || key == "CLAUDE_PID" || key == "CLAUDE_EFFORT" ||
			key == "CODEX_THREAD_ID" || key == "CODEX_ENV_FILE" {
			t.Setenv(key, "")
			if err := os.Unsetenv(key); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Preserve Go's default cache locations before HOME moves, while still
	// resolving the effective values with go env below. Explicit values win.
	if os.Getenv("GOPATH") == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOPATH", filepath.Join(home, "go"))
	}
	if os.Getenv("GOCACHE") == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			t.Fatal(err)
		}
		t.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
	}
	for _, key := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "A2A_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	t.Setenv("TEST_TELEMETRY_DIR", "")
	// GOTELEMETRY is a reported value, not an override. Seed the supported
	// mode file in the fixture's own UserConfigDir before even go env runs.
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	owner := os.Getenv("XDG_CONFIG_HOME")
	if runtime.GOOS == "darwin" || owner == "" {
		owner = os.Getenv("HOME")
	}
	rel, err := filepath.Rel(owner, config)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		t.Fatalf("fixture config escapes its store: %q (%v)", config, err)
	}
	telemetry := filepath.Join(config, "go", "telemetry")
	if err := os.MkdirAll(telemetry, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(telemetry, "mode"), []byte("off"), 0600); err != nil {
		t.Fatal(err)
	}
	for key, value := range goCacheEnv(t) {
		t.Setenv(key, value)
	}
}

var (
	goCacheOnce   sync.Once
	goCacheValues map[string]string
	goCacheErr    error
)

// goCacheEnv resolves GOMODCACHE, GOCACHE and GOPATH once, from the
// first fixture environment, with the original cache defaults pinned.
func goCacheEnv(t *testing.T) map[string]string {
	t.Helper()
	goCacheOnce.Do(func() {
		out, err := exec.Command("go", "env", "-json", "GOMODCACHE", "GOCACHE", "GOPATH").Output()
		if err != nil {
			goCacheErr = err
			return
		}
		goCacheErr = json.Unmarshal(out, &goCacheValues)
	})
	if goCacheErr != nil {
		t.Fatalf("resolve fixture Go caches: %v", goCacheErr)
	}
	for key, value := range goCacheValues {
		if value == "" {
			t.Fatalf("resolve fixture Go caches: %s is empty", key)
		}
	}
	return goCacheValues
}

// A subprocess starts with control-owned stores and terminal state, so this
// checks the fixture boundary even when the test runner starts with env -i.
func TestBindFixtureIsolation(t *testing.T) {
	isolateBindTest(t)
	stores := map[string]string{}
	for _, key := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "A2A_CONFIG_DIR"} {
		stores[key] = t.TempDir()
	}
	for _, root := range []string{filepath.Join(stores["HOME"], ".claude"), stores["CLAUDE_CONFIG_DIR"]} {
		if err := os.MkdirAll(filepath.Join(root, "sessions"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "sessions", "sentinel.json"), []byte(`{"sessionId":"inherited-store-sentinel"}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := json.Marshal(stores)
	if err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"sentinel-neighbor", "isolated"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, self, "-test.run=^TestBindIsolationChild$", "-test.v")
			cmd.Env = append(os.Environ(), "BIND_ISOLATION_CHILD="+mode, "BIND_ISOLATION_STORES="+string(raw),
				"TMUX=/synthetic/inherited,208,0", "TMUX_PANE=%208", "CLAUDECODE=1",
				"CLAUDE_CODE_SESSION_ID=ambient-session", "CLAUDE_CODE_FUTURE_VARIABLE=ambient-value",
				"CODEX_THREAD_ID=ambient-thread", "CLAUDE_ENV_FILE=/synthetic/inherited",
				"A2A_AGENT=ambient-agent", "A2A_SESSION_ID=ambient-session", "A2A_MATRIX_URL=https://synthetic.invalid")
			for key, path := range stores {
				cmd.Env = append(cmd.Env, key+"="+path)
			}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolation control %s: %v\n%s", mode, err, out)
			}
		})
	}
}

func TestBindFixtureNoTelemetry(t *testing.T) {
	// The fixture must restore the caller's environment when its test ends.
	keys := []string{"GOTELEMETRY", "TEST_TELEMETRY_DIR", "HOME", "XDG_CONFIG_HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "A2A_CONFIG_DIR", "GOMODCACHE", "GOCACHE", "GOPATH"}
	before := map[string]string{}
	present := map[string]bool{}
	for _, key := range keys {
		before[key], present[key] = os.LookupEnv(key)
	}
	t.Run("restore-environment", func(t *testing.T) {
		isolateBindTest(t)
		config, err := os.UserConfigDir()
		if err != nil {
			t.Fatal(err)
		}
		mode, err := os.ReadFile(filepath.Join(config, "go", "telemetry", "mode"))
		if err != nil || string(mode) != "off" {
			t.Fatalf("fixture mode: %q, %v", mode, err)
		}
	})
	for _, key := range keys {
		value, set := os.LookupEnv(key)
		if value != before[key] || set != present[key] {
			t.Fatalf("fixture leaked %s into caller environment", key)
		}
	}

	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	realGo, err = filepath.Abs(realGo)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	for _, dir := range []string{"bin", "home", "config", "tmp"} {
		if err := os.Mkdir(filepath.Join(root, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	// Check the mode at invocation and the store after the real Go tool. This
	// catches a late Setenv deterministically, without waiting for a sidecar.
	wrapper := `#!/bin/sh
set -eu
case "$(uname -s)" in
 Darwin) config="$HOME/Library/Application Support" ;;
 *) config="${XDG_CONFIG_HOME:-$HOME/.config}" ;;
esac
test "$(cat "$config/go/telemetry/mode")" = off || { echo 'Go invoked before fixture mode was off' >&2; exit 1; }
case "$config/" in "$TMPDIR/"*) ;; *) echo 'Go config escaped fixture temporary store' >&2; exit 1 ;; esac
printf '%s\n' "$config" >> "$BIND_ISOLATION_CONFIG_LOG"
printf '%s\n' "$*" >> "$BIND_ISOLATION_GO_LOG"
"$BIND_ISOLATION_REAL_GO" "$@"
test "$(find "$config/go/telemetry" -mindepth 1 ! -path "$config/go/telemetry/mode" -print)" = '' || { echo 'Go left telemetry artifacts' >&2; exit 1; }
`
	if err := os.WriteFile(filepath.Join(root, "bin", "go"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, self, "-test.run=^TestBindFixtureIsolation$", "-test.v")
	cmd.Env = append(os.Environ(), "GOTELEMETRY=on", "HOME="+filepath.Join(root, "home"),
		"XDG_CONFIG_HOME="+filepath.Join(root, "config"), "TMPDIR="+filepath.Join(root, "tmp"),
		"PATH="+filepath.Join(root, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"BIND_ISOLATION_REAL_GO="+realGo, "BIND_ISOLATION_GO_LOG="+filepath.Join(root, "go-calls"),
		"BIND_ISOLATION_CONFIG_LOG="+filepath.Join(root, "configs"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture lifecycle: %v\n%s", err, out)
	}
	calls, err := os.ReadFile(filepath.Join(root, "go-calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "env -json GOMODCACHE GOCACHE GOPATH\n") != 2 {
		t.Fatalf("missing parent/child real Go invocations: %s", calls)
	}
	configs, err := os.ReadFile(filepath.Join(root, "configs"))
	if err != nil || len(strings.Fields(string(configs))) == 0 {
		t.Fatalf("missing owned config checks: %v", err)
	}
	// CombinedOutput joins the owned fixture process after t.TempDir cleanup.
	// Both inherited config stores must still be empty, and no child fixture
	// directory may survive. There is no cleanup retry or timing allowance.
	for _, dir := range []string{"home", "config", "tmp"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Fatalf("fixture left files in %s: %v", dir, entries)
		}
	}
}

// Use fresh stores, including Linux's HOME fallback. The explicit child
// protocol lets us join the uploader ourselves instead of leaving a detached
// sidecar racing temporary-directory cleanup. It does not change mode lookup.
func TestBindFixtureTelemetryControl(t *testing.T) {
	for _, xdg := range []bool{true, false} {
		t.Run(map[bool]string{true: "xdg", false: "home-fallback"}[xdg], func(t *testing.T) {
			isolateBindTest(t)
			root := t.TempDir()
			t.Setenv("HOME", root)
			t.Setenv("XDG_CONFIG_HOME", "")
			if xdg {
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
			}
			config, err := os.UserConfigDir()
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(root, ".config")
			if xdg {
				want = filepath.Join(root, "config")
			}
			if runtime.GOOS == "darwin" {
				want = filepath.Join(root, "Library", "Application Support")
			}
			if config != want {
				t.Fatalf("UserConfigDir = %q, want %q", config, want)
			}
			for _, mode := range []string{"local", "off"} {
				t.Run(mode, func(t *testing.T) {
					// Every case gets a fresh store; no removal hides artifacts.
					store := t.TempDir()
					t.Setenv("HOME", store)
					if xdg {
						t.Setenv("XDG_CONFIG_HOME", filepath.Join(store, "config"))
					}
					config, err := os.UserConfigDir()
					if err != nil {
						t.Fatal(err)
					}
					telemetry := filepath.Join(config, "go", "telemetry")
					if mode == "off" {
						if err := os.MkdirAll(telemetry, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(telemetry, "mode"), []byte("off"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					t.Setenv("GOTELEMETRY", "off") // Deliberately ineffective env-only recipe in local control.
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					cmd := exec.CommandContext(ctx, "go", "env", "GOTELEMETRY")
					cmd.Env = append(os.Environ(), "GO_TELEMETRY_CHILD=2")
					out, err := cmd.CombinedOutput()
					if err != nil || strings.TrimSpace(string(out)) != mode {
						t.Fatalf("actual mode: %q, %v", out, err)
					}
					if mode == "local" {
						cmd = exec.CommandContext(ctx, "go", "** telemetry **")
						cmd.Env = append(os.Environ(), "GO_TELEMETRY_CHILD=1", "GO_TELEMETRY_CHILD_UPLOAD=1")
						if out, err := cmd.CombinedOutput(); err != nil {
							t.Fatalf("owned uploader: %v, %s", err, out)
						}
						for _, dir := range []string{"local", "upload"} {
							info, err := os.Stat(filepath.Join(telemetry, dir))
							if err != nil || !info.IsDir() {
								t.Fatalf("local control did not create %s: %v", dir, err)
							}
						}
					} else {
						entries, err := os.ReadDir(telemetry)
						if err != nil || len(entries) != 1 || entries[0].Name() != "mode" || entries[0].IsDir() {
							t.Fatalf("off mode left artifacts: %v, %v", entries, err)
						}
					}
				})
			}
		})
	}
}

// Only the parent above supplies this subprocess's synthetic inherited stores.
func TestBindIsolationChild(t *testing.T) {
	mode := os.Getenv("BIND_ISOLATION_CHILD")
	if mode == "" {
		return
	}
	var stores map[string]string
	if err := json.Unmarshal([]byte(os.Getenv("BIND_ISOLATION_STORES")), &stores); err != nil {
		t.Fatal(err)
	}
	if mode == "isolated" {
		isolateBindTest(t)
	}
	for _, root := range []string{filepath.Join(os.Getenv("HOME"), ".claude"), os.Getenv("CLAUDE_CONFIG_DIR")} {
		raw, err := os.ReadFile(filepath.Join(root, "sessions", "sentinel.json"))
		if mode == "sentinel-neighbor" {
			if err != nil || !strings.Contains(string(raw), "inherited-store-sentinel") {
				t.Fatalf("synthetic store neighbor missing: %v", err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("fixture reached inherited Claude sentinel store: %v", err)
		}
	}
	for key, inherited := range stores {
		value := os.Getenv(key)
		if mode == "isolated" && (value == "" || value == inherited) {
			t.Fatalf("fixture retained inherited %s", key)
		}
	}
	for _, entry := range os.Environ() {
		key, value, _ := strings.Cut(entry, "=")
		if key == "TMUX" || key == "TMUX_PANE" || key == "CLAUDECODE" ||
			strings.HasPrefix(key, "CLAUDE_CODE_") || key == "CODEX_THREAD_ID" ||
			key == "CLAUDE_ENV_FILE" || key == "A2A_AGENT" || key == "A2A_SESSION_ID" || key == "A2A_MATRIX_URL" {
			if mode == "isolated" && value != "" {
				t.Fatalf("fixture retained inherited terminal/config variable %s", key)
			}
		}
	}
}
