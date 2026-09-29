package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// fakeTmux answers the few tmux queries the bridge makes and records every call.
type fakeTmux struct {
	mu              sync.Mutex
	command         string  // #{pane_current_command}
	sessionActivity int64   // #{session_activity}
	clientActivity  []int64 // #{client_activity} per client
	screen          string  // capture-pane output
	calls           [][]string
	sent            []string // send-keys arguments after "-t <pane>"
}

// idleTmux is a pane running claude, untouched for two minutes, at an empty prompt.
func idleTmux() *fakeTmux {
	old := time.Now().Add(-2 * time.Minute).Unix()
	return &fakeTmux{command: "claude", sessionActivity: old, clientActivity: []int64{old, old - 10},
		screen: "previous answer\n\n> \n\n\n"}
}

func (f *fakeTmux) run(_ context.Context, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), args...))
	switch args[0] {
	case "display-message":
		return fmt.Sprintf("%s\t%d\twork\n", f.command, f.sessionActivity), nil
	case "list-clients":
		var b strings.Builder
		for _, t := range f.clientActivity {
			fmt.Fprintf(&b, "%d\n", t)
		}
		return b.String(), nil
	case "capture-pane":
		return f.screen, nil
	case "send-keys":
		f.sent = append(f.sent, strings.Join(args[3:], " "))
		return "", nil
	}
	return "", fmt.Errorf("unexpected tmux %v", args)
}

func (f *fakeTmux) sentKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *fakeTmux) set(fn func(*fakeTmux)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// emptyPrompt matches an empty "> " prompt line.
const emptyPrompt = `^>\s*$`

func bridgeArgs(extra ...string) []string {
	return append([]string{"--as", "alice", "bridge", "tmux", "--agent", "alice", "--pane", "work:1.0", "--program", "claude",
		"--empty-prompt", emptyPrompt, "--idle", "60s", "--interval", "1ms"}, extra...)
}

// waitSent polls until the fake tmux received n send-keys calls.
func waitSent(t *testing.T, ft *fakeTmux, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(ft.sentKeys()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("bridge never sent keys #%d: %q", n, ft.sentKeys())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Proves the tmux bridge rings an idle pane once per distinct set of
// undelivered events (one literal line, then Enter; nothing again while the
// set is unchanged, again when a new event joins it), stops cleanly on cancel,
// and never writes delivery state or subscriptions: the state file is
// byte-identical afterwards and a hook still delivers both events.
func TestBridgeRingsOncePerSetAndNeverWrites(t *testing.T) {
	e, room := subWorld(t)
	e.hook("alice", "prompt", `{}`).ok(t)
	e.subscribe("alice", "--kind", "system")
	ft := idleTmux()
	e.configure = func(a *App) { a.tmux = ft }
	stateBefore := readFile(t, e.paths().StateFile("alice"))
	puts := e.hs.CallCount(accountDataPut)
	first := e.hs.inject(room, "@bob:test.local", ping(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, 1)
	go func() { done <- e.runCtx(ctx, bridgeArgs("--poll", "5ms")...) }()
	waitSent(t, ft, 2)
	time.Sleep(60 * time.Millisecond) // many polls with the same set
	if got := ft.sentKeys(); len(got) != 2 || got[0] != "-l a2a: 1 new peer event (mention). They are delivered by the prompt hook." || got[1] != "Enter" {
		t.Fatalf("sent = %q", got)
	}
	second := e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "system", "landing", "PR#1", "landed"))
	waitSent(t, ft, 4)
	cancel()
	r := recv(t, done)
	e.configure = nil
	if r.code != ExitOK {
		t.Fatalf("bridge exit %d: %s", r.code, r.stderr)
	}
	if got := ft.sentKeys(); len(got) != 4 || got[2] != "-l a2a: 2 new peer events (mention, sub). They are delivered by the prompt hook." {
		t.Fatalf("sent = %q", got)
	}
	tuitestkit.ContainsStr(t, r.stdout, "rang work:1.0: a2a: 2 new peer events")
	if readFile(t, e.paths().StateFile("alice")) != stateBefore || e.hs.CallCount(accountDataPut) != puts {
		t.Fatal("the bridge wrote delivery state or subscriptions")
	}
	out := e.hook("alice", "prompt", `{}`).ok(t).stdout
	tuitestkit.ContainsStr(t, out, first)
	tuitestkit.ContainsStr(t, out, second)
}

// Proves the bridge never touches a busy pane: it skips when the pane runs
// another program, when a tmux client or the session had recent activity,
// and when the last non-empty line is not an empty prompt (a human is
// typing); an idle pane at an empty prompt is rung (positive control). No
// server text is ever typed.
func TestBridgeSkipsBusyPane(t *testing.T) {
	e, room := world(t)
	e.hook("alice", "prompt", `{}`).ok(t)
	e.hs.inject(room, "@bob:test.local", msg("@alice:test.local SECRET-PAYLOAD rm -rf", nil))
	now := time.Now().Unix()
	for _, c := range []struct {
		name  string
		setup func(*fakeTmux)
		rings bool
	}{
		{"other program", func(f *fakeTmux) { f.command = "zsh" }, false},
		{"recent client", func(f *fakeTmux) { f.clientActivity = []int64{now - 300, now - 5} }, false},
		{"recent session", func(f *fakeTmux) { f.sessionActivity = now - 1 }, false},
		{"typing", func(f *fakeTmux) { f.screen = "answer\n\n> half a sentence\n\n" }, false},
		{"idle", func(*fakeTmux) {}, true},
	} {
		ft := idleTmux()
		ft.set(c.setup)
		e.configure = func(a *App) { a.tmux = ft }
		e.run(bridgeArgs("--once")...).ok(t)
		e.configure = nil
		sent := ft.sentKeys()
		if c.rings != (len(sent) == 2) || (!c.rings && len(sent) != 0) {
			t.Fatalf("%s: sent %q", c.name, sent)
		}
		for _, s := range sent {
			tuitestkit.NotContainsStr(t, s, "SECRET-PAYLOAD")
		}
	}
}

// Proves bridge input errors are usage errors (exit 2) found before any
// request or tmux call: missing required flags (no defaults for the prompt
// regexp), a bad regexp, non-positive durations, an invalid agent, a
// conflicting --as, and a missing "tmux" adapter name; without a delivery
// cursor a check fails (exit 1) and still sends nothing.
func TestBridgeNegative(t *testing.T) {
	e, _ := world(t)
	ft := idleTmux()
	e.configure = func(a *App) { a.tmux = ft }
	before := len(e.hs.Calls())
	drop := func(args []string, flag string) []string {
		var out []string
		for i := 0; i < len(args); i++ {
			if args[i] == flag {
				i++
				continue
			}
			out = append(out, args[i])
		}
		return out
	}
	for _, args := range [][]string{
		drop(bridgeArgs(), "--empty-prompt"),
		drop(bridgeArgs(), "--program"),
		drop(bridgeArgs(), "--pane"),
		bridgeArgs("--empty-prompt", "("),
		bridgeArgs("--idle", "0s"),
		bridgeArgs("--interval", "-1s"),
		bridgeArgs("--agent", "Bad"),
		append([]string{"--as", "bob"}, bridgeArgs()[2:]...),
		{"--as", "alice", "bridge", "screen"},
		bridgeArgs("extra"),
	} {
		if r := e.run(args...); r.code != ExitUsage {
			t.Fatalf("%v: exit %d (%s)", args, r.code, r.stderr)
		}
	}
	if n := len(e.hs.Calls()); n != before || len(ft.calls) != 0 {
		t.Fatalf("usage errors reached the server (%d) or tmux (%d)", n-before, len(ft.calls))
	}
	e.run(bridgeArgs("--once")...).failsWith(t, ExitError, "no delivery cursor")
	if _, err := os.Stat(e.paths().StateFile("alice")); !os.IsNotExist(err) || len(ft.sentKeys()) != 0 {
		t.Fatal("the bridge wrote state or rang without a cursor")
	}
	e.configure = nil
}
