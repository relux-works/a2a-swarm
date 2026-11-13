//go:build linux || darwin

package cli

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// The fake replaces terminal I/O only, exercising the real dispatch,
// reservation, pre-type and recovery callbacks without executing a terminal.
type muteTerminal struct {
	b                           config.TerminalBinding
	observations, types, enters int
	owned                       bool
	draft                       bool
	onType                      func()
}

func (f *muteTerminal) assessment() tmuxwake.ReadinessAssessment {
	state := tmuxwake.InputEmpty
	if f.owned {
		state = tmuxwake.InputNonempty
	}
	return tmuxwake.ReadinessAssessment{
		Runtime: tmuxwake.RuntimeAssessment{State: tmuxwake.RuntimeReady, Reason: tmuxwake.RuntimeNone},
		Input:   tmuxwake.InputAssessment{State: state, Reason: tmuxwake.InputReasonNone},
		Key:     tmuxwake.ReadinessKey{Route: tmuxwake.LegacyCodex, Target: f.b.Target, Generation: f.b.Generation, SessionID: f.b.SessionID, PaneWidth: 120},
	}
}
func (f *muteTerminal) ObserveReadiness(context.Context, string) tmuxwake.ReadinessAssessment {
	f.observations++
	return f.assessment()
}
func (f *muteTerminal) AssessOwnedNotice(context.Context, string) tmuxwake.ReadinessAssessment {
	return f.assessment()
}
func (f *muteTerminal) Send(ctx context.Context, _ int, _ tmuxwake.ReadinessKey, before, after func()) (tmuxwake.SendResult, error) {
	if before != nil {
		before()
	}
	if ctx.Err() != nil {
		return tmuxwake.SendResult{}, ctx.Err()
	}
	f.types++
	if f.onType != nil {
		f.onType()
	}
	if after != nil {
		after()
	}
	if f.draft {
		f.owned = true
		return tmuxwake.SendResult{Disposition: tmuxwake.SendDraft, Outcome: tmuxwake.SendDraftUnavailable}, nil
	}
	f.enters++
	return tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, nil
}
func (f *muteTerminal) ResumeNotice(_ context.Context, _ int, _ string, _ tmuxwake.ReadinessKey, before ...func() error) (tmuxwake.SendResult, error) {
	for _, fence := range before {
		if err := fence(); err != nil {
			return tmuxwake.SendResult{}, err
		}
	}
	f.enters++
	f.owned = false
	return tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}, nil
}
func muteTerminalWorld(t *testing.T) (*env, *App, *session, *muteTerminal, *time.Time) {
	t.Helper()
	e, room := world(t)
	e.as("alice", "wait", "--timeout", "1ms").ok(t)
	runtimeMail(e, room, "pending")
	now := time.Now()
	e.now = func() time.Time { return now }
	a, s := muteApp(e)
	target := tmuxwake.Target{Runtime: "codex", OwnerPID: 42, OwnerBirth: "fixture-birth", Pane: "%1", Socket: "/fixture-only", SocketID: "fixture-socket"}
	b := config.TerminalBinding{Generation: "scope-terminal", SessionID: "fixture-session", Host: machineLabel(), OwnerPID: 42, OwnerBirth: target.OwnerBirth, DaemonPID: 43, DaemonBirth: "fixture-consumer", Target: target, Interval: time.Second, RetryMax: 2 * time.Second, MaxWakes: 30}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.Terminal = &b
		st.Lease = &config.Lease{Holder: b.SessionID, Host: b.Host, PID: 42, Expires: now.Add(time.Hour)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := &muteTerminal{b: b}
	a.terminalFixture = func(binding config.TerminalBinding) terminalEffects { fake.b = binding; return fake }
	fixture.Enroll(t, fixture.Scope(e.dir))
	return e, a, s, fake, &now
}
func muteDispatch(t *testing.T, a *App, s *session) {
	t.Helper()
	if err := a.dispatchTerminal(context.Background(), s, "scope-terminal"); err != nil {
		t.Fatal(err)
	}
}

// A pulse never sampled by the consumer invalidates old idle/input samples.
// Two fresh post-thaw observations are needed before the real pre-type fence.
func TestScopeMuteTerminalPulseFreshReadiness(t *testing.T) {
	e, a, s, fake, now := muteTerminalWorld(t)
	muteDispatch(t, a, s)
	*now = now.Add(2 * time.Second)
	if e.state("alice").Terminal.Idle == nil {
		t.Fatal("initial idle qualification missing")
	}
	op := fixture.Mute(t, fixture.Scope(e.dir), "pulse")
	fixture.Unmute(t, op)
	muteDispatch(t, a, s)
	if fake.types != 0 || e.state("alice").Terminal.Idle != nil {
		t.Fatal("pulse reused pre-mute idle qualification")
	}
	muteDispatch(t, a, s)
	if fake.types != 0 {
		t.Fatal("single fresh sample was enough")
	}
	*now = now.Add(2 * time.Second)
	muteDispatch(t, a, s)
	if fake.types != 1 || fake.enters != 1 {
		t.Fatal("fresh qualified positive did not wake")
	}
	if len(e.mailbox("alice").Pending()) != 1 {
		t.Fatal("terminal wake receipted mail")
	}
}

// Existing drafts, typing intent and owned-notice origin survive a pulse and
// consumer reconstruction. Thaw does not automatically retype or press Enter.
// The adjacent no-pulse control exercises the actual owned recovery callback.
func TestScopeMuteTerminalOwnedNoticeRecovery(t *testing.T) {
	for _, pulse := range []bool{false, true} {
		t.Run(map[bool]string{false: "eligible", true: "pulse-restart"}[pulse], func(t *testing.T) {
			e, a, s, fake, now := muteTerminalWorld(t)
			fake.draft = true
			muteDispatch(t, a, s)
			*now = now.Add(2 * time.Second)
			muteDispatch(t, a, s)
			before := *e.state("alice").Terminal
			if fake.types != 1 || !before.TypingStarted || before.NoticeOrigin == nil {
				t.Fatal("draft pre-type fence not exercised")
			}
			*now = now.Add(2 * config.NotificationEvery)
			if pulse {
				op := fixture.Mute(t, fixture.Scope(e.dir), "owned")
				fixture.Unmute(t, op)
				replacement, session := muteApp(e)
				replacement.terminalFixture = a.terminalFixture
				a, s = replacement, session
			}
			for i := 0; i < 3; i++ {
				muteDispatch(t, a, s)
				*now = now.Add(time.Second)
			}
			after := e.state("alice").Terminal
			if pulse {
				if fake.enters != 0 || fake.types != 1 || !after.WakeMuteHold || !reflect.DeepEqual(before.AttemptIDs, after.AttemptIDs) || before.Notice != after.Notice || !reflect.DeepEqual(before.NoticeOrigin, after.NoticeOrigin) {
					t.Fatal("thaw submitted or erased owned draft")
				}
			} else if fake.enters != 1 || fake.types != 1 || after.Recovery == nil {
				t.Fatal("owned recovery positive was not exercised")
			}
		})
	}
}

// The root admission lock spans both terminal typing and Enter, including a
// mute contender arriving between them. ACK cannot precede that local effect.
func TestScopeMuteTerminalPreTypeEnterRace(t *testing.T) {
	e, a, s, fake, now := muteTerminalWorld(t)
	scope := fixture.Scope(e.dir)
	muteDispatch(t, a, s)
	*now = now.Add(2 * time.Second)
	approved := fixture.Confirm(t, scope, "between-type-enter", "mute")
	entered, release := make(chan struct{}), make(chan struct{})
	fake.onType = func() { close(entered); <-release }
	done := make(chan error, 1)
	go func() { done <- a.dispatchTerminal(context.Background(), s, "scope-terminal") }()
	<-entered
	type response struct {
		out wakemute.Result
		err error
	}
	ack := make(chan response, 1)
	go func() { out, err := wakemute.Mute(fixture.Context(t), approved); ack <- response{out, err} }()
	select {
	case <-ack:
		t.Fatal("mute ACK split type and Enter")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	result := <-ack
	if result.err != nil || result.out.Quiesced || fake.types != 1 || fake.enters != 1 {
		t.Fatalf("race failed: %v", result.err)
	}
	types, enters := fake.types, fake.enters
	muteDispatch(t, a, s)
	if fake.types != types || fake.enters != enters {
		t.Fatal("terminal admitted after mute ACK")
	}
	fixture.Unmute(t, *result.out.Operation)
}

// An active mute prevents each real dispatch cycle from entering the terminal
// driver. After exact cleanup, new observations allow the adjacent valid wake.
func TestScopeMuteTerminalActiveGate(t *testing.T) {
	e, a, s, fake, now := muteTerminalWorld(t)
	scope := fixture.Scope(e.dir)
	muteDispatch(t, a, s)
	*now = now.Add(2 * time.Second)
	operation := fixture.Mute(t, scope, "terminal-active")
	for i := 0; i < 4; i++ {
		muteDispatch(t, a, s)
		*now = now.Add(2 * time.Second)
	}
	if fake.types != 0 || fake.enters != 0 {
		t.Fatal("terminal admitted while muted")
	}
	fixture.Unmute(t, operation)
	for i := 0; i < 3; i++ {
		muteDispatch(t, a, s)
		*now = now.Add(2 * time.Second)
	}
	if fake.types != 1 || fake.enters != 1 {
		t.Fatal("post-thaw terminal positive refused")
	}
}
