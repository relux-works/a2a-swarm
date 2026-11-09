package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"
)

var (
	ErrMonitorState       = errors.New("monitor state unavailable")
	ErrMonitorConflict    = errors.New("monitor consumer conflict")
	ErrMonitorGeneration  = errors.New("monitor stale generation")
	ErrIncompatibleWriter = errors.New("incompatible_writer: upgrade every a2a image used by this identity; explicit operator arm required")
	ErrMonitorUnavailable = errors.New("monitor unavailable")
	ErrMonitorOutput      = errors.New("monitor output unavailable")
)

func closedMonitorError(err error) error {
	if err == nil {
		return nil
	}
	for _, closed := range []error{ErrIncompatibleWriter, ErrMonitorConflict, ErrMonitorGeneration, ErrLockTimeout, ErrMonitorOutput, ErrMonitorOwner, ErrMonitorLease} {
		if errors.Is(err, closed) {
			return closed
		}
	}
	if errors.Is(err, ErrMonitorState) {
		return ErrIncompatibleWriter
	}
	return ErrMonitorUnavailable
}

// Monitor is bounded local authority metadata, never peer/model text. Its
// presence is not owner/session admission. That proof belongs to the caller's
// separately admitted association; this leaf supplies only exclusion/fencing.
type Monitor struct {
	Version           int           `json:"version"`
	Kind              string        `json:"kind"`
	Runtime           string        `json:"runtime"`
	Generation        string        `json:"generation"`
	SessionID         string        `json:"session_id"`
	Host              string        `json:"host"`
	OwnerPID          int           `json:"owner_pid"`
	OwnerBirth        string        `json:"owner_birth"`
	ConsumerPID       int           `json:"consumer_pid"`
	ConsumerBirth     string        `json:"consumer_birth"`
	Transport         string        `json:"transport"`
	Phase             string        `json:"phase"`
	HeartbeatEvery    time.Duration `json:"heartbeat_every"`
	HeartbeatAt       time.Time     `json:"heartbeat_at"`
	EndpointRef       string        `json:"endpoint_ref,omitempty"`
	Rung              []string      `json:"rung,omitempty"`
	NotificationToken string        `json:"notification_token,omitempty"`
	AttemptAt         time.Time     `json:"attempt_at,omitzero"`
	LastNoticeAt      time.Time     `json:"last_notice_at,omitzero"`
	LastErrorClass    string        `json:"last_error_class,omitempty"`
}

func (m Monitor) Validate() error {
	if m.Version != 1 || m.Kind != "monitor" || m.Runtime != "muse" || !lowerHex(m.Generation, 32) || !hostRef(m.SessionID, 200) || !hostRef(m.Host, 128) || m.OwnerPID <= 1 || !hostRef(m.OwnerBirth, 128) || m.ConsumerPID <= 1 || !hostRef(m.ConsumerBirth, 128) || m.HeartbeatEvery <= 0 || m.HeartbeatEvery >= LeaseTTL || m.HeartbeatAt.IsZero() || len(m.Rung) > HookRungMax {
		return ErrMonitorState
	}
	switch m.Phase {
	case "starting", "waiting-client", "streaming", "paused", "stopping", "stopped", "error", "unknown":
	default:
		return ErrMonitorState
	}
	switch m.Transport {
	case "stdout":
		if m.EndpointRef != "" {
			return ErrMonitorState
		}
	case "ws":
		if !hostRef(m.EndpointRef, 160) || strings.ContainsAny(m.EndpointRef, "/\\:") || m.EndpointRef == "." || m.EndpointRef == ".." {
			return ErrMonitorState
		}
	default:
		return ErrMonitorState
	}
	for _, k := range m.Rung {
		if !hostRef(k, 512) {
			return ErrMonitorState
		}
	}
	if m.NotificationToken != "" && !hostRef(m.NotificationToken, 128) {
		return ErrMonitorState
	}
	switch m.LastErrorClass {
	case "", "incompatible_writer", "stale_generation", "unavailable", "output":
	default:
		return ErrMonitorState
	}
	return nil
}

func (m *Monitor) UnmarshalJSON(data []byte) error {
	// Closed schema: unknown, duplicate and case-folded duplicate authority
	// members cannot be silently discarded or interpreted differently.
	d := json.NewDecoder(bytes.NewReader(data))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return ErrMonitorState
	}
	var keys []string
	for d.More() {
		t, err := d.Token()
		if err != nil {
			return ErrMonitorState
		}
		key, ok := t.(string)
		if !ok {
			return ErrMonitorState
		}
		for _, prior := range keys {
			if strings.EqualFold(key, prior) {
				return ErrMonitorState
			}
		}
		keys = append(keys, key)
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return ErrMonitorState
		}
	}
	type plain Monitor
	var v plain
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&v) != nil {
		return ErrMonitorState
	}
	*m = Monitor(v)
	return m.Validate()
}

func cloneMonitor(m *Monitor) *Monitor {
	if m == nil {
		return nil
	}
	c := *m
	c.Rung = append([]string(nil), m.Rung...)
	return &c
}
func sameMonitor(a, b *Monitor) bool { return reflect.DeepEqual(a, b) }
func sameMonitorCompanions(a, b State) bool {
	return reflect.DeepEqual(a.Terminal, b.Terminal) && reflect.DeepEqual(a.Runtime, b.Runtime) && reflect.DeepEqual(a.HostEnrollment, b.HostEnrollment) && reflect.DeepEqual(a.Lease, b.Lease)
}

// Every retained phase except a clean explicit stop blocks other consumers.
// Expiry, PID observations, or error/unknown phases never confer handover.
func (s State) RefuseMonitorConsumer() error {
	if s.Monitor != nil && s.Monitor.Phase != "stopped" {
		return ErrMonitorConflict
	}
	return nil
}

type MonitorEntry uint8

const (
	MonitorArm MonitorEntry = iota + 1 // explicit operator action only
	MonitorAutomatic
	MonitorRecovery
)

// MonitorGeneration cannot be manufactured by a raw/terminal/native caller:
// zero values are inert. The lifetime consumer lock and private active handle
// are required at every guarded operation, as is a fresh hook-admitted owner.
// There is no live monitor command until the later notice pipeline is admitted.
type MonitorGeneration struct {
	mu      sync.Mutex
	paths   Paths
	name    string
	record  Monitor
	release func()
	active  bool
	reason  string
	owner   *MonitorOwner
}

// WithMonitorGeneration is the inert L02 admission seam, not owner-association
// proof, a lease acquisition, a listener, service, or live activation command.
// The consumer lock precedes control/state. Failed admission changes no State.
func WithMonitorGeneration(p Paths, name string, deadline time.Time, entry MonitorEntry, record Monitor, run func(*MonitorGeneration) error) (result error) {
	if deadline.IsZero() || run == nil || record.Validate() != nil || record.ConsumerPID != os.Getpid() || record.Phase == "stopped" || entry < MonitorArm || entry > MonitorRecovery {
		return ErrMonitorState
	}
	if record.Transport == "ws" && record.EndpointRef != name+".monitor-"+record.Generation+".endpoint.json" {
		return ErrMonitorState
	}
	release, err := AcquireConsumerLock(p, name, deadline)
	if err != nil {
		return closedMonitorError(err)
	}
	g := &MonitorGeneration{paths: p, name: name, record: record, release: release}
	defer func() {
		if err := g.Close(); result == nil {
			result = err
		}
	}()
	err = WithRuntimeLock(p, name, "control", deadline, func() error {
		_, err := withStateLock(p, name, func(path string) (func(), error) { return lockFileBy(path, deadline) }, func(old State) (State, error) {
			valid := validWriterSeal(old)
			// Missing writer bootstraps only a truly first explicit arm. A
			// retained stopped/error row is history, not a fresh installation.
			first := old.Writer == nil && old.Monitor == nil
			if !valid && !(entry == MonitorArm && first) {
				return old, ErrIncompatibleWriter
			}
			if entry != MonitorArm && old.Writer != nil && !old.Writer.ForeignSeen.IsZero() {
				return old, ErrIncompatibleWriter
			}
			// Invalidation intentionally retains the old row. Once a compatible
			// ordinary writer has durably fenced it, explicit same-scope arm may
			// replace it while holding the now-free lifetime lock. It cannot
			// repair a mismatched seal, take foreign custody, or bypass a live
			// generation (which still owns that lock).
			fencedRearm := entry == MonitorArm && valid && old.Writer != nil && !old.Writer.ForeignSeen.IsZero()
			if old.Monitor != nil && (old.Monitor.Host != record.Host || old.Monitor.SessionID != record.SessionID) {
				return old, ErrMonitorConflict
			}
			if old.HostEnrollment != nil || old.Terminal != nil && !old.Terminal.Stopped || old.Runtime != nil && !old.Runtime.Retired || old.RefuseMonitorConsumer() != nil && !fencedRearm {
				return old, ErrMonitorConflict
			}
			if old.Monitor != nil && old.Monitor.Generation == record.Generation {
				return old, ErrMonitorGeneration
			}
			next := old
			next.Monitor = cloneMonitor(&record)
			saved, err := saveStateLocked(p, name, old, next, time.Time{}, true)
			if err == nil {
				g.record = *cloneMonitor(saved.Monitor)
				if saved.MonitorOwner != nil {
					owner := *saved.MonitorOwner
					g.owner = &owner
				}
			}
			return old, err
		})
		return err
	})
	if err != nil {
		return closedMonitorError(err)
	}
	g.active = true
	return closedMonitorError(run(g))
}

func (g *MonitorGeneration) stopMemory(reason string) {
	g.active = false
	g.reason = reason
	if g.release != nil {
		g.release()
		g.release = nil
	}
}

// guarded rechecks actual disk input without ordinary-writer repair. Invalid
// State is byte-identical on return; only in-memory status/authority is stopped.
// Callers hold g.mu, but no state/control lock while executing sink I/O.
func (g *MonitorGeneration) guarded(generation string, stop bool, renew bool) error {
	if !g.active || g.release == nil || generation != g.record.Generation {
		return ErrMonitorGeneration
	}
	deadline := time.Now().Add(time.Second)
	err := WithRuntimeLock(g.paths, g.name, "control", deadline, func() error {
		_, err := withStateLock(g.paths, g.name, func(path string) (func(), error) { return lockFileBy(path, deadline) }, func(old State) (State, error) {
			if !validWriterSeal(old) || !old.Writer.ForeignSeen.IsZero() {
				return old, ErrIncompatibleWriter
			}
			if old.Monitor == nil || !sameMonitor(old.Monitor, &g.record) {
				return old, ErrMonitorGeneration
			}
			if old.HostEnrollment != nil || old.Terminal != nil && !old.Terminal.Stopped || old.Runtime != nil && !old.Runtime.Retired {
				return old, ErrMonitorConflict
			}
			if !stop {
				now := time.Now()
				if !g.ownerMatches(old, now) {
					return old, ErrMonitorOwner
				}
				if !renew {
					return old, nil
				}
				l := old.Lease
				if l == nil || l.Holder != g.record.SessionID || l.Host != g.record.Host || !now.Before(l.Expires) {
					return old, ErrMonitorLease
				}
				next := old
				lease := *l
				if expiry := now.Add(LeaseTTL).UTC(); lease.Expires.Before(expiry) {
					lease.Expires = expiry
				}
				next.Lease = &lease
				return saveStateLocked(g.paths, g.name, old, next, time.Time{}, true)
			}
			next := old
			next.Monitor = cloneMonitor(old.Monitor)
			next.Monitor.Phase = "stopped"
			return saveStateLocked(g.paths, g.name, old, next, time.Time{}, true)
		})
		return err
	})
	if err != nil {
		err = closedMonitorError(err)
		reason := "unavailable"
		if errors.Is(err, ErrIncompatibleWriter) || errors.Is(err, ErrMonitorState) {
			reason = "incompatible_writer"
			err = ErrIncompatibleWriter
		}
		if errors.Is(err, ErrMonitorGeneration) {
			reason = "stale_generation"
		}
		if errors.Is(err, ErrMonitorOwner) {
			reason = "owner_unknown"
		}
		g.stopMemory(reason)
	}
	return err
}

// CheckRenewal is a read-only prerequisite, not an owner/lease renewal. The
// conditional-renewal transaction rechecks at its own effect boundary.
func (g *MonitorGeneration) CheckRenewal(generation string) error {
	if g == nil {
		return ErrMonitorGeneration
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.guarded(generation, false, false)
}

// Announce admits only this handle's exact generation. The callback is a
// trusted closed-projection sink supplied by later leaves, never peer text.
// Its I/O holds only the in-memory generation mutex, so inbox/prompt state and
// control locks stay available. A foreign writer after this check is observed
// at the next boundary; the checksum grants no atomic external-write promise.
func (g *MonitorGeneration) Announce(generation string, emit func() error) error {
	if g == nil || emit == nil {
		return ErrMonitorGeneration
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := g.guarded(generation, false, false); err != nil {
		return err
	}
	if g.record.Phase != "streaming" {
		return ErrMonitorConflict
	}
	if err := emit(); err != nil {
		g.reason = "output"
		return ErrMonitorOutput
	}
	return nil
}

// Status contains only closed local tokens; it never persists an invalidation.
func (g *MonitorGeneration) Status() (active bool, reason string) {
	if g == nil {
		return false, "unavailable"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		_ = g.guarded(g.record.Generation, false, false)
	}
	return g.active, g.reason
}

// Close normally persists only the matching generation's clean stop. On
// invalidation guarded releases authority and leaves invalid bytes untouched.
func (g *MonitorGeneration) Close() error {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.active {
		if g.release != nil {
			g.release()
			g.release = nil
		}
		return nil
	}
	err := g.guarded(g.record.Generation, true, false)
	if g.active {
		g.stopMemory("")
	}
	return err
}
