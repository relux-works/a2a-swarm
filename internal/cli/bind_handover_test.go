package cli

import (
	"context"
	"encoding/json"
	"net"
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

// Each supported runtime's current session comes from its own documented
// source, under the retained explicit A2A and Codex precedence. A malformed
// higher-priority value never falls through to a lower-priority source.
func TestBindHandoverSessionSources(t *testing.T) {
	for _, tc := range []struct {
		name, runtime string
		env           map[string]string
		want          string
	}{
		{name: "a2a-precedence-claude", runtime: "claude", env: map[string]string{EnvSessionID: "a", "CODEX_THREAD_ID": "c", "CLAUDE_CODE_SESSION_ID": "l", "MUSE_SESSION_ID": "m"}, want: "a"},
		{name: "a2a-precedence-codex", runtime: "codex", env: map[string]string{EnvSessionID: "a", "CODEX_THREAD_ID": "c"}, want: "a"},
		{name: "a2a-precedence-muse", runtime: "muse", env: map[string]string{EnvSessionID: "a", "MUSE_SESSION_ID": "m"}, want: "a"},
		{name: "codex-precedence-over-claude-native", runtime: "claude", env: map[string]string{"CODEX_THREAD_ID": "c", "CLAUDE_CODE_SESSION_ID": "l"}, want: "c"},
		{name: "claude-native", runtime: "claude", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "l"}, want: "l"},
		{name: "codex-native", runtime: "codex", env: map[string]string{"CODEX_THREAD_ID": "c"}, want: "c"},
		{name: "muse-native", runtime: "muse", env: map[string]string{"MUSE_SESSION_ID": "m"}, want: "m"},
		{name: "muse-ignores-claude-native", runtime: "muse", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "l"}},
		{name: "codex-ignores-claude-native", runtime: "codex", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "l"}},
		{name: "claude-ignores-muse-native", runtime: "claude", env: map[string]string{"MUSE_SESSION_ID": "m"}},
		{name: "no-source-claude", runtime: "claude"},
		{name: "no-source-muse", runtime: "muse"},
		{name: "malformed-a2a-no-fallthrough", runtime: "codex", env: map[string]string{EnvSessionID: "../bad", "CODEX_THREAD_ID": "c"}, want: "../bad"},
		{name: "malformed-codex-no-fallthrough", runtime: "claude", env: map[string]string{"CODEX_THREAD_ID": "../bad", "CLAUDE_CODE_SESSION_ID": "l"}, want: "../bad"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &App{Getenv: func(k string) string { return tc.env[k] }}
			if got := a.terminalSessionID(tc.runtime); got != tc.want {
				t.Fatalf("terminalSessionID(%q)=%q want=%q", tc.runtime, got, tc.want)
			}
		})
	}
}

// Session comparison is exact equality of two established ids. Anything
// unestablished is unknown, and a prefix of an id is a different session.
func TestBindHandoverClassifySession(t *testing.T) {
	for _, tc := range []struct {
		recorded, current string
		want              sessionMatch
	}{
		{"own", "own", sessionSame},
		{"own", "other", sessionAnother},
		{"own-session", "own", sessionAnother},
		{"own", "own-session", sessionAnother},
		{"", "own", sessionUnknown},
		{"own", "", sessionUnknown},
		{"", "", sessionUnknown},
		{"../bad", "../bad", sessionUnknown},
		{"own", "../bad", sessionUnknown},
		{"../bad", "own", sessionUnknown},
	} {
		if got := classifySession(tc.recorded, tc.current); got != tc.want {
			t.Fatalf("classifySession(%q, %q)=%d want=%d", tc.recorded, tc.current, got, tc.want)
		}
	}
}

// A recorded process is alive only when its pid exists with the recorded
// start. The matching-start control is alive; the same pid under another
// start (a reused pid), an exited pid and missing evidence are not.
func TestBindHandoverProcessAlive(t *testing.T) {
	ctx := context.Background()
	birth, err := tmuxwake.Birth(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if !bindingProcessAlive(ctx, os.Getpid(), birth) {
		t.Fatal("live process with its recorded start not alive")
	}
	if bindingProcessAlive(ctx, os.Getpid(), "another-start") {
		t.Fatal("reused pid reported alive")
	}
	if bindingProcessAlive(ctx, os.Getpid(), "") || bindingProcessAlive(ctx, 0, birth) {
		t.Fatal("missing evidence reported alive")
	}
	exited := exec.Command("sleep", "0.1")
	if err := exited.Start(); err != nil {
		t.Fatal(err)
	}
	exitedBirth, err := tmuxwake.Birth(ctx, exited.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := exited.Wait(); err != nil {
		t.Fatal(err)
	}
	if bindingProcessAlive(ctx, exited.Process.Pid, exitedBirth) {
		t.Fatal("exited process reported alive")
	}
}

// Unbind recognises its own session from every runtime's native source.
// A session that cannot be established is refused as unknown, never as
// another session, and a prefix of the recorded session is another session.
// Valid own-session controls retire exactly the recorded generation.
func TestBindHandoverUnbindRecognition(t *testing.T) {
	for _, tc := range []struct {
		name, runtime, recorded string
		env                     map[string]string
		want                    string // "" means success
	}{
		{name: "codex-own", runtime: "codex", recorded: "own", env: map[string]string{"CODEX_THREAD_ID": "own"}},
		{name: "claude-own", runtime: "claude", recorded: "own", env: map[string]string{"CLAUDE_CODE_SESSION_ID": "own"}},
		{name: "muse-own", runtime: "muse", recorded: "own", env: map[string]string{"MUSE_SESSION_ID": "own"}},
		{name: "unknown", runtime: "codex", recorded: "own", want: "current session unknown"},
		{name: "prefix", runtime: "codex", recorded: "own-session", env: map[string]string{"CODEX_THREAD_ID": "own"}, want: "another session's binding"},
		{name: "another", runtime: "codex", recorded: "own", env: map[string]string{"CODEX_THREAD_ID": "other"}, want: "another session's binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			daemonPID := 0
			if tc.want != "" {
				daemonPID = os.Getpid()
			}
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				st.Terminal = &config.TerminalBinding{Generation: "handover-gen", SessionID: tc.recorded, Host: machineLabel(), DaemonPID: daemonPID, DaemonBirth: "recorded-birth", Target: tmuxwake.Target{Runtime: tc.runtime}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.env {
				e.extra[k] = v
			}
			if tc.want == "" {
				e.as("alice", "unbind").ok(t)
			} else {
				e.as("alice", "unbind").failsWith(t, ExitError, tc.want)
			}
			b := e.state("alice").Terminal
			if tc.want == "" && (!b.Stopped || b.Outcome != "unbound") {
				t.Fatalf("own binding not retired: %+v", b)
			}
			if tc.want != "" && b.Stopped {
				t.Fatalf("refused unbind retired the binding: %+v", b)
			}
		})
	}
}

// Status names the recorded owner (pid, start, session, host) and consumer
// (pid, start) and whether each is alive now: alive requires the pid to
// exist with the recorded start, so a reused pid reads not alive beside
// the matching-start control.
func TestBindHandoverStatusProcesses(t *testing.T) {
	target, _, _ := usabilityTarget(t, "codex")
	e := newEnv(t)
	e.agent("alice")
	binding := &config.TerminalBinding{Generation: "status-gen", SessionID: "owned", Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, DaemonPID: os.Getpid(), DaemonBirth: target.OwnerBirth, Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 6}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = binding
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	type process struct {
		PID       int    `json:"pid"`
		Start     string `json:"start"`
		SessionID string `json:"session_id"`
		Host      string `json:"host"`
		Alive     bool   `json:"alive"`
	}
	status := func() (string, struct {
		Owner    *process `json:"owner"`
		Consumer *process `json:"consumer"`
	}) {
		text := e.as("alice", "bind", "status").ok(t).stdout
		raw := e.as("alice", "bind", "status", "--json").ok(t).stdout
		var decoded struct {
			Owner    *process `json:"owner"`
			Consumer *process `json:"consumer"`
		}
		if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
			t.Fatal(err)
		}
		return text, decoded
	}
	text, decoded := status()
	wantOwner := "owner pid=" + strconv.Itoa(os.Getpid()) + " start=\"" + target.OwnerBirth + "\" session=owned host=" + machineLabel() + " alive=true"
	wantConsumer := "consumer pid=" + strconv.Itoa(os.Getpid()) + " start=\"" + target.OwnerBirth + "\" alive=true"
	if !strings.Contains(text, wantOwner) || !strings.Contains(text, wantConsumer) {
		t.Fatalf("status missing live owner/consumer:\nwant %s\nwant %s\ngot:\n%s", wantOwner, wantConsumer, text)
	}
	if decoded.Owner == nil || decoded.Consumer == nil || !decoded.Owner.Alive || !decoded.Consumer.Alive ||
		decoded.Owner.PID != os.Getpid() || decoded.Owner.Start != target.OwnerBirth || decoded.Owner.SessionID != "owned" || decoded.Owner.Host != machineLabel() ||
		decoded.Consumer.PID != os.Getpid() || decoded.Consumer.Start != target.OwnerBirth {
		t.Fatalf("status JSON owner/consumer wrong: %+v %+v", decoded.Owner, decoded.Consumer)
	}
	// The same live pid with a different recorded start is a reused pid:
	// it is not the recorded owner or consumer any more.
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal.OwnerBirth = "reused-owner-start"
		st.Terminal.DaemonBirth = "reused-consumer-start"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	text, decoded = status()
	if !strings.Contains(text, "alive=false") || decoded.Owner == nil || decoded.Consumer == nil || decoded.Owner.Alive || decoded.Consumer.Alive {
		t.Fatalf("reused pid reported alive:\n%s\n%+v %+v", text, decoded.Owner, decoded.Consumer)
	}
	if decoded.Owner.Start != "reused-owner-start" || decoded.Consumer.Start != "reused-consumer-start" {
		t.Fatalf("recorded starts not shown: %+v %+v", decoded.Owner, decoded.Consumer)
	}
}

// handoverTarget pins a synthetic target whose pane and owner are this
// test binary's parent, so bind's ancestry checks pass for owner=parent
// without a tmux server. The fake tmux answers only the pinned formats.
func handoverTarget(t *testing.T, runtime string) tmuxwake.Target {
	t.Helper()
	dir := t.TempDir()
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	id, err := tmuxwake.SocketID(socket)
	if err != nil {
		t.Fatal(err)
	}
	ppid := os.Getppid()
	birth, err := tmuxwake.Birth(context.Background(), ppid)
	if err != nil {
		t.Fatal(err)
	}
	screen := filepath.Join(dir, "screen")
	if err := os.WriteFile(screen, []byte("synthetic screen\n"), 0600); err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	script := strings.NewReplacer("__PPID__", strconv.Itoa(ppid), "__SCREEN__", quote(screen)).Replace(`#!/bin/sh
case " $* " in
*' capture-pane '*) cat __SCREEN__;;
*' display-message '*)
  case " $* " in
  *'cursor_x'*) printf '%s\n' '%0 __PPID__ 0 2 0 120';;
  *) printf '%s\n' '%0 __PPID__ 0';;
  esac;;
*) exit 1;;
esac
`)
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return tmuxwake.Target{Socket: socket, SocketID: id, Pane: "%0", PanePID: ppid, PaneBirth: birth, OwnerPID: ppid, OwnerBirth: birth, Runtime: runtime}
}

// handoverLiveBinding installs one live recorded binding (owner is the
// test parent, consumer is an owned sleep) and returns it with its env.
func handoverLiveBinding(t *testing.T, runtime, session string) (*env, config.TerminalBinding) {
	e, binding, _ := handoverLiveBindingConsumer(t, runtime, session)
	return e, binding
}

// handoverLiveBindingConsumer is handoverLiveBinding and also returns the
// owned live consumer process, so a test can stop exactly that consumer.
func handoverLiveBindingConsumer(t *testing.T, runtime, session string) (*env, config.TerminalBinding, *exec.Cmd) {
	t.Helper()
	isolateBindTest(t)
	target := handoverTarget(t, runtime)
	e := newEnv(t)
	e.agent("alice")
	e.extra["TMUX"] = target.Socket + ",1,0"
	e.extra["TMUX_PANE"] = target.Pane
	e.extra["CLAUDE_CONFIG_DIR"] = t.TempDir()
	consumer := exec.Command("sleep", "120")
	if err := consumer.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		consumer.Process.Kill()
		consumer.Wait()
	})
	consumerBirth, err := tmuxwake.Birth(context.Background(), consumer.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	binding := config.TerminalBinding{Generation: "handover-gen", SessionID: session, Host: machineLabel(), OwnerPID: target.OwnerPID, OwnerBirth: target.OwnerBirth, DaemonPID: consumer.Process.Pid, DaemonBirth: consumerBirth, Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 6, Outcome: "armed: no new mail"}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		installed := binding
		st.Terminal = &installed
		st.Lease = &config.Lease{Holder: session, Host: machineLabel(), Expires: time.Now().Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, binding, consumer
}

// handoverDeadProcess returns a pid and its recorded start for a process
// that has already exited and been reaped.
func handoverDeadProcess(t *testing.T) (int, string) {
	t.Helper()
	p := exec.Command("sleep", "0.2")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	birth, err := tmuxwake.Birth(context.Background(), p.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	return p.Process.Pid, birth
}

// handoverLiveProcess starts one owned live process for the duration of
// the test (a test may stop it earlier; cleanup then is a no-op).
func handoverLiveProcess(t *testing.T) *exec.Cmd {
	t.Helper()
	p := exec.Command("sleep", "120")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		p.Process.Kill()
		p.Wait()
	})
	return p
}

// handoverGoneBinding installs a recorded binding whose owner and
// consumer have both exited, with the given lease. The invocation target
// (this test parent's pane) is admissible for a new session, so in a
// fixture built on it only lease evidence can refuse take-over. It
// returns the invocation owner pid (the new session's live owner).
func handoverGoneBinding(t *testing.T, session string, lease *config.Lease) (*env, config.TerminalBinding, int) {
	t.Helper()
	isolateBindTest(t)
	target := handoverTarget(t, "codex")
	e := newEnv(t)
	e.agent("alice")
	e.extra["TMUX"] = target.Socket + ",1,0"
	e.extra["TMUX_PANE"] = target.Pane
	e.extra["CLAUDE_CONFIG_DIR"] = t.TempDir()
	ownerPID, ownerBirth := handoverDeadProcess(t)
	consumerPID, consumerBirth := handoverDeadProcess(t)
	oldTarget := target
	oldTarget.OwnerPID, oldTarget.OwnerBirth = ownerPID, ownerBirth
	binding := config.TerminalBinding{Generation: "handover-gen", SessionID: session, Host: machineLabel(), OwnerPID: ownerPID, OwnerBirth: ownerBirth, DaemonPID: consumerPID, DaemonBirth: consumerBirth, Target: oldTarget, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 6, Outcome: "armed: no new mail"}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		installed := binding
		st.Terminal = &installed
		st.Lease = lease
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return e, binding, target.OwnerPID
}

func handoverStateAndLog(t *testing.T, e *env) (state, log []byte) {
	t.Helper()
	state, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	log, err = os.ReadFile(filepath.Join(e.dir, "state", "alice.binding.log"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return state, log
}

func handoverBindArgs(runtime string, ownerPID int, session string, extra ...string) []string {
	args := []string{"bind", "--runtime", runtime, "--owner-pid", strconv.Itoa(ownerPID), "--session-id", session, "--interval", "1s", "--retry-max", "2s", "--max-wakes-per-hour", "6"}
	return append(args, extra...)
}

func handoverSessionEnv(e *env, runtime, session string) {
	switch runtime {
	case "codex":
		e.extra["CODEX_THREAD_ID"] = session
	case "claude":
		e.extra["CLAUDE_CODE_SESSION_ID"] = session
	}
}

// A second bind from the same live session reports the recorded binding
// as already bound and exits 0 without restarting anything: the recorded
// generation and consumer pid are untouched. Codex is the field case;
// Claude behaves the same.
func TestBindHandoverAlreadyBound(t *testing.T) {
	for _, runtime := range []string{"codex", "claude"} {
		t.Run(runtime, func(t *testing.T) {
			e, binding := handoverLiveBinding(t, runtime, "own-session")
			handoverSessionEnv(e, runtime, "own-session")
			got := e.as("alice", handoverBindArgs(runtime, binding.OwnerPID, "own-session")...).ok(t)
			want := "alice already bound (generation handover-gen, readiness unknown)\n"
			if got.stdout != want {
				t.Fatalf("already bound output=%q want=%q", got.stdout, want)
			}
			after := e.state("alice").Terminal
			if after.Generation != binding.Generation || after.DaemonPID != binding.DaemonPID || after.Stopped {
				t.Fatalf("already bound restarted the binding: %+v", after)
			}
		})
	}
}

// The same live binding refuses a different live session as it does
// today, beside the valid same-session control on the same fixture.
// "Already bound" is never returned for another session.
func TestBindHandoverAnotherLiveSessionRefused(t *testing.T) {
	e, binding := handoverLiveBinding(t, "codex", "own-session")
	handoverSessionEnv(e, "codex", "own-session")
	e.as("alice", handoverBindArgs("codex", binding.OwnerPID, "own-session")...).ok(t)
	handoverSessionEnv(e, "codex", "other-session")
	e.as("alice", handoverBindArgs("codex", binding.OwnerPID, "other-session")...).
		failsWith(t, ExitError, "session already bound; unbind this generation first")
	after := e.state("alice").Terminal
	if after.Generation != binding.Generation || after.SessionID != binding.SessionID || after.DaemonPID != binding.DaemonPID || after.Stopped {
		t.Fatalf("foreign bind changed the binding: %+v", after)
	}
}

// Take-over refuses while the recorded owner is live, names that live
// holder in its fixed message, and changes nothing. The fixture isolates
// the owner gate: after the valid same-session control, the consumer is
// stopped and the lease is expired, so the live owner is the ONLY reason
// left to refuse. Bypassing the owner-gone check therefore cannot pass
// this test behind the consumer wait or the lease.
func TestBindHandoverTakeoverLiveOwnerRefused(t *testing.T) {
	e, binding, consumer := handoverLiveBindingConsumer(t, "codex", "old-session")
	handoverSessionEnv(e, "codex", "old-session")
	e.as("alice", handoverBindArgs("codex", binding.OwnerPID, "old-session")...).ok(t)
	// Isolate the owner gate: consumer gone, lease expired, owner live.
	if err := consumer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := consumer.Wait(); err == nil {
		t.Fatal("killed consumer exited cleanly")
	}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Lease = &config.Lease{Holder: "old-session", Host: machineLabel(), PID: binding.OwnerPID, Expires: time.Now().Add(-time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, beforeLog := handoverStateAndLog(t, e)
	handoverSessionEnv(e, "codex", "new-session")
	e.as("alice", handoverBindArgs("codex", binding.OwnerPID, "new-session", "--take-over")...).
		failsWith(t, ExitError, "take-over refused: binding held by live session old-session (generation handover-gen, owner pid "+strconv.Itoa(binding.OwnerPID)+", consumer pid "+strconv.Itoa(binding.DaemonPID)+")")
	after, afterLog := handoverStateAndLog(t, e)
	if string(before) != string(after) || string(beforeLog) != string(afterLog) {
		t.Fatalf("refused take-over changed state or log:\nbefore %s log %s\nafter %s log %s", before, beforeLog, after, afterLog)
	}
}

// Take-over refuses when the recorded owner is gone but its consumer
// does not stop within the bound, and changes nothing. Owner, lease and
// invocation are otherwise admissible, so the live consumer is the only
// reason to refuse.
func TestBindHandoverTakeoverLiveConsumerRefused(t *testing.T) {
	expired := &config.Lease{Holder: "old-session", Host: machineLabel(), Expires: time.Now().Add(-time.Hour)}
	e, binding, invokeOwner := handoverGoneBinding(t, "old-session", expired)
	// Replace the exited consumer with a live one that never stops.
	live := handoverLiveProcess(t)
	liveBirth, err := tmuxwake.Birth(context.Background(), live.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal.DaemonPID = live.Process.Pid
		st.Terminal.DaemonBirth = liveBirth
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	binding.DaemonPID = live.Process.Pid
	before, beforeLog := handoverStateAndLog(t, e)
	handoverSessionEnv(e, "codex", "new-session")
	e.as("alice", handoverBindArgs("codex", invokeOwner, "new-session", "--take-over")...).
		failsWith(t, ExitError, "take-over refused: binding held by live session old-session (generation handover-gen, owner pid "+strconv.Itoa(binding.OwnerPID)+", consumer pid "+strconv.Itoa(binding.DaemonPID)+")")
	after, afterLog := handoverStateAndLog(t, e)
	if string(before) != string(after) || string(beforeLog) != string(afterLog) {
		t.Fatalf("refused take-over changed state or log:\nbefore %s log %s\nafter %s log %s", before, beforeLog, after, afterLog)
	}
}

// Take-over never releases the previous session's lease on process
// evidence alone. The resumed-thread case: the recorded owner and
// consumer are gone, but the same session still refreshes its unexpired
// lease from a NEW live process (a pid nowhere in the binding). Only
// the lease's expiry can admit take-over here, and it has not passed:
// the fixed holder refusal extended by the lease expiry, with the
// lease, State and log unchanged.
func TestBindHandoverTakeoverLiveLeaseRefused(t *testing.T) {
	resumed := handoverLiveProcess(t)
	lease := &config.Lease{Holder: "old-session", Host: machineLabel(), PID: resumed.Process.Pid, Expires: time.Now().Add(time.Hour)}
	e, binding, invokeOwner := handoverGoneBinding(t, "old-session", lease)
	before, beforeLog := handoverStateAndLog(t, e)
	handoverSessionEnv(e, "codex", "new-session")
	e.as("alice", handoverBindArgs("codex", invokeOwner, "new-session", "--take-over")...).
		failsWith(t, ExitError, "take-over refused: binding held by live session old-session (generation handover-gen, owner pid "+strconv.Itoa(binding.OwnerPID)+", consumer pid "+strconv.Itoa(binding.DaemonPID)+"); lease held until "+lease.Expires.UTC().Format(time.RFC3339)+"; retry after it expires")
	after, afterLog := handoverStateAndLog(t, e)
	if string(before) != string(after) || string(beforeLog) != string(afterLog) {
		t.Fatalf("refused take-over changed state or log:\nbefore %s log %s\nafter %s log %s", before, beforeLog, after, afterLog)
	}
	st := e.state("alice")
	if st.Lease == nil || st.Lease.Holder != "old-session" || st.Lease.PID != resumed.Process.Pid || st.Terminal == nil || st.Terminal.Generation != binding.Generation {
		t.Fatalf("live lease displaced by refused take-over: lease %+v terminal %+v", st.Lease, st.Terminal)
	}
}

// Take-over never releases the previous session's lease on the lease's
// pid either. Lease.PID names only the last process that refreshed the
// lease, and that refresher is usually a one-shot hook or inbox run that
// has already exited: here the recorded owner and consumer are gone and
// the same session's unexpired lease carries such an exited refresher's
// pid. Only expiry can prove that holder gone, so take-over must refuse
// with the fixed holder message extended by the lease expiry, and State
// and binding.log stay byte-identical.
func TestBindHandoverTakeoverDeadRefresherLeaseRefused(t *testing.T) {
	deadPID, _ := handoverDeadProcess(t)
	lease := &config.Lease{Holder: "old-session", Host: machineLabel(), PID: deadPID, Expires: time.Now().Add(time.Hour)}
	e, binding, invokeOwner := handoverGoneBinding(t, "old-session", lease)
	before, beforeLog := handoverStateAndLog(t, e)
	handoverSessionEnv(e, "codex", "new-session")
	e.as("alice", handoverBindArgs("codex", invokeOwner, "new-session", "--take-over")...).
		failsWith(t, ExitError, "take-over refused: binding held by live session old-session (generation handover-gen, owner pid "+strconv.Itoa(binding.OwnerPID)+", consumer pid "+strconv.Itoa(binding.DaemonPID)+"); lease held until "+lease.Expires.UTC().Format(time.RFC3339)+"; retry after it expires")
	after, afterLog := handoverStateAndLog(t, e)
	if string(before) != string(after) || string(beforeLog) != string(afterLog) {
		t.Fatalf("refused take-over changed state or log:\nbefore %s log %s\nafter %s log %s", before, beforeLog, after, afterLog)
	}
	st := e.state("alice")
	if st.Lease == nil || st.Lease.Holder != "old-session" || st.Lease.PID != deadPID || st.Terminal == nil || st.Terminal.Generation != binding.Generation {
		t.Fatalf("lease of a session refreshed by an exited process displaced: lease %+v terminal %+v", st.Lease, st.Terminal)
	}
}

// Only expiry proves the displaced lease gone, on exactly the
// boundary ordinary acquisition uses: at or past Expires the lease is
// admitted, one second before it is not. Beside those controls, an
// unexpired lease is never proven gone whatever pid shape it carries:
// an exited refresher, a live process, this process, or no pid at all,
// on this host or another. Lease.PID is the last refresher, so none of
// those shapes says anything about the holding session.
func TestBindHandoverTakeoverLeaseEvidence(t *testing.T) {
	deadPID, _ := handoverDeadProcess(t)
	resumed := handoverLiveProcess(t)
	now := time.Now().UTC().Truncate(time.Second)
	previous := config.TerminalBinding{Generation: "handover-gen", SessionID: "old-session", OwnerPID: os.Getpid(), DaemonPID: deadPID}
	at := func(expires time.Time, pid int) *config.Lease {
		return &config.Lease{Holder: "old-session", Host: machineLabel(), PID: pid, Expires: expires}
	}
	for _, tc := range []struct {
		name  string
		lease *config.Lease
		want  bool
	}{
		{name: "no-lease", want: true},
		{name: "one-second-past-expiry-dead-refresher", lease: at(now.Add(-time.Second), deadPID), want: true},
		{name: "one-second-past-expiry-live-pid", lease: at(now.Add(-time.Second), resumed.Process.Pid), want: true},
		{name: "exactly-at-expiry-dead-refresher", lease: at(now, deadPID), want: true},
		{name: "one-second-before-expiry-dead-refresher", lease: at(now.Add(time.Second), deadPID), want: false},
		{name: "unexpired-dead-refresher", lease: at(now.Add(time.Hour), deadPID), want: false},
		{name: "unexpired-live-pid", lease: at(now.Add(time.Hour), resumed.Process.Pid), want: false},
		{name: "unexpired-self-pid", lease: at(now.Add(time.Hour), os.Getpid()), want: false},
		{name: "unexpired-no-pid", lease: at(now.Add(time.Hour), 0), want: false},
		{name: "unexpired-another-host", lease: &config.Lease{Holder: "old-session", Host: "elsewhere", PID: deadPID, Expires: now.Add(time.Hour)}, want: false},
	} {
		if got := takenOverLeaseExpired(tc.lease, now); got != tc.want {
			t.Fatalf("%s: takenOverLeaseExpired=%v want=%v", tc.name, got, tc.want)
		}
	}
	// At the lease gate itself, under the same fixed clock: every
	// unexpired shape refuses with the exact extended holder refusal;
	// the same dead-refresher lease one second past expiry, and at its
	// expiry exactly, is admitted. The gate releases nothing, so the
	// lease record is unchanged after every case, ready for ordinary
	// acquisition to decide.
	for _, tc := range []struct {
		name    string
		lease   *config.Lease
		refused bool
	}{
		{name: "dead-refresher-unexpired-refused", lease: at(now.Add(time.Hour), deadPID), refused: true},
		{name: "dead-refresher-one-second-past-expiry-admitted", lease: at(now.Add(-time.Second), deadPID)},
		{name: "dead-refresher-exactly-at-expiry-admitted", lease: at(now, deadPID)},
		{name: "dead-refresher-one-second-before-expiry-refused", lease: at(now.Add(time.Second), deadPID), refused: true},
		{name: "live-pid-unexpired-refused", lease: at(now.Add(time.Hour), resumed.Process.Pid), refused: true},
		{name: "self-pid-unexpired-refused", lease: at(now.Add(time.Hour), os.Getpid()), refused: true},
		{name: "no-pid-unexpired-refused", lease: at(now.Add(time.Hour), 0), refused: true},
		{name: "other-holder-left-to-admission", lease: &config.Lease{Holder: "other-session", Host: machineLabel(), PID: deadPID, Expires: now.Add(time.Hour)}},
		{name: "no-lease-admitted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.agent("alice")
			_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
				installed := previous
				st.Terminal = &installed
				st.Lease = tc.lease
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			a := &App{Now: func() time.Time { return now }}
			s := &session{app: a, paths: e.paths(), name: "alice"}
			err = a.checkTakenOverLease(s, previous)
			if !tc.refused && err != nil {
				t.Fatalf("checkTakenOverLease=%v want nil", err)
			}
			if tc.refused {
				want := "take-over refused: binding held by live session old-session (generation handover-gen, owner pid " + strconv.Itoa(previous.OwnerPID) + ", consumer pid " + strconv.Itoa(previous.DaemonPID) + "); lease held until " + tc.lease.Expires.UTC().Format(time.RFC3339) + "; retry after it expires"
				if err == nil || err.Error() != want {
					t.Fatalf("checkTakenOverLease=%v want %q", err, want)
				}
			}
			after := e.state("alice").Lease
			if tc.lease == nil && after != nil {
				t.Fatalf("lease after=%+v want none", after)
			}
			if tc.lease != nil && (after == nil || after.Holder != tc.lease.Holder || after.Host != tc.lease.Host || after.PID != tc.lease.PID || !after.Expires.Equal(tc.lease.Expires)) {
				t.Fatalf("lease after=%+v want unchanged %+v", after, tc.lease)
			}
		})
	}
}

// Take-over end to end: with the recorded owner and consumer gone and
// the previous session's lease expired, a new session of the same
// identity gets exactly one new generation; the previous generation is
// recorded as taken over by it in State and in binding.log, and the new
// session can unbind what it took over. This is the expired-lease
// control beside the live-lease negative above.
func TestBindHandoverTakeover(t *testing.T) {
	isolateBindTest(t)
	if testing.Short() {
		t.Skip("detached CLI integration")
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux unavailable")
	}
	dir, owned := newBindFixtureDir(t)
	e, _ := world(t)
	configDir := filepath.Join(dir, "config")
	if err := os.Rename(e.dir, configDir); err != nil {
		t.Fatal(err)
	}
	e.dir = configDir
	t.Cleanup(func() {
		if st, err := config.LoadState(e.paths(), "alice"); err == nil && st.Terminal != nil && st.Terminal.DaemonPID > 0 {
			if birth, err := bindingProcessBirth(context.Background(), st.Terminal.DaemonPID); err == nil && birth == st.Terminal.DaemonBirth {
				if proc, err := os.FindProcess(st.Terminal.DaemonPID); err == nil {
					proc.Signal(syscall.SIGTERM)
				}
			}
		}
		exec.Command("tmux", "-S", filepath.Join(dir, "s"), "kill-server").Run()
	})
	e.extra[EnvSessionID] = "old-session"
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dead := func() (int, string) {
		t.Helper()
		p := exec.Command("sleep", "0.2")
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
		birth, err := tmuxwake.Birth(context.Background(), p.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Wait(); err != nil {
			t.Fatal(err)
		}
		return p.Process.Pid, birth
	}
	ownerPID, ownerBirth := dead()
	consumerPID, consumerBirth := dead()
	oldGen := strings.Repeat("a", 32)
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &config.TerminalBinding{Generation: oldGen, SessionID: "old-session", Host: machineLabel(), OwnerPID: ownerPID, OwnerBirth: ownerBirth, DaemonPID: consumerPID, DaemonBirth: consumerBirth, Target: tmuxwake.Target{Runtime: "codex"}, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 6, Outcome: "armed: no new mail"}
		st.Lease = &config.Lease{Holder: "old-session", Host: machineLabel(), PID: ownerPID, Expires: time.Now().Add(-time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "a2a")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", exe, "../../cmd/a2a").CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, out)
	}
	script := filepath.Join(dir, "fixture.sh")
	fixture := "#!/bin/sh\n\"$1\" --as alice bind --runtime codex --owner-pid $$ --session-id new-session --take-over --interval 1s --retry-max 2s --max-wakes-per-hour 6 >\"$2/bind.out\" 2>\"$2/bind.err\"\necho $? >\"$2/bind.code\"\nexec sleep 300\n"
	if err := os.WriteFile(script, []byte(fixture), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	cmd := exec.CommandContext(ctx, "tmux", "-S", socket, "-f", "/dev/null", "new-session", "-d", "-x", "120", "-y", "24", "sh", script, exe, dir)
	cmd.Env = append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_SESSION_ID=", "A2A_AGENT=alice", "CODEX_THREAD_ID=new-session", "CLAUDE_CODE_SESSION_ID=", "MUSE_SESSION_ID=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("private tmux: %v %s", err, out)
	}
	serverRaw, err := exec.Command("tmux", "-S", socket, "display-message", "-p", "#{pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	serverPID, _ := strconv.Atoi(strings.TrimSpace(string(serverRaw)))
	owned.Server, err = bindFixtureIdentity(serverPID)
	if err != nil {
		t.Fatal(err)
	}
	paneRaw, err := exec.Command("tmux", "-S", socket, "list-panes", "-F", "#{pane_pid}").Output()
	if err != nil {
		t.Fatal(err)
	}
	panePID, _ := strconv.Atoi(strings.TrimSpace(string(paneRaw)))
	owned.Fixture, err = bindFixtureIdentity(panePID)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeBindFixtureRecord(dir, owned); err != nil {
		t.Fatal(err)
	}
	var code string
	until := time.Now().Add(60 * time.Second)
	for time.Now().Before(until) {
		if b, err := os.ReadFile(filepath.Join(dir, "bind.code")); err == nil {
			code = strings.TrimSpace(string(b))
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if code != "0" {
		out, _ := os.ReadFile(filepath.Join(dir, "bind.out"))
		errText, _ := os.ReadFile(filepath.Join(dir, "bind.err"))
		t.Fatalf("take-over bind code=%q out=%s err=%s", code, out, errText)
	}
	st := awaitBinding(t, e.paths(), func(st config.State) bool {
		return st.Terminal != nil && st.Terminal.Generation != oldGen && st.Terminal.DaemonPID > 0
	})
	b := st.Terminal
	if b.SessionID != "new-session" || b.Stopped || processGone(b.DaemonPID) {
		t.Fatalf("taken-over binding not live for the new session: %+v", b)
	}
	if st.Lease == nil || st.Lease.Holder != "new-session" {
		t.Fatalf("lease not held by the new session: %+v", st.Lease)
	}
	if b.TakenOver == nil || b.TakenOver.Generation != oldGen || b.TakenOver.SessionID != "old-session" || b.TakenOver.BySessionID != "new-session" || b.TakenOver.At.IsZero() {
		t.Fatalf("take-over not recorded in State: %+v", b.TakenOver)
	}
	logBytes, err := os.ReadFile(filepath.Join(e.dir, "state", "alice.binding.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logBytes), "taken_over generation="+oldGen+" session=old-session by=new-session at=") {
		t.Fatalf("take-over not recorded in binding.log: %s", logBytes)
	}
	consumer := b.DaemonPID
	stop := exec.CommandContext(ctx, exe, "--as", "alice", "unbind")
	stop.Env = append(os.Environ(), "A2A_CONFIG_DIR="+e.dir, "A2A_SESSION_ID=", "A2A_AGENT=alice", "CODEX_THREAD_ID=new-session", "CLAUDE_CODE_SESSION_ID=", "MUSE_SESSION_ID=")
	if out, err := stop.CombinedOutput(); err != nil {
		t.Fatalf("unbind after take-over: %v %s", err, out)
	}
	if !processGone(consumer) || !e.state("alice").Terminal.Stopped {
		t.Fatal("unbind after take-over did not stop the consumer")
	}
}
