package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
)

type nativeFixture struct {
	thread                codexwake.Snapshot
	endpoint, socket, cwd string
	wakes                 []string
	ambiguous             bool
	readErr               error
	historyErr, wakeErr   error
	acceptedErr           error
}

func (n *nativeFixture) Read(_ context.Context, _ string, history bool) (codexwake.Snapshot, error) {
	if history && n.historyErr != nil {
		return codexwake.Snapshot{}, n.historyErr
	}
	return n.thread, n.readErr
}
func (n *nativeFixture) Info() (string, string) { return "0.160.1", n.socket }

func (n *nativeFixture) Reconcile(_ context.Context, thread, output string, _ time.Time) (string, error) {
	if thread != n.thread.ID {
		return "", errors.New("wrong thread")
	}
	if turn, ok := codexwake.Accepted(n.thread, output); ok {
		return turn, nil
	}
	return "", nil
}

func (n *nativeFixture) TurnStatus(_ context.Context, thread, id string) (string, error) {
	if thread != n.thread.ID {
		return "", errors.New("wrong thread")
	}
	for _, turn := range n.thread.Turns {
		if turn.ID == id {
			return turn.Status, nil
		}
	}
	return "", errors.New("missing turn")
}
func (n *nativeFixture) Close() {}
func (n *nativeFixture) Wake(_ context.Context, thread, output string) (string, error) {
	if thread != n.thread.ID {
		return "", errors.New("wrong thread")
	}
	n.wakes = append(n.wakes, output)
	if n.wakeErr != nil {
		return "", n.wakeErr
	}
	raw, _ := json.Marshal(output)
	id := "turn-" + strconv.Itoa(len(n.wakes))
	n.thread.Turns = append(n.thread.Turns, codexwake.Turn{ID: id, Status: "completed", Items: []codexwake.Item{{Type: "functionCallOutput", Name: codexwake.ToolName, Namespace: codexwake.ToolNamespace, Output: raw}}})
	if n.acceptedErr != nil {
		return "", n.acceptedErr
	}
	if n.ambiguous {
		return "", errors.New("connection lost after accept")
	}
	return id, nil
}

// Proves known pre-submit refusals release only the attempt, retain the rate
// reservation and mail, and permit a valid retry. Internal/transport outcomes
// cannot use the same recovery path.
func TestRuntimeDefinitiveRefusalRecovery(t *testing.T) {
	for _, code := range []int{-32600, -32601, -32602, -32603} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			id := runtimeMail(e, room, "pending")
			n.wakeErr = &codexwake.RPCError{Method: "turn/start", Code: code}
			e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			st := e.state("alice")
			if (st.Wake.Attempt == nil) != (code != -32603) || len(st.Wake.Reservations) != 1 || e.received("alice", id) {
				t.Fatal("refusal classification lost its fence, rate slot or mail")
			}
			n.wakeErr = nil
			e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			want := 2
			if code == -32603 {
				want = 1
			}
			if len(n.wakes) != want || e.received("alice", id) {
				t.Fatal("unsafe refusal retry or missing valid control")
			}
		})
	}
}

// Proves reconciliation does not depend on an oversized full-thread read;
// the exact acceptance still closes the fence without another wake or receipt.
func TestRuntimeReconciliationAvoidsFullHistory(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "pending")
	n.ambiguous = true
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	n.historyErr = errors.New("full history exceeds frame bound")
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Attempt != nil || len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("full history wedged reconciliation")
	}
}

func runtimeWorld(t *testing.T) (*env, string, *nativeFixture, string) {
	t.Helper()
	e, room := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	dir, err := os.MkdirTemp("", "a2a-runtime-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "host.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := "unix://" + path
	socket, err := codexwake.SocketIdentity(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	cwd, _ := os.Getwd()
	n := &nativeFixture{thread: codexwake.Snapshot{ID: "owned-thread", Cwd: cwd, Status: codexwake.Status{Type: "idle"}}, endpoint: endpoint, socket: socket, cwd: cwd}
	e.configure = func(a *App) { a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil } }
	e.as("alice", "runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none").ok(t)
	e.extra[EnvSessionID] = n.thread.ID
	return e, room, n, e.state("alice").Runtime.Generation
}
func runtimeMail(e *env, room, body string) string {
	id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local "+body, nil))
	e.stdin = `{"session_id":"owned-thread"}`
	e.as("alice", "hook", "prompt", "--runtime", "codex").ok(e.t)
	return id
}

// Proves this CLI submits one metadata-only native notice for two pending
// entries, never receipts on wake, and inbox remains the receipt boundary.
func TestRuntimeNativeCoalescingAndReceiptBoundary(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	a := runtimeMail(e, room, "PEER_PAYLOAD_MUST_STAY_IN_INBOX")
	b := runtimeMail(e, room, "second")
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 1 || !strings.Contains(n.wakes[0], "2 new peer events") || strings.Contains(n.wakes[0], "PEER_PAYLOAD") {
		t.Fatalf("unsafe/duplicate notice: %q", n.wakes)
	}
	if e.received("alice", a) || e.received("alice", b) || len(e.mailbox("alice").Pending()) != 2 {
		t.Fatal("wake receipted mail")
	}
	e.as("alice", "inbox").ok(t)
	if !e.received("alice", a) || !e.received("alice", b) {
		t.Fatal("inbox failed to receipt")
	}
}

// Proves explicit host gates and native busy/human/unknown states preserve
// mail, with an idle positive control afterward. It exercises snapshot
// admission; native UI state can change after the read (no server CAS API).
func TestRuntimeSuppressionsKeepPending(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "pending")
	for _, gate := range []string{"paused", "budget", "stopping", "unknown"} {
		e.as("alice", "runtime", "gate", "--generation", g, gate).ok(t)
		e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
		if got := e.state("alice").Wake.Outcome; got != "wake suppressed: "+gate {
			t.Fatalf("gate %s: %s", gate, got)
		}
	}
	e.as("alice", "runtime", "gate", "--generation", g, "ready").ok(t)
	for _, state := range []codexwake.Status{{Type: "active"}, {Type: "active", ActiveFlags: []string{"waitingOnApproval"}}, {Type: "active", ActiveFlags: []string{"waitingOnUserInput"}}, {Type: "notLoaded"}, {Type: "systemError"}} {
		n.thread.Status = state
		e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
		if len(n.wakes) != 0 || e.received("alice", id) {
			t.Fatalf("gate bypass: %+v", state)
		}
	}
	n.thread.Status = codexwake.Status{Type: "idle"}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("positive control/receipt invariant")
	}
}

// Proves ambiguous accepted writes and a crash after submission reconcile
// exact tool history without a second wake; an unsent reserved attempt stays
// fenced rather than treating missing history as proof it is safe to retry.
func TestRuntimeAmbiguousCrashReconciliation(t *testing.T) {
	for _, mode := range []string{"ambiguous", "after-submit", "before-submit"} {
		t.Run(mode, func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			id := runtimeMail(e, room, "pending")
			e.configure = func(a *App) {
				a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
				if mode == "after-submit" {
					a.afterWakeSubmit = func() error { return errors.New("injected submit crash") }
				}
				if mode == "before-submit" {
					a.afterWakeReserve = func() error { return errors.New("injected reserve crash") }
				}
			}
			n.ambiguous = mode == "ambiguous"
			r := e.as("alice", "runtime", "dispatch", "--generation", g)
			if mode == "ambiguous" {
				r.ok(t)
			} else {
				r.failsWith(t, ExitError, "injected")
			}
			if e.state("alice").Wake.Attempt == nil {
				t.Fatal("missing durable fence")
			}
			e.configure = func(a *App) { a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil } }
			for i := 0; i < 2; i++ {
				e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			}
			want := 1
			if mode == "before-submit" {
				want = 0
				if !strings.Contains(e.state("alice").Wake.Outcome, "no authoritative acceptance") {
					t.Fatal("absence permitted retry")
				}
			}
			if len(n.wakes) != want || e.received("alice", id) {
				t.Fatal("duplicate wake or forbidden receipt")
			}
		})
	}
}

// Proves generation, host endpoint and manual human-state fences cannot be
// hidden by a fresh waiter or an empty mailbox; valid bind is idempotent.
func TestRuntimeBindingAndEmptyMailVisibility(t *testing.T) {
	e, _, n, g := runtimeWorld(t)
	e.as("alice", "runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", n.endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none").ok(t)
	if e.state("alice").Runtime.Generation != g {
		t.Fatal("idempotent bind replaced generation")
	}
	e.as("alice", "runtime", "dispatch", "--generation", "obsolete").failsWith(t, ExitError, "stale generation")
	e.as("alice", "runtime", "gate", "--generation", g, "budget").ok(t)
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Outcome != "wake suppressed: budget" {
		t.Fatal("empty mailbox concealed budget gate")
	}
	e.as("alice", "runtime", "gate", "--generation", g, "ready").ok(t)
	e.as("alice", statusArgs("waiting-human")...).ok(t)
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Outcome != "wake suppressed: human" {
		t.Fatal("manual human state ignored")
	}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Runtime.EndpointID = "obsolete"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Outcome != "unavailable: endpoint replaced" || len(n.wakes) != 0 {
		t.Fatal("replaced endpoint accepted")
	}
}

// Proves hourly reservations reject a third new event without consuming it.
func TestRuntimeHourlyLimit(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	now := time.Now()
	e.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		runtimeMail(e, room, "pending")
		e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	}
	if len(n.wakes) != 2 || e.state("alice").Wake.Outcome != "wake suppressed: rate" || len(e.mailbox("alice").Pending()) != 3 {
		t.Fatal("rate limit bypassed or mail lost")
	}
}

// Proves a second consumer fails before rebinding or changing telemetry; a
// missing binding has no runtime calls or mailbox receipts. A loaded own
// binding in runtimeWorld is the nearby positive control.
func TestRuntimeSoleConsumerAndMissingBinding(t *testing.T) {
	e, _, n, g := runtimeWorld(t)
	st := e.state("alice")
	err := config.WithRuntimeLock(e.paths(), "alice", "consumer", time.Now().Add(time.Second), func() error {
		e.as("alice", "runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", n.endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "budget", "--hooks", "none", "--watch").failsWith(t, ExitError, "consumer lock")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after := e.state("alice")
	if after.Runtime.Generation != g || after.Runtime.Gate != st.Runtime.Gate || after.Wake.Outcome != st.Wake.Outcome || after.Liveness.State != st.Liveness.State {
		t.Fatal("second consumer changed binding/status")
	}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Runtime = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "runtime", "status").ok(t)
	e.as("alice", "runtime", "dispatch", "--generation", g).failsWith(t, ExitError, "stale generation")
	if len(n.wakes) != 0 {
		t.Fatal("missing binding reached native turn")
	}
}

// Proves own lease/session metadata cannot admit a different live runtime,
// and idempotent bind does not clear an explicitly held human state.
func TestRuntimeBindOwnSessionAndManualGate(t *testing.T) {
	e, _, n, _ := runtimeWorld(t)
	args := []string{"runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", n.endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none"}
	e.extra["CODEX_THREAD_ID"] = "foreign-live-thread"
	e.as("alice", args...).failsWith(t, ExitUsage, "different live Codex thread")
	delete(e.extra, "CODEX_THREAD_ID")
	e.as("alice", statusArgs("waiting-human")...).ok(t)
	e.as("alice", args...).ok(t)
	if e.state("alice").Liveness.State != "waiting-human" || manualWakeGate(e.state("alice")) != "wake suppressed: human" {
		t.Fatal("idempotent bind cleared human gate")
	}
}

// Proves the observed native empty-rollout/metadata read failure is visible
// and cannot grant admission, advance an attempt or receipt pending mail;
// a restored authoritative idle read admits the same retained entry.
func TestRuntimeNativeReadUnavailableRetainsMail(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "pending")
	n.readErr = errors.New("runtime thread/read refused (code -32603)")
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	st := e.state("alice")
	if len(n.wakes) != 0 || st.Wake.Attempt != nil || st.Wake.Outcome != "unavailable: native thread" || e.received("alice", id) {
		t.Fatal("unavailable read admitted or consumed mail")
	}
	n.readErr = nil
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("recovery lost entry or violated receipt boundary")
	}
}

// Proves owner exit cannot start a new inference, but an exact accepted
// historical tool output can still close its durable fence without a retry.
func TestRuntimeOrphanedAttemptReconcilesReadOnly(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "pending")
	n.ambiguous = true
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Runtime.OwnerPID = 2147483647
		st.Runtime.Gate = "stopping"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Attempt != nil || len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("orphan reconciliation failed or created effect")
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 1 || e.state("alice").Wake.Outcome != "unavailable: launcher" {
		t.Fatal("dead owner admitted a wake")
	}
}

// Proves a native RPC acknowledgement followed by terminal failure cannot
// become ready merely because the waiter is alive; retries need the owner's
// explicit ready gate and remain hourly capped, with no inbox receipts.
func TestRuntimeFailedAcceptedTurnRequiresOwnerGate(t *testing.T) {
	for _, status := range []string{"failed", "interrupted", "unknown"} {
		t.Run(status, func(t *testing.T) {
			e, room, n, g := runtimeWorld(t)
			id := runtimeMail(e, room, "pending")
			e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			n.thread.Turns[0].Status = status
			for i := 0; i < 2; i++ {
				e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			}
			if len(n.wakes) != 1 || strings.Contains(e.state("alice").Wake.Outcome, "runtime ready") || e.received("alice", id) {
				t.Fatal("failed wake concealed or automatically retried")
			}
			if status == "unknown" {
				return
			}
			e.as("alice", "runtime", "gate", "--generation", g, "ready").ok(t)
			e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
			if len(n.wakes) != 2 || e.received("alice", id) {
				t.Fatal("explicit owner recovery lost pending entry")
			}
		})
	}
}

// Proves only an observed-dead owner in the stopping generation may retire an
// uncertain attempt. Retirement performs no RPC/receipt; replacement preserves
// suppressed old entries and hourly reservations while permitting new mail.
func TestRuntimeAbandonRetiresWithoutReplay(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "uncertain")
	e.configure = func(a *App) {
		a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
		a.afterWakeReserve = func() error { return errors.New("reserved crash") }
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).failsWith(t, ExitError, "reserved crash")
	e.configure = func(a *App) { a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil } }
	e.as("alice", "runtime", "abandon", "--generation", g).failsWith(t, ExitError, "still live")
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.Runtime.OwnerPID = 2147483647; return nil })
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "runtime", "abandon", "--generation", g).failsWith(t, ExitError, "stopping generation")
	e.as("alice", "runtime", "gate", "--generation", g, "stopping").ok(t)
	e.as("alice", "runtime", "abandon", "--generation", "obsolete").failsWith(t, ExitError, "stale generation")
	e.as("alice", "runtime", "abandon", "--generation", g).ok(t)
	st := e.state("alice")
	if !st.Runtime.Retired || st.Wake.Attempt != nil || len(st.Wake.Reservations) != 1 || len(n.wakes) != 0 || e.received("alice", id) {
		t.Fatal("retirement lost mail/rate or produced effects")
	}
	e.as("alice", "runtime", "gate", "--generation", g, "ready").failsWith(t, ExitError, "retired generation")
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if len(n.wakes) != 0 {
		t.Fatal("retired generation resumed")
	}
	e.as("alice", "runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", n.endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none").ok(t)
	next := e.state("alice").Runtime.Generation
	if next == g {
		t.Fatal("replacement inherited retired generation")
	}
	e.as("alice", "runtime", "dispatch", "--generation", next).ok(t)
	if len(n.wakes) != 0 || e.received("alice", id) {
		t.Fatal("replacement replayed or receipted uncertain mail")
	}
	runtimeMail(e, room, "new control")
	e.as("alice", "runtime", "dispatch", "--generation", next).ok(t)
	if len(n.wakes) != 1 {
		t.Fatal("new generation refused valid new mail")
	}
	runtimeMail(e, room, "rate control")
	e.as("alice", "runtime", "dispatch", "--generation", next).ok(t)
	if len(n.wakes) != 1 || e.state("alice").Wake.Outcome != "wake suppressed: rate" {
		t.Fatal("replacement reset rate reservations")
	}
}

// Proves a correlated internal error after input was queued stays fenced and
// reconciles exact native acceptance rather than submitting it a second time.
func TestRuntimeServerErrorAfterAcceptedInput(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	id := runtimeMail(e, room, "pending")
	n.acceptedErr = &codexwake.RPCError{Method: "turn/start", Code: -32603}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Attempt == nil {
		t.Fatal("internal error discarded possibly accepted input")
	}
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	if e.state("alice").Wake.Attempt != nil || len(n.wakes) != 1 || e.received("alice", id) {
		t.Fatal("internal error retried instead of reconciling acceptance")
	}
}
