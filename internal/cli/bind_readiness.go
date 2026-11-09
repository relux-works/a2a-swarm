package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

func boundTerminal(b config.TerminalBinding) tmuxwake.BoundTerminal {
	return tmuxwake.BoundTerminal{Target: b.Target, ClaudeRegistryRoot: b.ClaudeRegistryRoot, Generation: b.Generation, SessionID: b.SessionID, NoticePosition: b.NoticePosition}
}
func sameTerminalScope(a, b config.TerminalBinding) bool {
	return a.Generation == b.Generation && a.SessionID == b.SessionID && a.Host == b.Host && a.OwnerPID == b.OwnerPID && a.OwnerBirth == b.OwnerBirth && a.DaemonPID == b.DaemonPID && a.DaemonBirth == b.DaemonBirth && a.Target == b.Target && a.ClaudeRegistryRoot == b.ClaudeRegistryRoot
}
func sameTerminalIdle(a, b *config.TerminalIdle) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// Called only after bind has proved this invocation→owner→pane ancestry.
// The detached consumer never calls this or supplies its ambient home.
func (a *App) ownRegistryRoot(ctx context.Context, target tmuxwake.Target, session string) string {
	if target.Runtime != "claude" {
		return ""
	}
	root := a.Getenv("CLAUDE_CONFIG_DIR")
	if root == "" {
		home := a.Getenv("HOME")
		if home == "" {
			home, _ = os.UserHomeDir()
		}
		root = filepath.Join(home, ".claude")
	}
	probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	return tmuxwake.AssociateRegistryRoot(probe, target, session, root)
}
func (a *App) associateTerminalRoot(s *session, expected config.TerminalBinding, root string) error {
	if expected.ClaudeRegistryRoot == root {
		return nil
	}
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := a.updateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		if st.Terminal == nil || st.Terminal.Stopped || !sameTerminalScope(*st.Terminal, expected) {
			return errors.New("binding changed before root association")
		}
		st.Terminal.ClaudeRegistryRoot = root
		st.Terminal.Readiness = nil
		st.Terminal.Idle = nil
		return nil
	})
	return err
}
func readinessInput(r tmuxwake.ReadinessAssessment) string {
	if r.Input.State == tmuxwake.InputNonempty {
		return "nonempty"
	}
	if r.Eligible() {
		return "empty"
	}
	return "unknown"
}

type terminalReadinessStatus struct {
	UnknownKeys string                 `json:"unknown_keys,omitempty"`
	State       tmuxwake.RuntimeState  `json:"state"`
	Reason      tmuxwake.RuntimeReason `json:"reason"`
	Version     string                 `json:"version,omitempty"`
	Since       time.Time              `json:"since"`
}

func readinessStatus(r tmuxwake.RuntimeAssessment, stored *config.TerminalReadiness, now time.Time) terminalReadinessStatus {
	if !tmuxwake.ValidRuntimeReason(r.Reason) || r.State != tmuxwake.RuntimeStateFor(r.Reason) {
		r.Reason = tmuxwake.RuntimeRegistryUnavailable
	}
	r.State = tmuxwake.RuntimeStateFor(r.Reason)
	r.Version = tmuxwake.SafeRegistryVersion(r.Version)
	out := terminalReadinessStatus{State: r.State, Reason: r.Reason, Version: r.Version, Since: now.UTC(), UnknownKeys: tmuxwake.SafeRegistryUnknownKeys(r.UnknownKeys)}
	if stored != nil && stored.Reason == r.Reason && stored.Version == r.Version && !stored.Since.IsZero() && !stored.Since.After(now) {
		out.Since = stored.Since.UTC()
	}
	return out
}
func (a *App) observeTerminalReadiness(s *session, expected config.TerminalBinding, r tmuxwake.ReadinessAssessment) error {
	now := a.Now()
	projected := readinessStatus(r.Runtime, expected.Readiness, now)
	validSince := func(old *config.TerminalReadiness) bool { return !old.Since.IsZero() && !old.Since.After(now) }
	if old := expected.Readiness; old != nil && old.Reason == projected.Reason && old.Version == projected.Version && old.UnknownKeys == projected.UnknownKeys && validSince(old) {
		return nil
	}
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := a.updateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || !sameTerminalScope(*b, expected) {
			return errors.New("binding changed before readiness observation")
		}
		// An intervening diagnostic writer cannot reset the continuous since.
		if old := b.Readiness; old != nil && old.Reason == projected.Reason && old.Version == projected.Version && old.UnknownKeys == projected.UnknownKeys && validSince(old) {
			return nil
		}
		if old := b.Readiness; old != nil && old.Reason == projected.Reason && old.Version == projected.Version && validSince(old) {
			projected.Since = old.Since
		}
		b.Readiness = &config.TerminalReadiness{Reason: projected.Reason, Version: projected.Version, Since: projected.Since, UnknownKeys: projected.UnknownKeys}
		if !r.Eligible() {
			b.Idle = nil
		}
		return nil
	})
	return err
}

func (a *App) qualifyTerminalIdle(s *session, generation string, b config.TerminalBinding, r tmuxwake.ReadinessAssessment) (bool, error) {
	k := r.Key
	if !r.Eligible() || k.Target != b.Target || k.Generation != b.Generation || k.SessionID != b.SessionID || k.ClaudeRegistryRoot != b.ClaudeRegistryRoot || generation != b.Generation {
		if err := s.checkConnection(); err != nil {
			return false, err
		}
		_, err := a.updateState(s.paths, s.name, func(st *config.State) error {
			if err := s.checkConnection(); err != nil {
				return err
			}
			if st.Terminal == nil || st.Terminal.Stopped || generation != b.Generation || !sameTerminalScope(*st.Terminal, b) || !sameTerminalIdle(st.Terminal.Idle, b.Idle) {
				return errors.New("binding changed before idle refusal")
			}
			st.Terminal.Idle = nil
			st.Terminal.Outcome = string(tmuxwake.SendSuppressedInput)
			return nil
		})
		return false, err
	}
	now := a.Now()
	observation := config.TerminalIdle{At: now, Generation: b.Generation, SessionID: b.SessionID, Host: b.Host, DaemonPID: b.DaemonPID, DaemonBirth: b.DaemonBirth, Target: b.Target, Interval: b.Interval, ClaudeRegistryRoot: k.ClaudeRegistryRoot, RegistrySessionID: k.RegistrySessionID, RegistryVersion: k.RegistryVersion, RegistryStatus: k.RegistryStatus, RegistryStatusAt: k.RegistryStatusAt, ReadinessRoute: k.Route, ComposerRow: k.ComposerRow, ComposerColumn: k.ComposerColumn, PaneWidth: k.PaneWidth}
	if previous := b.Idle; previous != nil {
		identity := *previous
		identity.At = now
		if identity == observation && b.Interval > 0 && !now.Before(previous.At.Add(b.Interval)) {
			return true, nil
		}
	}
	if err := s.checkConnection(); err != nil {
		return false, err
	}
	_, err := a.updateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		if st.Terminal == nil || st.Terminal.Stopped || !sameTerminalScope(*st.Terminal, b) || !sameTerminalIdle(st.Terminal.Idle, b.Idle) {
			return errors.New("binding changed before idle observation")
		}
		st.Terminal.Idle = &observation
		st.Terminal.Outcome = "armed: qualifying idle"
		return nil
	})
	return false, err
}
