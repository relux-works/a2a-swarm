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

// Preserve the historical mark after 30 minutes, but keep retrying afterwards.
const startupCredentialRetryLimit = 30 * time.Minute

const startupOwnerPollInterval = 250 * time.Millisecond

var (
	errBindingStaleConsumer          = errors.New("binding stale consumer")
	errStartupCredentialsUnavailable = errors.New("startup credentials unavailable")
)

func (a *App) boundChildSession(inherited int) (*session, error) {
	if inherited != 0 {
		return a.sessionMetadata()
	}
	return a.session()
}

// Only exec supplies an inherited descriptor. Ordinary starts retain their
// immediate credential refusal; resumed images adopt the lock before secrets.
func (a *App) withBoundChildSession(ctx context.Context, generation string, inherited int, run func(*session, config.TerminalBinding, *os.File) error) error {
	s, err := a.boundChildSession(inherited)
	if err != nil {
		return err
	}
	return a.withBoundChildStartup(ctx, s, generation, inherited, run)
}

func (a *App) withBoundChildStartup(ctx context.Context, s *session, generation string, inherited int, run func(*session, config.TerminalBinding, *os.File) error) error {
	if err := refuseHostConsumer(s); err != nil {
		return err
	}
	return config.WithConsumerLock(s.paths, s.name, time.Now().Add(100*time.Millisecond), inherited, func(lock *os.File) error {
		probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
		birth, err := tmuxwake.Birth(probe, os.Getpid())
		cancel()
		if err != nil {
			return errBindingStaleConsumer
		}
		delay := time.Duration(0)
		var firstRefusal time.Time
		degraded := false
		diagnostic := &terminalRefresh{}
		availability := &bindingAvailability{}
		markDegraded := func() {
			if firstRefusal.IsZero() || a.Now().Before(firstRefusal.Add(startupCredentialRetryLimit)) {
				return
			}
			diagnostic.recordStatus(s, generation, "startup_credentials_unavailable")
			degraded = diagnostic.lastStatus == "startup_credentials_unavailable"
		}
		readBinding := func() (config.TerminalBinding, error) {
			var initial config.TerminalBinding
			err := a.withRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
				if err := s.checkConnection(); err != nil {
					return err
				}
				st, err := config.LoadState(s.paths, s.name)
				if err != nil {
					return err
				}
				if err := st.RefuseMonitorConsumer(); err != nil {
					return err
				}
				b := st.Terminal
				if st.HostEnrollment != nil || b == nil || b.Stopped || b.Generation != generation || b.DaemonPID != os.Getpid() || b.DaemonBirth != birth || b.Host != machineLabel() {
					return errBindingStaleConsumer
				}
				initial = *b
				return nil
			})
			return initial, err
		}
		checkOwner := func(b config.TerminalBinding) error {
			return a.checkBoundStartupOwner(ctx, b)
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			markDegraded()
			initial, err := readBinding()
			if err != nil {
				return err
			}
			if inherited == 0 {
				return run(s, initial, lock)
			}
			if err := checkOwner(initial); err != nil {
				return err
			}
			fresh, err := a.session()
			if err == nil {
				// Credential loading can take time. Recheck both the binding and its
				// owner before allowing this resumed image to arm.
				initial, err = readBinding()
				if err != nil {
					return err
				}
				if err := checkOwner(initial); err != nil {
					return err
				}
				if err := s.checkConnection(); err != nil {
					return err
				}
				// Cancellation after credential and connection validation must not
				// publish readiness. The arm transaction records startup_ready with
				// its lease update only after its own final fences pass.
				if err := ctx.Err(); err != nil {
					return err
				}
				// Credential readiness alone is not a successful delivery cycle.
				// runTerminal clears/logs the episode after fresh target and sync checks.
				return run(fresh, initial, lock)
			}
			reason := transientCredentialReason(err)
			if reason == "" {
				return err
			}
			if firstRefusal.IsZero() {
				firstRefusal = a.Now()
			}
			markDegraded()
			// Do not begin another potentially long retry wait after the bound
			// runtime has stopped or the binding has been replaced.
			initial, err = readBinding()
			if err != nil {
				return err
			}
			if err := checkOwner(initial); err != nil {
				return err
			}
			// Use the same per-binding exponential policy as consumer sync errors.
			// A corrupt policy cannot turn a refusal into a busy loop.
			if initial.Interval < time.Second || initial.Interval > maxPoll || initial.RetryMax < initial.Interval || initial.RetryMax > 5*time.Minute {
				return errors.New("binding retry policy invalid")
			}
			delay = min(initial.RetryMax, max(initial.Interval, delay*2))
			if !degraded {
				diagnostic.recordStatus(s, generation, "startup_"+reason)
			}
			// No pane or network access without credentials. Retained notice evidence
			// can still age into suspicion and queue the ordinary stranded alarm.
			if a.observeTerminalNotice(s, generation, initial, "unknown") == nil {
				_ = a.reportStrandedTerminalNoticeMode(ctx, s, generation, false)
			}
			fmt.Fprintf(a.Stderr, "binding consumer unavailable; bounded retry cause=%s\n", reason)
			a.observeBindingConsumer(s, generation, availability, false)
			wait := delay
			if err := waitBoundStartupOwner(ctx, wait, func(waitCtx context.Context, delay time.Duration) error {
				if a.bindStartupWait != nil {
					return a.bindStartupWait(waitCtx, delay)
				}
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-waitCtx.Done():
					return waitCtx.Err()
				case <-timer.C:
					return nil
				}
			}, func(ownerCtx context.Context) error { return a.checkBoundStartupOwner(ownerCtx, initial) }); err != nil {
				return err
			}
		}
	})
}

func waitBoundStartupOwner(ctx context.Context, delay time.Duration, wait func(context.Context, time.Duration) error, checkOwner func(context.Context) error) error {
	waitCtx, cancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(startupOwnerPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return
			case <-ticker.C:
				if checkOwner(waitCtx) != nil {
					cancel()
					return
				}
			}
		}
	}()

	waitErr := wait(waitCtx, delay)
	cancel()
	<-watchDone
	if err := checkOwner(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return waitErr
}

func (a *App) startupOwnerBirth(ctx context.Context, pid int) (string, error) {
	if a.bindStartupOwnerBirth != nil {
		return a.bindStartupOwnerBirth(ctx, pid)
	}
	return bindingProcessBirth(ctx, pid)
}

func (a *App) checkBoundStartupOwner(ctx context.Context, b config.TerminalBinding) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.OwnerPID <= 1 || b.OwnerBirth == "" || processGone(b.OwnerPID) {
		return errBindingStaleConsumer
	}
	ownerBirth, err := a.startupOwnerBirth(ctx, b.OwnerPID)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil && ownerBirth != b.OwnerBirth || processGone(b.OwnerPID) {
		return errBindingStaleConsumer
	}
	if err != nil {
		return err
	}
	return nil
}
