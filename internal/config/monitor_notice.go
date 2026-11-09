package config

import "time"

// RecordNotice is called only after a successful closed-projection delivery.
// It rechecks the live owner, writer seal and exact generation at the effect;
// refused, failed and stale outputs never become idle-wake evidence.
func (g *MonitorGeneration) RecordNotice(generation string, at time.Time) error {
	if g == nil || at.IsZero() {
		return ErrMonitorGeneration
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.guarded(generation, false, false); err != nil {
		return err
	}
	deadline := time.Now().Add(time.Second)
	return WithRuntimeLock(g.paths, g.name, "control", deadline, func() error {
		_, err := withMonitorStateLock(g.paths, g.name, deadline, func(old State) (State, error) {
			if !validWriterSeal(old) || !old.Writer.ForeignSeen.IsZero() {
				return old, ErrIncompatibleWriter
			}
			if !sameMonitor(old.Monitor, &g.record) {
				return old, ErrMonitorGeneration
			}
			if !g.ownerMatches(old, time.Now()) {
				return old, ErrMonitorOwner
			}
			next := old
			next.Monitor = cloneMonitor(old.Monitor)
			next.Monitor.LastNoticeAt = at.UTC()
			saved, err := saveStateLocked(g.paths, g.name, old, next, time.Time{}, true, false)
			if err == nil {
				g.record = *cloneMonitor(saved.Monitor)
			}
			return old, err
		})
		return err
	})
}
