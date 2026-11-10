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
const bindingUnavailableAlarmAfter = 10 * time.Minute

var errIncidentUnchanged = errors.New("incident report unchanged")

// RAM retains failures even when the state file itself is unavailable. The
// separate persisted report carries the episode/fence across executable refresh
// without inventing an own notice, touching mail or granting terminal authority.
type bindingAvailability struct {
	since       time.Time
	failed      bool
	readyLogged bool
	alarmLogged bool
}

func (a *App) observeBindingConsumer(s *session, generation string, episode *bindingAvailability, ready bool) {
	if !ready && !episode.failed {
		episode.since = a.Now()
		episode.failed, episode.readyLogged, episode.alarmLogged = true, false, false
	}
	recovered := episode.failed
	outcome := ""
	_, err := a.updateStateBy(s.paths, s.name, time.Now(), func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation {
			return errIncidentUnchanged
		}
		if ready {
			recovered = recovered || b.ConsumerReport != nil
			if b.ConsumerReport == nil {
				return errIncidentUnchanged
			}
			b.ConsumerReport = nil
			dropRetiredStrandedAlarms(st)
			return nil
		}
		created := b.ConsumerReport == nil
		if created {
			b.ConsumerReport = &config.NoticeReport{Since: episode.since.UTC().Format(time.RFC3339Nano)}
		}
		r := b.ConsumerReport
		since, parseErr := time.Parse(time.RFC3339Nano, r.Since)
		if parseErr != nil {
			since, r.Since = episode.since, episode.since.UTC().Format(time.RFC3339Nano)
			created = true
		}
		episode.since = since
		// Outcome is the durable once-per-episode attempt fence, including a
		// missing/invalid mail-alarm route. The sender owns bounded send retries.
		if r.Outcome != "" || a.Now().Before(since.Add(bindingUnavailableAlarmAfter)) {
			if created {
				return nil
			}
			return errIncidentUnchanged
		}
		if err := queueIncidentAlarm(s, st, r, consumerUnavailableAlarmKind, consumerUnavailableReason, a.Now()); err != nil && r.Outcome == "" {
			r.Outcome = "unavailable: invalid alarm state"
		}
		r.LoggedOutcome = r.Outcome
		outcome = r.Outcome
		return nil
	})
	if ready {
		if recovered && !episode.readyLogged {
			fmt.Fprintln(a.Stderr, "binding consumer ready")
		}
		episode.failed, episode.readyLogged = false, true
		return
	}
	if err == nil && outcome != "" {
		fmt.Fprintf(a.Stderr, "binding incident report: %s\n", outcome)
		episode.alarmLogged = true
	} else if err != nil && !errors.Is(err, errIncidentUnchanged) && !episode.alarmLogged && !a.Now().Before(episode.since.Add(bindingUnavailableAlarmAfter)) {
		// An unreadable/unwritable journal must not crash or spin the binding.
		// Its existing unavailable vocabulary is logged once locally; a later
		// writable cycle can still persist the outbox and its durable fence.
		fmt.Fprintln(a.Stderr, "binding incident report: unavailable: invalid alarm state")
		episode.alarmLogged = true
	}
}

// Reporting grants no input authority. Persist the destination, transaction and
// immutable content before sending; Matrix deduplicates a possibly applied send.
// Dispatch owns the control lock during observation; the state lock serializes
// report writers and retirement during each bounded network operation.
func (a *App) reportStrandedTerminalNotice(ctx context.Context, s *session, generation string) error {
	return a.reportStrandedTerminalNoticeMode(ctx, s, generation, true)
}

func (a *App) reportStrandedTerminalNoticeMode(ctx context.Context, s *session, generation string, credentialsReady bool) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	reportCtx, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	defer cancel()
	deadline, _ := reportCtx.Deadline()
	var pending *config.NoticeReport
	var reportErr error
	_, err := a.updateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Stopped || b.Generation != generation {
			return errIncidentUnchanged
		}
		n := b.OwnNotice
		if noticeValid(n) != nil || n.PossiblyOrphaned == nil || !*n.PossiblyOrphaned {
			return errIncidentUnchanged
		}
		since, ok := parseNoticeTimestamp(n.Since)
		due := since
		due.second = due.second.Add(strandedNoticeThreshold)
		now, _ := parseNoticeTimestamp(a.Now().UTC().Format(time.RFC3339Nano))
		if !ok || now.before(due) {
			return errIncidentUnchanged
		}
		if name, ok := doorbellName(s.creds.UserID, s.server); !ok || name != s.name {
			return errors.New("incident report refuses foreign identity")
		}
		r := b.NoticeReport
		if r == nil || r.Since != n.Since {
			r = &config.NoticeReport{Since: n.Since, Message: config.LivenessAlertMessage{Txn: matrix.NewTxnID()}}
			b.NoticeReport = r
			dropRetiredStrandedAlarms(st)
		}
		if r.Sent || a.Now().Before(r.RetryAt) {
			return errIncidentUnchanged
		}
		if r.Alarm {
			reportErr = queueStrandedAlarm(s, st, a.Now())
			r.RetryAt = a.Now().Add(b.RetryMax)
			return nil
		}
		if !credentialsReady {
			// Persist the usual owner/lead jobs now; authenticated delivery resumes
			// after credential recovery. Missing routes retain the own-room fallback.
			reportErr = queueStrandedAlarm(s, st, a.Now())
			if !r.Alarm {
				r.Outcome = "unavailable: credentials"
			}
			r.RetryAt = a.Now().Add(b.RetryMax)
			return nil
		}
		room, routeErr := s.strandedOwnRoom(reportCtx, r.Message.Room)
		if routeErr != nil && routeErr.Error() == "incident route not configured" && r.Message.Room == "" {
			reportErr = queueStrandedAlarm(s, st, a.Now())
			r.RetryAt = a.Now().Add(b.RetryMax)
			return nil
		}
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
	if errors.Is(err, errIncidentUnchanged) {
		return nil
	}
	if err != nil {
		return err
	}
	if pending == nil {
		return reportErr
	}
	_, err = a.updateStateBy(s.paths, s.name, deadline, func(st *config.State) error {
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
	case "unavailable: alarm acknowledgement missing", "unavailable: invalid alarm state", "queued via mail-alarm", "published via mail-alarm", "unavailable: alarm destination missing", "unavailable: invalid alarm config", "pending", "published", "unavailable: system incident report failed",
		"incident route not configured", "unavailable: credentials", "unavailable: incident route", "unavailable: pinned incident route changed":
		return b.NoticeReport.Outcome
	default:
		return "unavailable: invalid report state"
	}
}

// Report errors are local diagnostics, never consumer failures. This is the
// dispatch completion path used by runTerminal after a healthy observation.
func (a *App) completeTerminalObservation(ctx context.Context, s *session, generation string) error {
	a.publishTerminalNotice(ctx, s, generation)
	a.reportTerminalIncident(ctx, s, generation)
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

// Persist diagnostic transitions, so repeated retries and consumer restarts do
// not fill the binding log. Only closed status codes enter the log.
func (a *App) reportTerminalIncident(ctx context.Context, s *session, generation string) {
	_ = a.reportStrandedTerminalNotice(ctx, s, generation)
	st, err := config.LoadState(s.paths, s.name)
	if err != nil || st.Terminal == nil || st.Terminal.Generation != generation || st.Terminal.NoticeReport == nil {
		return
	}
	status := terminalNoticeReportStatus(st.Terminal)
	if status == "not due" || status == st.Terminal.NoticeReport.LoggedOutcome {
		return
	}
	outcome := ""
	_, err = a.updateStateBy(s.paths, s.name, time.Now(), func(st *config.State) error {
		b := st.Terminal
		if b == nil || b.Generation != generation || b.NoticeReport == nil {
			return errors.New("incident report changed before logging")
		}
		status := terminalNoticeReportStatus(b)
		if status != "not due" && status != b.NoticeReport.LoggedOutcome {
			b.NoticeReport.LoggedOutcome = status
			outcome = status
			return nil
		}
		return errors.New("incident report already logged")
	})
	if err == nil && outcome != "" {
		fmt.Fprintf(a.Stderr, "binding incident report: %s\n", outcome)
	}
}
