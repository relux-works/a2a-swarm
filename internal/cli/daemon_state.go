package cli

import (
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Daemon callback paths share the invocation's cancellation, including native
// hook bookkeeping reached indirectly from a command. Local commands retain
// their existing independent cleanup and lock-deadline semantics.
func (a *App) updateState(p config.Paths, name string, fn func(*config.State) error) (config.State, error) {
	if a != nil && a.wakeContext != nil {
		return config.UpdateStateContext(a.wakeContext, p, name, time.Time{}, fn)
	}
	if a != nil && a.daemonEngine {
		return config.UpdateStateContext(a.ctx, p, name, time.Time{}, fn)
	}
	return config.UpdateState(p, name, fn)
}

func (a *App) updateStateBy(p config.Paths, name string, deadline time.Time, fn func(*config.State) error) (config.State, error) {
	if a != nil && a.wakeContext != nil {
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			// Deadline-now callers intentionally make one nonblocking attempt.
			// Preserve that probe while checking the outer cycle before locking
			// and again before its callback can mutate state.
			if err := a.wakeContext.Err(); err != nil {
				return config.State{}, err
			}
			return config.UpdateStateBy(p, name, deadline, func(st *config.State) error {
				if err := a.wakeContext.Err(); err != nil {
					return err
				}
				return fn(st)
			})
		}
		return config.UpdateStateContext(a.wakeContext, p, name, deadline, fn)
	}
	if a != nil && a.daemonEngine {
		return config.UpdateStateContext(a.ctx, p, name, deadline, fn)
	}
	return config.UpdateStateBy(p, name, deadline, fn)
}

func (a *App) withRuntimeLock(p config.Paths, name, kind string, deadline time.Time, fn func() error) error {
	if a != nil && a.wakeContext != nil {
		return config.WithRuntimeLockContext(a.wakeContext, p, name, kind, deadline, fn)
	}
	if a != nil && a.daemonEngine {
		return config.WithRuntimeLockContext(a.ctx, p, name, kind, deadline, fn)
	}
	return config.WithRuntimeLock(p, name, kind, deadline, fn)
}

func (a *App) openPostJournal(p config.Paths, name string, human, create bool) (*config.PostJournal, error) {
	if a != nil && a.daemonEngine {
		return config.OpenPostJournalContext(a.ctx, p, name, human, create)
	}
	return config.OpenPostJournal(p, name, human, create)
}
