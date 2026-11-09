package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"time"
)

var (
	ErrMonitorOwner = errors.New("owner_unknown")
	ErrMonitorLease = errors.New("monitor owner lease unavailable")
	ownerSessionRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

const DefaultOwnerTTL = 12 * time.Hour

// MonitorConfig is local policy. The default is materialized on first admitted
// SessionStart so the freshness limit is explicit in the sealed State.
type MonitorConfig struct {
	OwnerTTL time.Duration `json:"owner_ttl"`
}

func (c *MonitorConfig) UnmarshalJSON(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	v, err := canonicalValue(d)
	m, ok := v.(map[string]any)
	if err != nil || !ok || len(m) != 1 || m["owner_ttl"] == nil {
		return ErrMonitorOwner
	}
	type plain MonitorConfig
	var p plain
	if json.Unmarshal(data, &p) != nil || p.OwnerTTL <= 0 {
		return ErrMonitorOwner
	}
	*c = MonitorConfig(p)
	return nil
}

// MonitorOwner proves only what the local runtime hook declared under this UID.
// It is not process evidence, bind evidence, or authentication beyond the host
// account. Station is reserved for a later authenticated Courier hook path.
type MonitorOwner struct {
	Runtime       string    `json:"runtime"`
	SessionIDHash string    `json:"session_id_hash"`
	FirstSeen     time.Time `json:"first_seen"`
	LastHookAt    time.Time `json:"last_hook_at"`
}

func ownerRuntime(runtime string) bool {
	return runtime == "muse" || runtime == "claude" || runtime == "codex"
}

func ownerHash(session string) string {
	h := sha256.Sum256([]byte(session))
	return hex.EncodeToString(h[:])
}

func (o MonitorOwner) valid() bool {
	return ownerRuntime(o.Runtime) && lowerHex(o.SessionIDHash, 64) && !o.FirstSeen.IsZero() && !o.LastHookAt.Before(o.FirstSeen)
}

func (o *MonitorOwner) UnmarshalJSON(data []byte) error {
	// Reuse the canonical decoder's duplicate-key rejection. Authority keys
	// must have their exact spelling; case-fold aliases are not evidence.
	d := json.NewDecoder(bytes.NewReader(data))
	v, err := canonicalValue(d)
	m, ok := v.(map[string]any)
	if err != nil || !ok || len(m) != 4 {
		return ErrMonitorOwner
	}
	for k := range m {
		switch k {
		case "runtime", "session_id_hash", "first_seen", "last_hook_at":
		default:
			return ErrMonitorOwner
		}
	}
	type plain MonitorOwner
	var p plain
	if json.Unmarshal(data, &p) != nil || !MonitorOwner(p).valid() {
		return ErrMonitorOwner
	}
	*o = MonitorOwner(p)
	return nil
}

func (s State) ownerTTL() time.Duration {
	if s.MonitorConfig == nil {
		return 0
	}
	return s.MonitorConfig.OwnerTTL
}

func (s State) currentMonitorOwner(now time.Time) bool {
	o := s.MonitorOwner
	return o != nil && o.valid() && s.ownerTTL() > 0 && !now.Before(o.LastHookAt) && now.Sub(o.LastHookAt) < s.ownerTTL()
}

func sameOwner(a, b *MonitorOwner) bool { return reflect.DeepEqual(a, b) }

// RecordMonitorOwnerHook is the local hook writer seam. Call only after the
// explicit --as and identity guard, with the stdin session/event (never env or
// bind data). Ordinary State writers cannot alter MonitorOwner. Hook writes
// use L02's control/state locks, seal and sticky ordinary-writer fence; they
// cannot clear a foreign-writer observation or rearm a monitor.
func RecordMonitorOwnerHook(p Paths, name string, deadline time.Time, runtime, session, event string, exit bool, now time.Time) error {
	return recordMonitorOwnerHook(nil, p, name, deadline, runtime, session, event, exit, now)
}

// RecordMonitorOwnerHookContext cancels Courier hook lock acquisition and checks
// cancellation before admitting the owner update. Local hooks keep their
// existing deadline-only contract through RecordMonitorOwnerHook.
func RecordMonitorOwnerHookContext(ctx context.Context, p Paths, name string, deadline time.Time, runtime, session, event string, exit bool, now time.Time) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !deadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, deadline)
		defer cancel()
	}
	return recordMonitorOwnerHook(ctx, p, name, deadline, runtime, session, event, exit, now)
}

func recordMonitorOwnerHook(ctx context.Context, p Paths, name string, deadline time.Time, runtime, session, event string, exit bool, now time.Time) error {
	if deadline.IsZero() || now.IsZero() || !ownerRuntime(runtime) || !ownerSessionRE.MatchString(session) {
		return ErrMonitorOwner
	}
	switch event {
	case "SessionStart", "UserPromptSubmit", "Stop", "SessionEnd":
	default:
		return ErrMonitorOwner
	}
	if exit && event != "Stop" {
		return ErrMonitorOwner
	}
	withControl := func(fn func() error) error {
		if ctx != nil {
			return WithRuntimeLockContext(ctx, p, name, "control", deadline, fn)
		}
		return WithRuntimeLock(p, name, "control", deadline, fn)
	}
	stateLock := func(path string) (func(), error) {
		if ctx == nil {
			return lockFileBy(path, deadline)
		}
		unlock, err := lockFileContext(ctx, path)
		if errors.Is(err, context.DeadlineExceeded) {
			err = ErrLockTimeout
		}
		return unlock, err
	}
	return withControl(func() error {
		_, err := withStateLock(p, name, stateLock, func(old State) (State, error) {
			if old.Migration != nil {
				return old, ErrIdentityMigrated
			}
			if old.HostEnrollment != nil {
				return old, ErrMonitorConflict
			}
			if ctx != nil {
				if err := ctx.Err(); err != nil {
					return old, err
				}
			}
			next := old
			o := old.MonitorOwner
			hash := ownerHash(session)
			if event == "SessionStart" {
				if o != nil && !now.After(o.LastHookAt) {
					// FirstSeen distinguishes same-session restarts for active handles.
					// Refuse ambiguous event times loudly; never report admission
					// while retaining a different or indistinguishable association.
					return old, ErrMonitorOwner
				}
				next.MonitorOwner = &MonitorOwner{Runtime: runtime, SessionIDHash: hash, FirstSeen: now.UTC(), LastHookAt: now.UTC()}
				if next.MonitorConfig == nil {
					next.MonitorConfig = &MonitorConfig{OwnerTTL: DefaultOwnerTTL}
				}
			} else {
				if o == nil || o.Runtime != runtime || o.SessionIDHash != hash {
					return old, ErrMonitorOwner
				}
				if event == "SessionEnd" || exit {
					next.MonitorOwner = nil
				} else {
					if !old.currentMonitorOwner(now) {
						return old, ErrMonitorOwner // only Start re-admits after expiry
					}
					copy := *o
					copy.LastHookAt = now.UTC()
					next.MonitorOwner = &copy
				}
			}
			if ctx != nil {
				if err := ctx.Err(); err != nil {
					return old, err
				}
			}
			return saveStateLocked(p, name, old, next, ordinaryWriterFence(old, now), true, false)
		})
		return err
	})
}

func (s State) monitorOwnerMatches(record Monitor, now time.Time) bool {
	return s.currentMonitorOwner(now) && s.MonitorOwner.Runtime == record.Runtime && s.MonitorOwner.SessionIDHash == ownerHash(record.SessionID)
}

func (g *MonitorGeneration) ownerMatches(s State, now time.Time) bool {
	o := s.MonitorOwner
	return s.monitorOwnerMatches(g.record, now) && g.owner != nil && o.Runtime == g.owner.Runtime && o.SessionIDHash == g.owner.SessionIDHash && o.FirstSeen.Equal(g.owner.FirstSeen)
}

// RenewOwnerLease renews only an unexpired exact owner's existing lease. The
// owner and generation are rechecked in the SAME transaction as the effect,
// so an earlier successful prerequisite cannot authorize a replaced owner.
// It never acquires, takes over, releases or rewrites process metadata.
func (g *MonitorGeneration) RenewOwnerLease(generation string) error {
	if g == nil {
		return ErrMonitorGeneration
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.guarded(generation, false, true)
}
