package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// A full-row renderer is a transport countermodel, not a capture of either
// installed runtime. It serializes a viewport of cells: explicit input spaces
// and blank viewport cells have the same glyph/style. Nothing reads oracle
// state to authorize production effects. No product source is edited.
func rev3PaddedFixture(runtime string) string {
	s := orphanFixture
	s = strings.ReplaceAll(s, "prefix+text+suffix", "prefix+text.ljust(100)+suffix")
	if runtime == "claude" {
		// Reuse the renderer's rules/row controls instead of replacing a fixed
		// ANSI row literal, which would silently miss a movable layout.
		s = strings.Replace(s, "if mode.get('claude'):", "if True:", 1)
	}
	return s
}

func newRev3PaddedFixture(t *testing.T, runtime string) *orphanDispatchFixture {
	t.Helper()
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable; private dispatch lane unrun")
		}
	}
	e, room := world(t)
	e.extra[EnvSessionID] = "owned-thread"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dir, err := os.MkdirTemp("/tmp", "ao-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	script := filepath.Join(dir, "fixture.py")
	if err := os.WriteFile(script, []byte(rev3PaddedFixture(runtime)), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	if out, err := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "python3", script, dir).CombinedOutput(); err != nil {
		t.Fatalf("private tmux: %v %s", err, out)
	}
	f := &orphanDispatchFixture{t: t, e: e, dir: dir, room: room, now: time.Now()}
	state := f.wait(func(s orphanScreen) bool { return s.PID > 1 })
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	serverPID, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || serverPID <= 1 {
		t.Fatal("missing private server PID")
	}
	t.Cleanup(func() {
		for _, pid := range []int{state.PID, serverPID} {
			p, _ := os.FindProcess(pid)
			_ = p.Signal(syscall.SIGTERM)
		}
	})
	target, err := tmuxwake.Pin(context.Background(), socket, "%0", runtime)
	if err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), state.PID)
	if err != nil {
		t.Fatal(err)
	}
	target.OwnerPID = state.PID
	target.OwnerBirth = birth
	f.a = &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Now: func() time.Time { return f.now }, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client()}
	f.s, err = f.a.session()
	if err != nil {
		t.Fatal(err)
	}
	f.update(func(st *config.State) {
		st.Terminal = &config.TerminalBinding{Generation: "orphan-gen", SessionID: "owned-thread", Host: machineLabel(), OwnerPID: state.PID, OwnerBirth: birth, Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 30}
	})
	f.rendered()
	return f
}
