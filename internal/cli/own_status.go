package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// writeOwnStatus reserves one bounded attempt under the state lock, performs
// HTTP on a private snapshot, then commits only status knowledge. Mail and
// binding writers can progress throughout the request. A concurrent status
// writer refuses locally; a crashed writer's fence expires at its RPC deadline.
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
	token := matrix.NewTxnID()
	var record config.AgentStatus
	var send, priorUncertain bool
	snapshot, err := config.UpdateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if err := writeCtx.Err(); err != nil {
			return err
		}
		if st.OwnStatusWrite != nil && time.Now().Before(st.OwnStatusWrite.Until) {
			return errors.New("own status publication already pending")
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
		return err
	}
	// Persist uncertainty before HTTP, while letting pre-send failures and
	// explicit refusals restore this attempt's prior knowledge on completion.
	snapshot.NoticeWriteUncertain = priorUncertain
	previousLiveness, previousNotice := snapshot.Liveness, snapshot.NoticeStatus
	publicationErr := s.putOwnStatus(writeCtx, &snapshot, record)
	_, err = config.UpdateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
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
