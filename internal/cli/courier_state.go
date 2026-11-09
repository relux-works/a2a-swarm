package cli

import (
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Courier callback paths share the invocation's cancellation, including native
// hook bookkeeping reached indirectly from a command. Local commands retain
// their existing independent cleanup and lock-deadline semantics.
func (a *App) updateState(p config.Paths, name string, fn func(*config.State) error) (config.State, error) {
	if a != nil && a.courierEngine {
		return config.UpdateStateContext(a.ctx, p, name, time.Time{}, fn)
	}
	return config.UpdateState(p, name, fn)
}

func (a *App) updateStateBy(p config.Paths, name string, deadline time.Time, fn func(*config.State) error) (config.State, error) {
	if a != nil && a.courierEngine {
		return config.UpdateStateContext(a.ctx, p, name, deadline, fn)
	}
	return config.UpdateStateBy(p, name, deadline, fn)
}

func (a *App) withRuntimeLock(p config.Paths, name, kind string, deadline time.Time, fn func() error) error {
	if a != nil && a.courierEngine {
		return config.WithRuntimeLockContext(a.ctx, p, name, kind, deadline, fn)
	}
	return config.WithRuntimeLock(p, name, kind, deadline, fn)
}

func (a *App) openPostJournal(p config.Paths, name string, human, create bool) (*config.PostJournal, error) {
	if a != nil && a.courierEngine {
		return config.OpenPostJournalContext(a.ctx, p, name, human, create)
	}
	return config.OpenPostJournal(p, name, human, create)
}
