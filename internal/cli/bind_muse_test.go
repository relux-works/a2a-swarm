//go:build linux || darwin

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// fakeMuseSender scripts the session-message seam of the muse carrier. The
// real dispatch, qualification, reservation and finish paths run unchanged.
type fakeMuseSender struct {
	live            bool
	liveErr         error
	sendErr         error
	targets, bodies []string
}

func (f *fakeMuseSender) SessionLive(_ context.Context, sessionID string) (bool, error) {
	if f.liveErr != nil {
		return false, f.liveErr
	}
	return f.live && sessionID == "muse-session-1", nil
}

func (f *fakeMuseSender) SendSessionMessage(_ context.Context, sessionID, body string) error {
	f.targets = append(f.targets, sessionID)
	f.bodies = append(f.bodies, body)
	return f.sendErr
}

func museDispatchWorld(t *testing.T, fake *fakeMuseSender, maxWakes int) (*env, *App, *session, *time.Time, string) {
	t.Helper()
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	runtimeMail(e, room, "pending")
	now := time.Now()
	e.now = func() time.Time { return now }
	a, s := muteApp(e)
	a.museSender = fake
	birth, err := tmuxwake.Birth(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	target := tmuxwake.Target{Runtime: "muse", OwnerPID: os.Getpid(), OwnerBirth: birth}
	b := config.TerminalBinding{Generation: "muse-gen", SessionID: "muse-session-1", Host: machineLabel(), OwnerPID: os.Getpid(), OwnerBirth: birth, DaemonPID: os.Getpid(), DaemonBirth: birth, Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: maxWakes}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &b
		st.Lease = &config.Lease{Holder: b.SessionID, Host: b.Host, PID: os.Getpid(), Expires: now.Add(time.Hour)}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture.Enroll(t, wakemute.Scope{{Root: e.dir, Connections: []wakemute.Connection{{Path: "."}}}})
	return e, a, s, &now, room
}

func museDispatch(t *testing.T, a *App, s *session) {
	t.Helper()
	if err := a.dispatchTerminal(context.Background(), s, "muse-gen"); err != nil {
		t.Fatal(err)
	}
}

// qualify runs the two-sample idle qualification: one observing cycle, then a
// later cycle at least one interval on.
func museQualify(t *testing.T, a *App, s *session, now *time.Time) {
	t.Helper()
	museDispatch(t, a, s)
	*now = now.Add(2 * time.Second)
}

func TestMuseDispatchDeliversAndSuppressesDuplicate(t *testing.T) {
	fake := &fakeMuseSender{live: true}
	e, a, s, now, _ := museDispatchWorld(t, fake, 30)
	museQualify(t, a, s, now)
	if len(fake.bodies) != 0 {
		t.Fatalf("single idle sample delivered: %v", fake.bodies)
	}
	museDispatch(t, a, s)
	if len(fake.bodies) != 1 || fake.bodies[0] != tmuxwake.Notice(1) || fake.targets[0] != "muse-session-1" {
		t.Fatalf("delivery: targets=%v bodies=%v", fake.targets, fake.bodies)
	}
	if got := e.state("alice").Terminal.Outcome; got != "wake submitted" {
		t.Fatalf("outcome = %q", got)
	}
	if len(e.mailbox("alice").Pending()) != 1 {
		t.Fatal("wake receipted mail; only inbox may")
	}
	// The rung event is never rung again.
	*now = now.Add(2 * time.Second)
	museDispatch(t, a, s)
	if len(fake.bodies) != 1 {
		t.Fatalf("duplicate wake delivered: %v", fake.bodies)
	}
}

func TestMuseDispatchRateCapHold(t *testing.T) {
	fake := &fakeMuseSender{live: true}
	e, a, s, now, room := museDispatchWorld(t, fake, 1)
	museQualify(t, a, s, now)
	museDispatch(t, a, s)
	if len(fake.bodies) != 1 {
		t.Fatalf("first delivery missing: %v", fake.bodies)
	}
	runtimeMail(e, room, "second")
	museQualify(t, a, s, now)
	museDispatch(t, a, s)
	if len(fake.bodies) != 1 {
		t.Fatalf("rate cap did not hold: %v", fake.bodies)
	}
	if got := e.state("alice").Terminal.Outcome; got != "suppressed: hourly rate" {
		t.Fatalf("outcome = %q", got)
	}
}

func TestMuseDispatchSenderUnverified(t *testing.T) {
	fake := &fakeMuseSender{live: true, sendErr: tmuxwake.ErrMuseSenderUnverified}
	e, a, s, now, _ := museDispatchWorld(t, fake, 30)
	museQualify(t, a, s, now)
	museDispatch(t, a, s)
	b := e.state("alice").Terminal
	if b.SendRefusal == nil || b.SendRefusal.Reason != tmuxwake.ReasonSenderUnverified || b.SendRefusal.At.IsZero() {
		t.Fatalf("send refusal = %+v", b.SendRefusal)
	}
	if b.Outcome != string(tmuxwake.SendMuseRefused) {
		t.Fatalf("outcome = %q", b.Outcome)
	}
	if len(e.mailbox("alice").Pending()) != 1 || len(b.Rung) != 0 {
		t.Fatalf("refused wake consumed mail: pending=%d rung=%v", len(e.mailbox("alice").Pending()), b.Rung)
	}
}

func TestMuseDispatchDeadSession(t *testing.T) {
	fake := &fakeMuseSender{live: false}
	e, a, s, _, _ := museDispatchWorld(t, fake, 30)
	museDispatch(t, a, s)
	b := e.state("alice").Terminal
	if len(fake.bodies) != 0 {
		t.Fatalf("dead session received a wake: %v", fake.bodies)
	}
	if b.Outcome != "suppressed: muse session dead" {
		t.Fatalf("outcome = %q", b.Outcome)
	}
	if b.SendRefusal == nil || b.SendRefusal.Reason != tmuxwake.ReasonSessionDead || b.SendRefusal.At.IsZero() {
		t.Fatalf("send refusal = %+v", b.SendRefusal)
	}
	if b.Readiness == nil || b.Readiness.Reason != tmuxwake.RuntimeSessionDead {
		t.Fatalf("readiness = %+v", b.Readiness)
	}
}

func TestTerminalSessionIDMuse(t *testing.T) {
	for _, tc := range []struct {
		env     map[string]string
		runtime string
		want    string
	}{
		{map[string]string{EnvSessionID: "a2a-1", "MUSE_SESSION_ID": "muse-1"}, "muse", "a2a-1"},
		{map[string]string{"MUSE_SESSION_ID": "muse-1", "CODEX_THREAD_ID": "codex-1"}, "muse", "muse-1"},
		{map[string]string{"MUSE_THREAD_ID": "muse-2"}, "muse", "muse-2"},
		{map[string]string{"MUSE_SESSION_ID": "muse-1", "MUSE_THREAD_ID": "muse-2"}, "muse", "muse-1"},
		{map[string]string{"CODEX_THREAD_ID": "codex-1"}, "codex", "codex-1"},
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "claude-1"}, "claude", "claude-1"},
		{map[string]string{"MUSE_SESSION_ID": "muse-1"}, "claude", ""},
		{map[string]string{"CLAUDE_CODE_SESSION_ID": "claude-1"}, "muse", ""},
	} {
		a := &App{Getenv: func(k string) string { return tc.env[k] }}
		if got := a.terminalSessionID(tc.runtime); got != tc.want {
			t.Fatalf("terminalSessionID(%q) with %v = %q, want %q", tc.runtime, tc.env, got, tc.want)
		}
	}
}

// museBindSetup builds one manual muse bind world. HooksPaused stops an
// authorized bind at the manual gate, right after the authority stage, so
// no detached consumer is armed in-process.
func museBindSetup(t *testing.T, env map[string]string) (*env, *App) {
	t.Helper()
	e, _ := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	for k, v := range env {
		e.extra[k] = v
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.HooksPaused = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	a, _ := muteApp(e)
	a.museSender = &fakeMuseSender{live: true}
	return e, a
}

func museBindRun(t *testing.T, e *env, a *App, owner int) error {
	t.Helper()
	return museBindRunArgs(t, e, a, owner, "--session-id", "muse-session-1")
}

// museBindRunArgs runs one manual muse bind with the given session flags;
// none means the session id resolves from the environment, the documented
// form (docs/cli.md, onboard guidance).
func museBindRunArgs(t *testing.T, e *env, a *App, owner int, flags ...string) error {
	t.Helper()
	err := a.bindTerminal(context.Background(), append([]string{"--runtime", "muse", "--owner-pid", strconv.Itoa(owner)}, flags...), bindingManual)
	if st := e.state("alice"); st.Terminal != nil {
		t.Fatalf("bind persisted a terminal past the gate: %+v", st.Terminal)
	}
	return err
}

// museBindAttempt runs one manual muse bind against the authority seams.
// The seams assert their pid arguments instead of ignoring them: ancestry
// must be asked as (this invocation's parent, the --owner-pid owner) and
// the image lookup must be asked for the owner, so a production call that
// asks about the wrong process (for example the owner against itself)
// fails here. The returned counts pin whether each guard ran at all.
func museBindAttempt(t *testing.T, env map[string]string, ancestorOK bool, ownerName string, flagless bool) (err error, ancestorCalls, nameCalls int) {
	t.Helper()
	e, a := museBindSetup(t, env)
	owner := os.Getpid()
	a.museBindAncestor = func(_ context.Context, pid, ancestor int) bool {
		ancestorCalls++
		if pid != os.Getppid() || ancestor != owner {
			t.Errorf("museBindAncestor(pid=%d, ancestor=%d), want (ppid=%d, owner=%d)", pid, ancestor, os.Getppid(), owner)
		}
		return ancestorOK
	}
	a.museOwnerName = func(_ context.Context, pid int) (string, error) {
		nameCalls++
		if pid != owner {
			t.Errorf("museOwnerName(pid=%d), want owner=%d", pid, owner)
		}
		return ownerName, nil
	}
	if flagless {
		err = museBindRunArgs(t, e, a, owner)
	} else {
		err = museBindRun(t, e, a, owner)
	}
	return err, ancestorCalls, nameCalls
}

// A manual muse bind binds only its own session: Muse's own session
// selector (MUSE_SESSION_ID or MUSE_THREAD_ID) naming the bound session,
// invocation ancestry from the owner, and a muse/muse-bin* owner image
// are each required; A2A_SESSION_ID may agree but never stands alone.
func TestMuseBindAuthority(t *testing.T) {
	own := map[string]string{"MUSE_SESSION_ID": "muse-session-1"}
	for name, tc := range map[string]struct {
		env       map[string]string
		ancestor  bool
		ownerName string
		want      string
		// authority marks rows that reach the ancestry guard; the image
		// guard runs only after ancestry passes. Selector refusals must
		// reach neither guard.
		authority bool
		// flagless rows pass no --session-id: the id resolves from the
		// environment, as in the documented command.
		flagless bool
	}{
		"own session":            {own, true, "muse-bin-1.4.24", "manual gate", true, false},
		"own session via thread": {map[string]string{"MUSE_THREAD_ID": "muse-session-1"}, true, "/usr/local/bin/muse", "manual gate", true, false},
		"own session via a2a":    {map[string]string{EnvSessionID: "muse-session-1", "MUSE_SESSION_ID": "muse-session-1"}, true, "muse-bin", "manual gate", true, false},
		// A live session named only by A2A_SESSION_ID (stale, exported
		// by another lane, or another session's id) must not bind: the
		// selected session is live in the fake registry and the owner
		// passes every process guard, so only the missing Muse selector
		// refuses. Before the fix this row reached the manual gate.
		"live a2a-only selector": {map[string]string{EnvSessionID: "muse-session-1"}, true, "muse-bin", "requires this Muse session", false, false},
		"a2a over foreign muse":  {map[string]string{EnvSessionID: "muse-session-1", "MUSE_SESSION_ID": "muse-session-9"}, true, "muse-bin", "another live Muse session", false, false},
		// The same selector rules without --session-id (B1 in its
		// natural form), and with the older MUSE_THREAD_ID spelling.
		"flagless own session":     {own, true, "muse-bin-1.4.24", "manual gate", true, true},
		"flagless live a2a-only":   {map[string]string{EnvSessionID: "muse-session-1"}, true, "muse-bin", "requires this Muse session", false, true},
		"flagless a2a with thread": {map[string]string{EnvSessionID: "muse-session-1", "MUSE_THREAD_ID": "muse-session-1"}, true, "muse-bin", "manual gate", true, true},
		"flagless a2a over thread": {map[string]string{EnvSessionID: "muse-session-1", "MUSE_THREAD_ID": "muse-session-9"}, true, "muse-bin", "another live Muse session", false, true},
		// Disagreeing native spellings refuse instead of resolving to
		// MUSE_SESSION_ID, with or without the flag.
		"disagreeing native selectors": {map[string]string{"MUSE_SESSION_ID": "muse-session-1", "MUSE_THREAD_ID": "muse-session-9"}, true, "muse-bin", "disagreeing Muse session selectors", false, false},
		"flagless disagreeing native":  {map[string]string{"MUSE_SESSION_ID": "muse-session-1", "MUSE_THREAD_ID": "muse-session-9"}, true, "muse-bin", "disagreeing Muse session selectors", false, true},
		"unrelated owner":              {own, false, "muse-bin-1.4.24", "not this invocation's muse runtime", true, false},
		"owner is not muse":            {own, true, "/bin/sleep", "not a muse process", true, false},
		"missing Muse session env":     {map[string]string{}, true, "muse-bin-1.4.24", "requires this Muse session", false, false},
		"claude env only":              {map[string]string{"CLAUDE_CODE_SESSION_ID": "claude-1"}, true, "muse-bin-1.4.24", "requires this Muse session", false, false},
		"foreign session id":           {map[string]string{"MUSE_SESSION_ID": "muse-session-9"}, true, "muse-bin-1.4.24", "another live", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			err, ancestorCalls, nameCalls := museBindAttempt(t, tc.env, tc.ancestor, tc.ownerName, tc.flagless)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("bind err = %v, want it to contain %q", err, tc.want)
			}
			wantAncestor, wantName := 0, 0
			if tc.authority {
				wantAncestor = 1
				if tc.ancestor {
					wantName = 1
				}
			}
			if ancestorCalls != wantAncestor || nameCalls != wantName {
				t.Fatalf("guard calls: ancestry=%d image=%d, want %d/%d", ancestorCalls, nameCalls, wantAncestor, wantName)
			}
		})
	}
	for name, ok := range map[string]bool{
		"muse": true, "/opt/muse/muse": true, "muse-bin": true, "muse-bin-1.4.24": true,
		"sleep": false, "museum": false, "a2a": false, "": false,
	} {
		if got := isMuseOwnerName(name); got != ok {
			t.Fatalf("isMuseOwnerName(%q) = %t, want %t", name, got, ok)
		}
	}
}

// The flip side of the Muse session guard: a claude or codex bind refuses
// while a Muse session selector is in the environment, instead of binding
// some other runtime's session from inside Muse.
func TestBindRefusesOtherRuntimesUnderMuseEnv(t *testing.T) {
	for _, runtime := range []string{"claude", "codex"} {
		t.Run(runtime, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "wait", "--timeout", "1ms").ok(t)
			e.extra["MUSE_SESSION_ID"] = "muse-session-1"
			a, _ := muteApp(e)
			err := a.bindTerminal(context.Background(), []string{"--runtime", runtime, "--owner-pid", strconv.Itoa(os.Getpid()), "--session-id", "some-session-1"}, bindingManual)
			if err == nil || !strings.Contains(err.Error(), "another live Muse session") {
				t.Fatalf("%s bind under a Muse env: err = %v", runtime, err)
			}
		})
	}
}

// stubMuseOnPath installs a `muse` shell double whose sessions registry
// always lists muse-session-1; sends record stdin and reply per mode file.
func stubMuseOnPath(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "sessions" ]; then
  printf '{"sessions":[{"sessionId":"muse-session-1"}]}'
  exit 0
fi
cat > %q
printf 'MUSE_NO_AUTO_UPDATE=%%s\n' "$MUSE_NO_AUTO_UPDATE" > %q
if [ "$(cat %q)" = "ingress" ]; then
  printf '{"error_code":"external_agent_ingress_closed"}'
  exit 1
fi
printf '{"ok":true}'
`, filepath.Join(dir, "stdin.log"), filepath.Join(dir, "env.log"), filepath.Join(dir, "mode"))
	if err := os.WriteFile(filepath.Join(dir, "muse"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(mode), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

// The real ExecMuseSender drives a full dispatch: qualification, the
// session-message send through the stub CLI, and the submitted outcome.
func TestMuseDispatchThroughExecRoute(t *testing.T) {
	dir := stubMuseOnPath(t, "ok")
	e, a, s, now, _ := museDispatchWorld(t, &fakeMuseSender{live: true}, 30)
	a.museSender = tmuxwake.ExecMuseSender{}
	museQualify(t, a, s, now)
	museDispatch(t, a, s)
	if got := e.state("alice").Terminal.Outcome; got != "wake submitted" {
		t.Fatalf("outcome = %q", got)
	}
	body, err := os.ReadFile(filepath.Join(dir, "stdin.log"))
	if err != nil || string(body) != tmuxwake.Notice(1) {
		t.Fatalf("stub stdin = %q (%v), want the fixed notice", body, err)
	}
	envLog, err := os.ReadFile(filepath.Join(dir, "env.log"))
	if err != nil || !strings.Contains(string(envLog), "MUSE_NO_AUTO_UPDATE=1") {
		t.Fatalf("stub env = %q (%v), want the auto-update opt-out", envLog, err)
	}
}

// An ingress refusal from the real route is recorded as the named refusal,
// never as a submitted wake.
func TestMuseDispatchThroughExecRouteIngressRefused(t *testing.T) {
	stubMuseOnPath(t, "ingress")
	e, a, s, now, _ := museDispatchWorld(t, &fakeMuseSender{live: true}, 30)
	a.museSender = tmuxwake.ExecMuseSender{}
	museQualify(t, a, s, now)
	museDispatch(t, a, s)
	b := e.state("alice").Terminal
	if b.SendRefusal == nil || b.SendRefusal.Reason != tmuxwake.ReasonIngressClosed {
		t.Fatalf("send refusal = %+v", b.SendRefusal)
	}
	if b.Outcome != string(tmuxwake.SendMuseRefused) {
		t.Fatalf("outcome = %q", b.Outcome)
	}
}

// The pre-send refusal appends one binding-log line per reason: repeated
// dead observations stay one line, and a later unavailable reason adds one.
func TestMusePreSendRefusalLogOnlyOnReasonChange(t *testing.T) {
	fake := &fakeMuseSender{live: false}
	e, a, s, _, _ := museDispatchWorld(t, fake, 30)
	logPath := filepath.Join(s.paths.Base, "state", "alice.binding.log")
	refusalLines := func() []string {
		t.Helper()
		b, err := os.ReadFile(logPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "send_refusal=") {
				lines = append(lines, line)
			}
		}
		return lines
	}
	museDispatch(t, a, s)
	museDispatch(t, a, s)
	if lines := refusalLines(); len(lines) != 1 || !strings.Contains(lines[0], "send_refusal="+string(tmuxwake.ReasonSessionDead)) {
		t.Fatalf("refusal log after two dead cycles = %q", lines)
	}
	if got := e.state("alice").Terminal.SendRefusal; got == nil || got.Reason != tmuxwake.ReasonSessionDead {
		t.Fatalf("state refusal = %+v", got)
	}
	fake.liveErr = errors.New("registry unavailable")
	museDispatch(t, a, s)
	if lines := refusalLines(); len(lines) != 2 || !strings.Contains(lines[1], "send_refusal="+string(tmuxwake.ReasonSessionUnavailable)) {
		t.Fatalf("refusal log after the reason changed = %q", lines)
	}
}
