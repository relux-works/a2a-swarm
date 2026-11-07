package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Only exec supplies an inherited descriptor. Ordinary starts retain their
// immediate credential refusal; resumed images adopt the lock before secrets.
func (a *App) withBoundChildSession(ctx context.Context, generation string, inherited int, run func(*session, config.TerminalBinding, *os.File) error) error {
	var s *session
	var err error
	if inherited != 0 {
		s, err = a.sessionMetadata()
	} else {
		s, err = a.session()
	}
	if err != nil {
		return err
	}
	if err := refuseHostConsumer(s); err != nil {
		return err
	}
	return config.WithConsumerLock(s.paths, s.name, time.Now().Add(100*time.Millisecond), inherited, func(lock *os.File) error {
		probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
		birth, err := tmuxwake.Birth(probe, os.Getpid())
		cancel()
		if err != nil {
			return err
		}
		delay := time.Duration(0)
		diagnostic := &terminalRefresh{}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var initial config.TerminalBinding
			err := config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
				if err := s.checkConnection(); err != nil {
					return err
				}
				st, err := config.LoadState(s.paths, s.name)
				if err != nil {
					return err
				}
				b := st.Terminal
				if st.HostEnrollment != nil || b == nil || b.Stopped || b.Generation != generation || b.DaemonPID != os.Getpid() || b.DaemonBirth != birth || b.Host != machineLabel() {
					return errors.New("binding stale consumer")
				}
				initial = *b
				return nil
			})
			if err != nil {
				return err
			}
			if inherited == 0 {
				return run(s, initial, lock)
			}
			fresh, err := a.session()
			if err == nil {
				if err := s.checkConnection(); err != nil {
					return err
				}
				diagnostic.recordStatus(fresh, generation, "startup_ready")
				return run(fresh, initial, lock)
			}
			reason := transientCredentialReason(err)
			if reason == "" {
				return err
			}
			// Use the same per-binding exponential policy as consumer sync errors.
			// A corrupt policy cannot turn a refusal into a busy loop.
			if initial.Interval < time.Second || initial.Interval > maxPoll || initial.RetryMax < initial.Interval || initial.RetryMax > 5*time.Minute {
				return errors.New("binding retry policy invalid")
			}
			delay = min(initial.RetryMax, max(initial.Interval, delay*2))
			diagnostic.recordStatus(s, generation, "startup_"+reason)
			// No pane or network access without credentials. Retained notice evidence
			// can still age into suspicion and queue the ordinary stranded alarm.
			if a.observeTerminalNotice(s, generation, initial, "unknown") == nil {
				_ = a.reportStrandedTerminalNoticeMode(ctx, s, generation, false)
			}
			fmt.Fprintf(a.Stderr, "binding consumer unavailable; bounded retry cause=%s\n", reason)
			if a.bindStartupWait != nil {
				if err := a.bindStartupWait(ctx, delay); err != nil {
					return err
				}
			} else {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				case <-timer.C:
				}
			}
		}
	})
}
