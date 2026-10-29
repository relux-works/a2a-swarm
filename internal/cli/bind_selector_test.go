package cli

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// Bind selects only this invocation's own tmux pane. Without TMUX/TMUX_PANE
// (a Codex tool shell), with only one of them, or with explicit selectors that
// differ from them, bind refuses before tmux admission without writing a binding
// or binding log, even for plausible selectors. The positive control shows that selectors
// equal to both variables pass this check and reach the socket pin. A real own
// pane binding end to end is TestBindDetachedLifecycle.
func TestBindSelectorsRequireOwnPaneVariables(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(root, "default")
	pane := "%7"
	cases := []struct {
		name  string
		env   map[string]string
		flags []string
		want  string
	}{
		{"absent variables, guessed selectors", nil, []string{"--socket", socket, "--pane", pane}, "own tmux pane/socket"},
		{"absent variables, no selectors", nil, nil, "own tmux pane/socket"},
		{"only TMUX", map[string]string{"TMUX": socket + ",1,0"}, nil, "own tmux pane/socket"},
		{"only TMUX_PANE", map[string]string{"TMUX_PANE": pane}, nil, "own tmux pane/socket"},
		{"only TMUX, explicit pane", map[string]string{"TMUX": socket + ",1,0"}, []string{"--pane", pane}, "own tmux pane/socket"},
		{"only TMUX_PANE, explicit socket", map[string]string{"TMUX_PANE": pane}, []string{"--socket", socket}, "own tmux pane/socket"},
		{"different pane", map[string]string{"TMUX": socket + ",1,0", "TMUX_PANE": pane}, []string{"--pane", "%8"}, "own tmux pane/socket"},
		{"different socket", map[string]string{"TMUX": socket + ",1,0", "TMUX_PANE": pane}, []string{"--socket", socket + "-other"}, "own tmux pane/socket"},
		{"positive control: matching selectors reach the pin", map[string]string{"TMUX": socket + ",1,0", "TMUX_PANE": pane}, []string{"--socket", socket, "--pane", pane}, "tmux socket unavailable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			e.extra["CODEX_THREAD_ID"] = "selector-thread"
			for k, v := range c.env {
				e.extra[k] = v
			}
			args := append([]string{"bind", "--runtime", "codex", "--owner-pid", strconv.Itoa(os.Getpid()), "--session-id", "selector-thread",
				"--interval", "1s", "--retry-max", "2s", "--max-wakes-per-hour", "6"}, c.flags...)
			code := ExitUsage
			if c.want != "own tmux pane/socket" {
				code = ExitError
			}
			e.as("alice", args...).failsWith(t, code, c.want)
			if b := e.state("alice").Terminal; b != nil {
				t.Fatalf("refused bind wrote a binding: %+v", b)
			}
			if _, err := os.Stat(filepath.Join(e.dir, "state", "alice.binding.log")); !os.IsNotExist(err) {
				t.Fatalf("refused bind created a binding log: %v", err)
			}
		})
	}
}
