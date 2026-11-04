//go:build n1_fixture && darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/n1fixture"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"golang.org/x/sys/unix"
)

var n1Clock = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func n1Must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func n1Await(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	n1Must(t, n1fixture.Until(ctx, func() bool { _, e := os.Stat(filepath.Join(n1fixture.Dir(), name)); return e == nil }))
}
func n1RoleEnv(role, nonce string) []string {
	var out []string
	for _, s := range os.Environ() {
		if !strings.HasPrefix(s, "N1_ROLE=") && !strings.HasPrefix(s, "N1_NONCE=") {
			out = append(out, s)
		}
	}
	return append(out, "N1_ROLE="+role, "N1_NONCE="+nonce)
}
func n1Spawn(exe, role string, args ...string) (*n1fixture.Child, error) {
	nonce := n1fixture.Nonce()
	cmd := exec.Command(exe, append([]string{"-test.run=^TestN1Role$", "--"}, args...)...)
	cmd.Env = n1RoleEnv(role, nonce)
	f, e := os.OpenFile(filepath.Join(n1fixture.Dir(), role+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = f
	return n1fixture.Start(cmd, nonce, "", role)
}

// This selector is an explicit child entry point, never a substitute for the
// public witness. A missing sealed parent launch fails rather than skipping.
func TestN1Role(t *testing.T) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	n1fixture.WatchCancellation(ctx, cancel)
	ctx, deadline := context.WithTimeout(ctx, 55*time.Second)
	defer deadline()
	role := os.Getenv("N1_ROLE")
	if role == "pane-supervisor" {
		n1Must(t, n1fixture.BootstrapSelf(role))
		n1Must(t, n1fixture.Latch(ctx, "pane-sealed"))
		child, e := n1Spawn(filepath.Join(n1fixture.Dir(), "codex"), "runtime")
		n1Must(t, e)
		n1Must(t, n1fixture.Write("pane-child.json", child.Row))
		select {
		case <-child.Done:
			n1Must(t, n1fixture.CloseChild(ctx, child))
			if child.Err != nil {
				t.Fatal("runtime exited unsuccessfully")
			}
		case <-ctx.Done():
			n1Must(t, n1fixture.CloseChild(context.Background(), child))
		}
		n1Must(t, n1fixture.Write("pane-exit.json", map[string]any{"pid": os.Getpid(), "nonce": os.Getenv("N1_NONCE"), "runtime_wait_returned": true}))
		return
	}
	n1Must(t, n1fixture.Enter(ctx))
	if role == "runtime" {
		n1Runtime(t, ctx)
		return
	}
	if role == "conflict" || role == "valid-ledger" {
		<-ctx.Done()
		return
	}
	a := NewApp()
	a.Now = func() time.Time { return n1Clock }
	a.Stdin = strings.NewReader("")
	if role == "prior-consumer" || role == "new-consumer" {
		i := 0
		for ; i < len(os.Args) && os.Args[i] != "--"; i++ {
		}
		if i == len(os.Args) {
			t.Fatal("missing production argv")
		}
		code := a.Run(ctx, os.Args[i+1:])
		n1Must(t, n1fixture.Write(role+"-result.json", map[string]any{"app_exit": code, "context_canceled": ctx.Err() != nil}))
		// Preserve main's real exit code, including startup cancellation. A parent
		// Wait receipt is never represented as a successful App execution.
		if code != 0 {
			os.Exit(code)
		}
		return
	}
	if role != "initial" && role != "replacement" {
		t.Fatal("unknown fixture role")
	}
	// Initialize the ordinary App connection, then take the actual automatic bind
	// path. No fake state writer, guard bypass or altered control lock is used.
	if code := a.Run(ctx, []string{"--as", "alice", "whoami"}); code != 0 {
		t.Fatal("connection setup failed")
	}
	args := []string{"--runtime", "codex", "--owner-pid", strconv.Itoa(os.Getppid()), "--session-id", "n1-fixture-session", "--interval", "1s", "--retry-max", "2s", "--max-wakes-per-hour", "6"}
	e := a.bindTerminal(ctx, args, bindingSessionStart)
	result := map[string]any{"ok": e == nil, "old_image_refusal": errors.Is(e, ErrBindingOldImage)}
	n1Must(t, n1fixture.Write(role+"-result.json", result))
	// bindTerminal's real child Wait is owned by this explicit fixture supervisor.
	// It remains alive until that Wait returns, even after the bind call completed.
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer waitCancel()
	if waitErr := n1fixture.WaitAll(waitCtx); waitErr != nil {
		_ = n1fixture.Write(role+"-wait-STOP.json", map[string]any{"reason": "owned child Wait unresolved; no clean exit claim; retain entire attempt"})
		t.Fatalf("owned consumer Wait/custody unresolved: %v", waitErr)
	}

	if e != nil && !errors.Is(e, ErrBindingOldImage) {
		t.Fatal("bind failed")
	}
}
func n1Runtime(t *testing.T, ctx context.Context) {
	tty, e := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	n1Must(t, e)
	defer tty.Close()
	saved, e := unix.IoctlGetTermios(int(tty.Fd()), unix.TIOCGETA)
	n1Must(t, e)
	defer unix.IoctlSetTermios(int(tty.Fd()), unix.TIOCSETA, saved)
	raw := *saved
	raw.Iflag &^= unix.BRKINT | unix.ICRNL | unix.INPCK | unix.ISTRIP | unix.IXON
	raw.Oflag &^= unix.OPOST
	raw.Cflag |= unix.CS8
	raw.Lflag &^= unix.ECHO | unix.ICANON | unix.IEXTEN | unix.ISIG
	raw.Cc[unix.VMIN] = 1
	raw.Cc[unix.VTIME] = 0
	n1Must(t, unix.IoctlSetTermios(int(tty.Fd()), unix.TIOCSETA, &raw))
	n1Must(t, unix.SetNonblock(int(tty.Fd()), true))
	notice := tmuxwake.Notice(1)
	_, e = fmt.Fprintf(tty, "\x1b[2J\x1b[20;1H\x1b[1m›\x1b[0m %s\x1b[22;1H  Codex · Ready · Context 50%% left\x1b[20;%dH", notice, 3+len(notice))
	n1Must(t, e)
	n1Must(t, n1fixture.Write("runtime-ready.json", map[string]any{"pid": os.Getpid(), "input_bytes": 0}))
	var children []*n1fixture.Child
	// Each Start immediately returns a handle with an installed Wait owner. Cleanup
	// is registered here before any later assertion, request or latch operation.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		for i := len(children) - 1; i >= 0; i-- {
			if e := n1fixture.CloseChild(cleanup, children[i]); e != nil {
				t.Errorf("owned invocation cleanup failed: %v", e)
			}
		}
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	input := 0
	for {
		select {
		case <-ctx.Done():
			n1Must(t, n1fixture.Write("runtime-input.json", map[string]int{"input_bytes": input}))
			return
		case <-ticker.C:
			var buf [4096]byte
			n, e := unix.Read(int(tty.Fd()), buf[:])
			if n > 0 {
				input += n
			}
			if e != nil && e != unix.EAGAIN && e != unix.EINTR {
				t.Fatal("fixture tty read failed")
			}
			if n1fixture.Canceled() {
				continue
			}
			for _, role := range []string{"initial", "replacement", "conflict", "valid-ledger"} {
				path := filepath.Join(n1fixture.Dir(), role+".request")
				if _, e := os.Stat(path); e != nil {
					continue
				}
				n1Must(t, os.Remove(path))
				exe := filepath.Join(n1fixture.Dir(), "a2a-n1.test")
				if role == "conflict" {
					exe = filepath.Join(n1fixture.Dir(), "a2a-oldimage.test")
				}
				child, e := n1Spawn(exe, role)
				if child != nil {
					children = append(children, child)
				}
				n1Must(t, e)
				n1Must(t, n1fixture.Write(role+"-child.json", child.Row))
			}
		}
	}
}

// The loopback server applies the real fakeHS send into its idempotency table
// before ACK is held. Pinning and Sent are production report-writer operations.
func n1AckServer(t *testing.T, hs *fakeHS, ctx context.Context) {
	handler := hs.srv.Config.Handler
	hs.srv.Close()
	hs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PUT" && strings.Contains(r.URL.Path, "/send/m.room.message/") {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, r)
			if response.Code != 200 {
				t.Error("fixture send did not apply")
			}
			_ = n1fixture.Write("send-applied.json", map[string]any{"status": response.Code, "at": time.Now().UTC()})
			if _, e := os.Stat(filepath.Join(n1fixture.Dir(), "ack-hold")); e == nil {
				if e = n1fixture.Latch(ctx, "send-ack"); e != nil {
					http.Error(w, "fixture canceled", 503)
					return
				}
			}
			for k, v := range response.Header() {
				w.Header()[k] = v
			}
			w.WriteHeader(response.Code)
			_, _ = w.Write(response.Body.Bytes())
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(hs.srv.Close)
}
func n1FixtureEnv(t *testing.T, dir string, ctx context.Context) *env {
	hs := newFakeHS(t)
	n1AckServer(t, hs, ctx)
	configDir := filepath.Join(dir, "config")
	n1Must(t, os.MkdirAll(configDir, 0700))
	e := &env{t: t, hs: hs, dir: configDir, tokens: map[string]string{}, extra: map[string]string{EnvSessionID: "n1-fixture-session"}, now: func() time.Time { return n1Clock }}
	n1Must(t, os.WriteFile(filepath.Join(configDir, "matrix.env"), []byte("A2A_MATRIX_URL="+hs.srv.URL+"\nA2A_MATRIX_SERVER_NAME="+testServer+"\nA2A_MATRIX_REGISTRATION_TOKEN="+hs.regToken+"\n"), 0600))
	e.agent("alice")
	e.agent("bob")
	return e
}
func n1Command(t *testing.T, ctx context.Context, args ...string) string {
	t.Helper()
	s, e := n1CommandErr(ctx, args...)
	n1Must(t, e)
	return s
}
func n1CommandErr(ctx context.Context, args ...string) (string, error) {
	cmd := exec.Command("tmux", append([]string{"-S", filepath.Join(n1fixture.Dir(), "socket")}, args...)...)
	// Synchronous private clients are direct Start/Wait children, not server receipts.
	f, e := os.CreateTemp(n1fixture.Dir(), "client-")
	if e != nil {
		return "", e
	}
	defer os.Remove(f.Name())
	defer f.Close()
	cmd.Stdout = f
	cmd.Stderr = f
	// Fast clients are never enumerated/signalled. Their actual Wait is a client
	// receipt only; no claim that it reaps the foreground server.
	if e = n1fixture.Client(ctx, cmd); e != nil {
		return "", e
	}
	_, e = f.Seek(0, 0)
	if e != nil {
		return "", e
	}
	b, e := io.ReadAll(f)
	return strings.TrimSpace(string(b)), e
}
func n1Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

// This is a process witness, NOT EXECUTED under the R4 preparation admission.
// The three leaves exercise the original load and real consumer report/ACK/death,
// then both replacement writes and a new real consumer observation. Global ps
// enumeration is replaced by the sealed ledger; all claimed scope is fixture-only.
func TestTerminalNoticeReplacementInterleaving(t *testing.T) {
	for _, leaf := range []string{"pin-after-load", "ack-after-load", "unchanged"} {
		t.Run(leaf, func(t *testing.T) { n1Witness(t, leaf, false) })
	}
}

// A registered OWN conflicting executable must reach the real guard refusal,
// with zero replacement/send effects. The nearby same-image row must pass the
// same guard and the full witness. Neither leaf has execution admission in R4.
func TestTerminalNoticeLedgerGuard(t *testing.T) {
	for _, leaf := range []string{"valid", "owned-conflict"} {
		t.Run(leaf, func(t *testing.T) { n1Witness(t, "unchanged", leaf == "owned-conflict") })
	}
}
func n1Witness(t *testing.T, leaf string, conflict bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	dir, e := os.MkdirTemp(os.Getenv("TMPDIR"), "n1-owned-")
	n1Must(t, e)
	n1Must(t, os.Chmod(dir, 0700))
	t.Setenv("N1_DIR", dir)
	t.Setenv("N1_NONCE", n1fixture.Nonce())
	n1Must(t, n1fixture.BootstrapSelf("driver"))
	// No t.TempDir: failure must retain the attempt if any descendant is uncertain.
	var server *n1fixture.Child
	panePID := 0
	paneStarted := false
	paneNonce := os.Getenv("N1_NONCE")
	var cleanupErr error
	defer func() {
		// Cancel blocked report/ACK/load/startup latches BEFORE stopping consumers.
		// Cancellation is visible to their signal contexts; never release replacement
		// into a fresh Start during cleanup. Signal consumers first; parent supervisors
		// then exit after their real Wait. Driver holds no state/control lock here.
		addFailure := func(stage string, e error) {
			if e != nil {
				cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", stage, e))
			}
		}
		addFailure("cancel fixture latches", n1fixture.Cancel())
		cancel()
		cleanup, done := context.WithTimeout(context.Background(), 18*time.Second)
		defer done()
		rows, err := n1fixture.Rows()
		addFailure("read cleanup ledger", err)
		for _, role := range []string{"consumer", "initial", "replacement", "conflict", "valid-ledger", "runtime"} {
			for _, r := range rows {
				if r.Role == role {
					// Reaped is an actual Wait fact, not permission to skip its evidence.
					addFailure(fmt.Sprintf("stop/verify %s PID %d", role, r.PID), n1fixture.Stop(cleanup, r.PID))
				}
			}
		}
		paneExitVerified := !paneStarted
		if paneStarted && panePID == 0 {
			addFailure("verify pane identity", errors.New("created pane was never sealed"))
		}
		if panePID > 0 {
			var receipt map[string]any
			err = n1fixture.Until(cleanup, func() bool { return n1fixture.Read("pane-exit.json", &receipt) == nil })
			if err != nil {
				addFailure("read pane exit receipt", err)
			} else if receipt["nonce"] != paneNonce || receipt["runtime_wait_returned"] != true {
				addFailure("verify pane exit receipt", errors.New("pane nonce/runtime Wait mismatch"))
			} else {
				var clientErr error
				err = n1fixture.Until(cleanup, func() bool {
					s, e := n1CommandErr(cleanup, "display-message", "-p", "-t", "%0", "#{pane_id} #{pane_pid} #{pane_dead} #{pane_dead_status}")
					clientErr = e
					return e == nil && s == fmt.Sprintf("%%0 %d 1 0", panePID)
				})
				if err != nil {
					addFailure("verify private pane exit", errors.Join(err, clientErr))
				} else {
					paneExitVerified = true
				}
			}
		}
		// Keep the explicit pane alternative: no driver waitpid is invented.
		if server != nil && paneExitVerified {
			addFailure("close foreground server", n1fixture.CloseChild(cleanup, server))
		} else if server != nil {
			addFailure("close foreground server", errors.New("pane exit unverified; retain server and attempt"))
		}
		addFailure("aggregate child/client custody", n1fixture.WaitAll(cleanup))
		var input struct {
			InputBytes int `json:"input_bytes"`
		}
		if panePID > 0 {
			err = n1fixture.Read("runtime-input.json", &input)
			addFailure("read renderer input evidence", err)
			if err == nil && input.InputBytes != 0 {
				addFailure("verify renderer input", errors.New("unexpected input bytes"))
			}
		}
		if cleanupErr == nil && panePID > 0 {
			addFailure("persist explicit pane exit", n1fixture.MarkPaneExited(panePID))
		}
		// Validates completed rows again and propagates final receipt write errors
		// through this existing test path, even if no STOP file can be written.
		if err = n1fixture.FinishCleanup(cleanupErr); err != nil {
			t.Errorf("cleanup custody unresolved; retained attempt %s: %v", dir, err)
			return
		}

		// Receipts copied by the execution wrapper before explicit later removal.
		// This driver deliberately retains every attempt; no secret-bearing export.
	}()
	env := n1FixtureEnv(t, dir, ctx)
	room := env.hs.addRoom("proj", "@alice:test.local", "@bob:test.local")
	own := env.ownReportRoom("alice")
	env.as("alice", "wait", "--timeout", "1ms").ok(t)
	mail := runtimeMail(env, room, "N1_SYNTHETIC_PENDING")
	exe, e := os.Executable()
	n1Must(t, e)
	n1Must(t, os.Link(exe, filepath.Join(dir, "a2a-n1.test")))
	n1Must(t, os.Link(exe, filepath.Join(dir, "codex")))
	// Different inode, identical bytes: the actual running-image guard must refuse.
	b, e := os.ReadFile(exe)
	n1Must(t, e)
	n1Must(t, os.WriteFile(filepath.Join(dir, "a2a-oldimage.test"), b, 0700))
	cmd := exec.Command("tmux", "-D", "-S", filepath.Join(dir, "socket"), "-f", "/dev/null")
	cmd.Env = append(n1RoleEnv("pane-supervisor", paneNonce), "A2A_CONFIG_DIR="+env.dir, "A2A_AGENT=alice", "A2A_SESSION_ID=n1-fixture-session", "TERM=xterm-256color")
	log, e := os.OpenFile(filepath.Join(dir, "server.log"), os.O_CREATE|os.O_WRONLY, 0600)
	n1Must(t, e)
	defer log.Close()
	cmd.Stdout = log
	cmd.Stderr = log
	server, e = n1fixture.OwnServer(cmd, paneNonce)
	n1Must(t, e)
	n1Must(t, n1fixture.Until(ctx, func() bool { _, e := os.Stat(filepath.Join(dir, "socket")); return e == nil }))
	n1Command(t, ctx, "set-option", "-g", "remain-on-exit", "on")
	shell := "exec " + n1Quote(filepath.Join(dir, "a2a-n1.test")) + " -test.run=^TestN1Role$ > " + n1Quote(filepath.Join(dir, "pane.log")) + " 2>&1"
	paneStarted = true
	n1Command(t, ctx, "new-session", "-d", "-s", "n1", "-x", "120", "-y", "24", shell)
	n1Await(t, ctx, "pane-bootstrap.json")
	var pane struct {
		PID, Parent int
		Nonce       string
	}
	n1Must(t, n1fixture.Read("pane-bootstrap.json", &pane))
	panePID = pane.PID
	if pane.Parent != server.Row.PID || pane.Nonce != paneNonce {
		t.Fatal("private pane bootstrap parent/nonce mismatch")
	}
	if got := n1Command(t, ctx, "display-message", "-p", "-t", "%0", "#{pane_id} #{pane_pid} #{pane_dead}"); got != fmt.Sprintf("%%0 %d 0", panePID) {
		t.Fatal("private pane metadata mismatch")
	}
	n1Must(t, n1fixture.Require(panePID))
	n1Must(t, n1fixture.Release("pane-sealed"))
	n1Await(t, ctx, "runtime-ready.json")
	n1Must(t, n1fixture.Write("initial.request", true))
	n1Await(t, ctx, "initial-result.json")
	n1Await(t, ctx, "prior-consumer-startup.arrived")
	var initial map[string]any
	n1Must(t, n1fixture.Read("initial-result.json", &initial))
	if initial["ok"] != true {
		t.Fatal("initial bind failed")
	}
	st := env.state("alice")
	prior := *st.Terminal
	_, e = config.UpdateState(env.paths(), "alice", func(st *config.State) error {
		b := st.Terminal
		b.Typed = true
		b.Notice = tmuxwake.Notice(1)
		b.NoticeCount = 1
		b.AttemptIDs = []string{mail}
		b.Rung = []string{mail}
		b.AttemptAt = n1Clock.Add(-10 * time.Minute)
		b.Reservations = []time.Time{n1Clock.Add(-10 * time.Minute)}
		yes := true
		b.OwnNotice = config.OwnNoticeObservation{PossiblyOrphaned: &yes, Reason: orphanReason, Since: n1Clock.Add(-10 * time.Minute).Format(time.RFC3339Nano), ObservedAt: n1Clock.Format(time.RFC3339Nano)}
		return nil
	})
	n1Must(t, e)
	n1Must(t, n1fixture.Release("prior-consumer-startup"))
	n1Await(t, ctx, "prior-report.arrived")
	if leaf == "ack-after-load" {
		n1Must(t, n1fixture.Write("ack-hold", true))
		n1Must(t, n1fixture.Release("prior-report"))
		n1Await(t, ctx, "send-ack.arrived")
	}
	if leaf == "unchanged" {
		n1Must(t, n1fixture.Release("prior-report"))
		n1Await(t, ctx, "prior-consumer-reported.arrived")
	}
	role := "valid-ledger"
	if conflict {
		role = "conflict"
	}
	n1Must(t, n1fixture.Write(role+".request", true))
	n1Await(t, ctx, role+"-child.json")
	n1Must(t, n1fixture.Write("replacement.request", true))
	n1Await(t, ctx, "replacement-load.arrived")
	var loaded config.TerminalBinding
	n1Must(t, n1fixture.Read("loaded.json", &loaded))
	if leaf == "pin-after-load" {
		if loaded.NoticeReport != nil {
			t.Fatal("pin leaf did not load before pin")
		}
		n1Must(t, n1fixture.Release("prior-report"))
	}
	if leaf == "ack-after-load" {
		if loaded.NoticeReport == nil || loaded.NoticeReport.Sent {
			t.Fatal("ACK leaf did not load pending pin")
		}
		n1Must(t, n1fixture.Release("send-ack"))
	}
	n1Await(t, ctx, "prior-consumer-reported.arrived")
	before := env.state("alice")
	mailBefore, err := os.ReadFile(env.paths().MailboxFile("alice"))
	n1Must(t, err)
	callsBeforeGuard := env.hs.CallCount(sendRoute)
	pin := before.Terminal.NoticeReport
	if pin == nil || !pin.Sent || len(env.projectionsFrom(own, "@alice:test.local")) != 1 {
		t.Fatal("setup: real report/ACK not committed")
	}
	// Replacement still holds control. Driver holds neither control nor state.
	// Prior is outside both locks at a context-cancelable report-completion latch.
	n1Must(t, n1fixture.StopConsumer(ctx, prior.DaemonPID, prior.Generation))
	n1Await(t, ctx, fmt.Sprintf("wait-%d.json", prior.DaemonPID))
	var priorWait struct {
		Exit   int
		Signal string
	}
	n1Must(t, n1fixture.Read(fmt.Sprintf("wait-%d.json", prior.DaemonPID), &priorWait))
	if priorWait.Exit != 0 || priorWait.Signal != "" {
		t.Fatal("prior consumer did not terminate normally after report")
	}
	var priorResult struct {
		AppExit int `json:"app_exit"`
	}
	n1Must(t, n1fixture.Read("prior-consumer-result.json", &priorResult))
	if priorResult.AppExit != 0 {
		t.Fatal("prior real consumer App failure")
	}
	n1Must(t, n1fixture.Release("replacement-load"))
	n1Await(t, ctx, "replacement-result.json")
	var result map[string]any
	n1Must(t, n1fixture.Read("replacement-result.json", &result))
	if conflict {
		if result["old_image_refusal"] != true || !reflect.DeepEqual(before, env.state("alice")) || env.hs.CallCount(sendRoute) != callsBeforeGuard || len(env.projectionsFrom(own, "@alice:test.local")) != 1 {
			t.Fatal("OWN guard conflict did not refuse with zero effects")
		}
		return
	}
	if result["ok"] != true {
		t.Fatal("valid own-ledger guard refused")
	}
	n1Await(t, ctx, "new-consumer-startup.arrived")
	after := env.state("alice")
	if !reflect.DeepEqual(after.Terminal.NoticeReport, pin) || after.Terminal.NoticeReport == nil || !after.Terminal.NoticeReport.Sent {
		t.Fatal("OWN replacement lost current pin or Sent")
	}
	n1Authority(t, before, after)
	calls := env.hs.CallCount(sendRoute)
	n1Must(t, n1fixture.Release("new-consumer-startup"))
	n1Await(t, ctx, "new-report.arrived")
	n1Must(t, n1fixture.Release("new-report"))
	n1Await(t, ctx, "new-consumer-reported.arrived")
	final := env.state("alice")
	if !reflect.DeepEqual(final.Terminal.NoticeReport, pin) || env.hs.CallCount(sendRoute) != calls || len(env.projectionsFrom(own, "@alice:test.local")) != 1 {
		t.Fatal("OWN replacement repeated report/event/transaction")
	}
	n1Authority(t, before, final)
	mailAfter, err := os.ReadFile(env.paths().MailboxFile("alice"))
	n1Must(t, err)
	if string(mailBefore) != string(mailAfter) {
		t.Fatal("OWN fixture changed mailbox bytes")
	}
	if env.received("alice", mail) {
		t.Fatal("OWN fixture receipted pending mail")
	}
	_ = n1fixture.Write("witness.json", map[string]any{"leaf": leaf, "applied_events": 1, "exact_report": true, "no_new_txn": true, "journal_notification_mail_unchanged": true})
}
func n1Authority(t *testing.T, before, after config.State) {
	t.Helper()
	a, b := before.Terminal, after.Terminal
	if !reflect.DeepEqual(a.AttemptIDs, b.AttemptIDs) || !reflect.DeepEqual(a.Rung, b.Rung) || !reflect.DeepEqual(a.Reservations, b.Reservations) || a.Notice != b.Notice || a.NoticeCount != b.NoticeCount || !a.AttemptAt.Equal(b.AttemptAt) || a.Typed != b.Typed || a.Retried != b.Retried || a.NotificationToken != b.NotificationToken || !reflect.DeepEqual(before.Notification, after.Notification) || !reflect.DeepEqual(before.Wake, after.Wake) {
		t.Fatal("OWN replacement changed journal/reservation/notification/input authority")
	}
}
