package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// putOwnStatus is the single owner of knowledge about this identity's status
// event. Callers hold the bounded state lock and must save st even on a PUT
// error, then return that error separately. Liveness is the exact last acknowledged
// record, not a desired record. Uncertainty belongs to the identity, not a binding.
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
	// survives this call. Resolution shares the caller's original lock/RPC budget.
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
