package config

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

type BindPartOpenOptions struct {
	Holder string
	Now    time.Time
	Dead   func(holder string) bool
}

type BindOperationOwner struct {
	Holder string
	Host   string
	PID    int
	Birth  string
}

type BindCursorReservation struct {
	Token    string
	Revision uint64
	Cursor   string
}

type BindControlExpectation struct {
	Revision   uint64
	Generation string
	SessionID  string
}

type BindControlTarget struct {
	Generation string
	SessionID  string
	Stopped    bool
	Kind       string
}

type BindControlProof struct {
	OwnerPID       int
	OwnerBirth     string
	Target         tmuxwake.Target
	TargetVerified bool
	OwnerGone      bool
}

type BindOwnStatusResult struct {
	Token       string
	Status      *AgentStatus
	Published   bool
	Uncertain   bool
	Publication string
	Retry       *NoticeRetry
}

type BindLeaseTransfer struct {
	OldHolder string
	NewHolder string
	Host      string
	PID       int
	Now       time.Time
	TTL       time.Duration
	Dead      func(holder string) bool
}

// NewBindToken returns a 128-bit opaque operation token in canonical hex.
func NewBindToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func BindPartActive(state State) bool {
	return bindPartMarker(state) && state.bindPartRollback == bindPartRollbackOK
}

// BindPartMarked reports the permanent migration tombstone independently of
// rollback mode. Callers must never use it as permission to fall back to State.
func BindPartMarked(state State) bool { return bindPartMarker(state) }

// EnsureBindPart migrates legacy provider fields once. An interrupted
// sidecar-before-marker migration is resumed only when both projections still
// match exactly.
func EnsureBindPart(p Paths, name string, options BindPartOpenOptions) (State, error) {
	if err := ValidateName(name); err != nil {
		return State{}, err
	}
	if options.Now.IsZero() {
		options.Now = time.Now()
	}
	dir := filepath.Dir(p.StateFile(name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return State{}, err
	}
	unlock, err := lockFile(filepath.Join(dir, name+".lock"))
	if err != nil {
		return State{}, err
	}
	defer unlock()

	path := p.StateFile(name)
	sidecar := p.BindPartFile(name)
	_, sidecarErr := os.Lstat(sidecar)
	sidecarExists := sidecarErr == nil
	if sidecarErr != nil && !errors.Is(sidecarErr, fs.ErrNotExist) {
		return State{}, sidecarErr
	}
	var old State
	if sidecarExists {
		old, err = LoadState(p, name)
		if errors.Is(err, ErrBindPartUnmarked) {
			old, err = loadLegacyStateForBindMigration(p, name)
		}
	} else {
		old, err = LoadState(p, name)
	}
	if err != nil {
		return State{}, err
	}
	if bindPartMarker(old) {
		if old.bindPartRollback != bindPartRollbackOK {
			return State{}, ErrBindPartOperation
		}
		return old, nil
	}
	legacy := bindProjectionFromState(old)
	var part bindPartDisk
	if sidecarExists {
		part, _, err = readBindPart(p, name)
		if err != nil {
			return State{}, err
		}
		if part.Rollback != bindPartRollbackOK || !reflect.DeepEqual(legacy, part.LegacyShadow) || !reflect.DeepEqual(legacy, part.projection()) {
			return State{}, fmt.Errorf("%w: interrupted migration projection mismatch", ErrBindPartInvalid)
		}
	} else {
		part = bindPartDisk{LegacyShadow: cloneBindPartProjection(legacy), Rollback: bindPartRollbackOK}
		part.Lease = cloneBindPartLease(legacy.Lease)
		part.NextBatch = legacy.NextBatch
		part.Generation = legacy.Generation
		part.SessionID = legacy.SessionID
		part.Stopped = legacy.Stopped
		part.OwnNotice = cloneBindPartNotice(legacy.OwnNotice)
		part.NoticePublication = legacy.NoticePublication
	}
	if err := old.RefuseMonitorConsumer(); err != nil {
		return State{}, err
	}
	if old.Migration != nil {
		return State{}, ErrIdentityMigrated
	}
	if lease := old.Lease; lease != nil && options.Now.Before(lease.Expires) && lease.Holder != options.Holder {
		if options.Dead == nil || !options.Dead(lease.Holder) {
			return State{}, &LeaseHeldError{Name: name, Lease: *lease}
		}
	}
	if !sidecarExists {
		if err := writeBindPart(p, name, part); err != nil {
			return State{}, err
		}
	}

	working := old
	working.opaque = cloneRawMap(old.opaque)
	working.rawState = cloneBytes(old.rawState)
	if working.opaque == nil {
		working.opaque = make(map[string]json.RawMessage)
	}
	working.opaque[bindPartMarkerKey] = json.RawMessage("true")
	fence, sealed := ordinaryWriterFenceSealed(old, options.Now)
	working.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
	if _, err := persistStateLocked(p, name, old, working, fence, false, false, sealed); err != nil {
		return State{}, err
	}
	if p.readCache != nil {
		p.readCache.invalidateState(name, path)
	}
	merged, err := LoadState(p, name)
	if err != nil {
		return State{}, err
	}
	if !bindPartMarker(merged) || merged.bindPartShadow == nil {
		return State{}, ErrBindPartWrite
	}
	return merged, nil
}

func loadLegacyStateForBindMigration(p Paths, name string) (State, error) {
	path := p.StateFile(name)
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return State{}, nil
		}
		return State{}, err
	}
	data, err := readStateFile(path)
	if err != nil {
		return State{}, err
	}
	marked, err := bindPartMarkerInInput(data)
	if err != nil {
		return State{}, err
	}
	if marked {
		return State{}, ErrBindPartUnmarked
	}
	state, err := decodeFreshState(p, name, path, data)
	if err != nil {
		return State{}, err
	}
	if err := state.validateHostNotifications(); err != nil {
		return State{}, err
	}
	return state, nil
}

func bindWorkingCopy(p Paths, name string, state State) (State, error) {
	working := state
	if len(state.rawState) != 0 {
		var err error
		working, err = writerWorkingCopy(p, name, &state)
		if err != nil {
			return State{}, err
		}
	}
	working.opaque = cloneRawMap(state.opaque)
	working.rawState = cloneBytes(state.rawState)
	if bindPartMarker(state) {
		applyBindProjection(&working, bindProjectionFromState(state))
		if state.bindPartShadow != nil {
			shadow := cloneBindPartProjection(*state.bindPartShadow)
			working.bindPartShadow = &shadow
		}
		working.bindPartStamp = state.bindPartStamp
		working.bindPartRollback = state.bindPartRollback
	}
	return working, nil
}

func OpenBindPart(p Paths, name string, options BindPartOpenOptions) (BindPartView, error) {
	state, err := EnsureBindPart(p, name, options)
	if err != nil {
		return BindPartView{}, err
	}
	return ReadBindPartView(p, name, state)
}

func ReadBindPartView(p Paths, name string, state State) (BindPartView, error) {
	if !bindPartMarker(state) {
		return BindPartView{}, ErrBindPartUnmarked
	}
	part, _, err := readBindPart(p, name)
	if err != nil {
		return BindPartView{}, err
	}
	return bindPartViewFromDisk(part), nil
}

func bindPartViewFromDisk(part bindPartDisk) BindPartView {
	var lease *Lease
	if part.Lease != nil {
		lease = &Lease{Holder: part.Lease.Holder, Host: part.Lease.Host, PID: part.Lease.PID, Expires: part.Lease.Expires}
	}
	notice := OwnNoticeObservation{PossiblyOrphaned: part.OwnNotice.PossiblyOrphaned, Reason: part.OwnNotice.Reason, Since: part.OwnNotice.Since, ObservedAt: part.OwnNotice.ObservedAt}
	return BindPartView{
		Revision: part.Revision, Lease: lease, NextBatch: part.NextBatch,
		Generation: part.Generation, SessionID: part.SessionID, Stopped: part.Stopped,
		OwnNotice: cloneOwnNotice(notice), NoticePublication: part.NoticePublication,
	}
}

// RefreshBindPartLegacy forces a full existing-reader pass while holding the
// State lock. An unchanged marked State stamp can be reused by ordinary reads.
func RefreshBindPartLegacy(p Paths, name string) (State, error) {
	if p.readCache != nil {
		p.readCache.invalidateState(name, p.StateFile(name))
	}
	return withStateLock(p, name, lockFile, func(state State) (State, error) {
		if !bindPartMarker(state) {
			return state, ErrBindPartUnmarked
		}
		return state, nil
	})
}

func loadRawStateForBindRollback(p Paths, name string) (State, error) {
	path := p.StateFile(name)
	if err := checkSecretFile(path); err != nil {
		return State{}, err
	}
	data, err := readStateFile(path)
	if err != nil {
		return State{}, err
	}
	marked, err := bindPartMarkerInInput(data)
	if err != nil || !marked {
		return State{}, ErrBindPartUnmarked
	}
	state, err := decodeFreshState(p, name, path, data)
	if err != nil {
		return State{}, err
	}
	if err := state.validateHostNotifications(); err != nil {
		return State{}, err
	}
	return state, nil
}

func withBindRollbackLock(p Paths, name string, fn func(State, *bindPartDisk, bindPartProjection) error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	dir := filepath.Dir(p.StateFile(name))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	unlock, err := lockFile(filepath.Join(dir, name+".lock"))
	if err != nil {
		return err
	}
	defer unlock()
	state, err := loadRawStateForBindRollback(p, name)
	if err != nil {
		return err
	}
	part, _, err := readBindPart(p, name)
	if err != nil {
		return err
	}
	projection := bindProjectionFromState(state)
	switch part.Rollback {
	case bindPartRollbackOK:
		if !reflect.DeepEqual(projection, part.LegacyShadow) {
			return ErrBindPartInvalid
		}
	case bindPartRollbackPre:
		if !reflect.DeepEqual(projection, part.LegacyShadow) && !reflect.DeepEqual(projection, part.projection()) {
			return ErrBindPartInvalid
		}
	case bindPartRollbackOut:
		if err := validateBindProjection(projection); err != nil {
			return err
		}
	default:
		return ErrBindPartInvalid
	}
	return fn(state, &part, projection)
}

func persistBindRollbackState(p Paths, name string, old, next State, now time.Time) error {
	guardOld := old
	guardOld.opaque = cloneRawMap(old.opaque)
	delete(guardOld.opaque, bindPartMarkerKey)
	fence, sealed := ordinaryWriterFenceSealed(old, now)
	next.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
	_, err := persistStateLocked(p, name, guardOld, next, fence, false, false, sealed)
	if p.readCache != nil {
		p.readCache.invalidateState(name, p.StateFile(name))
	}
	return err
}

// ExportBindPartForRollback stages the permanent provider projection into the
// ordinary sealed State document before a legacy reader is admitted. The
// caller must hold the existing identity consumer exclusion for the handback.
func ExportBindPartForRollback(p Paths, name string, now time.Time) error {
	return withBindRollbackLock(p, name, func(state State, part *bindPartDisk, projection bindPartProjection) error {
		if err := state.RefuseMonitorConsumer(); err != nil {
			return err
		}
		if part.CursorOp != nil || part.StatusOp != nil || part.ControlTxn != nil {
			return ErrBindPartOperation
		}
		if part.Rollback == bindPartRollbackOut {
			return nil
		}
		if part.Rollback == bindPartRollbackOK {
			part.Rollback = bindPartRollbackPre
			if err := commitBindPart(p, name, *part); err != nil {
				return err
			}
			stored, _, err := readBindPart(p, name)
			if err != nil {
				return err
			}
			*part = stored
		}
		if !reflect.DeepEqual(projection, part.projection()) {
			working := state
			working.opaque = cloneRawMap(state.opaque)
			working.rawState = cloneBytes(state.rawState)
			applyBindProjection(&working, part.projection())
			if err := persistBindRollbackState(p, name, state, working, now); err != nil {
				return err
			}
			state, err := loadRawStateForBindRollback(p, name)
			if err != nil {
				return err
			}
			projection = bindProjectionFromState(state)
		}
		if !reflect.DeepEqual(projection, part.projection()) {
			return fmt.Errorf("%w: rollback export readback mismatch", ErrBindPartWrite)
		}
		part.Rollback = bindPartRollbackOut
		return commitBindPart(p, name, *part)
	})
}

// ImportBindPartAfterRollback adopts a valid legacy State projection after
// handback, then returns the permanent marker to normal provider operation.
func ImportBindPartAfterRollback(p Paths, name string) error {
	err := withBindRollbackLock(p, name, func(state State, part *bindPartDisk, projection bindPartProjection) error {
		if part.Rollback != bindPartRollbackOut || part.CursorOp != nil || part.StatusOp != nil || part.ControlTxn != nil {
			return ErrBindPartOperation
		}
		if err := state.RefuseMonitorConsumer(); err != nil {
			return err
		}
		if err := validateBindProjection(projection); err != nil {
			return err
		}
		setBindPartProjection(part, projection)
		part.LegacyShadow = cloneBindPartProjection(projection)
		part.Rollback = bindPartRollbackOK
		if err := commitBindPart(p, name, *part); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	state, err := LoadState(p, name)
	if err != nil {
		return err
	}
	if !bindPartMarker(state) || state.bindPartRollback != bindPartRollbackOK {
		return ErrBindPartInvalid
	}
	return nil
}

func setBindPartProjection(part *bindPartDisk, projection bindPartProjection) {
	part.Lease = cloneBindPartLease(projection.Lease)
	part.NextBatch = projection.NextBatch
	part.Generation = projection.Generation
	part.SessionID = projection.SessionID
	part.Stopped = projection.Stopped
	part.OwnNotice = cloneBindPartNotice(projection.OwnNotice)
	part.NoticePublication = projection.NoticePublication
}

func cloneLease(lease *Lease) *Lease {
	if lease == nil {
		return nil
	}
	copy := *lease
	return &copy
}

func (s State) TerminalOwnNotice() OwnNoticeObservation {
	if s.Terminal == nil {
		return OwnNoticeObservation{}
	}
	return cloneOwnNotice(s.Terminal.OwnNotice)
}

func withBindPartLock(p Paths, name string, fn func(State, *bindPartDisk) error) (State, error) {
	return withBindPartOperationLock(p, name, true, fn)
}

func withBindPartOperationLock(p Paths, name string, rejectPending bool, fn func(State, *bindPartDisk) error) (State, error) {
	return withBindPartOperationLockBy(p, name, rejectPending, lockFile, fn)
}

func withBindPartOperationLockBy(p Paths, name string, rejectPending bool, lock func(string) (func(), error), fn func(State, *bindPartDisk) error) (State, error) {
	return withStateLock(p, name, lock, func(state State) (State, error) {
		if !bindPartMarker(state) {
			return state, ErrBindPartUnmarked
		}
		if state.bindPartRollback != bindPartRollbackOK {
			return state, ErrBindPartOperation
		}
		part, _, err := readBindPart(p, name)
		if err != nil {
			return state, err
		}
		if rejectPending && (part.CursorOp != nil || part.StatusOp != nil || part.ControlTxn != nil) {
			return state, ErrBindPartOperation
		}
		before := cloneBindPartDisk(part)
		if err := fn(state, &part); err != nil {
			return state, err
		}
		if !reflect.DeepEqual(before, part) {
			if err := commitBindPart(p, name, part); err != nil {
				return state, err
			}
		}
		return state, nil
	})
}

func cloneBindPartDisk(part bindPartDisk) bindPartDisk {
	part.Lease = cloneBindPartLease(part.Lease)
	part.OwnNotice = cloneBindPartNotice(part.OwnNotice)
	part.LegacyShadow = cloneBindPartProjection(part.LegacyShadow)
	if part.CursorOp != nil {
		v := *part.CursorOp
		part.CursorOp = &v
	}
	if part.StatusOp != nil {
		v := *part.StatusOp
		v.Notice = cloneBindPartNotice(v.Notice)
		part.StatusOp = &v
	}
	if part.ControlTxn != nil {
		v := *part.ControlTxn
		v.Lease = cloneBindPartLease(v.Lease)
		v.OwnNotice = cloneBindPartNotice(v.OwnNotice)
		part.ControlTxn = &v
	}
	return part
}

func commitBindPart(p Paths, name string, part bindPartDisk) error {
	if err := writeBindPart(p, name, part); err != nil {
		return err
	}
	if p.readCache != nil && part.Rollback != bindPartRollbackOK {
		p.readCache.invalidateState(name, p.StateFile(name))
	}
	return nil
}

// AcquireOrRenewBindLease applies the existing State lease predicates to the
// merged provider view. renewBefore lets a steady bind leave a half-life
// margin without rewriting either durable file on every idle tick.
func AcquireOrRenewBindLease(p Paths, name, holder, host string, pid int, now time.Time, ttl, renewBefore time.Duration, dead func(string) bool) (BindPartView, error) {
	return acquireOrRenewBindLeaseWithLock(p, name, holder, host, pid, now, ttl, renewBefore, dead, lockFile)
}

func AcquireOrRenewBindLeaseContext(ctx context.Context, p Paths, name, holder, host string, pid int, now time.Time, ttl, renewBefore time.Duration, dead func(string) bool) (BindPartView, error) {
	return acquireOrRenewBindLeaseWithLock(p, name, holder, host, pid, now, ttl, renewBefore, dead, func(path string) (func(), error) { return lockFileContext(ctx, path) })
}

// AcquireOrRenewBindLeaseCheckedContext runs check against the current merged
// State while holding the same lock used to read and update the bind lease.
// The checked path deliberately skips the cached no-op shortcut.
func AcquireOrRenewBindLeaseCheckedContext(ctx context.Context, p Paths, name, holder, host string, pid int, now time.Time, ttl, renewBefore time.Duration, dead func(string) bool, check func(*State) error) (BindPartView, error) {
	return acquireOrRenewBindLeaseCheckedWithLock(p, name, holder, host, pid, now, ttl, renewBefore, dead, check, func(path string) (func(), error) { return lockFileContext(ctx, path) })
}

func acquireOrRenewBindLeaseWithLock(p Paths, name, holder, host string, pid int, now time.Time, ttl, renewBefore time.Duration, dead func(string) bool, lock func(string) (func(), error)) (BindPartView, error) {
	return acquireOrRenewBindLeaseCheckedWithLock(p, name, holder, host, pid, now, ttl, renewBefore, dead, nil, lock)
}

func acquireOrRenewBindLeaseCheckedWithLock(p Paths, name, holder, host string, pid int, now time.Time, ttl, renewBefore time.Duration, dead func(string) bool, check func(*State) error, lock func(string) (func(), error)) (BindPartView, error) {
	if check == nil {
		if view, handled, err := cachedBindLeaseNoop(p, name, holder, host, now, renewBefore, lock); handled {
			return view, err
		}
	}
	var view BindPartView
	_, err := withBindPartOperationLockBy(p, name, true, lock, func(state State, part *bindPartDisk) error {
		if check != nil {
			if err := check(&state); err != nil {
				return err
			}
		}
		before := cloneBindPartLease(part.Lease)
		if before != nil && before.Holder == holder && before.Host == host && now.Add(renewBefore).Before(before.Expires) {
			view = bindPartViewFromDisk(*part)
			return nil
		}
		working := state
		working.Lease = nil
		if part.Lease != nil {
			working.Lease = &Lease{Holder: part.Lease.Holder, Host: part.Lease.Host, PID: part.Lease.PID, Expires: part.Lease.Expires}
		}
		if err := working.Acquire(name, holder, host, pid, now, ttl, dead); err != nil {
			return err
		}
		if !reflect.DeepEqual(before, working.Lease) {
			if working.Lease == nil {
				part.Lease = nil
			} else {
				part.Lease = &bindPartLease{Holder: working.Lease.Holder, Host: working.Lease.Host, PID: working.Lease.PID, Expires: working.Lease.Expires}
			}
			view = bindPartViewFromDisk(*part)
			// writeBindPart increments the durable revision on successful commit.
			view.Revision++
		} else {
			view = bindPartViewFromDisk(*part)
		}
		return nil
	})
	if err != nil {
		return BindPartView{}, err
	}
	return view, nil
}

// cachedBindLeaseNoop skips copying the large State history only when the
// validated marked State stamp is unchanged and the current sidecar proves
// this call is the existing no-renewal path. Every uncertain or changing case
// falls through to the ordinary locked State reader and lease predicates.
func cachedBindLeaseNoop(p Paths, name, holder, host string, now time.Time, renewBefore time.Duration, lock func(string) (func(), error)) (BindPartView, bool, error) {
	if p.readCache == nil {
		return BindPartView{}, false, nil
	}
	if err := ValidateName(name); err != nil {
		return BindPartView{}, true, err
	}
	path := p.StateFile(name)
	if err := checkSecretFile(path); err != nil {
		return BindPartView{}, false, nil
	}
	state, hit := p.readCache.markedStateGuardHit(name, path, StampFile(path))
	if !hit {
		return BindPartView{}, false, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return BindPartView{}, true, err
	}
	unlock, err := lock(filepath.Join(dir, name+".lock"))
	if err != nil {
		return BindPartView{}, true, err
	}
	defer unlock()
	// Recheck after taking the same lock used by State and sidecar writers. A
	// replacement between the initial stamp and lock acquisition uses fallback.
	state, hit = p.readCache.markedStateGuardHit(name, path, StampFile(path))
	if !hit {
		return BindPartView{}, false, nil
	}
	if err := state.validateHostNotifications(); err != nil {
		return BindPartView{}, true, err
	}
	if !bindPartMarker(state) {
		return BindPartView{}, false, nil
	}
	if state.bindPartRollback != bindPartRollbackOK {
		return BindPartView{}, true, ErrBindPartOperation
	}
	part, sidecarStamp, err := readBindPart(p, name)
	if err != nil {
		return BindPartView{}, true, err
	}
	base, err := verifiedBindPartBase(state, part, sidecarStamp)
	if err != nil {
		return BindPartView{}, true, err
	}
	if base.bindPartRollback != bindPartRollbackOK {
		return BindPartView{}, true, ErrBindPartOperation
	}
	if part.CursorOp != nil || part.StatusOp != nil || part.ControlTxn != nil {
		return BindPartView{}, true, ErrBindPartOperation
	}
	lease := part.Lease
	if lease == nil || lease.Holder != holder || lease.Host != host || !now.Add(renewBefore).Before(lease.Expires) {
		return BindPartView{}, false, nil
	}
	return bindPartViewFromDisk(part), true, nil
}

func ReleaseBindLease(p Paths, name, holder string, force bool, now time.Time, dead func(string) bool) (bool, error) {
	changed := false
	_, err := withBindPartLock(p, name, func(state State, part *bindPartDisk) error {
		working := state
		working.Lease = nil
		if part.Lease != nil {
			working.Lease = &Lease{Holder: part.Lease.Holder, Host: part.Lease.Host, PID: part.Lease.PID, Expires: part.Lease.Expires}
		}
		var releaseErr error
		changed, releaseErr = working.Release(name, holder, force, now, dead)
		if releaseErr != nil || !changed {
			return releaseErr
		}
		part.Lease = nil
		return nil
	})
	return changed, err
}

func RenewBindMonitorLease(p Paths, name string, record Monitor, owner *MonitorOwner, now time.Time) error {
	_, err := withBindPartLock(p, name, func(state State, part *bindPartDisk) error {
		if state.Monitor == nil || !sameMonitor(state.Monitor, &record) || state.MonitorOwner == nil || !sameOwner(state.MonitorOwner, owner) || !state.currentMonitorOwner(now) {
			return ErrMonitorOwner
		}
		if record.Phase == "stopped" || record.SessionID == "" || record.Host == "" {
			return ErrMonitorGeneration
		}
		lease := part.Lease
		if lease == nil || lease.Holder != record.SessionID || lease.Host != record.Host || !now.Before(lease.Expires) {
			return ErrMonitorLease
		}
		expires := now.Add(LeaseTTL).UTC()
		if lease.Expires.Before(expires) {
			lease.Expires = expires
		}
		return nil
	})
	return err
}

func BeginBindCursor(p Paths, name string, token, kind, expectedCursor, generation string, owner BindOperationOwner, dead func(BindOperationOwner) bool) (BindCursorReservation, error) {
	if !lowerHex(token, 32) || !hostRef(owner.Holder, 200) || !hostRef(owner.Host, 128) || owner.PID <= 1 || !hostRef(owner.Birth, 128) || (kind != "bind" && kind != "delivery" && kind != "monitor" && kind != "projector" && kind != "runtime") {
		return BindCursorReservation{}, ErrBindPartInvalid
	}
	var reservation BindCursorReservation
	_, err := withBindPartOperationLock(p, name, false, func(_ State, part *bindPartDisk) error {
		if expectedCursor != part.NextBatch || generation != "" && generation != part.Generation {
			return ErrBindPartStale
		}
		if part.StatusOp != nil || part.ControlTxn != nil {
			return ErrBindPartOperation
		}
		if op := part.CursorOp; op != nil {
			if op.Token == token && op.Kind == kind && op.Holder == owner.Holder && op.Host == owner.Host && op.PID == owner.PID && op.Birth == owner.Birth && op.ExpectedCursor == expectedCursor && op.ExpectedGeneration == generation {
				reservation = BindCursorReservation{Token: token, Revision: part.Revision, Cursor: expectedCursor}
				return nil
			}
			prior := BindOperationOwner{Holder: op.Holder, Host: op.Host, PID: op.PID, Birth: op.Birth}
			if dead == nil || !dead(prior) {
				return ErrBindPartOperation
			}
		}
		part.CursorOp = &bindPartCursorOperation{Token: token, Kind: kind, Holder: owner.Holder, Host: owner.Host, PID: owner.PID, Birth: owner.Birth, ExpectedCursor: expectedCursor, ExpectedGeneration: generation}
		reservation = BindCursorReservation{Token: token, Revision: part.Revision + 1, Cursor: expectedCursor}
		return nil
	})
	return reservation, err
}

func CommitBindCursor(p Paths, name, token, nextCursor string) (BindPartView, error) {
	_, err := withBindPartOperationLock(p, name, false, func(_ State, part *bindPartDisk) error {
		op := part.CursorOp
		if op == nil || op.Token != token || part.NextBatch != op.ExpectedCursor || op.ExpectedGeneration != "" && part.Generation != op.ExpectedGeneration {
			return ErrBindPartStale
		}
		part.NextBatch = nextCursor
		part.CursorOp = nil
		return nil
	})
	if err != nil {
		return BindPartView{}, err
	}
	state, err := LoadState(p, name)
	if err != nil {
		return BindPartView{}, err
	}
	return ReadBindPartView(p, name, state)
}

// TryCommitEmptyBindCursor advances a reserved bind cursor without entering
// delivery persistence when the sync response has no events and both the
// legacy delivery fields and mailbox are empty. The ordinary commit path
// remains responsible for every non-empty or uncertain case.
func TryCommitEmptyBindCursor(p Paths, name, token, nextCursor string, snapshot BindIdleDeliveryState) (BindPartView, bool, error) {
	if !snapshot.Marked || !snapshot.Active || !snapshot.SealValid || !snapshot.stateStamp.Known {
		return BindPartView{}, false, nil
	}
	var view BindPartView
	committed := false
	_, err := withBindPartOperationLock(p, name, false, func(state State, part *bindPartDisk) error {
		op := part.CursorOp
		if op == nil || op.Token != token || part.NextBatch != op.ExpectedCursor || op.ExpectedGeneration != "" && part.Generation != op.ExpectedGeneration {
			return ErrBindPartStale
		}
		if state.Migration != nil {
			return ErrIdentityMigrated
		}
		if !snapshot.stateStamp.Equal(StampFile(p.StateFile(name))) {
			return nil
		}
		if len(state.LegacySubscriptions) != 0 || len(state.LegacyAcked) != 0 || len(state.LegacyPending) != 0 {
			return nil
		}
		mailbox, err := OpenMailbox(p, name)
		if err != nil {
			return err
		}
		if !mailbox.EmptyForIdleBindSync() {
			return nil
		}
		part.NextBatch = nextCursor
		part.CursorOp = nil
		view = bindPartViewFromDisk(*part)
		committed = true
		return nil
	})
	return view, committed, err
}

func AbortBindCursor(p Paths, name, token string) error {
	_, err := withBindPartOperationLock(p, name, false, func(_ State, part *bindPartDisk) error {
		if part.CursorOp == nil || part.CursorOp.Token != token {
			return ErrBindPartStale
		}
		part.CursorOp = nil
		return nil
	})
	return err
}

// RecoverBindCursor discards an interrupted reservation only after the exact
// recorded process owner is proven gone. The cursor itself remains unchanged,
// so the next sync replays the original range through existing event dedup.
func RecoverBindCursor(p Paths, name string, dead func(BindOperationOwner) bool) error {
	_, err := withBindPartOperationLock(p, name, false, func(_ State, part *bindPartDisk) error {
		op := part.CursorOp
		if op == nil {
			return nil
		}
		owner := BindOperationOwner{Holder: op.Holder, Host: op.Host, PID: op.PID, Birth: op.Birth}
		if dead == nil || !dead(owner) {
			return ErrBindPartOperation
		}
		part.CursorOp = nil
		return nil
	})
	return err
}

func RecordBindNotice(p Paths, name string, expectedRevision uint64, generation string, notice OwnNoticeObservation, publication string) (BindPartView, error) {
	if err := validateBindOwnNotice(bindNoticeFromObservation(notice)); err != nil || !validBindNoticePublication(publication) {
		return BindPartView{}, ErrBindPartInvalid
	}
	_, err := withBindPartOperationLock(p, name, false, func(_ State, part *bindPartDisk) error {
		if part.Revision != expectedRevision || part.Generation != generation {
			return ErrBindPartStale
		}
		if part.StatusOp != nil || part.ControlTxn != nil {
			return ErrBindPartOperation
		}
		part.OwnNotice = bindNoticeFromObservation(notice)
		part.NoticePublication = publication
		return nil
	})
	if err != nil {
		return BindPartView{}, err
	}
	state, err := LoadState(p, name)
	if err != nil {
		return BindPartView{}, err
	}
	return ReadBindPartView(p, name, state)
}

func bindNoticeFromObservation(n OwnNoticeObservation) bindPartOwnNotice {
	return cloneBindPartNotice(bindPartOwnNotice{PossiblyOrphaned: n.PossiblyOrphaned, Reason: n.Reason, Since: n.Since, ObservedAt: n.ObservedAt})
}

func SetBindControl(p Paths, name string, expected BindControlExpectation, target BindControlTarget, proof BindControlProof) (BindPartView, error) {
	if target.Kind != "create" && target.Kind != "stop" && target.Kind != "handover" && target.Kind != "recover" {
		return BindPartView{}, ErrBindPartInvalid
	}
	if (target.Generation == "") != (target.SessionID == "") || target.Generation != "" && (!hostRef(target.Generation, 128) || !hostRef(target.SessionID, 200)) {
		return BindPartView{}, ErrBindPartInvalid
	}
	if target.Generation != "" && target.Generation != expected.Generation && !lowerHex(target.Generation, 32) {
		return BindPartView{}, ErrBindPartInvalid
	}
	needsProof := target.Generation != expected.Generation || target.SessionID != expected.SessionID || !target.Stopped
	if needsProof && (!proof.TargetVerified || proof.OwnerPID <= 1 || !hostRef(proof.OwnerBirth, 128)) {
		return BindPartView{}, ErrBindPartStale
	}
	if (expected.Generation == "") != (expected.SessionID == "") || expected.Generation != "" && (!hostRef(expected.Generation, 128) || !hostRef(expected.SessionID, 200)) {
		return BindPartView{}, ErrBindPartStale
	}
	token, err := NewBindToken()
	if err != nil {
		return BindPartView{}, err
	}
	_, err = withBindPartOperationLock(p, name, false, func(state State, part *bindPartDisk) error {
		if part.StatusOp != nil || part.CursorOp != nil {
			return ErrBindPartOperation
		}
		if part.ControlTxn != nil {
			txn := part.ControlTxn
			if txn.ExpectedRevision != expected.Revision || txn.OldGeneration != expected.Generation || txn.OldSessionID != expected.SessionID || txn.Generation != target.Generation || txn.SessionID != target.SessionID || txn.Stopped != target.Stopped || txn.Kind != target.Kind {
				return ErrBindPartOperation
			}
		} else if part.Revision != expected.Revision || part.Generation != expected.Generation || part.SessionID != expected.SessionID {
			return ErrBindPartStale
		}
		if proof.TargetVerified && state.Terminal != nil && expected.Generation != "" {
			if state.Terminal.OwnerPID != proof.OwnerPID || state.Terminal.OwnerBirth != proof.OwnerBirth || !reflect.DeepEqual(state.Terminal.Target, proof.Target) {
				return ErrBindPartStale
			}
		}
		if part.StatusOp != nil {
			return ErrBindPartOperation
		}
		if part.ControlTxn == nil && part.Generation == target.Generation && part.SessionID == target.SessionID && part.Stopped == target.Stopped {
			return nil
		}
		if part.ControlTxn == nil {
			part.ControlTxn = &bindPartControlTxn{Token: token, Kind: target.Kind, ExpectedRevision: part.Revision, OldGeneration: part.Generation, OldSessionID: part.SessionID, Generation: target.Generation, SessionID: target.SessionID, Stopped: target.Stopped, Lease: cloneBindPartLease(part.Lease), OwnNotice: cloneBindPartNotice(part.OwnNotice), NoticePublication: part.NoticePublication}
			if err := writeBindPart(p, name, *part); err != nil {
				return err
			}
			stored, _, err := readBindPart(p, name)
			if err != nil {
				return err
			}
			*part = stored
		}
		txn := part.ControlTxn
		part.Generation = txn.Generation
		part.SessionID = txn.SessionID
		part.Stopped = txn.Stopped
		part.Lease = cloneBindPartLease(txn.Lease)
		part.OwnNotice = cloneBindPartNotice(txn.OwnNotice)
		part.NoticePublication = txn.NoticePublication
		part.ControlTxn = nil
		return nil
	})
	if err != nil {
		return BindPartView{}, err
	}
	state, err := LoadState(p, name)
	if err != nil {
		return BindPartView{}, err
	}
	return ReadBindPartView(p, name, state)
}

func TransferBindSession(p Paths, name string, expected BindControlExpectation, target BindControlTarget, proof BindControlProof) (BindPartView, error) {
	if target.Kind != "handover" {
		return BindPartView{}, ErrBindPartInvalid
	}
	return SetBindControl(p, name, expected, target, proof)
}

func TransferBindSessionAndLease(p Paths, name string, expected BindControlExpectation, target BindControlTarget, transfer BindLeaseTransfer, proof BindControlProof) (BindPartView, error) {
	if target.Kind != "handover" || !hostRef(transfer.OldHolder, 200) || !hostRef(transfer.NewHolder, 200) || !hostRef(transfer.Host, 128) || transfer.PID <= 1 || transfer.TTL <= 0 {
		return BindPartView{}, ErrBindPartInvalid
	}
	if target.Generation != expected.Generation || target.SessionID != transfer.NewHolder {
		return BindPartView{}, ErrBindPartStale
	}
	if !proof.TargetVerified || proof.OwnerPID <= 1 || !hostRef(proof.OwnerBirth, 128) {
		return BindPartView{}, ErrBindPartStale
	}
	token, err := NewBindToken()
	if err != nil {
		return BindPartView{}, err
	}
	_, err = withBindPartOperationLock(p, name, false, func(state State, part *bindPartDisk) error {
		if part.CursorOp != nil || part.StatusOp != nil {
			return ErrBindPartStale
		}
		if txn := part.ControlTxn; txn != nil {
			if txn.ExpectedRevision != expected.Revision || txn.OldGeneration != expected.Generation || txn.OldSessionID != expected.SessionID || txn.Generation != target.Generation || txn.SessionID != target.SessionID || txn.Stopped != target.Stopped || txn.Kind != target.Kind {
				return ErrBindPartOperation
			}
			part.Generation, part.SessionID, part.Stopped = txn.Generation, txn.SessionID, txn.Stopped
			part.Lease = cloneBindPartLease(txn.Lease)
			part.OwnNotice = cloneBindPartNotice(txn.OwnNotice)
			part.NoticePublication = txn.NoticePublication
			part.ControlTxn = nil
			return nil
		}
		if part.Revision != expected.Revision || part.Generation != expected.Generation || part.SessionID != expected.SessionID || part.Stopped != target.Stopped {
			return ErrBindPartStale
		}
		if state.Terminal == nil || state.Terminal.OwnerPID != proof.OwnerPID || state.Terminal.OwnerBirth != proof.OwnerBirth || !reflect.DeepEqual(state.Terminal.Target, proof.Target) {
			return ErrBindPartStale
		}
		working := state
		working.Lease = nil
		if part.Lease != nil {
			working.Lease = &Lease{Holder: part.Lease.Holder, Host: part.Lease.Host, PID: part.Lease.PID, Expires: part.Lease.Expires}
		}
		if working.Lease != nil && working.Lease.Host == transfer.Host && working.Lease.Holder == transfer.OldHolder {
			if _, err := working.Release(name, transfer.OldHolder, false, transfer.Now, transfer.Dead); err != nil {
				return err
			}
		}
		if err := working.Acquire(name, transfer.NewHolder, transfer.Host, transfer.PID, transfer.Now, transfer.TTL, transfer.Dead); err != nil {
			return err
		}
		lease := (*bindPartLease)(nil)
		if working.Lease != nil {
			lease = &bindPartLease{Holder: working.Lease.Holder, Host: working.Lease.Host, PID: working.Lease.PID, Expires: working.Lease.Expires}
		}
		notice := bindPartOwnNotice{}
		publication := part.NoticePublication
		if publication == "published" {
			publication = "unavailable: own notice changed"
		}
		part.ControlTxn = &bindPartControlTxn{Token: token, Kind: target.Kind, ExpectedRevision: part.Revision, OldGeneration: part.Generation, OldSessionID: part.SessionID, Generation: target.Generation, SessionID: target.SessionID, Stopped: target.Stopped, Lease: lease, OwnNotice: notice, NoticePublication: publication}
		if err := writeBindPart(p, name, *part); err != nil {
			return err
		}
		stored, _, err := readBindPart(p, name)
		if err != nil {
			return err
		}
		*part = stored
		part.Generation = part.ControlTxn.Generation
		part.SessionID = part.ControlTxn.SessionID
		part.Stopped = part.ControlTxn.Stopped
		part.Lease = cloneBindPartLease(part.ControlTxn.Lease)
		part.OwnNotice = cloneBindPartNotice(part.ControlTxn.OwnNotice)
		part.NoticePublication = part.ControlTxn.NoticePublication
		part.ControlTxn = nil
		return nil
	})
	if err != nil {
		return BindPartView{}, err
	}
	state, err := LoadState(p, name)
	if err != nil {
		return BindPartView{}, err
	}
	return ReadBindPartView(p, name, state)
}

func BeginBindOwnStatus(p Paths, name, token string, expectedRevision uint64, generation, sessionID string, owner BindOperationOwner, until, now time.Time) error {
	if !lowerHex(token, 32) || !hostRef(generation, 128) || !hostRef(sessionID, 200) || !hostRef(owner.Holder, 200) || !hostRef(owner.Host, 128) || owner.PID <= 1 || !hostRef(owner.Birth, 128) || !until.After(now) {
		return ErrBindPartInvalid
	}
	_, err := withStateLock(p, name, lockFile, func(state State) (State, error) {
		if !bindPartMarker(state) || state.bindPartRollback != bindPartRollbackOK {
			return state, ErrBindPartUnmarked
		}
		part, _, err := readBindPart(p, name)
		if err != nil {
			return state, err
		}
		if part.Revision != expectedRevision || part.Generation != generation || part.SessionID != sessionID || part.Stopped {
			return state, ErrBindPartStale
		}
		if part.CursorOp != nil || part.StatusOp != nil || part.ControlTxn != nil {
			return state, ErrBindPartOperation
		}
		if state.OwnStatusWrite != nil {
			return state, ErrBindPartOperation
		}
		priorPublication := part.NoticePublication
		part.NoticePublication = ""
		part.StatusOp = &bindPartStatusOperation{Token: token, Generation: generation, SessionID: sessionID, ExpectedRevision: part.Revision + 1, Notice: cloneBindPartNotice(part.OwnNotice), Holder: owner.Holder, Host: owner.Host, PID: owner.PID, Birth: owner.Birth}
		if err := commitBindPart(p, name, part); err != nil {
			return state, err
		}
		working, err := bindWorkingCopy(p, name, state)
		if err != nil {
			return state, err
		}
		working.OwnStatusWrite = &OwnStatusWrite{Token: token, Until: until}
		working.NoticeWriteUncertain = true
		fence, sealed := ordinaryWriterFenceSealed(state, now)
		working.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
		saved, err := persistStateLocked(p, name, state, working, fence, false, false, sealed)
		if err != nil {
			fresh, readErr := LoadState(p, name)
			if readErr == nil && fresh.OwnStatusWrite != nil && fresh.OwnStatusWrite.Token == token && fresh.NoticeWriteUncertain {
				return fresh, nil
			}
			if readErr == nil && (fresh.OwnStatusWrite == nil || fresh.OwnStatusWrite.Token != token) {
				stored, _, partErr := readBindPart(p, name)
				if partErr == nil && stored.StatusOp != nil && stored.StatusOp.Token == token {
					stored.StatusOp = nil
					stored.NoticePublication = priorPublication
					if cleanupErr := commitBindPart(p, name, stored); cleanupErr != nil {
						return state, errors.Join(err, cleanupErr)
					}
				}
			}
			return state, errors.Join(err, readErr)
		}
		return saved, nil
	})
	return err
}

// RecoverBindOwnStatus clears only a status reservation whose recorded process
// owner is proven gone. It preserves uncertainty and never infers that an
// interrupted PUT was published.
func RecoverBindOwnStatus(p Paths, name string, dead func(BindOperationOwner) bool, now time.Time) error {
	_, err := withStateLock(p, name, lockFile, func(state State) (State, error) {
		if !bindPartMarker(state) || state.bindPartRollback != bindPartRollbackOK {
			return state, ErrBindPartOperation
		}
		part, _, err := readBindPart(p, name)
		if err != nil {
			return state, err
		}
		op := part.StatusOp
		if op == nil {
			return state, nil
		}
		owner := BindOperationOwner{Holder: op.Holder, Host: op.Host, PID: op.PID, Birth: op.Birth}
		if state.OwnStatusWrite != nil && state.OwnStatusWrite.Token != op.Token {
			return state, ErrBindPartOperation
		}
		// A missing State token means the request already crossed its local
		// finalization boundary; it is safe to clear only as unavailable. A
		// present token can be released only after its process is proven gone.
		if state.OwnStatusWrite != nil && (dead == nil || !dead(owner)) {
			return state, ErrBindPartOperation
		}
		working, err := bindWorkingCopy(p, name, state)
		if err != nil {
			return state, err
		}
		working.OwnStatusWrite = nil
		working.NoticeWriteUncertain = true
		fence, sealed := ordinaryWriterFenceSealed(state, now)
		working.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
		if _, err := persistStateLocked(p, name, state, working, fence, false, false, sealed); err != nil {
			return state, err
		}
		part.StatusOp = nil
		part.NoticePublication = "unavailable: own liveness publication failed"
		if err := commitBindPart(p, name, part); err != nil {
			return state, err
		}
		return state, nil
	})
	return err
}

// AbortBindOwnStatus is the exact token owner's conservative cleanup after a
// failed post-PUT finalizer. It never records success and keeps uncertainty.
func AbortBindOwnStatus(p Paths, name, token string, now time.Time) error {
	if !lowerHex(token, 32) {
		return ErrBindPartInvalid
	}
	_, err := withStateLock(p, name, lockFile, func(state State) (State, error) {
		if !bindPartMarker(state) || state.bindPartRollback != bindPartRollbackOK {
			return state, ErrBindPartOperation
		}
		part, _, err := readBindPart(p, name)
		if err != nil {
			return state, err
		}
		if part.StatusOp == nil || part.StatusOp.Token != token || state.OwnStatusWrite != nil && state.OwnStatusWrite.Token != token {
			return state, ErrBindPartStale
		}
		working, err := bindWorkingCopy(p, name, state)
		if err != nil {
			return state, err
		}
		working.OwnStatusWrite = nil
		working.NoticeWriteUncertain = true
		fence, sealed := ordinaryWriterFenceSealed(state, now)
		working.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
		if _, err := persistStateLocked(p, name, state, working, fence, false, false, sealed); err != nil {
			return state, err
		}
		part.StatusOp = nil
		part.NoticePublication = "unavailable: own liveness publication failed"
		if err := commitBindPart(p, name, part); err != nil {
			return state, err
		}
		return state, nil
	})
	return err
}

func FinishBindOwnStatus(p Paths, name string, result BindOwnStatusResult, now time.Time) error {
	if !lowerHex(result.Token, 32) || !validBindNoticePublication(result.Publication) || result.Published && (result.Publication != "published" || result.Status == nil) || !result.Published && result.Publication == "published" {
		return ErrBindPartInvalid
	}
	_, err := withStateLock(p, name, lockFile, func(state State) (State, error) {
		if !bindPartMarker(state) || state.bindPartRollback != bindPartRollbackOK {
			return state, ErrBindPartUnmarked
		}
		part, _, err := readBindPart(p, name)
		if err != nil {
			return state, err
		}
		op := part.StatusOp
		if op == nil || op.Token != result.Token || op.Generation != part.Generation || op.SessionID != part.SessionID || op.ExpectedRevision != part.Revision || state.OwnStatusWrite == nil || state.OwnStatusWrite.Token != result.Token {
			return state, ErrBindPartStale
		}
		if result.Published && (validateBindOwnNotice(bindNoticeFromObservation(result.Status.OwnNotice)) != nil || !reflect.DeepEqual(op.Notice, bindNoticeFromObservation(result.Status.OwnNotice))) {
			return state, ErrBindPartStale
		}
		working, err := bindWorkingCopy(p, name, state)
		if err != nil {
			return state, err
		}
		if result.Published {
			status := *result.Status
			if status.State == "unknown" {
				working.NoticeStatus = &status
			} else {
				working.Liveness = &status
				working.NoticeStatus = nil
			}
			working.NoticeRetry = nil
		} else if result.Retry != nil {
			retry := *result.Retry
			working.NoticeRetry = &retry
		}
		working.OwnStatusWrite = nil
		working.NoticeWriteUncertain = result.Uncertain
		fence, sealed := ordinaryWriterFenceSealed(state, now)
		working.Writer = &StateWriter{Protocol: stateProtocol, ForeignSeen: fence}
		if _, err := persistStateLocked(p, name, state, working, fence, false, false, sealed); err != nil {
			return state, err
		}
		part.StatusOp = nil
		part.NoticePublication = result.Publication
		if err := commitBindPart(p, name, part); err != nil {
			return state, err
		}
		return state, nil
	})
	return err
}
