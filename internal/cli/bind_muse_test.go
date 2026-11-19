//go:build linux || darwin

package cli

import (
	"context"
	"os"
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
	live             bool
	sendErr          error
	targets, bodies  []string
}

func (f *fakeMuseSender) SessionLive(_ context.Context, sessionID string) (bool, error) {
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
