//go:build linux || darwin

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

func muteApp(e *env) (*App, *session) {
	a := NewApp()
	a.as = "alice"
	a.HTTP = e.hs.srv.Client()
	a.ctx = context.Background()
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}
	if e.now != nil {
		a.Now = e.now
	}
	s, err := a.session()
	if err != nil {
		e.t.Fatal(err)
	}
	return a, s
}
func scopeFile(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scope.json")
	b, _ := json.Marshal(map[string]any{"version": 1, "roots": []any{map[string]any{"root": root, "connections": []string{""}}}})
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Public status/capability/undo ignore ambient station and credentials. Muting
// requires actual fds, explicit scope/run/budget and no approval flag fallback.
func TestScopeMuteCLIExplicitInterface(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	file := scopeFile(t, root)
	var out, diag bytes.Buffer
	a := NewApp()
	a.Stdout = &out
	a.Stderr = &diag
	a.Stdin = strings.NewReader("")
	a.Getenv = func(string) string { t.Fatal("host mute consulted ambient identity/config"); return "" }
	if code := a.Run(context.Background(), []string{"host", "mute", "capability"}); code != 0 {
		t.Fatal(diag.String())
	}
	if !strings.Contains(out.String(), `"quiesced":false`) || !strings.Contains(out.String(), `"version":1`) {
		t.Fatal("missing machine capability")
	}
	out.Reset()
	diag.Reset()
	if code := a.Run(context.Background(), []string{"host", "mute", "status", "--scope", file, "--budget", "1s"}); code == 0 || !strings.Contains(out.String(), `"status":"unsupported"`) {
		t.Fatal("unenrolled accepted")
	}
	fixture.Enroll(t, scope)
	for _, args := range [][]string{
		{"host", "mute", "--scope", file, "--run", "x"},
		{"--as", "alice", "host", "mute", "--scope", file, "--run", "x", "--budget", "1s"},
		{"host", "mute", "--scope", file, "--run", "x", "--budget", "1s"},
		{"host", "mute", "--scope", file, "--run", "x", "--budget", "1s", "--approved"},
		{"host", "unmute", "--scope", file, "--run", "x", "--budget", "1s"},
	} {
		before, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
		out.Reset()
		diag.Reset()
		if a.Run(context.Background(), args) == 0 {
			t.Fatal("invalid interface accepted")
		}
		after, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
		if !bytes.Equal(before, after) {
			t.Fatal("invalid interface changed ledger")
		}
	}
	op := fixture.Mute(t, scope, "cleanup")
	out.Reset()
	diag.Reset()
	if a.Run(context.Background(), []string{"host", "unmute", "--scope", file, "--run", op.Run, "--budget", "1s", "--token", op.ID}) != 0 {
		t.Fatal(diag.String())
	}
}

// Hook output alone is muted: current subscriptions, private fixture credential
// bytes and mailbox receipts are preserved; ingestion still accepts new mail.
// Explicit inbox is the presentation/receipt boundary during an active mute.
func TestScopeMuteHookMailAndIndependentPauses(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, runtime := range []string{"claude", "codex", "muse"} {
		t.Run(runtime, func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			clk := e.clock()
			e.extra[EnvSessionID] = "fixture-session"
			// Prime positioning before mail arrives, without terminal auto-binding.
			e.stdin = `{"session_id":"fixture-session"}`
			e.as("alice", "hook", "prompt", "--runtime", runtime).ok(t)
			scope := fixture.Scope(e.dir)
			fixture.Enroll(t, scope)
			creds, _ := os.ReadFile(e.paths().AgentFile("alice"))
			beforeID := e.hs.inject(room, "@bob:test.local", ping(1))
			op := fixture.Mute(t, scope, "mail")
			duringID := e.hs.inject(room, "@bob:test.local", ping(2))
			a, s := muteApp(e)
			subs, err := s.getSubs(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				r := e.as("alice", "hook", "prompt", "--runtime", runtime).ok(t)
				if r.stdout != "" {
					t.Fatal("muted hook emitted output")
				}
				clk.add(config.NotificationEvery)
			}
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if len(mb.Pending()) != 2 {
				t.Fatalf("muted ingestion lost mail: %d", len(mb.Pending()))
			}
			for _, en := range mb.Pending() {
				if en.Shown {
					t.Fatal("suppressed hook marked Shown")
				}
			}
			if _, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error { st.HooksPaused = true; return nil }); err != nil {
				t.Fatal(err)
			}
			fixture.Unmute(t, op)
			if !e.state("alice").HooksPaused {
				t.Fatal("unmute cleared independent pause")
			}
			// A pause changed during mute survives another overlapping operation too.
			second := fixture.Mute(t, scope, "already-paused")
			fixture.Unmute(t, second)
			if !e.state("alice").HooksPaused {
				t.Fatal("pre-paused identity resumed")
			}
			third := fixture.Mute(t, scope, "explicit-inbox")
			inbox := e.as("alice", "inbox").ok(t)
			if inbox.stdout == "" || !e.received("alice", beforeID) || !e.received("alice", duringID) {
				t.Fatal("explicit inbox unavailable during mute")
			}
			fixture.Unmute(t, third)
			afterID := e.hs.inject(room, "@bob:test.local", ping(3))
			e.as("alice", "inbox").ok(t)
			if !e.received("alice", afterID) {
				t.Fatal("post-thaw mail lost")
			}
			currentCreds, _ := os.ReadFile(e.paths().AgentFile("alice"))
			if !bytes.Equal(creds, currentCreds) {
				t.Fatal("mute opened or rewrote credentials")
			}
			currentSubs, err := s.getSubs(context.Background())
			if err != nil || !reflect.DeepEqual(subs, currentSubs) {
				t.Fatal("mute rewrote subscriptions")
			}
			_ = a
		})
	}
}

// A mute between native reservation and turn/start prevents the actual native
// call, keeps the uncertain journal, and leaves mail for explicit inbox.
func TestScopeMuteNativeLateAdmission(t *testing.T) {
	e, room, n, generation := runtimeWorld(t)
	id := runtimeMail(e, room, "pending")
	scope := fixture.Scope(e.dir)
	fixture.Enroll(t, scope)
	approved := fixture.Confirm(t, scope, "late-native", "mute")
	var operation wakemute.Operation
	e.configure = func(a *App) {
		a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
		a.afterWakeReserve = func() error {
			out, err := wakemute.Mute(fixture.Context(t), approved)
			if err == nil {
				operation = *out.Operation
			}
			return err
		}
	}
	e.as("alice", "runtime", "dispatch", "--generation", generation).ok(t)
	st := e.state("alice")
	if len(n.wakes) != 0 || st.Wake.Attempt == nil || e.received("alice", id) {
		t.Fatal("muted native call admitted or uncertainty erased")
	}
	evidence := *st.Wake.Attempt
	fixture.Unmute(t, operation)
	e.configure = func(a *App) { a.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil } }
	e.as("alice", "runtime", "dispatch", "--generation", generation).ok(t)
	if len(n.wakes) != 0 || !reflect.DeepEqual(evidence, *e.state("alice").Wake.Attempt) {
		t.Fatal("thaw retriggered or erased native uncertainty")
	}
	// Adjacent independent eligible native identity proves the route can wake.
	e2, r2, n2, g2 := runtimeWorld(t)
	runtimeMail(e2, r2, "eligible")
	fixture.Enroll(t, fixture.Scope(e2.dir))
	e2.as("alice", "runtime", "dispatch", "--generation", g2).ok(t)
	if len(n2.wakes) != 1 {
		t.Fatal("native positive refused")
	}
}

// Daemon notice output and MarkShown share the actual effect gate. A muted
// notice writes no bytes and changes no mailbox bytes; the valid neighbor is
// shown without becoming an inbox receipt.
func TestScopeMuteDaemonNoticeShown(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	id := runtimeMail(e, room, "notice")
	scope := fixture.Scope(e.dir)
	fixture.Enroll(t, scope)
	a, s := muteApp(e)
	var out bytes.Buffer
	a.Stdout = &out
	op := fixture.Mute(t, scope, "notice")
	before, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	err := a.emitScopedNotice(context.Background(), s, []byte("fixed notice\n"), []string{id})
	after, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	if !errors.Is(err, wakemute.ErrMuted) || out.Len() != 0 || !bytes.Equal(before, after) {
		t.Fatal("suppressed daemon notice was printed/shown")
	}
	fixture.Unmute(t, op)
	if err = a.emitScopedNotice(context.Background(), s, []byte("fixed notice\n"), []string{id}); err != nil {
		t.Fatal(err)
	}
	mb, _ := config.OpenMailbox(e.paths(), "alice")
	if out.Len() == 0 || len(mb.Pending()) != 1 || !mb.Pending()[0].Shown || mb.Received(id) {
		t.Fatal("daemon positive or receipt boundary failed")
	}
}

// Result-output loss is explicitly uncertain and never silently undoes the
// authorized cleanup that committed before output failed.
func TestScopeMuteCLIOutputFailure(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	op := fixture.Mute(t, scope, "output-loss")
	a := NewApp()
	a.Stdout = cmOutput(func([]byte) (int, error) { return 0, io.ErrClosedPipe })
	a.Stderr = io.Discard
	err := cmdHostMute(context.Background(), a, []string{"unmute", "--scope", scopeFile(t, root), "--budget", "1s", "--run", op.Run, "--token", op.ID})
	if !errors.Is(err, wakemute.ErrUncertain) {
		t.Fatal("output failure reported rollback/success")
	}
	result, e := wakemute.Status(fixture.Context(t), scope)
	if e != nil || result.Status != "unmuted" {
		t.Fatal("committed cleanup was rolled back")
	}
}

// Required participation is read from admitted launch configuration rather
// than inferred from mutable file absence. Explicit inbox remains unaffected.
func TestScopeMuteRequiredLaunchVersion(t *testing.T) {
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	runtimeMail(e, room, "required")
	a, s := muteApp(e)
	e.extra[wakemute.EnvRequiredVersion] = "1"
	called := false
	err := a.withWakeAdmission(fixture.Context(t), s.paths, func(uint64) error { called = true; return nil })
	if !errors.Is(err, wakemute.ErrUnsafe) || called {
		t.Fatal("required launch defaulted to legacy")
	}
	e.as("alice", "inbox").ok(t)
	fixture.Enroll(t, fixture.Scope(e.dir))
	err = a.withWakeAdmission(fixture.Context(t), s.paths, func(uint64) error { called = true; return nil })
	if err != nil || !called {
		t.Fatal("required launch positive refused")
	}
}

// Actual named Paths preserve the original root and immutable connection ID;
// a selected alpha mute cannot broaden to beta, and a nested-root override
// cannot hide an enrolled ancestor. No secret-store or network call is needed.
func TestScopeMuteNamedPathsAndRegistryBinding(t *testing.T) {
	f := newNamedFixture(t, false)
	file := filepath.Join(t.TempDir(), "scope.json")
	b, _ := json.Marshal(map[string]any{"version": 1, "roots": []any{map[string]any{"root": f.root, "connections": []string{"alpha", "beta"}}}})
	if err := os.WriteFile(file, b, 0600); err != nil {
		t.Fatal(err)
	}
	scope, err := loadWakeScope(file)
	if err != nil {
		t.Fatal(err)
	}
	fixture.Enroll(t, scope)
	alpha := wakemute.Scope{{Root: f.root, Connections: []wakemute.Connection{scope[0].Connections[0]}}}
	op := fixture.Mute(t, alpha, "named")
	a := f.app()
	a.profile = "alpha"
	paths, err := a.pathsFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err = a.withWakeAdmission(fixture.Context(t), paths, func(uint64) error { t.Fatal("named alpha admitted"); return nil }); !errors.Is(err, wakemute.ErrMuted) {
		t.Fatal(err)
	}
	beta, err := f.r.Profiles["beta"].Paths(config.Paths{Base: f.root, WakeMuteVersion: 1}, "alice", false)
	if err != nil {
		t.Fatal(err)
	}
	if beta.WakeMuteVersion != 1 {
		t.Fatal("named Paths dropped required control version")
	}
	called := false
	if err = a.withWakeAdmission(fixture.Context(t), beta, func(uint64) error { called = true; return nil }); err != nil || !called {
		t.Fatal("unmuted named beta refused")
	}
	if err = a.withWakeAdmission(fixture.Context(t), config.Paths{Base: paths.MetadataBase}, func(uint64) error { t.Fatal("nested override admitted"); return nil }); err == nil {
		t.Fatal("override bypassed ancestor")
	}
	if f.effects() != 0 {
		t.Fatal("host scope opened credentials or network")
	}
	fixture.Unmute(t, op)
}

// This native receiver fixture accepts work locally before mute, then completes
// that same work after ACK. No cancel/drain is inferred from the local fence.
func TestScopeMuteNativeAlreadyAdmittedCompletion(t *testing.T) {
	e, room, n, g := runtimeWorld(t)
	runtimeMail(e, room, "queued")
	scope := fixture.Scope(e.dir)
	fixture.Enroll(t, scope)
	receiverAdmitted, receiverRelease, receiverDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
	n.onWake = func() { close(receiverAdmitted); go func() { <-receiverRelease; close(receiverDone) }() }
	e.as("alice", "runtime", "dispatch", "--generation", g).ok(t)
	<-receiverAdmitted
	operation := fixture.Mute(t, scope, "already-admitted")
	result, err := wakemute.Status(fixture.Context(t), scope)
	if err != nil || result.Quiesced {
		t.Fatal("mute claimed receiver drain")
	}
	select {
	case <-receiverDone:
		t.Fatal("receiver unexpectedly completed before release")
	default:
	}
	close(receiverRelease)
	<-receiverDone
	if len(n.wakes) != 1 {
		t.Fatal("already-admitted work lost or duplicated")
	}
	fixture.Unmute(t, operation)
}

// An identity state-lock contender cannot retain the scope admission lock
// beyond the cycle budget. The independent fixture holder releases itself;
// a valid mute immediately afterwards proves the root lock was not stranded.
func TestScopeMuteStateWaitBudget(t *testing.T) {
	e, _, _ := runtimeUnboundWorld(t)
	a, s := muteApp(e)
	scope := fixture.Scope(e.dir)
	fixture.Enroll(t, scope)
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := config.UpdateState(e.paths(), "alice", func(*config.State) error { close(entered); <-release; return nil })
		done <- err
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	bounded := *a
	bounded.wakeContext = ctx
	start := time.Now()
	err := bounded.withWakeAdmission(ctx, s.paths, func(uint64) error {
		_, e := bounded.updateState(s.paths, s.name, func(*config.State) error { t.Fatal("contended state callback ran"); return nil })
		return e
	})
	close(release)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("state lock budget: %v", err)
	}
	operation := fixture.Mute(t, scope, "after-contention")
	fixture.Unmute(t, operation)
}

// A scope mute is not a broad return over outbound maintenance: the existing
// stale-mail reporting transaction still posts its two fixture destinations.
func TestScopeMutePreservesAlarmReporting(t *testing.T) {
	e, a, s, _, sample := alarmFixture(t)
	primeAlarm(t, e, a, s, sample)
	scope := fixture.Scope(e.dir)
	fixture.Enroll(t, scope)
	operation := fixture.Mute(t, scope, "reporting")
	a.sendMailAlarm(context.Background(), s, "alarm")
	if len(alarmEvents(e)) != 2 {
		t.Fatal("scope mute suppressed existing outbound alarm contract")
	}
	fixture.Unmute(t, operation)
}
