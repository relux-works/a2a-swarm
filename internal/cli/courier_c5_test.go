//go:build linux || darwin

package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
	"github.com/relux-works/a2a-swarm/internal/courierenroll"
)

type c5Station struct {
	home, socket string
	done         chan error
	cancel       context.CancelFunc
}

func newC5Station(t *testing.T, handler courier.Handler) *c5Station {
	t.Helper()
	home := canonicalRoot(t)
	daemon := filepath.Join(canonicalRoot(t), "home")
	socket := filepath.Join(shortCourierBase(t), "s", "c.sock")
	key, err := courier.Register(daemon, "station", "identity", uint32(os.Geteuid()), false)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	payload, err := json.Marshal(map[string]string{"socket": socket, "station": "station", "identity": "identity", "secret_b64": base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	if err := courierenroll.Install(filepath.Join(home, ".a2a"), bytes.NewReader(append(payload, '\n')), false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &c5Station{home: home, socket: socket, done: make(chan error, 1), cancel: cancel}
	go func() { s.done <- courier.ServeWithHandler(ctx, daemon, socket, io.Discard, handler) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-s.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(courier.LockReleaseBound):
			t.Error("shutdown timeout")
		}
	})
	timer := time.NewTimer(courier.LockReleaseBound)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if c, err := net.DialTimeout("unix", socket, 10*time.Millisecond); err == nil {
			c.Close()
			break
		}
		select {
		case err := <-s.done:
			t.Fatal("serve refused", err)
		case <-timer.C:
			t.Fatal("startup timeout")
		case <-tick.C:
		}
	}
	return s
}
func (s *c5Station) run(ctx context.Context, input string, args ...string) result {
	var out, diag bytes.Buffer
	a := &App{Stdout: &out, Stderr: &diag, Stdin: strings.NewReader(input), Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return s.home
		}
		return ""
	}}
	code := a.Run(ctx, args)
	return result{code: code, stdout: out.String(), stderr: diag.String()}
}

// Proves station diagnostics remain closed through the public dispatcher and
// never suggest enrollment for availability, identity or allowlist failures.
// Every mutation restores its valid neighbor; kernel UID isolation is separate.
func TestCourierStationClosedDiagnostics(t *testing.T) {
	s := newC5Station(t, func(context.Context, courier.Invocation) int { return 0 })
	good := func() {
		t.Helper()
		r := s.run(context.Background(), "", "whoami")
		if r.code != 0 || r.stderr != "" || r.stdout != "" {
			t.Fatal("valid neighbor refused", r)
		}
	}
	good()
	for _, tc := range []struct {
		args  []string
		class courier.Diagnostic
	}{
		{[]string{"--as", "other", "whoami"}, courier.IdentityMismatch},
		{[]string{"post", "room", "BODY_TOKEN_MARKER", "--id-file", "PRIVATE_MARKER"}, courier.NotAllowed},
		{[]string{"post", "room", "BODY_TOKEN_MARKER", "--config", "PRIVATE_MARKER"}, courier.NotAllowed},
		{[]string{"whoami", "--profile", "a", "--profile", "b"}, courier.NotAllowed},
	} {
		r := s.run(context.Background(), "", tc.args...)
		if r.code != ExitError || r.stdout != "" || r.stderr != "a2a: courier: "+string(tc.class)+"\n" {
			t.Fatal("station diagnostic differs", r)
		}
		good()
	}
	path := filepath.Join(s.home, ".a2a", "station.key")
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	r := s.run(context.Background(), "", "whoami")
	if r.code != ExitError || r.stdout != "" || r.stderr != "a2a: courier: enrollment_invalid; re-run enroll\n" {
		t.Fatal("invalid enrollment diagnostic", r)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	good()
	pointer := filepath.Join(s.home, ".a2a", "courier.sock")
	if err := os.WriteFile(pointer, []byte(filepath.Join(filepath.Dir(s.socket), "missing")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r = s.run(context.Background(), "", "whoami")
	if r.code != ExitError || r.stdout != "" || r.stderr != "a2a: courier: courier_unavailable\n" {
		t.Fatal("unavailable diagnostic", r)
	}
	if err := os.WriteFile(pointer, []byte(s.socket+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	good()
}

// Proves nil environment discovery and a failing home resolution never fall
// through to local commands. The actual environment override selects the valid
// enrollment beside these guards; no local credential source is consulted.
func TestCourierStationDiscoveryGuards(t *testing.T) {
	for _, mode := range []string{"nil", "resolve-fails"} {
		t.Run(mode, func(t *testing.T) {
			var out, diag bytes.Buffer
			a := &App{Stdout: &out, Stderr: &diag, Stdin: strings.NewReader(""), HTTP: nil, connectionSecretStore: courierSecretTrap{}}
			if mode == "resolve-fails" {
				t.Setenv("HOME", "")
				a.Getenv = func(string) string { return "" }
			}
			handled, code := a.stationInvocation(context.Background(), []string{"whoami"})
			if !handled || code != ExitError || out.Len() != 0 || diag.String() != "a2a: courier: enrollment_invalid; re-run enroll\n" {
				t.Fatal("discovery fell through")
			}
		})
	}
	s := newC5Station(t, func(context.Context, courier.Invocation) int { return 0 })
	t.Setenv(config.EnvConfigDir, s.home)
	var out, diag bytes.Buffer
	a := &App{Stdout: &out, Stderr: &diag, Stdin: strings.NewReader(""), Getenv: os.Getenv, connectionSecretStore: courierSecretTrap{}}
	if a.Run(context.Background(), []string{"whoami"}) != 0 || diag.Len() != 0 {
		t.Fatal("environment config override ignored")
	}
}

// Proves fail-open retains no partial stdout, persists only redacted failure
// metadata, reports a counts-only gap exactly once, and leaves successful
// doorbell bytes unchanged. This is station behavior, not native idle wake.
func TestCourierHookFailOpenAndGap(t *testing.T) {
	const doorbell = "{\"decision\":\"block\",\"reason\":\"mail pending\"}\n"
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		if strings.Contains(string(inv.Request.Stdin), "FAIL_TOKEN_BODY_MARKER") {
			io.WriteString(inv.Stdout, "PARTIAL_PRIVATE_MARKER")
			return 1
		}
		io.WriteString(inv.Stdout, doorbell)
		return 0
	})
	good := func(wantGap bool) {
		t.Helper()
		r := s.run(context.Background(), `{"session_id":"ok"}`, "hook", "stop", "--runtime", "claude")
		if r.code != 0 || r.stdout != doorbell {
			t.Fatal("doorbell bytes changed", r)
		}
		if wantGap {
			if !strings.HasPrefix(r.stderr, "courier: 1 hook deliveries failed open since ") || strings.Count(r.stderr, "\n") != 1 {
				t.Fatal("gap missing", r)
			}
		} else if r.stderr != "" {
			t.Fatal("gap repeated or invented", r)
		}
	}
	good(false)
	if _, err := os.Stat(filepath.Join(s.home, "state")); !os.IsNotExist(err) {
		t.Fatal("no-failure hook wrote local state")
	}
	r := s.run(context.Background(), `{"session_id":"FAIL_TOKEN_BODY_MARKER"}`, "--as", "identity", "hook", "stop", "--runtime", "claude")
	if r.code != 0 || r.stdout != "" || r.stderr != "" {
		t.Fatal("hook did not fail open", r)
	}
	logPath := filepath.Join(s.home, "state", hookFailureLog)
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Fields(string(data))
	if len(parts) != 3 || parts[1] != "Stop" || parts[2] != string(courier.Unavailable) || bytes.Contains(data, []byte("MARKER")) {
		t.Fatal("failure log not redacted")
	}
	if _, err := time.Parse(time.RFC3339Nano, parts[0]); err != nil {
		t.Fatal("timestamp invalid")
	}
	st, err := os.Stat(logPath)
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("failure log mode")
	}
	status := s.run(context.Background(), "", "courier", "status")
	if status.code != 0 || !strings.Contains(status.stdout, "hook_failures: 1\noldest_unreported: "+parts[0]+"\n") {
		t.Fatal("status omitted gap", status)
	}
	good(true)
	good(false)
	status = s.run(context.Background(), "", "courier", "status")
	if status.code != 0 || status.stdout != "hook_failures: 0\noldest_unreported: \n" {
		t.Fatal("gap counter not reset", status)
	}
	after, err := os.ReadFile(logPath)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatal("success erased failure history")
	}
}

// Proves refused, unavailable, busy and budget-expired hooks all return zero
// with empty stdout; a working invocation is the adjacent success control.
func TestCourierHookFailureClassesAndBudget(t *testing.T) {
	entered := make(chan struct{}, courier.MaxStationInvocations+1)
	s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
		if inv.Request.Argv[0] == "whoami" || string(inv.Request.Stdin) == "block" {
			entered <- struct{}{}
			<-ctx.Done()
			return 1
		}
		return 0
	})
	// Actual production 5-second hook budget, including a blocked daemon.
	start := time.Now()
	r := s.run(context.Background(), "block", "hook", "prompt", "--runtime", "codex")
	if r.code != 0 || r.stdout != "" || time.Since(start) < courier.HookBudget-courier.HookStateReserve || time.Since(start) > courier.HookBudget+100*time.Millisecond {
		t.Fatal("hook budget/fail-open contract", r)
	}
	for len(entered) > 0 {
		<-entered
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan result, courier.MaxStationInvocations)
	for range courier.MaxStationInvocations {
		go func() { done <- s.run(cancelCtx, "", "whoami") }()
	}
	for range courier.MaxStationInvocations {
		select {
		case <-entered:
		case r := <-done:
			t.Fatal("invocation ended", r)
		case <-s.done:
			t.Fatal("daemon stopped")
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("invocation not entered")
		}
	}
	r = s.run(context.Background(), "", "hook", "prompt", "--runtime", "codex")
	if r.code != 0 || r.stdout != "" {
		t.Fatal("busy hook did not fail open", r)
	}
	cancel()
	for range courier.MaxStationInvocations {
		select {
		case <-done:
		case <-s.done:
			t.Fatal("daemon stopped")
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("client stuck")
		}
	}
	for _, args := range [][]string{{"--as", "other", "hook", "prompt"}, {"hook", "prompt", "--id-file", "PRIVATE_BODY_MARKER"}} {
		r = s.run(context.Background(), "TOKEN_MARKER", args...)
		if r.code != 0 || r.stdout != "" {
			t.Fatal("refused hook did not fail open", r)
		}
	}
	pointer := filepath.Join(s.home, ".a2a", "courier.sock")
	if err := os.WriteFile(pointer, []byte(filepath.Join(filepath.Dir(s.socket), "missing")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r = s.run(context.Background(), "TOKEN_MARKER", "hook", "prompt")
	if r.code != 0 || r.stdout != "" {
		t.Fatal("unavailable hook did not fail open", r)
	}
	if err := os.WriteFile(pointer, []byte(s.socket+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r = s.run(context.Background(), "", "hook", "prompt")
	if r.code != 0 || r.stdout != "" || !strings.HasPrefix(r.stderr, "courier: 5 hook deliveries failed open since ") {
		t.Fatal("success neighbor/gap failed", r)
	}
	data, err := os.ReadFile(filepath.Join(s.home, "state", hookFailureLog))
	if err != nil {
		t.Fatal(err)
	}
	for _, class := range []courier.Diagnostic{courier.IdleTimeout, courier.Busy, courier.IdentityMismatch, courier.NotAllowed, courier.Unavailable} {
		if !bytes.Contains(data, []byte(" "+string(class)+"\n")) {
			t.Fatal("missing class", class)
		}
	}
	if bytes.Contains(data, []byte("MARKER")) {
		t.Fatal("request leaked")
	}
}

// Proves the bounded log discards oldest lines while the unreported count
// survives the cap, and unknown events/classes cannot inject log content.
func TestCourierHookFailureLogCap(t *testing.T) {
	home := canonicalRoot(t)
	var diag bytes.Buffer
	if err := updateHookFailures(home, "Stop", "", &diag); err != nil || diag.Len() != 0 {
		t.Fatal("clean neighbor failed")
	}
	for index := range courier.HookFailureLines + 3 {
		class := courier.Unavailable
		if index < 3 {
			class = courier.NotAllowed
		}
		if err := updateHookFailures(home, "Stop", class, &diag); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(home, "state", hookFailureLog)
	before, err := os.ReadFile(path)
	if err != nil || bytes.Count(before, []byte("\n")) != courier.HookFailureLines || bytes.Contains(before, []byte(courier.NotAllowed)) {
		t.Fatal("log cap violated")
	}
	for _, tc := range []struct {
		event string
		class courier.Diagnostic
	}{{"BODY_MARKER", courier.Unavailable}, {"Stop", "TOKEN_MARKER"}} {
		if updateHookFailures(home, tc.event, tc.class, &diag) == nil {
			t.Fatal("open log schema")
		}
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid log fields written")
	}
	if err := updateHookFailures(home, "Stop", "", &diag); err != nil || !strings.HasPrefix(diag.String(), "courier: 259 hook deliveries failed open since ") {
		t.Fatal("capped log lost counter")
	}
}

// Proves a pipe that never reaches EOF cannot outlive the station hook input
// budget, with the same reader's completed input as the valid neighbor.
func TestCourierHookInputCancellation(t *testing.T) {
	for _, complete := range []bool{false, true} {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		if complete {
			if _, err := w.Write([]byte("{}")); err != nil {
				t.Fatal(err)
			}
			w.Close()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		data, err := readStationInput(ctx, r, true)
		cancel()
		r.Close()
		w.Close()
		if complete {
			if err != nil || string(data) != "{}" {
				t.Fatal("valid input refused")
			}
		} else if err == nil {
			t.Fatal("input ignored budget")
		}
	}
}

// Proves post/inbox/read/wait and both hook outcomes leave exactly the enrolled
// four entries and the permitted failure log in the station home. Backend state
// is exercised in Courier's separate identity home, using only a local fixture.
func TestCourierStationHomeWriteBoundary(t *testing.T) {
	f := newCourierFixture(t)
	f.run("alice", "", "topic", "new", f.rooms["alice"], "footprint", "Footprint").ok(t)
	f.run("alice", "", "post", f.rooms["alice"], "--topic", "footprint", "hello").ok(t)
	f.run("alice", "", "inbox").ok(t)
	f.run("alice", "", "read", f.rooms["alice"]).ok(t)
	f.run("alice", "", "wait", "--timeout", "1ms").ok(t)
	f.run("alice", `{"session_id":"ok"}`, "hook", "prompt", "--runtime", "claude").ok(t)
	pointer := filepath.Join(f.stations["alice"], ".a2a", "courier.sock")
	if err := os.WriteFile(pointer, []byte(filepath.Join(filepath.Dir(f.socket), "missing")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r := f.run("alice", `{"session_id":"FAIL_PRIVATE_MARKER"}`, "hook", "prompt", "--runtime", "claude")
	if r.code != 0 || r.stdout != "" {
		t.Fatal("fail-open failed")
	}
	if err := os.WriteFile(pointer, []byte(f.socket+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	assertStationFootprint(t, f.stations["alice"])
	// Planted-write control: the same oracle must refuse one extra product file.
	if err := os.WriteFile(filepath.Join(f.stations["alice"], "mailbox"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if stationFootprint(f.stations["alice"]) == nil {
		t.Fatal("footprint oracle missed planted write")
	}
	if err := os.Remove(filepath.Join(f.stations["alice"], "mailbox")); err != nil {
		t.Fatal(err)
	}
	assertStationFootprint(t, f.stations["alice"])
}
func stationFootprint(home string) error {
	expected := map[string]bool{".": true, ".a2a": true, ".a2a/courier.sock": true, ".a2a/identity": true, ".a2a/station": true, ".a2a/station.key": true, "state": true, "state/" + hookFailureLog: true}
	err := filepath.WalkDir(home, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		if !expected[rel] {
			return courier.NotAllowed
		}
		delete(expected, rel)
		return nil
	})
	if err != nil {
		return err
	}
	if len(expected) != 0 {
		return courier.NotAllowed
	}
	return nil
}
func assertStationFootprint(t *testing.T, home string) {
	t.Helper()
	if stationFootprint(home) != nil {
		t.Fatal("station wrote outside admitted footprint")
	}
}

// Proves canceling a real wait returns 130 and releases both the consumer and
// state/lease lock within five seconds; the adjacent wait starts and completes.
func TestCourierWaitDisconnectLockRelease(t *testing.T) {
	f := newCourierFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out, diag bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- f.app("alice", "", &out, &diag).Run(ctx, []string{"wait", "--timeout", "10m"}) }()
	deadline := time.NewTimer(courier.LockReleaseBound)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for f.envs["alice"].hs.SyncCount("@alice:"+testServer) == 0 {
		select {
		case code := <-done:
			t.Fatal("wait ended early", code)
		case <-f.done:
			t.Fatal("daemon stopped")
		case <-deadline.C:
			t.Fatal("wait never polled")
		case <-tick.C:
		}
	}
	if release, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", time.Now().Add(time.Millisecond)); err == nil {
		release()
		t.Fatal("wait did not hold consumer lock")
	}
	start := time.Now()
	cancel()
	select {
	case code := <-done:
		if code != 130 {
			t.Fatal("cancel exit", code)
		}
	case <-f.done:
		t.Fatal("daemon stopped")
	case <-time.After(courier.LockReleaseBound):
		t.Fatal("client did not cancel")
	}
	for {
		release, err := config.AcquireRawConsumerLock(f.paths["alice"], "alice", time.Now().Add(time.Millisecond))
		if err == nil {
			release()
			break
		}
		if time.Since(start) > courier.LockReleaseBound {
			t.Fatal("consumer lock retained")
		}
		select {
		case <-f.done:
			t.Fatal("daemon stopped")
		case <-tick.C:
		}
	}
	// This takes the actual state/lease path, proving it remains usable too.
	f.run("alice", "", "wait", "--timeout", "1ms").ok(t)
	if time.Since(start) > courier.LockReleaseBound {
		t.Fatal("state/lease lock release exceeded bound")
	}
}

// Proves native SIGINT closes a station connection and returns 130, and native
// SIGTERM cancels in-flight daemon invocations with the unavailable class.
// The subprocess runs the same signal context as the command entry point;
// this is process/socket behavior, not native runtime idle-wake acceptance.
func TestCourierProcessSignals(t *testing.T) {
	if mode := os.Getenv("C5_SIGNAL_CHILD"); mode != "" {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if mode == "station" {
			os.Exit(NewApp().Run(ctx, []string{"wait", "--timeout", "10m"}))
		}
		err := courier.ServeWithHandler(ctx, os.Getenv("C5_DAEMON_HOME"), os.Getenv("C5_SOCKET"), os.Stderr, func(ctx context.Context, inv courier.Invocation) int {
			_, _ = io.WriteString(os.Stdout, "entered\n")
			<-ctx.Done()
			return 1
		})
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	t.Run("station-SIGINT", func(t *testing.T) {
		entered := make(chan struct{})
		released := make(chan struct{})
		s := newC5Station(t, func(ctx context.Context, inv courier.Invocation) int {
			close(entered)
			<-ctx.Done()
			close(released)
			return 1
		})
		cmd := exec.Command(os.Args[0], "-test.run=^TestCourierProcessSignals$")
		cmd.Env = append(os.Environ(), "C5_SIGNAL_CHILD=station", config.EnvConfigDir+"="+s.home)
		var out, diag bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &diag
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			if err := cmd.Process.Signal(os.Interrupt); err == nil {
				select {
				case <-done:
				case <-time.After(courier.LockReleaseBound):
					t.Error("child did not stop")
				}
			}
		})
		select {
		case <-entered:
		case err := <-done:
			t.Fatal("child ended early", err)
		case <-s.done:
			t.Fatal("daemon ended")
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("child never invoked")
		}
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 130 {
				t.Fatal("SIGINT exit differs", err)
			}
		case <-s.done:
			t.Fatal("daemon ended")
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("SIGINT stuck")
		}
		select {
		case <-released:
		case <-s.done:
			t.Fatal("daemon ended")
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("SIGINT did not cancel engine")
		}
		if out.Len() != 0 || diag.Len() != 0 {
			t.Fatal("cancellation printed request diagnostics")
		}
	})
	t.Run("daemon-SIGTERM", func(t *testing.T) {
		home := filepath.Join(canonicalRoot(t), "home")
		socket := filepath.Join(shortCourierBase(t), "s", "c.sock")
		key, err := courier.Register(home, "station", "identity", uint32(os.Geteuid()), false)
		if err != nil {
			t.Fatal(err)
		}
		defer clear(key)
		cmd := exec.Command(os.Args[0], "-test.run=^TestCourierProcessSignals$")
		cmd.Env = append(os.Environ(), "C5_SIGNAL_CHILD=daemon", "C5_DAEMON_HOME="+home, "C5_SOCKET="+socket)
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		logRead, logWrite, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		defer logRead.Close()
		defer logWrite.Close()
		cmd.Stderr = logWrite
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		t.Cleanup(func() {
			if err := cmd.Process.Signal(syscall.SIGTERM); err == nil {
				select {
				case <-done:
				case <-time.After(courier.LockReleaseBound):
					t.Error("child did not stop")
				}
			}
		})
		timer := time.NewTimer(courier.LockReleaseBound)
		defer timer.Stop()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		for {
			if c, err := net.DialTimeout("unix", socket, 10*time.Millisecond); err == nil {
				c.Close()
				break
			}
			select {
			case err := <-done:
				t.Fatal("child ended", err)
			case <-timer.C:
				t.Fatal("child not ready")
			case <-tick.C:
			}
		}
		clientDone := make(chan error, 1)
		go func() {
			_, err := courier.Client(context.Background(), socket, "station", "identity", key, courier.Request{Argv: []string{"wait"}}, io.Discard, io.Discard)
			clientDone <- err
		}()
		entered := make(chan bool, 1)
		go func() {
			line, err := bufio.NewReader(stdout).ReadString('\n')
			entered <- err == nil && line == "entered\n"
		}()
		select {
		case ok := <-entered:
			if !ok {
				t.Fatal("handler did not enter")
			}
		case err := <-clientDone:
			t.Fatal("client ended", err)
		case err := <-done:
			t.Fatal("daemon ended", err)
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("no invocation")
		}
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-clientDone:
			if err != courier.Unavailable {
				t.Fatal("shutdown class", err)
			}
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("shutdown retained client")
		}
		select {
		case err := <-done:
			if err != nil {
				t.Fatal("daemon exit", err)
			}
		case <-time.After(courier.LockReleaseBound):
			t.Fatal("SIGTERM stuck")
		}
	})
}

// Proves station failure accounting refuses unsafe modes/links and keeps the
// valid existing log byte-for-byte. Restoring each valid neighbor permits the
// next redacted append; no alternate local file is adopted.
func TestCourierHookFailureStateCustody(t *testing.T) {
	for _, mode := range []string{"file-mode", "file-link", "hardlink", "state-link", "base-link"} {
		t.Run(mode, func(t *testing.T) {
			home := canonicalRoot(t)
			var diag bytes.Buffer
			if err := updateHookFailures(home, "Stop", courier.Unavailable, &diag); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(home, "state", hookFailureLog)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			restore := func() {}
			switch mode {
			case "file-mode":
				if err := os.Chmod(path, 0644); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := os.Chmod(path, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "file-link":
				saved := path + "-saved"
				if err := os.Rename(path, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(saved, path); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(saved, path); err != nil {
						t.Fatal(err)
					}
				}
			case "hardlink":
				link := filepath.Join(home, "link")
				if err := os.Link(path, link); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := os.Remove(link); err != nil {
						t.Fatal(err)
					}
				}
			case "state-link":
				state := filepath.Join(home, "state")
				saved := filepath.Join(home, "saved")
				if err := os.Rename(state, saved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(saved, state); err != nil {
					t.Fatal(err)
				}
				restore = func() {
					if err := os.Remove(state); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(saved, state); err != nil {
						t.Fatal(err)
					}
				}
			case "base-link":
				link := filepath.Join(canonicalRoot(t), "alias")
				if err := os.Symlink(home, link); err != nil {
					t.Fatal(err)
				}
				realHome := home
				home = link
				restore = func() { home = realHome }
			}
			if updateHookFailures(home, "Stop", courier.Unavailable, &diag) != courier.Unavailable {
				t.Fatal("unsafe log adopted")
			}
			restore()
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, original) {
				t.Fatal("refusal modified existing log")
			}
			if err := updateHookFailures(home, "Stop", courier.Unavailable, &diag); err != nil {
				t.Fatal("valid neighbor refused", err)
			}
		})
	}
}
