package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	// Pin the Go caches before HOME moves: a nested go build would otherwise
	// fill a read-only module cache under the temporary HOME (which
	// t.TempDir cleanup cannot remove) and could fetch modules again.
	for key, value := range goCacheEnv(t) {
		t.Setenv(key, value)
	}
	for _, key := range []string{"HOME", "CLAUDE_CONFIG_DIR", "CODEX_HOME", "XDG_CONFIG_HOME", "A2A_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
}

var (
	goCacheOnce   sync.Once
	goCacheValues map[string]string
	goCacheErr    error
)

// goCacheEnv resolves GOMODCACHE, GOCACHE and GOPATH once, from the
// environment the test binary started with.
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
		t.Fatalf("resolve Go caches before isolating HOME: %v", goCacheErr)
	}
	for key, value := range goCacheValues {
		if value == "" {
			t.Fatalf("resolve Go caches before isolating HOME: %s is empty", key)
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
