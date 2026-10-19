package cli

import (
	"context"
	"encoding/json"
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

// A synthetic runtime executable owns a Python terminal fixture and invokes
// real public hooks below. It never loads a provider, token or live pane.
func TestAutoBindingRuntimeChild(t *testing.T) {
	if os.Getenv("A2A_AUTO_FIXTURE") != "1" {
		return
	}
	cmd := exec.Command("python3", os.Getenv("AUTO_SCRIPT"), os.Getenv("AUTO_DIR"), os.Getenv("AUTO_EXE"))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// Real SessionStart under each runtime ancestor binds without agent action.
// Repeated startup/resume/compact/clear preserve generation/PID. Opposite wait
// admission refuses, explicit unbind remains stopped, and no mailbox receipts
// are invented by binding. An unsupported parent is covered by the ancestry controls.
func TestHookAutomaticBindingLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("private terminal integration")
	}
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	dir, err := os.MkdirTemp("/tmp", "ah-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe := filepath.Join(dir, "a2a")
	if raw, err := exec.Command("go", "build", "-o", exe, "../../cmd/a2a").CombinedOutput(); err != nil {
		t.Fatalf("CLI build: %v %s", err, raw)
	}
	self, _ := os.Executable()
	runtimeBytes, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e, _ := world(t)

			e.extra[EnvSessionID] = "auto-fixture"
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
			home := filepath.Join(dir, runtime)
			os.Mkdir(home, 0700)
			runtimeExe := filepath.Join(home, runtime)
			if err := os.WriteFile(runtimeExe, runtimeBytes, 0700); err != nil {
				t.Fatal(err)
			}
			script := bindFixture
			start := strings.Index(script, "        args=[exe,")
			end := strings.Index(script[start:], "        with open(root+'/result.next'") + start
			script = script[:start] + `        args=[exe,'--as','alice','hook','session-start','--runtime','` + runtime + `']
        source=['startup','resume','compact','clear'][min(request-1,3)]
        p=subprocess.run(args,input=json.dumps({'session_id':'auto-fixture','source':source}),capture_output=True,text=True,timeout=12)
` + script[end:]
			// Build the Claude draw directly: Go raw strings retain Python escapes.
			if runtime == "claude" {
				a := strings.Index(script, "def draw():")
				b := strings.Index(script[a:], "draw()\nwhile") + a
				script = script[:a] + `def draw():
    os.write(1,('\x1b[2J\x1b[19;1H'+'─'*100+'\x1b[20;1H❯ '+text+'\x1b[21;1H'+'─'*100+'\x1b[22;1H  bypass permissions on\x1b[20;'+str(3+len(text))+'H').encode());save()
` + script[b:]
			}
			scriptPath := filepath.Join(home, "fixture.py")
			if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(home, "socket")
			cmd := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", runtimeExe, "-test.run=^TestAutoBindingRuntimeChild$")
			thread := ""
			if runtime == "codex" {
				thread = "auto-fixture"
			}
			env := append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_AGENT=alice", "A2A_SESSION_ID=auto-fixture", "CODEX_THREAD_ID="+thread, "A2A_SECRET_STORE=file", "CLAUDE_ENV_FILE=", "CODEX_ENV_FILE=", "A2A_AUTO_FIXTURE=1", "AUTO_SCRIPT="+scriptPath, "AUTO_DIR="+home, "AUTO_EXE="+exe)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("tmux: %v %s", err, out)
			}
			raw, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
			if err != nil {
				t.Fatal(err)
			}
			serverPID, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			t.Cleanup(func() {
				st, _ := config.LoadState(e.paths(), "alice")
				if st.Terminal != nil {
					birth, _ := tmuxwake.Birth(context.Background(), st.Terminal.DaemonPID)
					if birth != "" && birth == st.Terminal.DaemonBirth {
						p, _ := os.FindProcess(st.Terminal.DaemonPID)
						p.Signal(syscall.SIGTERM)
					}
				}
				p, _ := os.FindProcess(serverPID)
				p.Signal(syscall.SIGTERM)
			})
			request := func(id int) {
				t.Helper()
				data, _ := json.Marshal(id)
				if err := os.WriteFile(filepath.Join(home, "command.json"), data, 0600); err != nil {
					t.Fatal(err)
				}
				until := time.Now().Add(12 * time.Second)
				for time.Now().Before(until) {
					data, err := os.ReadFile(filepath.Join(home, "result.json"))
					var v struct {
						Code     int
						Out, Err string
						Request  int
					}
					if err == nil && json.Unmarshal(data, &v) == nil && v.Request == id {
						if v.Code != 0 || id != 5 && v.Err != "" || id == 5 && !strings.Contains(v.Err, "explicitly stopped") {
							t.Fatalf("hook %d: code%d %s %s", id, v.Code, v.Out, v.Err)
						}
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
				t.Fatal("hook did not return")
			}
			request(1)
			st := awaitBinding(t, e.paths(), func(st config.State) bool { return st.Terminal != nil && st.Terminal.Outcome == "armed: no new mail" })
			generation, pid := st.Terminal.Generation, st.Terminal.DaemonPID
			for _, n := range []int{2, 3, 4} {
				request(n)
				b := e.state("alice").Terminal
				if b.Generation != generation || b.DaemonPID != pid {
					t.Fatal("SessionStart replaced generation")
				}
			}
			// A killed own fixture consumer is not a live binding: resume
			// must recover it without requiring an agent to run bind/unbind.
			oldPID := pid
			dead, _ := os.FindProcess(oldPID)
			dead.Signal(syscall.SIGTERM)
			until := time.Now().Add(5 * time.Second)
			for !processGone(oldPID) && time.Now().Before(until) {
				time.Sleep(10 * time.Millisecond)
			}
			if !processGone(oldPID) {
				t.Fatal("owned fixture consumer did not exit")
			}
			request(6)
			recovered := awaitBinding(t, e.paths(), func(st config.State) bool {
				return st.Terminal != nil && st.Terminal.DaemonPID != oldPID && st.Terminal.Outcome == "armed: no new mail"
			})
			generation, pid = recovered.Terminal.Generation, recovered.Terminal.DaemonPID

			e.extra[EnvSessionID] = "auto-fixture"
			e.as("alice", "wait", "--timeout", "1ms").failsWith(t, ExitError, "same identity has a live wait/watch")
			stop := exec.Command(exe, "--as", "alice", "unbind")
			stop.Env = env
			if out, err := stop.CombinedOutput(); err != nil {
				t.Fatalf("unbind: %v %s", err, out)
			}
			request(5)
			b := e.state("alice").Terminal
			if !b.Stopped || b.Generation != generation || b.DaemonPID != pid {
				t.Fatal("hook revived explicit unbind")
			}
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
		})
	}
}

// Retirement and cancellation have distinct fixed reasons, with no network or
// terminal effects. This includes loss of the field by a legacy schema writer.
func TestBindingExitReasons(t *testing.T) {
	for _, c := range []struct {
		name string
		b    *config.TerminalBinding
		want string
	}{{"lost", nil, "binding vanished"}, {"replacement", &config.TerminalBinding{Generation: "other"}, "generation replaced"}, {"stopped", &config.TerminalBinding{Generation: "own", Stopped: true}, "stopped"}} {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Terminal = c.b; return nil })
			if err != nil {
				t.Fatal(err)
			}
			a := NewApp()
			a.as = "alice"
			a.Getenv = func(k string) string {
				if k == config.EnvConfigDir {
					return e.dir
				}
				return ""
			}
			a.Stdout = io.Discard
			a.Stderr = io.Discard
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			reason, err := a.runTerminal(context.Background(), s, "own", config.TerminalBinding{})
			if err != nil || reason != c.want {
				t.Fatalf("got %q %v", reason, err)
			}
		})
	}
}
