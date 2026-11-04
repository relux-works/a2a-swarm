package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
		since, ok := parseNoticeTimestamp(n.Since)
		due := since
		due.second = due.second.Add(strandedNoticeThreshold)
		now, _ := parseNoticeTimestamp(a.Now().UTC().Format(time.RFC3339Nano))
		if !ok || now.before(due) {
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
		room, routeErr := s.strandedOwnRoom(reportCtx, r.Message.Room)
		if routeErr != nil {
			r.Outcome = routeErr.Error()
			r.RetryAt = a.Now().Add(b.RetryMax)
			reportErr = routeErr
			return nil
		}
		if r.Message.Room == "" {
			r.Message.Room = room
			r.Message.Content = map[string]any{
				"msgtype":      "m.text",
				"body":         "SUSPECTED-DEAF: " + s.name + " has a possibly stranded own terminal notice since=" + n.Since + ". Later wakes may be blocked. Manual inspection and recovery required; mail remains unreceived.",
				"ai.relux.a2a": Header{V: 1, Type: "message", From: s.name, At: a.Now().UTC().Format(time.RFC3339Nano), Kind: "system", Subtype: "incident", Subject: "identity:" + s.name},
			}
		}
		r.Outcome = "pending"
		snapshot := *r
		pending = &snapshot
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
		if _, routeErr := s.strandedOwnRoom(reportCtx, r.Message.Room); routeErr != nil {
			r.Outcome = routeErr.Error()
			r.RetryAt = a.Now().Add(b.RetryMax)
			reportErr = routeErr
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
	case "pending", "published", "unavailable: system incident report failed",
		"incident route not configured", "unavailable: incident route", "unavailable: pinned incident route changed":
		return b.NoticeReport.Outcome
	default:
		return "unavailable: invalid report state"
	}
}

// Report errors are local diagnostics, never consumer failures. This is the
// dispatch completion path used by runTerminal after a healthy observation.
func (a *App) completeTerminalObservation(ctx context.Context, s *session, generation string) error {
	a.publishTerminalNotice(ctx, s, generation)
	if err := a.reportStrandedTerminalNotice(ctx, s, generation); err != nil {
		fmt.Fprintln(a.Stderr, "binding incident report unavailable; local status retained")
	}
	return nil
}

// Authority comes only from own identity metadata. Revalidate before every
// send, including an ambiguous retry; a changed route never replaces the pin.
func (s *session) strandedOwnRoom(ctx context.Context, pinned string) (string, error) {
	unavailable := errors.New("unavailable: incident route")
	metadata, err := config.LoadCredentialMetadata(s.paths, s.name, false)
	if err != nil || metadata.UserID != s.creds.UserID || metadata.Homeserver != s.creds.Homeserver {
		return "", unavailable
	}
	room := metadata.OwnRoom
	if room == "" {
		return "", errors.New("incident route not configured")
	}
	if !strings.HasPrefix(room, "!") || strings.ContainsAny(room, " \t\r\n") {
		return "", unavailable
	}
	if pinned != "" && pinned != room {
		return "", errors.New("unavailable: pinned incident route changed")
	}
	rooms, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return "", unavailable
	}
	joined := false
	for _, id := range rooms {
		if id == room {
			joined = true
		}
	}
	if !joined {
		return "", unavailable
	}
	var alias struct {
		Alias string `json:"alias"`
	}
	if err := s.client.GetState(ctx, room, "m.room.canonical_alias", "", &alias); err != nil || alias.Alias == "" || strings.Split(alias.Alias, ":")[0] == SystemRoom {
		return "", unavailable
	}
	var power struct {
		Users         map[string]int `json:"users"`
		UsersDefault  *int           `json:"users_default"`
		Events        map[string]int `json:"events"`
		EventsDefault *int           `json:"events_default"`
	}
	if err := s.client.GetState(ctx, room, "m.room.power_levels", "", &power); err != nil {
		return "", unavailable
	}
	level, known := power.Users[s.creds.UserID]
	if !known {
		if power.UsersDefault == nil {
			return "", unavailable
		}
		level = *power.UsersDefault
	}
	required, known := power.Events["m.room.message"]
	if !known {
		if power.EventsDefault == nil {
			return "", unavailable
		}
		required = *power.EventsDefault
	}
	if level < required {
		return "", unavailable
	}
	return room, nil
}
