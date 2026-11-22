package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// ErrOwnStatusPending is a local refusal, before any preparation or HTTP.
var ErrOwnStatusPending = errors.New("own status publication already pending")

// retryOwnStatus polls local contention without holding a lock. Waiting and
// publication share one RPC budget (or the caller's earlier deadline).
func (s *session) retryOwnStatus(ctx context.Context, prepare func(*config.State) (config.AgentStatus, bool, error), finish func(*config.State, error)) error {
	ctx, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	return retryOwnStatusAttempts(ctx, func(ctx context.Context) (bool, error) {
		prepared := false
		err := s.writeOwnStatus(ctx, func(st *config.State) (config.AgentStatus, bool, error) {
			prepared = true
			return prepare(st)
		}, finish)
		return prepared, err
	})
}

func retryOwnStatusAttempts(ctx context.Context, attempt func(context.Context) (bool, error)) error {
	deadline, hasDeadline := ctx.Deadline()
	pending := false
	for {
		prepared, err := attempt(ctx)
		// The deadline can fall between polls or during state-lock acquisition.
		// The lock or child context can observe it before ctx's timer fires.
		// Preserve a prior pending refusal only if no preparation/HTTP began;
		// an actual publication timeout must retain its own error/uncertainty.
		if pending && !prepared && hasDeadline && !time.Now().Before(deadline) && !errors.Is(ctx.Err(), context.Canceled) && (errors.Is(err, context.DeadlineExceeded) || errors.Is(err, config.ErrLockTimeout)) {
			return ErrOwnStatusPending
		}
		if !errors.Is(err, ErrOwnStatusPending) {
			return err
		}
		pending = true
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		timer.Stop()
		if err := ctx.Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return ErrOwnStatusPending
			}
			return err
		}
	}
}

// writeOwnStatus reserves one bounded attempt under the state lock, performs
// HTTP on a private snapshot, then commits only status knowledge. Mail and
// binding writers can progress throughout the request. A concurrent status
// writer returns ErrOwnStatusPending without changing state: automatic writers
// skip until their next cycle, explicit writers use retryOwnStatus. A crashed
// writer's fence expires at its RPC deadline.
func (s *session) writeOwnStatus(ctx context.Context, prepare func(*config.State) (config.AgentStatus, bool, error), finish func(*config.State, error)) error {
	deadline := time.Now().Add(nativeRPCTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	writeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := writeCtx.Err(); err != nil {
		return err
	}
	current, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	marked := config.BindPartMarked(current)
	if marked {
		if !config.BindPartActive(current) {
			return config.ErrBindPartOperation
		}
		// The sidecar holds a binding's notice and its publication result. A
		// marked identity without a terminal has neither, so its status write
		// changes State's own fields only and takes the ordinary path below.
		if current.Terminal != nil {
			err := s.writeMarkedOwnStatus(writeCtx, deadline, prepare, finish)
			if errors.Is(err, ErrOwnStatusPending) && s.app.afterOwnStatusPending != nil {
				s.app.afterOwnStatusPending()
			}
			return err
		}
	}
	token := matrix.NewTxnID()
	var record config.AgentStatus
	var send, priorUncertain bool
	snapshot, err := config.UpdateStateContext(ctx, s.paths, s.name, deadline, func(st *config.State) error {
		if err := writeCtx.Err(); err != nil {
			return err
		}
		if marked && st.Terminal != nil {
			// A binding appeared since the read above. Its notice belongs to
			// the sidecar path; this attempt changes nothing and is retried.
			return ErrOwnStatusPending
		}
		if st.OwnStatusWrite != nil && time.Now().Before(st.OwnStatusWrite.Until) {
			return ErrOwnStatusPending
		}
		var err error
		record, send, err = prepare(st)
		if err != nil || !send {
			return err
		}
		st.OwnStatusWrite = &config.OwnStatusWrite{Token: token, Until: deadline}
		priorUncertain = st.NoticeWriteUncertain
		st.NoticeWriteUncertain = true
		return nil
	})
	if err != nil || !send {
		if errors.Is(err, ErrOwnStatusPending) && s.app.afterOwnStatusPending != nil {
			s.app.afterOwnStatusPending()
		}
		return err
	}
	// Persist uncertainty before HTTP, while letting pre-send failures and
	// explicit refusals restore this attempt's prior knowledge on completion.
	snapshot.NoticeWriteUncertain = priorUncertain
	previousLiveness, previousNotice := snapshot.Liveness, snapshot.NoticeStatus
	publicationErr := s.putOwnStatus(writeCtx, &snapshot, record)
	// Finalize even after cancellation: otherwise a failed publication can keep
	// its old "published" observation and fence. UpdateStateBy first tries the
	// nonblocking lock even at an expired deadline, so uncontended cleanup is
	// possible without extending the original budget. Any contention is capped
	// by both that budget and the consumer-release bound.
	cleanupDeadline := time.Now().Add(daemon.LockReleaseBound)
	if deadline.Before(cleanupDeadline) {
		cleanupDeadline = deadline
	}
	_, err = config.UpdateStateBy(s.paths, s.name, cleanupDeadline, func(st *config.State) error {
		if st.OwnStatusWrite == nil || st.OwnStatusWrite.Token != token {
			return errors.New("own status publication superseded")
		}
		st.OwnStatusWrite = nil
		st.NoticeWriteUncertain = snapshot.NoticeWriteUncertain
		if publicationErr == nil && reflect.DeepEqual(st.Liveness, previousLiveness) && reflect.DeepEqual(st.NoticeStatus, previousNotice) {
			st.Liveness, st.NoticeStatus = snapshot.Liveness, snapshot.NoticeStatus
			st.NoticeRetry = nil
		} else if publicationErr == nil {
			// A local policy/status edit raced the request. Preserve it and
			// require reconciliation rather than replacing it with old data.
			st.NoticeWriteUncertain = true
			publicationErr = errors.New("own status changed during publication")
		}
		if st.Terminal != nil && snapshot.Terminal != nil && st.Terminal.Generation == snapshot.Terminal.Generation && st.Terminal.SessionID == snapshot.Terminal.SessionID {
			st.Terminal.NoticePublication = "unavailable: own liveness publication failed"
			if publicationErr == nil && !st.NoticeWriteUncertain {
				st.Terminal.NoticePublication = "unavailable: own notice changed"
				if reflect.DeepEqual(record.OwnNotice, st.Terminal.OwnNotice) {
					st.Terminal.NoticePublication = "published"
				}
			}
		}
		if finish != nil {
			finish(st, publicationErr)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return publicationErr
}

// writeMarkedOwnStatus keeps the provider notice/publication pair in its
// sidecar and durably pins both the sidecar operation and State token before
// the status PUT. State uncertainty is overlaid while a sidecar operation is
// pending, so a failed final sidecar write can never report published.
//
// The notice record and the status reservation name the sidecar revision the
// writer read. Another command of the identity moves that revision with every
// cursor it commits and every lease it renews, which changes nothing a status
// publication rests on. A writer overtaken that way, with the binding's
// generation, session and stopped flag as it read them, starts again from the
// State as it is now, before any request is sent. A binding that was replaced
// or stopped meanwhile is still answered as stale.
func (s *session) writeMarkedOwnStatus(ctx context.Context, deadline time.Time, prepare func(*config.State) (config.AgentStatus, bool, error), finish func(*config.State, error)) error {
	// It starts again for as long as the caller's deadline allows, and for at
	// most heldLimit without one, with a pause that grows to markedStatusPause:
	// a writer beside a busy consumer is overtaken several times in a row.
	limit := time.Now().Add(heldLimit)
	if !deadline.IsZero() && deadline.Before(limit) {
		limit = deadline
	}
	for attempt := 1; ; attempt++ {
		err := s.writeMarkedOwnStatusOnce(ctx, deadline, prepare, finish)
		if !errors.Is(err, errMarkedStatusOvertaken) {
			return err
		}
		pause := time.Duration(attempt) * time.Millisecond
		if pause > markedStatusPause {
			pause = markedStatusPause
		}
		if ctx.Err() != nil || !time.Now().Add(pause).Before(limit) {
			return config.ErrBindPartStale
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			return config.ErrBindPartStale
		case <-timer.C:
		}
	}
}

// markedStatusPause is the longest pause between two attempts of a status
// write that another command's sidecar write overtook.
const markedStatusPause = 20 * time.Millisecond

// errMarkedStatusOvertaken is writeMarkedOwnStatusOnce's answer when the
// sidecar's revision moved under it and the binding did not change.
var errMarkedStatusOvertaken = errors.New("own status writer overtaken by another sidecar write")

// overtaken turns a stale answer of a revision-bound sidecar write into
// errMarkedStatusOvertaken when the binding the writer read is still the one
// in the sidecar. Every other error, and a stale answer for a binding that
// changed, is returned as it is.
func (s *session) overtaken(err error, read config.BindPartView) error {
	if !errors.Is(err, config.ErrBindPartStale) {
		return err
	}
	current, loadErr := config.LoadState(s.paths, s.name)
	if loadErr != nil {
		return err
	}
	now, viewErr := config.ReadBindPartView(s.paths, s.name, current)
	if viewErr != nil || now.Generation != read.Generation || now.SessionID != read.SessionID || now.Stopped != read.Stopped {
		return err
	}
	return errMarkedStatusOvertaken
}

func (s *session) writeMarkedOwnStatusOnce(ctx context.Context, deadline time.Time, prepare func(*config.State) (config.AgentStatus, bool, error), finish func(*config.State, error)) error {
	dead := s.app.bindOperationOwnerDead(ctx)
	// The lock waits before the request end at the caller's deadline; those
	// after it end at that deadline too, but are not cut short by a caller
	// that is being stopped, so that a finished request is still recorded.
	lockCtx, cancelLock := boundedBy(ctx, deadline)
	defer cancelLock()
	if err := config.RecoverBindOwnStatusContext(lockCtx, s.paths, s.name, dead, s.app.Now()); err != nil {
		if errors.Is(err, config.ErrBindPartOperation) {
			return ErrOwnStatusPending
		}
		return err
	}
	current, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if !config.BindPartActive(current) {
		return config.ErrBindPartOperation
	}
	view, err := config.ReadBindPartView(s.paths, s.name, current)
	if err != nil {
		return err
	}
	candidate := ownStatusCandidate(current)
	record, send, err := prepare(&candidate)
	if err != nil {
		return err
	}
	if candidate.Terminal != nil && (candidate.Terminal.Generation != view.Generation || candidate.Terminal.SessionID != view.SessionID || candidate.Terminal.Stopped != view.Stopped) {
		return config.ErrBindPartStale
	}
	if candidate.Terminal != nil && (!reflect.DeepEqual(candidate.Terminal.OwnNotice, view.OwnNotice) || candidate.Terminal.NoticePublication != view.NoticePublication) {
		recorded, err := config.RecordBindNoticeContext(lockCtx, s.paths, s.name, view.Revision, view.Generation, candidate.Terminal.OwnNotice, candidate.Terminal.NoticePublication)
		if err != nil {
			return s.overtaken(err, view)
		}
		view = recorded
		if s.app.afterMarkedOwnStatusNoticeRecord != nil {
			s.app.afterMarkedOwnStatusNoticeRecord()
		}
		current, err = config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		view, err = config.ReadBindPartView(s.paths, s.name, current)
		if err != nil {
			return err
		}
	}
	if candidate.Terminal != nil && (candidate.Terminal.Generation != view.Generation || candidate.Terminal.SessionID != view.SessionID || candidate.Terminal.Stopped != view.Stopped) {
		return config.ErrBindPartStale
	}
	if !send {
		if !reflect.DeepEqual(candidate.NoticeRetry, current.NoticeRetry) {
			_, err = config.UpdateStateContext(ctx, s.paths, s.name, deadline, func(st *config.State) error {
				if st.Terminal == nil || st.Terminal.Generation != view.Generation || st.Terminal.SessionID != view.SessionID || st.Terminal.Stopped != view.Stopped {
					return config.ErrBindPartStale
				}
				st.NoticeRetry = candidate.NoticeRetry
				return nil
			})
		}
		return err
	}
	if candidate.Terminal == nil || !reflect.DeepEqual(record.OwnNotice, view.OwnNotice) {
		return config.ErrBindPartStale
	}
	token, err := config.NewBindToken()
	if err != nil {
		return err
	}
	owner, err := s.app.bindOperationOwner(ctx, s, deadline)
	if err != nil {
		return err
	}
	if err := config.BeginBindOwnStatusContext(lockCtx, s.paths, s.name, token, view.Revision, view.Generation, view.SessionID, owner, deadline, s.app.Now()); err != nil {
		if errors.Is(err, config.ErrBindPartOperation) {
			return ErrOwnStatusPending
		}
		return s.overtaken(err, view)
	}

	snapshot := ownStatusCandidate(current)
	snapshot.NoticeWriteUncertain = current.NoticeWriteUncertain
	publicationErr := s.putOwnStatus(ctx, &snapshot, record)
	finished := ownStatusCandidate(snapshot)
	if finish != nil {
		finish(&finished, publicationErr)
	}
	var status *config.AgentStatus
	if publicationErr == nil {
		if record.State == "unknown" {
			status = finished.NoticeStatus
		} else {
			status = finished.Liveness
		}
	}
	publication := "unavailable: own liveness publication failed"
	if finished.Terminal != nil && finished.Terminal.NoticePublication != "" {
		publication = finished.Terminal.NoticePublication
	}
	result := config.BindOwnStatusResult{
		Token: token, Status: status, Published: publicationErr == nil,
		Uncertain: snapshot.NoticeWriteUncertain, Publication: publication,
		Retry: finished.NoticeRetry,
	}
	finishCtx, cancelFinish := releaseContext(deadline)
	defer cancelFinish()
	if err := config.FinishBindOwnStatusContext(finishCtx, s.paths, s.name, result, s.app.Now()); err != nil {
		if abortErr := config.AbortBindOwnStatusContext(finishCtx, s.paths, s.name, token, s.app.Now()); abortErr != nil {
			return errors.Join(err, abortErr)
		}
		return err
	}
	return publicationErr
}

func ownStatusCandidate(state config.State) config.State {
	candidate := state
	if state.Terminal != nil {
		terminal := *state.Terminal
		terminal.OwnNotice.PossiblyOrphaned = cloneBool(state.Terminal.OwnNotice.PossiblyOrphaned)
		candidate.Terminal = &terminal
	}
	if state.NoticeRetry != nil {
		retry := *state.NoticeRetry
		candidate.NoticeRetry = &retry
	}
	return candidate
}

// putOwnStatus performs HTTP and updates a private snapshot, without locks.
// Liveness is the exact last acknowledged record, not a desired record.
// Uncertainty belongs to the identity, not a binding.
// This is not a crash-atomic transaction with Matrix or an external-writer CAS.
func (s *session) putOwnStatus(ctx context.Context, st *config.State, record config.AgentStatus) error {
	name, ok := doorbellName(s.creds.UserID, s.server)
	if !ok || name != s.name {
		return errors.New("notice publication refuses foreign liveness key")
	}
	if err := noticeValid(record.OwnNotice); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Resolve only after the caller decides a real PUT is needed. Room aliases
	// can rotate silently while the old room remains writable, so no room ID
	// survives this call. Resolution shares the caller's original RPC budget.
	// Neither room resolution nor the write may follow redirects. A response
	// from another request cannot establish this identity's status knowledge.
	writer := *s
	writer.client = s.client.ForOwnStatus()
	room, _, err := writer.resolveRoom(ctx, StatusRoom)
	if err != nil {
		if st.Terminal != nil {
			st.Terminal.NoticePublication = "unavailable: own liveness publication failed"
		}
		return fmt.Errorf("liveness: join #status after a2a status init: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// No validation/resolve work that can fail before send follows this point.
	priorUncertain := st.NoticeWriteUncertain
	st.NoticeWriteUncertain = true
	if st.Terminal != nil {
		st.Terminal.NoticePublication = "unavailable: own liveness publication failed"
	}
	_, err = writer.client.PutState(ctx, room, StatusEventType, s.creds.UserID, record)
	var refusal *matrix.Error
	if errors.As(err, &refusal) && refusal.OriginalRequest && refusal.Status == http.StatusForbidden && refusal.ErrCode == "M_FORBIDDEN" {
		// This refusal only rejects THIS attempt; it cannot reconcile a prior
		// possibly-applied write. Retain the exact prior ACK in either case.
		st.NoticeWriteUncertain = priorUncertain
		return fmt.Errorf("liveness: administrator must set power_levels.events[%q]=0; agent power escalation is not a remedy: %w", StatusEventType, err)
	}
	if err != nil {
		return err
	}
	if record.State == "unknown" {
		st.NoticeStatus = &record
	} else {
		st.Liveness = &record
		st.NoticeStatus = nil
	}
	st.NoticeWriteUncertain = false
	st.NoticeRetry = nil
	if st.Terminal != nil {
		if reflect.DeepEqual(record.OwnNotice, st.Terminal.OwnNotice) {
			st.Terminal.NoticePublication = "published"
		} else {
			st.Terminal.NoticePublication = "unavailable: own notice changed"
		}
	}
	return nil
}
