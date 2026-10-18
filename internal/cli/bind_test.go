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

// Invalid/session-mismatched bindings fail before any process or target write;
// unbind cannot retire another session's persisted generation.
func TestBindRefusals(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.extra[EnvSessionID] = "mine"
	e.as("alice", "bind").failsWith(t, ExitUsage, "bind requires")
	e.as("alice", "bind", "--runtime", "codex", "--owner-pid", strconv.Itoa(os.Getpid()), "--session-id", "other", "--interval", "1s", "--retry-max", "2s", "--max-wakes-per-hour", "6").failsWith(t, ExitUsage, "another live session")
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "other", SessionID: "other", Host: machineLabel()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "unbind").failsWith(t, ExitError, "another session")
	if e.state("alice").Terminal.Stopped {
		t.Fatal("foreign binding retired")
	}
}

// An uncertain attempt cannot replay merely because new mail arrives. Only
// inbox receipts clear its IDs; pause preserves the fence and mailbox too.
func TestBindUncertainFenceAndPause(t *testing.T) {
	e, room := world(t)
	e.extra[EnvSessionID] = "owned-thread"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	id := runtimeMail(e, room, "SYNTHETIC_PENDING")
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: "fence", SessionID: "owned-thread", AttemptIDs: []string{id}, Reservations: []time.Time{time.Now()}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	a := &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Now: time.Now, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client()}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	dispatch := func() {
		t.Helper()
		if err := a.dispatchTerminal(context.Background(), s, "fence"); err != nil {
			t.Fatal(err)
		}
	}
	id2 := runtimeMail(e, room, "SYNTHETIC_NEW_PENDING")
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dispatch()
	if b := e.state("alice").Terminal; b.Outcome != "uncertain attempt; retained mail requires inbox" || len(b.AttemptIDs) != 1 || len(b.Reservations) != 1 || len(b.Rung) != 0 {
		t.Fatalf("uncertain fence changed: %+v", b)
	}
	e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
	dispatch()
	if b := e.state("alice").Terminal; b.Outcome != "suppressed: manual gate" || len(b.AttemptIDs) != 1 {
		t.Fatalf("pause lost fence: %+v", b)
	}
	if e.received("alice", id) || e.received("alice", id2) {
		t.Fatal("suppression receipted mail")
	}
	e.as("alice", "hook", "resume", "--agent", "alice").ok(t)
	e.as("alice", "inbox").ok(t)
	dispatch()
	if b := e.state("alice").Terminal; b.Outcome != "armed: no new mail" || len(b.AttemptIDs) != 0 || len(b.Reservations) != 1 {
		t.Fatalf("receipt did not release only the uncertain IDs: %+v", b)
	}
}

const bindFixture = `import os,sys,tty,json,select,subprocess
tty.setraw(0)
root=sys.argv[1]; exe=sys.argv[2]; text='';submitted=[]
def save():
    with open(root+'/screen.next','w') as f: json.dump({'text':text,'submitted':submitted,'pid':os.getpid()},f)
    os.replace(root+'/screen.next',root+'/screen.json')
def draw():
    os.write(1,('\x1b[2J\x1b[20;1H› '+text+'\x1b[22;1H  ? for shortcuts\x1b[20;'+str(3+len(text))+'H').encode());save()
draw()
while True:
    if os.path.exists(root+'/command.json'):
        with open(root+'/command.json') as f: request=json.load(f)
        os.unlink(root+'/command.json')
        args=[exe,'--as','alice','bind','--runtime','codex','--owner-pid',str(os.getpid()),'--interval','1s','--retry-max','2s','--max-wakes-per-hour','6']
        p=subprocess.run(args,capture_output=True,text=True,timeout=12)
        with open(root+'/result.next','w') as f: json.dump({'code':p.returncode,'out':p.stdout,'err':p.stderr,'request':request},f)
        os.replace(root+'/result.next',root+'/result.json')
    readable,_,_=select.select([0],[],[],0.02)
    if readable:
        c=os.read(0,1)
        if c in (b'\r',b'\n'): submitted.append(text);text=''
        elif c in (b'\x7f',b'\x08'): text=text[:-1]
        else:text+=c.decode('ascii')
        draw()
`

func awaitBinding(t *testing.T, p config.Paths, test func(config.State) bool) config.State {
	t.Helper()
	until := time.Now().Add(8 * time.Second)
	for time.Now().Before(until) {
		st, err := config.LoadState(p, "alice")
		if err == nil && test(st) {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := config.LoadState(p, "alice")
	t.Fatalf("binding condition not reached: terminal=%+v err=%v", st.Terminal, err)
	return st
}

// Public CLI integration uses a fake homeserver and a newly created private
// tmux server. It proves detached bind survives the command, dedups without
// receipts, resumes after transient sync failure, refuses duplicates, and
// unbind retires/stops its own PID without dropping mail. No real credentials,
// provider sessions, hooks or Keychain commands participate.
func TestBindDetachedLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("detached CLI integration")
	}
	for _, tool := range []string{"tmux", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " unavailable")
		}
	}
	e, room := world(t)
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(e.dir, "state", "alice.binding.log"))
			t.Logf("synthetic binding log: %s", b)
		}
	})
	e.extra[EnvSessionID] = "bind-fixture"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dir, err := os.MkdirTemp("/tmp", "ab-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	exe := filepath.Join(dir, "a2a")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", exe, "../../cmd/a2a").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	script := filepath.Join(dir, "fixture.py")
	if err = os.WriteFile(script, []byte(bindFixture), 0600); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "socket")
	cmd := exec.CommandContext(ctx, "tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "python3", script, dir, exe)
	cmd.Env = append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_SESSION_ID=bind-fixture", "A2A_AGENT=alice", "CODEX_THREAD_ID=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private tmux: %v %s", err, out)
	}
	var screen struct {
		Text      string
		Submitted []string
		PID       int
	}
	readScreen := func() bool {
		b, err := os.ReadFile(filepath.Join(dir, "screen.json"))
		return err == nil && json.Unmarshal(b, &screen) == nil
	}
	until := time.Now().Add(5 * time.Second)
	for !readScreen() && time.Now().Before(until) {
		time.Sleep(5 * time.Millisecond)
	}
	if screen.PID <= 0 {
		t.Fatal("fixture not ready")
	}
	out, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	serverPID, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	t.Cleanup(func() {
		st, _ := config.LoadState(e.paths(), "alice")
		if st.Terminal != nil {
			if birth, _ := tmuxwake.Birth(context.Background(), st.Terminal.DaemonPID); birth == st.Terminal.DaemonBirth && birth != "" {
				p, _ := os.FindProcess(st.Terminal.DaemonPID)
				p.Signal(syscall.SIGTERM)
			}
		}
		for _, pid := range []int{screen.PID, serverPID} {
			p, _ := os.FindProcess(pid)
			p.Signal(syscall.SIGTERM)
		}
	})
	request := func(id int) result {
		b, _ := json.Marshal(id)
		if err := os.WriteFile(filepath.Join(dir, "command.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
		var r struct {
			Code     int
			Out, Err string
			Request  int
		}
		until := time.Now().Add(12 * time.Second)
		for time.Now().Before(until) {
			b, err := os.ReadFile(filepath.Join(dir, "result.json"))
			if err == nil && json.Unmarshal(b, &r) == nil && r.Request == id {
				return result{r.Code, r.Out, r.Err}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("bind command did not finish")
		return result{}
	}
	request(1).ok(t)
	st := awaitBinding(t, e.paths(), func(st config.State) bool {
		return st.Terminal != nil && strings.HasPrefix(st.Terminal.Outcome, "armed:")
	})
	pid := st.Terminal.DaemonPID
	if pid <= 0 || processGone(pid) {
		t.Fatal("detached process absent")
	}
	request(2).failsWith(t, ExitError, "already bound")
	id := runtimeMail(e, room, "SYNTHETIC_PEER_BODY_MUST_NOT_BE_TYPED")
	awaitBinding(t, e.paths(), func(st config.State) bool {
		return st.Terminal.Outcome == "wake submitted" || len(st.Terminal.Rung) == 1
	})
	if e.received("alice", id) {
		t.Fatal("wake receipted mail")
	}
	readScreen()
	if len(screen.Submitted) != 1 || strings.Contains(screen.Submitted[0], "SYNTHETIC_PEER_BODY") {
		t.Fatalf("unsafe wake: %#v", screen)
	}
	// Wait across additional real syncs: the same unreceived event cannot ring again.
	count := e.hs.SyncCount("@alice:" + testServer)
	until = time.Now().Add(5 * time.Second)
	for e.hs.SyncCount("@alice:"+testServer) < count+2 && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	readScreen()
	if len(screen.Submitted) != 1 {
		t.Fatal("unreceived event replayed")
	}
	e.hs.fail("GET /_matrix/client/v3/sync", 503, "M_UNKNOWN", "synthetic transient")
	awaitBinding(t, e.paths(), func(st config.State) bool { return st.Terminal.Outcome == "sync unavailable; retrying" })
	e.hs.mu.Lock()
	delete(e.hs.failures, "GET /_matrix/client/v3/sync")
	e.hs.mu.Unlock()
	id2 := runtimeMail(e, room, "AFTER_RETRY")
	awaitBinding(t, e.paths(), func(st config.State) bool { return len(st.Terminal.Rung) == 2 })
	stop := exec.CommandContext(ctx, exe, "--as", "alice", "unbind")
	stop.Env = append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_SESSION_ID=bind-fixture", "A2A_AGENT=alice", "CODEX_THREAD_ID=")
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("unbind: %v %s", err, out)
	}
	if !processGone(pid) || e.received("alice", id) || e.received("alice", id2) {
		t.Fatal("unbind leaked process or receipted mail")
	}
	if !e.state("alice").Terminal.Stopped {
		t.Fatal("unbind did not retire generation")
	}
	request(3).ok(t)
	st = awaitBinding(t, e.paths(), func(st config.State) bool {
		return st.Terminal.DaemonPID != pid && strings.HasPrefix(st.Terminal.Outcome, "armed:")
	})
	newPID := st.Terminal.DaemonPID
	readScreen()
	if len(screen.Submitted) != 2 {
		t.Fatal("same-session rebind replayed retained mail")
	}
	owner, _ := os.FindProcess(screen.PID)
	owner.Signal(syscall.SIGTERM)
	awaitBinding(t, e.paths(), func(st config.State) bool {
		return st.Terminal.Stopped && st.Terminal.Outcome == "target exited or replaced"
	})
	until = time.Now().Add(3 * time.Second)
	for !processGone(newPID) && time.Now().Before(until) {
		time.Sleep(10 * time.Millisecond)
	}
	if !processGone(newPID) {
		t.Fatal("dead target left its daemon alive")
	}
}
