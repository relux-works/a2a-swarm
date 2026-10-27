package cli

import (
	"context"
	"errors"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const strandedNoticeThreshold = 10 * time.Minute

// Reporting grants no input authority. Persist the destination, transaction and
// immutable content before sending; Matrix deduplicates a possibly applied send.
// Dispatch owns the control lock during observation; the state lock serializes
// report writers and retirement during each bounded network operation.
func (a *App) reportStrandedTerminalNotice(ctx context.Context, s *session, generation string) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	reportCtx, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	deadline, _ := reportCtx.Deadline()
	var pending *config.NoticeReport
	var reportErr error
	_, err := config.UpdateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation {
			return nil
		}
		n := b.OwnNotice
		if noticeValid(n) != nil || n.PossiblyOrphaned == nil || !*n.PossiblyOrphaned {
			return nil
		}
		since, err := time.Parse(time.RFC3339Nano, n.Since)
		if err != nil || a.Now().Before(since.Add(strandedNoticeThreshold)) {
			return nil
		}
		if name, ok := doorbellName(s.creds.UserID, s.server); !ok || name != s.name {
			return errors.New("incident report refuses foreign identity")
		}
		r := b.NoticeReport
		if r == nil || r.Since != n.Since {
			r = &config.NoticeReport{Since: n.Since, Message: config.LivenessAlertMessage{Txn: matrix.NewTxnID()}}
			b.NoticeReport = r
		}
		if r.Sent || a.Now().Before(r.RetryAt) {
			return nil
		}
		if r.Message.Room == "" {
			room, _, err := s.resolveRoom(reportCtx, SystemRoom)
			if err != nil {
				r.Outcome = "unavailable: system incident report failed"
				r.RetryAt = a.Now().Add(b.RetryMax)
				reportErr = errors.New(r.Outcome)
				return nil
			}
			r.Message.Room = room
			r.Message.Content = map[string]any{
				"msgtype":      "m.text",
				"body":         "SUSPECTED-DEAF: " + s.name + " has a possibly stranded own terminal notice since=" + n.Since + ". Later wakes may be blocked. Keeper inspection and manual recovery required; mail remains unreceived.",
				"ai.relux.a2a": Header{V: 1, Type: "message", From: s.name, At: since.Add(strandedNoticeThreshold).UTC().Format(time.RFC3339Nano), Kind: "system", Subtype: "incident"},
			}
		}
		r.Outcome = "pending"
		copy := *r
		pending = &copy
		return nil
	})
	if err != nil {
		return err
	}
	if pending == nil {
		return reportErr
	}
	_, err = config.UpdateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation || b.NoticeReport == nil {
			return nil
		}
		r := b.NoticeReport
		n := b.OwnNotice
		if r.Sent || a.Now().Before(r.RetryAt) || r.Since != pending.Since || r.Message.Txn != pending.Message.Txn || n.Since != r.Since || noticeValid(n) != nil || n.PossiblyOrphaned == nil || !*n.PossiblyOrphaned {
			return nil
		}
		// A failed/ambiguous send retries under the ordinary binding retry cap,
		// without spending a wake reservation, changing receipts or renewing status.
		if _, sendErr := s.client.SendMessageTxnReceipt(reportCtx, r.Message.Room, r.Message.Txn, r.Message.Content); sendErr != nil {
			r.Outcome = "unavailable: system incident report failed"
			r.RetryAt = a.Now().Add(b.RetryMax)
			reportErr = errors.New(r.Outcome)
			return nil
		}
		r.Sent = true
		r.Outcome = "published"
		r.RetryAt = time.Time{}
		return nil
	})
	if err != nil {
		return err
	}
	return reportErr
}

func terminalNoticeReportStatus(b *config.TerminalBinding) string {
	if b == nil || b.NoticeReport == nil || b.NoticeReport.Since != b.OwnNotice.Since {
		return "not due"
	}
	switch b.NoticeReport.Outcome {
	case "pending", "published", "unavailable: system incident report failed":
		return b.NoticeReport.Outcome
	default:
		return "unavailable: invalid report state"
	}
}
