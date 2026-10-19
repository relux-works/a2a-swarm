package cli

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Explicit SessionStart policy: 2s observation, <=30s retry, 30 attempts/hour.
// Hook failure is diagnostic only; ordinary mailbox delivery remains usable.
func (a *App) autoBindHook(ctx context.Context, runtime string, in hookInput) error {
	session, err := a.session()
	if err != nil {
		return err
	}
	st, err := config.LoadState(session.paths, session.name)
	if err != nil {
		return err
	}
	if st.HooksPaused {
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
	return clone.bindTerminal(ctx, []string{"--runtime", runtime, "--owner-pid", strconv.Itoa(owner), "--session-id", in.SessionID, "--interval", "2s", "--retry-max", "30s", "--max-wakes-per-hour", "30"}, true)
}

// The opposite admission direction closes new-version bind/wait races. An old
// binary cannot enforce this, hence bind also inspects live executable images.
func refuseBoundWait(ctx context.Context, s *session) error {
	return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.DaemonPID <= 1 {
			return nil
		}
		birth, err := tmuxwake.Birth(ctx, b.DaemonPID)
		if err == nil && birth != "" && birth == b.DaemonBirth {
			return ErrBindingParallelWait
		}
		if !processGone(b.DaemonPID) {
			return ErrBindingProcessUnknown
		}
		return nil
	})
}
