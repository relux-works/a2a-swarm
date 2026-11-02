package cli

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

type notificationDeadline struct{ stored, until time.Time }
type notificationClock struct {
	mu        sync.Mutex
	deadlines map[string]notificationDeadline
}

// Apps are also copied for in-process hook routing. Keep the clock behind a
// pointer so those copies share one monotonic cache without copying a mutex.
var notificationInit sync.Mutex

func (a *App) notificationClockState() *notificationClock {
	notificationInit.Lock()
	defer notificationInit.Unlock()
	if a.notification == nil {
		a.notification = &notificationClock{}
	}
	return a.notification
}

// Preserve the monotonic part of Now within a running consumer. Across CLI
// restart only the explicitly documented wall-clock deadline is available.
func (a *App) notificationReady(s *session, n config.Notification, now time.Time) bool {
	clock := a.notificationClockState()
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if clock.deadlines == nil {
		clock.deadlines = map[string]notificationDeadline{}
	}
	key := s.paths.StateFile(s.name)
	d, ok := clock.deadlines[key]
	if !ok || !d.stored.Equal(n.NotBefore) {
		d = notificationDeadline{stored: n.NotBefore, until: now.Add(n.NotBefore.Sub(now))}
		clock.deadlines[key] = d
	}
	return !now.Before(d.until)
}
func (a *App) rememberNotification(s *session, now time.Time) {
	clock := a.notificationClockState()
	clock.mu.Lock()
	defer clock.mu.Unlock()
	if clock.deadlines == nil {
		clock.deadlines = map[string]notificationDeadline{}
	}
	clock.deadlines[s.paths.StateFile(s.name)] = notificationDeadline{stored: now.Add(config.NotificationEvery).UTC(), until: now.Add(config.NotificationEvery)}
}

func notificationLegacy(st *config.State) time.Time {
	var latest time.Time
	add := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	for _, h := range st.HookSessions {
		if h.LastBlock != 0 {
			add(time.Unix(h.LastBlock, 0))
		}
		if len(h.Rung) > 0 && latest.IsZero() {
			latest = time.Unix(1, 0)
		}
	}
	for _, t := range st.Wake.Reservations {
		add(t)
	}
	if st.Wake.Attempt != nil {
		add(st.Wake.Attempt.Started)
	}
	if b := st.Terminal; b != nil {
		add(b.AttemptAt)
		for _, t := range b.Reservations {
			add(t)
		}
	}
	return latest
}

// Caller holds control before state. The selection callback may mutate only
// local attempt/rate state, never perform output. A saved token precedes effects.
func (a *App) reserveNotification(ctx context.Context, s *session, deadline time.Time, path, sessionID, generation string, bypass bool, selectBatch func(*config.State, *config.Mailbox, string) (bool, error)) (string, error) {
	var token string
	var reservedAt time.Time
	_, err := s.lockedDeliver(ctx, deadline, false, func(st *config.State, mb *config.Mailbox) ([]string, error) {
		if st.HostEnrollment != nil {
			return nil, config.ErrHostEnrollment
		}
		now := a.Now()
		if err := st.Notification.Prepare(now, notificationLegacy(st)); err != nil {
			return nil, err
		}
		if !bypass && !a.notificationReady(s, st.Notification, now) {
			if path == "native" {
				st.Wake.Outcome = "wake suppressed: notification cooldown"
			}
			if path == "terminal" && st.Terminal != nil && st.Terminal.Generation == generation {
				st.Terminal.Outcome = "suppressed: notification cooldown"
			}
			return nil, nil
		}
		prior := st.Notification
		candidate, err := st.Notification.Reserve(path, sessionID, generation, now, bypass)
		if err != nil {
			return nil, err
		}
		selected, err := selectBatch(st, mb, candidate)
		if err != nil {
			return nil, err
		}
		if !selected {
			st.Notification = prior
			return nil, nil
		}
		token = candidate
		reservedAt = now
		return nil, nil
	})
	if err == nil && token != "" {
		a.rememberNotification(s, reservedAt)
	}
	if err == nil && token != "" && a.afterNotificationReserve != nil {
		err = a.afterNotificationReserve()
	}
	return token, err
}

// A late channel reconciliation must not complete another path's newer token.
// Its channel-specific acceptance still commits, with common spacing left intact.
func (a *App) completeNotification(s *session, st *config.State, token string) error {
	if token == "" || st.Notification.Reservation == nil || st.Notification.Reservation.Token != token {
		return nil
	}
	now := a.Now()
	if err := st.Notification.Complete(token, now); err != nil {
		return err
	}
	a.rememberNotification(s, now)
	return nil
}

// Live consumer ownership is checked, never inferred from binding metadata.
// Native watch holds consumer for its lifetime; a registration without watch
// does not own trailing delivery. A nonblocking lock probe cannot invert waits.
func (a *App) hasTrailingConsumer(ctx context.Context, s *session, st config.State, sessionID string) bool {
	if st.HooksPaused || manualWakeGate(st) != "" || st.Lease == nil || st.Lease.Holder != sessionID || st.Lease.Host != machineLabel() || !st.Lease.Expires.After(a.Now()) {
		return false
	}
	held := false
	err := config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now(), func() error { return nil })
	if errors.Is(err, config.ErrLockTimeout) {
		held = true
	} else if err != nil {
		return false
	}
	if !held {
		return false
	}
	if b := st.Runtime; b != nil && !b.Retired && b.SessionID == sessionID && b.Host == machineLabel() && b.Gate == "ready" && b.Validate() == nil && b.ConsumerPID > 0 && b.ConsumerGeneration == b.Generation {
		birth, err := ownerBirth(ctx, b.OwnerPID)
		if err != nil || birth != b.OwnerStart {
			return false
		}
		consumer, err := ownerBirth(ctx, b.ConsumerPID)
		return err == nil && consumer != "" && consumer == b.ConsumerStart
	}
	if b := st.Terminal; b != nil && !b.Stopped && b.SessionID == sessionID && ownTerminalTarget(b, b.Target) {
		owner, e1 := bindingProcessBirth(ctx, b.OwnerPID)
		daemon, e2 := bindingProcessBirth(ctx, b.DaemonPID)
		return e1 == nil && e2 == nil && owner == b.OwnerBirth && daemon != "" && daemon == b.DaemonBirth
	}
	return false
}

// A positive deadline can shorten an existing consumer wait. Already-due but
// gated work uses its normal poll/backoff, never a zero-duration busy loop.
func (a *App) notificationWait(st config.State, normal time.Duration) time.Duration {
	if st.Notification.Version == 1 {
		left := st.Notification.NotBefore.Sub(a.Now())
		if left > 0 && left < normal {
			return left
		}
	}
	return normal
}

func (a *App) cancelNotification(ctx context.Context, s *session, deadline time.Time, token string) error {
	_, err := s.lockedDeliver(ctx, deadline, false, func(st *config.State, _ *config.Mailbox) ([]string, error) { return nil, st.Notification.Cancel(token) })
	return err
}

// A closed, read-only projection; persisted path/session/token values never
// reach diagnostics, including when the private ledger is malformed.
type notificationStatus struct {
	State     string     `json:"state"`
	NotBefore *time.Time `json:"not_before,omitempty"`
}

func notificationDiagnostic(n config.Notification, now time.Time) notificationStatus {
	if n.Version == 0 && n.NotBefore.IsZero() && n.Reservation == nil {
		return notificationStatus{State: "uninitialized"}
	}
	validated := n
	if err := validated.Prepare(now, time.Time{}); err != nil {
		return notificationStatus{State: "invalid"}
	}
	state := "ready"
	if now.Before(n.NotBefore) {
		state = "cooldown"
	}
	if n.Reservation != nil {
		state = "uncertain " + state
	}
	due := n.NotBefore.UTC()
	return notificationStatus{State: state, NotBefore: &due}
}

// Only the already-running native watch records this generation-scoped lease.
// A registration or one-off dispatch is never advertised as a trailing owner.
func (a *App) nativeConsumer(ctx context.Context, s *session, generation string, arm bool) error {
	birth := ""
	var err error
	if arm {
		birth, err = ownerBirth(ctx, os.Getpid())
		if err != nil {
			return err
		}
	}
	return config.WithRuntimeLock(s.paths, s.name, "control", time.Now().Add(nativeRPCTimeout), func() error {
		_, err := s.lockedDeliver(ctx, time.Now().Add(nativeRPCTimeout), false, func(st *config.State, _ *config.Mailbox) ([]string, error) {
			b := st.Runtime
			if b == nil || b.Generation != generation {
				return nil, errors.New("runtime stale consumer generation")
			}
			if arm {
				b.ConsumerPID = os.Getpid()
				b.ConsumerStart = birth
				b.ConsumerGeneration = generation
			} else if b.ConsumerPID == os.Getpid() && b.ConsumerGeneration == generation {
				b.ConsumerPID = 0
				b.ConsumerStart = ""
				b.ConsumerGeneration = ""
			}
			return nil, nil
		})
		return err
	})
}
