package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Explicit SessionStart policy: 2s observation, <=30s retry, 30 attempts/hour.
// Hook failure is diagnostic only; ordinary mailbox delivery remains usable.
func (a *App) autoBindHook(ctx context.Context, runtime string, in hookInput, mode bindingMode) error {
	session, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(session.paths, session.name)
	if err != nil {
		return err
	}
	if st.HostEnrollment != nil || st.HooksPaused {
		return nil
	}
	if manualWakeGate(st) != "" || st.Runtime != nil && !st.Runtime.Retired {
		return nil
	}
	if a.Getenv("TMUX") == "" || a.Getenv("TMUX_PANE") == "" {
		return errors.New("auto-bind unavailable: this session has no tmux target")
	}
	if !claudeSessionRE.MatchString(in.SessionID) {
		return errors.New("auto-bind refused: valid hook session required")
	}
	owner, err := hookRuntimeOwner(ctx, runtime)
	if err != nil {
		return err
	}
	clone := *a
	clone.Stdout = io.Discard
	if mode == bindingSessionStart {
		// /clear's trusted hook names the new session before the runtime
		// imports its updated environment. Do not retain the old ID here.
		clone.Getenv = func(k string) string {
			if k == EnvSessionID || k == "CODEX_THREAD_ID" && runtime == "codex" {
				return in.SessionID
			}
			return a.Getenv(k)
		}
	}
	return clone.bindTerminal(ctx, []string{"--runtime", runtime, "--owner-pid", strconv.Itoa(owner), "--session-id", in.SessionID, "--interval", defaultBindInterval.String(), "--retry-max", defaultBindRetryMax.String(), "--max-wakes-per-hour", strconv.Itoa(defaultBindMaxWakes)}, mode)
}

// Prompt/Stop take only state and birth observations for a healthy consumer.
// Rebinding uses the full ancestry/admission checks and keeps explicit unbind.
func (a *App) healHookBinding(ctx context.Context, runtime string, in hookInput) error {
	if a.Getenv("TMUX") == "" || a.Getenv("TMUX_PANE") == "" {
		return nil
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if st.HostEnrollment != nil || st.HooksPaused || manualWakeGate(st) != "" || st.Runtime != nil && !st.Runtime.Retired {
		return nil
	}
	if b := st.Terminal; b != nil {
		if b.Stopped && b.SessionID == in.SessionID && b.Outcome == "unbound" {
			return nil
		}
		if !b.Stopped && b.SessionID == in.SessionID && b.Host == machineLabel() {
			owner, ownerErr := bindingProcessBirth(ctx, b.OwnerPID)
			daemon, daemonErr := bindingProcessBirth(ctx, b.DaemonPID)
			if ownerErr == nil && owner == b.OwnerBirth && daemonErr == nil && daemon != "" && daemon == b.DaemonBirth {
				return nil
			}
		}
	}
	return a.autoBindHook(ctx, runtime, in, bindingRecovery)
}

// Caller holds the control lock and has verified the invoking runtime/pane
// against this exact binding. Only SessionStart can transfer its own lease;
// old prompt/inbox/unbind calls cannot transfer it back. No force release.
func ownTerminalTarget(b *config.TerminalBinding, target tmuxwake.Target) bool {
	return b.Host == machineLabel() && b.OwnerPID == target.OwnerPID && b.OwnerBirth == target.OwnerBirth && b.Target == target
}

func (a *App) handoverTerminalSession(s *session, expected *config.TerminalBinding, target tmuxwake.Target, next string) (config.State, error) {
	return config.UpdateState(s.paths, s.name, func(st *config.State) error {
		b := st.Terminal
		if b == nil || !ownTerminalTarget(b, target) || b.Generation != expected.Generation || b.SessionID != expected.SessionID || b.Host != expected.Host || b.OwnerPID != expected.OwnerPID || b.OwnerBirth != expected.OwnerBirth || b.Target != expected.Target {
			return errors.New("binding changed before session handover")
		}
		if l := st.Lease; l != nil && l.Host == machineLabel() && l.Holder == expected.SessionID {
			if _, err := st.Release(s.name, expected.SessionID, false, a.Now(), nil); err != nil {
				return err
			}
		}
		ttl := config.LeaseTTL
		if a.leaseTTL != 0 {
			ttl = a.leaseTTL
		}
		if err := st.Acquire(s.name, next, machineLabel(), os.Getpid(), a.Now(), ttl, a.leaseDead(s.paths)); err != nil {
			return err
		}
		// A new loaded session is a new incident episode, even when its pane
		// and consumer generation continue. Attempt/rate journals remain; old
		// session observations and report transactions cannot cross this edge.
		b.OwnNotice = config.OwnNoticeObservation{}
		b.NoticeReport = nil
		if b.NoticePublication == "published" {
			b.NoticePublication = "unavailable: own notice changed"
		}
		b.SessionID = next
		b.ClaudeRegistryRoot = ""
		b.Readiness = nil
		b.Idle = nil
		return nil
	})
}

func bindingProcessBirth(ctx context.Context, pid int) (string, error) {
	probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	birth, err := tmuxwake.Birth(probe, pid)
	if err != nil {
		return "", bindingInspectionFailure("birth", err)
	}
	return birth, nil
}

// Never signal a prior consumer. A confirmed-dead/replaced owner permits only
// a bounded wait; reserve one second of the hook budget for the new startup.
func waitPriorBindingConsumer(ctx context.Context, b *config.TerminalBinding) error {
	bound := min(2*b.Interval+2*time.Second, 3*time.Second)
	if d, ok := ctx.Deadline(); ok {
		bound = min(bound, time.Until(d)-time.Second)
	}
	until := time.Now().Add(max(bound, time.Duration(0)))
	for {
		birth, err := bindingProcessBirth(ctx, b.DaemonPID)
		if err == nil && birth != b.DaemonBirth || processGone(b.DaemonPID) {
			return nil
		}
		if err != nil {
			return bindingInspectionFailure("prior-consumer-birth", err)
		}
		remaining := time.Until(until)
		if remaining <= 0 {
			return errors.New("prior bound consumer still stopping")
		}
		timer := time.NewTimer(min(50*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.New("prior bound consumer still stopping")
		case <-timer.C:
		}
	}
}

// The opposite admission direction closes new-version bind/wait races. An old
// binary cannot enforce this, hence bind also inspects live executable images.
func refuseBoundWait(ctx context.Context, s *session) error {
	return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.HostEnrollment != nil {
			return config.ErrHostEnrollment
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.DaemonPID <= 1 {
			return nil
		}
		birth, err := tmuxwake.Birth(ctx, b.DaemonPID)
		if err == nil && birth != "" && birth == b.DaemonBirth {
			return ErrWaitWhileBound
		}
		if !processGone(b.DaemonPID) {
			return bindingInspectionFailure("bound-wait-birth", err)
		}
		return nil
	})
}

// Check before process/socket probes as well as under the final control lock.
// Expiry never authorizes a second consumer or a legacy backend handback.
func refuseHostConsumer(s *session) error {
	return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		if st.HostEnrollment != nil {
			return config.ErrHostEnrollment
		}
		return nil
	})
}
