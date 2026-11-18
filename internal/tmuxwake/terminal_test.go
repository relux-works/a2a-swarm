package tmuxwake

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Match orphanFixture's input batching: per-byte redraw/JSON writes can leave
// the synthetic screen mid-notice at Send's bounded verification, even when
// this test intended a ready renderer. Delayed rendering is tested separately.
const inputFixture = `import os,sys,tty,json,time
tty.setraw(0)
text=''; submitted=[]; enters=0; pastes=0; path=sys.argv[1]; runtime=sys.argv[2]; pending=bytearray()
os.write(1,b'\x1b[?2004h')
def draw():
    global text
    options={}
    try:
        with open(path+'.render') as f: options=json.load(f)
    except FileNotFoundError: pass
    shown='' if options.get('empty',False) else text
    row=options.get('row',20)
    col=options.get('column',0)
    cursor=options.get('cursor',0)
    cursor_row=options.get('cursor-row',row)
    extra=options.get('extra','')
    busy=options.get('busy',False)
    if runtime=='claude' or runtime.startswith('claude-named'):
        content=shown+'\x1b[7m \x1b[0m' if shown else '\x1b[7mC\x1b[0;2montinue\x1b[0m'
        top='─'*110 if runtime=='claude' else '─'*90+' synthetic-session'
        footer='  ⏵⏵ bypass permissions on'
        if runtime=='claude-named-busy': footer+=' (esc to interrupt)'
        if runtime=='claude-named-dialog': footer+=' Permission required'
        if runtime=='claude-named-unknown': top='Unrecognized layout'
        if busy: footer+=' (esc to interrupt)'
        screen='\x1b[2J\x1b['+str(row-1)+';1H'+top+'\r\n'+' '*col+'❯\u00a0'+content+'\r\n'+'─'*110+'\r\n'+footer+extra+'\x1b['+str(cursor_row)+';'+str(3+col+len(shown)+cursor)+'H'
    else:
        footer='Codex · Ready · Context 50% left' if shown else '? for shortcuts'
        if busy: footer+=' · Esc to interrupt'
        screen='\x1b[2J\x1b['+str(row)+';1H'+' '*col+'\x1b[1m›\x1b[0m '+shown+'\x1b['+str(row+2)+';1H  '+footer+extra+'\x1b['+str(cursor_row)+';'+str(3+col+len(shown)+cursor)+'H'
    os.write(1,screen.encode())
    with open(path+'.next','w') as f: json.dump({'text':text,'submitted':submitted,'enters':enters,'pastes':pastes,'busy':busy or 'esc to interrupt' in extra.lower(),'pid':os.getpid(),'row':row,'render':options.get('render',0)},f)
    os.replace(path+'.next',path)
draw()
while True:
    pending.extend(os.read(0,4096))
    while os.path.exists(path+'.hold'): time.sleep(0.005)
    while pending:
        if pending.startswith(b'\x1b[200~'):
            del pending[:6]; pastes+=1; continue
        if pending.startswith(b'\x1b[201~'):
            del pending[:6]; continue
        if any(marker.startswith(bytes(pending)) for marker in (b'\x1b[200~',b'\x1b[201~')): break
        c=pending[0]; del pending[0]
        if c==12: continue # fixture-only redraw without input changes
        if c in (13,10):
            enters+=1
            try:
                with open(path+'.control') as f: mode=f.read().strip()
            except FileNotFoundError: mode=''
            if mode=='never' or mode=='drop-first' and enters==1: continue
            submitted.append(text);text=''
        elif c in (127,8): text=text[:-1]
        else: text+=chr(c)
    draw()
`

type fixtureState struct {
	Text      string
	Submitted []string
	Enters    int
	Pastes    int
	Busy      bool
	PID       int
	Row       int
	Render    int
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
	targetRuntime := runtime
	if strings.HasPrefix(runtime, "claude-named") {
		targetRuntime = "claude"
	}
	f.target, err = Pin(context.Background(), socket, "%0", targetRuntime)
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
// leaves a racing prefix or suffix intact. This does
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
					} else if race == "prefix" {
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

// A renderer that has not consumed the typed bytes cannot authorize Enter.
// Even after it catches up, Send has left exactly one unsubmitted notice.
// The adjacent two-phase "none" cases are the ready-renderer positive controls.
func TestTerminalDelayedRendererDoesNotSubmit(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			f := newTerminalFixture(t, runtime)
			hold := f.path + ".hold"
			defer os.Remove(hold)
			outcome, err := f.target.Send(context.Background(), 2, func() {
				// The fixture consumes no input until this test removes its gate.
				if err := os.WriteFile(hold, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}, nil)
			if err != nil || outcome != "notice left in draft: changed input" {
				t.Fatalf("delayed renderer authorized submission: %q %v", outcome, err)
			}
			if err := os.Remove(hold); err != nil {
				t.Fatal(err)
			}
			s := f.wait(func(s fixtureState) bool { return s.Text == Notice(2) || len(s.Submitted) != 0 })
			if len(s.Submitted) != 0 {
				t.Fatalf("delayed notice was submitted: %+v", s)
			}
		})
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
	if got := f.target.PendingInput(context.Background(), notice); got != "blocked" {
		t.Fatalf("journal-owned notice rejected: %s", got)
	}
	if outcome, err := f.target.Resume(context.Background(), 1, notice); err != nil || !strings.HasPrefix(outcome, "suppressed:") {
		t.Fatalf("recovery not refused: %s %v", outcome, err)
	}
	s := f.wait(func(s fixtureState) bool { return true })
	if s.Text != notice || len(s.Submitted) != 0 {
		t.Fatal("renderer recovery touched draft")
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

// Continuation may never degrade to a fresh type when an owned composer was
// emptied between the dispatch observation and Resume's fresh verification.
func TestTerminalResumeNeverTypesEmpty(t *testing.T) {
	f := newTerminalFixture(t, "codex")
	notice := Notice(1)
	if outcome, err := f.target.Resume(context.Background(), 1, notice); err != nil || !strings.HasPrefix(outcome, "suppressed:") {
		t.Fatalf("empty Resume was not refused: %q %v", outcome, err)
	}
	s := f.wait(func(s fixtureState) bool { return true })
	if s.Text != "" || len(s.Submitted) != 0 {
		t.Fatalf("Resume freshly typed into empty input: %+v", s)
	}
	f.typeText(notice)
	f.wait(func(s fixtureState) bool { return s.Text == notice })
	if outcome, err := f.target.Resume(context.Background(), 1, notice); err != nil || !strings.HasPrefix(outcome, "suppressed:") {
		t.Fatalf("owned Resume not refused: %s %v", outcome, err)
	}
	s = f.wait(func(s fixtureState) bool { return true })
	if s.Text != notice || len(s.Submitted) != 0 {
		t.Fatal("owned Resume touched input")
	}
}

// Keeper o-JKabzo retires erase authority, including valid-looking frames.
func TestTerminalClearFreshFence(t *testing.T) {
	for _, name := range []string{"valid", "missing", "error", "changed"} {
		t.Run(name, func(t *testing.T) {
			f := newTerminalFixture(t, "codex")
			notice := Notice(1)
			f.typeText(notice)
			f.wait(func(s fixtureState) bool { return s.Text == notice })
			called := false
			cleared, err := f.target.ClearNotice(context.Background(), 1, notice, func() error { called = true; return errors.New("must not run") })
			if cleared || err != nil || called {
				t.Fatal("renderer authorized erase")
			}
			s := f.wait(func(s fixtureState) bool { return true })
			if s.Text != notice || len(s.Submitted) != 0 {
				t.Fatal("renderer erased input")
			}
		})
	}
}

// #89 regression: a renderer that catches up inside the bounded verification
// window is submitted exactly once instead of being stranded as a draft. The
// hold models a slow (background-priority) terminal; it does not prove any
// bound beyond redrawWait, which TestTerminalDelayedRendererDoesNotSubmit covers.
func TestTerminalLateRendererInsideWindowSubmitsOnce(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			f := newTerminalFixture(t, runtime)
			hold := f.path + ".hold"
			defer os.Remove(hold)
			outcome, err := f.target.Send(context.Background(), 2, func() {
				if err := os.WriteFile(hold, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}, func() {
				go func() { time.Sleep(400 * time.Millisecond); os.Remove(hold) }()
			})
			if err != nil || outcome != "wake submitted" {
				t.Fatalf("late renderer not submitted: %q %v", outcome, err)
			}
			s := f.wait(func(s fixtureState) bool { return len(s.Submitted) > 0 })
			if len(s.Submitted) != 1 || s.Submitted[0] != Notice(2) || s.Text != "" {
				t.Fatalf("unexpected submission: %+v", s)
			}
		})
	}
}

// Negative control for the polling window: once the box is empty because the
// notice was already submitted by someone else, polling never adds a second
// Enter; the attempt ends as a draft outcome with exactly one submission.
func TestTerminalAlreadySubmittedNoticeGetsNoSecondEnter(t *testing.T) {
	old := redrawWait
	redrawWait = 600 * time.Millisecond
	defer func() { redrawWait = old }()
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			f := newTerminalFixture(t, runtime)
			hold := f.path + ".hold"
			defer os.Remove(hold)
			outcome, err := f.target.Send(context.Background(), 2, func() {
				if err := os.WriteFile(hold, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}, func() {
				// Foreign Enter queued behind the typed notice, then the renderer catches up.
				if _, err := f.target.command(context.Background(), "send-keys", "-t", f.target.Pane, "Enter"); err != nil {
					t.Fatal(err)
				}
				os.Remove(hold)
				f.wait(func(s fixtureState) bool { return len(s.Submitted) == 1 })
			})
			if err != nil || outcome != "notice left in draft: changed input" {
				t.Fatalf("unexpected outcome: %q %v", outcome, err)
			}
			time.Sleep(200 * time.Millisecond)
			s := f.wait(func(s fixtureState) bool { return true })
			if len(s.Submitted) != 1 || s.Submitted[0] != Notice(2) || s.Text != "" {
				t.Fatalf("second Enter or lost notice: %+v", s)
			}
		})
	}
}

// Foreign text appended to the notice never gets Enter; the refusal-only
// composer observation stops both runtimes without making foreign input eligible.
func TestTerminalForeignTextDuringWindowGetsNoEnter(t *testing.T) {
	for _, c := range []struct {
		runtime    string
		outcome    string
		stopsEarly bool
	}{
		{"codex", "notice left in draft: changed input", true},
		{"claude", "notice left in draft: changed input", true},
	} {
		t.Run(c.runtime, func(t *testing.T) {
			f := newTerminalFixture(t, c.runtime)
			var typed time.Time
			outcome, err := f.target.Send(context.Background(), 2, nil, func() {
				f.typeText("X")
				f.wait(func(s fixtureState) bool { return s.Text == Notice(2)+"X" })
				typed = time.Now()
			})
			elapsed := time.Since(typed)
			if err != nil || outcome != c.outcome {
				t.Fatalf("unexpected outcome: %q %v", outcome, err)
			}
			if stopped := elapsed < redrawWait; stopped != c.stopsEarly {
				t.Fatalf("polling window: elapsed %v of %v, want stopsEarly=%v", elapsed, redrawWait, c.stopsEarly)
			}
			s := f.wait(func(s fixtureState) bool { return s.Text == Notice(2)+"X" })
			if len(s.Submitted) != 0 {
				t.Fatalf("foreign draft submitted: %+v", s)
			}
		})
	}
}

// Both runtimes retry a swallowed Enter, and credit the wake only after the
// synthetic composer clears; one bracketed paste proves the notice was never retyped.
func TestTerminalDroppedFirstEnterRequiresSubmitWitness(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			f := newTerminalFixture(t, runtime)
			if err := os.WriteFile(f.path+".control", []byte("drop-first"), 0600); err != nil {
				t.Fatal(err)
			}
			outcome, err := f.target.Send(context.Background(), 2, nil, nil)
			if err != nil || outcome != "wake submitted" {
				t.Fatalf("dropped Enter was credited without recovery: %q %v", outcome, err)
			}
			s := f.wait(func(s fixtureState) bool { return len(s.Submitted) != 0 })
			if s.Enters != 2 || s.Pastes != 1 || len(s.Submitted) != 1 || s.Submitted[0] != Notice(2) || s.Text != "" {
				t.Fatalf("unexpected submit witness: %+v", s)
			}
		})
	}
}

// A composer that never clears exhausts Enter-only retries and remains an
// unsubmitted notice; the ready neighbor above proves the successful witness.
func TestTerminalNeverClearedNoticeIsNotSubmitted(t *testing.T) {
	f := newTerminalFixture(t, "codex")
	if err := os.WriteFile(f.path+".control", []byte("never"), 0600); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.target.Send(context.Background(), 1, nil, nil)
	if err != nil || outcome != "uncertain: Enter not witnessed" {
		t.Fatalf("unwitnessed notice credited: %q %v", outcome, err)
	}
	s := f.wait(func(s fixtureState) bool { return s.Enters >= 3 })
	if s.Enters != 3 || s.Pastes != 1 || len(s.Submitted) != 0 || s.Text != Notice(1) {
		t.Fatalf("retry bound or pending notice lost: %+v", s)
	}
}

// Recognized foreign input after typing stops the attempt before any Enter.
// Codex's no-hint filled composer is covered by the conservative raw refusal path.
func TestTerminalForeignTextBeforeEnterStopsImmediately(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			f := newTerminalFixture(t, runtime)
			outcome, err := f.target.Send(context.Background(), 1, nil, func() {
				f.typeText("KEEP")
				f.wait(func(s fixtureState) bool { return s.Text == Notice(1)+"KEEP" })
			})
			if err != nil || outcome != "notice left in draft: changed input" {
				t.Fatalf("foreign composer did not stop immediately: %q %v", outcome, err)
			}
			s := f.wait(func(s fixtureState) bool { return s.Text == Notice(1)+"KEEP" })
			if s.Enters != 0 || s.Pastes != 1 || len(s.Submitted) != 0 {
				t.Fatalf("foreign input received Enter or notice was retyped: %+v", s)
			}
		})
	}
}

// Claude becoming busy after the paste is not proof of a submitted wake because
// no Enter was sent; the notice stays pending and is not typed a second time.
func TestTerminalClaudeBusyAfterTypingIsNotSubmitted(t *testing.T) {
	f := newTerminalFixture(t, "claude")
	outcome, err := f.target.Send(context.Background(), 1, nil, func() {
		f.render(map[string]any{"row": 20, "extra": " (esc to interrupt)"})
		f.wait(func(s fixtureState) bool { return s.Busy })
	})
	if err != nil || outcome != "notice left in draft: runtime became busy before Enter" {
		t.Fatalf("pre-Enter busy transition misreported: %q %v", outcome, err)
	}
	s := f.wait(func(s fixtureState) bool { return s.Busy })
	if s.Enters != 0 || s.Pastes != 1 || len(s.Submitted) != 0 || s.Text != Notice(1) {
		t.Fatalf("busy transition repeated input or submitted a notice: %+v", s)
	}
}
