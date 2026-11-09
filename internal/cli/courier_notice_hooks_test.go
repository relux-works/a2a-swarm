//go:build linux || darwin

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func TestCMInstalledHookBytes(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			f := newCourierFixture(t)
			e := f.envs["alice"]
			e.exePath("/usr/local/bin/a2a")
			settings := filepath.Join(canonicalRoot(t), "hooks.json")
			e.run("hook", "install", "--runtime", runtime, "--agent", "alice", "--settings", settings).ok(t)
			before := readFile(t, settings)
			e.run("hook", "install", "--runtime", runtime, "--agent", "alice", "--settings", settings).ok(t)
			if readFile(t, settings) != before {
				t.Fatal("idempotent hook install changed bytes")
			}
			var installed struct {
				Hooks map[string][]hookGroup `json:"hooks"`
			}
			if json.Unmarshal([]byte(before), &installed) != nil {
				t.Fatal("invalid hook settings")
			}
			if err := config.SaveState(e.paths(), "alice", config.State{NextBatch: "s0"}); err != nil {
				t.Fatal(err)
			}
			f.envs["alice"].hs.inject(f.rooms["alice"], "@bob:"+testServer, ping(19))
			groups := installed.Hooks["UserPromptSubmit"]
			if len(groups) != 1 || len(groups[0].Hooks) != 1 {
				t.Fatal("missing installed prompt command")
			}
			args := strings.Fields(groups[0].Hooks[0].Command)
			if len(args) < 2 || args[0] != "/usr/local/bin/a2a" {
				t.Fatal("unexpected installed command format")
			}
			station := f.run("alice", `{}`, args[1:]...).ok(t)
			e.stdin = `{}`
			local := e.run(args[1:]...).ok(t)
			if station.stdout == "" || station.stdout != local.stdout {
				t.Fatal("Courier and local hook output differ byte for byte")
			}
			// Configuration lives in the project, not the enrolled station home.
			e.run("hook", "uninstall", "--runtime", runtime, "--settings", settings).ok(t)
			if strings.Contains(readFile(t, settings), "a2a --as") {
				t.Fatal("uninstall retained a command")
			}
			entries, err := os.ReadDir(f.stations["alice"])
			if err != nil || len(entries) != 1 || entries[0].Name() != ".a2a" {
				t.Fatal("hook install/uninstall left a station file")
			}
		})
	}
}
