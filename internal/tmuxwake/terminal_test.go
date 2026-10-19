package tmuxwake

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const inputFixture = `import os,sys,tty,json
tty.setraw(0)
text=''; submitted=[]; path=sys.argv[1]; runtime=sys.argv[2]
def draw():
    global text
    if runtime=='claude':
        content=text+'\x1b[7m \x1b[0m' if text else '\x1b[7mC\x1b[0;2montinue\x1b[0m'
        screen='\x1b[2J\x1b[19;1H'+'─'*110+'\r\n❯\u00a0'+content+'\r\n'+'─'*110+'\r\n  ⏵⏵ bypass permissions on\x1b[20;'+str(3+len(text))+'H'
    else:
        footer='Codex · Ready · Context 50% left' if text else '? for shortcuts'
        screen='\x1b[2J\x1b[20;1H\x1b[1m›\x1b[0m '+text+'\x1b[22;1H  '+footer+'\x1b[20;'+str(3+len(text))+'H'
    os.write(1,screen.encode())
    with open(path+'.next','w') as f: json.dump({'text':text,'submitted':submitted,'pid':os.getpid()},f)
    os.replace(path+'.next',path)
draw()
while True:
    c=os.read(0,1)
    if c in (b'\r',b'\n'): submitted.append(text);text=''
    elif c in (b'\x7f',b'\x08'): text=text[:-1]
    else: text+=c.decode('ascii')
    draw()
`

type fixtureState struct {
	Text      string
	Submitted []string
	PID       int
}
type terminalFixture struct {
	target Target
	path   string
	t      *testing.T
}

func newTerminalFixture(t *testing.T, runtime string) *terminalFixture {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable; isolated integration lane not run")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable; isolated integration lane not run")
	}
	dir, err := os.MkdirTemp("/tmp", "aw-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	script := filepath.Join(dir, "fixture.py")
	path := filepath.Join(dir, "input.json")
	socket := filepath.Join(dir, "socket")
	if err = os.WriteFile(script, []byte(inputFixture), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "python3", script, path, runtime)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("isolated tmux startup: %v %s", err, out)
	}
	f := &terminalFixture{path: path, t: t}
	state := f.wait(func(s fixtureState) bool { return s.PID > 0 })
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	serverPID, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	t.Cleanup(func() {
		// Only PIDs obtained from this newly created private server/fixture.
		for _, pid := range []int{state.PID, serverPID} {
			if p, e := os.FindProcess(pid); e == nil {
				p.Signal(syscall.SIGTERM)
			}
		}
	})
	f.target, err = Pin(context.Background(), socket, "%0", runtime)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *terminalFixture) wait(matches func(fixtureState) bool) fixtureState {
	f.t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(f.path)
		var s fixtureState
		if err == nil && json.Unmarshal(b, &s) == nil && matches(s) {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.t.Fatal("synthetic input did not reach expected state")
	return fixtureState{}
}

func (f *terminalFixture) typeText(text string) {
	f.t.Helper()
	_, err := f.target.command(context.Background(), "send-keys", "-t", f.target.Pane, "-l", text)
	if err != nil {
		f.t.Fatal(err)
	}
}

// Actual private tmux and a synthetic raw-input application prove that the
// product Send path submits the fixed notice only after exact verification,
// restores a racing prefix, and leaves a racing suffix untouched. This does
// not prove atomicity of the final verify-to-Enter/erase gap.
func TestTerminalTwoPhaseRaceControls(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		for _, race := range []string{"none", "prefix", "suffix", "draft"} {
			t.Run(runtime+"/"+race, func(t *testing.T) {
				f := newTerminalFixture(t, runtime)
				notice := Notice(2)
				var before, after func()
				if race == "draft" {
					f.typeText("KEEP")
					f.wait(func(s fixtureState) bool { return s.Text == "KEEP" })
				}
				if race == "prefix" {
					before = func() { f.typeText("KEEP"); f.wait(func(s fixtureState) bool { return s.Text == "KEEP" }) }
				}
				if race == "suffix" {
					after = func() {
						f.wait(func(s fixtureState) bool { return s.Text == notice })
						f.typeText("KEEP")
						f.wait(func(s fixtureState) bool { return s.Text == notice+"KEEP" })
					}
				}
				outcome, err := f.target.Send(context.Background(), 2, before, after)
				if err != nil {
					t.Fatal(err)
				}
				if race == "none" {
					s := f.wait(func(s fixtureState) bool { return len(s.Submitted) > 0 })
					if len(s.Submitted) != 1 || s.Submitted[0] != notice || outcome != "wake submitted" {
						t.Fatalf("unexpected submission: %#v %s", s, outcome)
					}
				} else {
					want := "KEEP"
					if race == "suffix" {
						want = notice + "KEEP"
					} else if runtime == "codex" && race == "prefix" {
						// No hint with real input: unknown draft is left intact.
						want = "KEEP" + notice
					}
					s := f.wait(func(s fixtureState) bool { return s.Text == want })
					if len(s.Submitted) != 0 {
						t.Fatalf("human draft submitted: %#v", s)
					}
				}
			})
		}
	}
}

// The captured Codex layout drops its shortcut hint when text is present.
// Only the caller's exact journal notice is recognized; no journal, a changed
// count or appended human text cannot acquire ownership or submit anything.
func TestCodexJournalNoticeWithoutHint(t *testing.T) {
	f := newTerminalFixture(t, "codex")
	notice := Notice(1)
	f.typeText(notice)
	f.wait(func(s fixtureState) bool { return s.Text == notice })
	for _, wrong := range []string{"", Notice(2), "human text"} {
		if got := f.target.PendingInput(context.Background(), wrong); got != "blocked" {
			t.Fatalf("non-journal input admitted: %s", got)
		}
	}
	if got := f.target.PendingInput(context.Background(), notice); got != "own" {
		t.Fatalf("journal-owned notice rejected: %s", got)
	}
	if outcome, err := f.target.Resume(context.Background(), 1, notice); err != nil || outcome != "wake submitted" {
		t.Fatalf("owned notice not completed: %s %v", outcome, err)
	}
	s := f.wait(func(s fixtureState) bool { return len(s.Submitted) == 1 })
	if s.Submitted[0] != notice {
		t.Fatal("submitted something other than the journal notice")
	}
	f.typeText(notice + "KEEP")
	f.wait(func(s fixtureState) bool { return s.Text == notice+"KEEP" })
	if got := f.target.PendingInput(context.Background(), notice); got != "blocked" {
		t.Fatal("appended human input acquired ownership")
	}
	if _, err := f.target.Resume(context.Background(), 1, notice); err != nil {
		t.Fatal(err)
	}
	s = f.wait(func(s fixtureState) bool { return s.Text == notice+"KEEP" })
	if len(s.Submitted) != 1 {
		t.Fatal("appended human input was submitted")
	}
}

// A recycled pane PID, owner birth or socket cannot inherit the binding;
// nearby valid pin remains usable. All targets are private synthetic fixtures.
func TestTerminalTargetPinning(t *testing.T) {
	f := newTerminalFixture(t, "codex")
	if !f.target.Idle(context.Background()) {
		t.Fatal("valid target not idle")
	}
	for _, change := range []func(*Target){func(t *Target) { t.PanePID++ }, func(t *Target) { t.PaneBirth = "replaced" }, func(t *Target) { t.SocketID = "replaced" }, func(t *Target) { t.OwnerPID = os.Getpid(); t.OwnerBirth = "replaced" }} {
		bad := f.target
		change(&bad)
		if bad.Idle(context.Background()) {
			t.Fatal("replaced target admitted")
		}
	}
}
