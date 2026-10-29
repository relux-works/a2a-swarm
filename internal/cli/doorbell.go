package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// These errors contain no upstream error, command argument or peer metadata.
// stdout alone is the notification channel. In particular, timeout is not a bell.
var (
	errDoorbellTimeout   = errors.New("doorbell: timeout")
	errDoorbellCanceled  = errors.New("doorbell: canceled")
	errDoorbellFailed    = errors.New("doorbell: unavailable; no receipt recorded")
	errDoorbellScope     = errors.New("doorbell: own enrolled session and generation required")
	errDoorbellUncertain = errors.New("doorbell: retained uncertain attempt requires authoritative reconciliation")
	errDoorbellMetadata  = errors.New("doorbell: invalid mailbox metadata retained")
)

const doorbellUsage = "usage: a2a doorbell --generation G [--once] [--timeout DUR] (0 < DUR <= 25m)"

// cmdDoorbell is an additive deterministic detector, not a native wake adapter.
// It never creates a binding, acquires a new lease or records native acceptance.
func cmdDoorbell(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("doorbell")
	once := fs.Bool("once", false, "return after one eligible notice")
	timeout := fs.Duration("timeout", DefaultWaitTimeout, "bounded detector lifetime")
	generation := fs.String("generation", "", "existing own binding generation (required)")
	pos, err := parseInterspersed(fs, args)
	if err != nil || len(pos) != 0 || *timeout <= 0 || *timeout > DefaultWaitTimeout || !claudeSessionRE.MatchString(*generation) {
		return usagef("%s", doorbellUsage)
	}
	s, err := a.session()
	if err != nil {
		return errDoorbellScope
	}
	// Unlike legacy wait, there is no PID/session-name fallback here.
	sid := a.Getenv(EnvSessionID)
	native := a.Getenv("CODEX_THREAD_ID")
	if sid == "" {
		sid = native
	}
	if !claudeSessionRE.MatchString(sid) || (native != "" && native != sid) || s.creds.UserID != "@"+s.name+":"+s.server {
		return errDoorbellScope
	}
	run, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	var result error
	err = config.WithRuntimeLock(s.paths, s.name, "consumer", time.Now().Add(100*time.Millisecond), func() error {
		result = a.runDoorbell(run, s, sid, *generation, *once)
		return result
	})
	if err != nil && result == nil {
		result = doorbellContextFailure(run, errDoorbellFailed)
	}
	// A completed or uncertain local transport result outranks a deadline
	// reached while its synchronous writer/settlement was returning.
	if errors.Is(result, context.Canceled) || errors.Is(result, context.DeadlineExceeded) {
		if ctx.Err() != nil {
			return errDoorbellCanceled
		}
		return errDoorbellTimeout
	}
	return result
}

// Before output, a canceled operation is a non-notice outcome. Never apply
// this conversion to an actual stdout/settlement result.
func doorbellContextFailure(ctx context.Context, failure error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return failure
}

// doorbellItems projects only the fields consumed by the existing hook encoder
// and membership verifier. It never calls the raw wait/item/record renderers.
func doorbellItems(entries []config.MailEntry) ([]waitItem, error) {
	items := make([]waitItem, 0, len(entries))
	for _, e := range entries {
		it := waitItem{id: e.ID, rec: EventRecord{RoomID: e.Room}}
		switch e.Kind {
		case config.MailGap:
			gap := e
			it.gap = &gap
		case config.MailInvite:
			it.rec.Type = "invite"
		case config.MailEvent:
			if !slices.Contains(doorbellTypes, e.Rel) || e.Rel == "invite" {
				return nil, errDoorbellMetadata
			}
			var ev matrix.Event
			if json.Unmarshal(e.Event, &ev) != nil || ev.Type != "m.room.message" || ev.EventID != e.ID {
				return nil, errDoorbellMetadata
			}
			it.rec.Type, it.ev = e.Rel, &ev
		default:
			return nil, errDoorbellMetadata
		}
		items = append(items, it)
	}
	return items, nil
}

// doorbellScope checks actual existing source enrollment. No generic Monitor
// enrollment exists on this base: absent native/terminal enrollment is closed.
// The lifecycle owner must keep its own lease live; this detector cannot adopt it.
func (a *App) doorbellScope(ctx context.Context, s *session, st config.State, sid, generation string) error {
	if err := s.checkConnection(); err != nil {
		return errDoorbellScope
	}
	if st.Lease == nil || st.Lease.Holder != sid || st.Lease.Host != machineLabel() || !st.Lease.Expires.After(a.Now()) {
		return errDoorbellScope
	}
	if b := st.Runtime; b != nil && !b.Retired {
		if b.Validate() != nil || b.Identity != s.name || b.SessionID != sid || b.Generation != generation || b.Host != machineLabel() {
			return errDoorbellScope
		}
		birth, err := ownerBirth(ctx, b.OwnerPID)
		if err != nil || birth != b.OwnerStart {
			return errDoorbellScope
		}
		identity, err := codexwake.SocketIdentity(b.Endpoint)
		if err != nil || identity != b.EndpointID {
			return errDoorbellScope
		}
		// Ambiguous dual enrollment cannot select its preferred owner.
		if st.Terminal != nil && !st.Terminal.Stopped {
			return errDoorbellScope
		}
		return nil
	}
	if b := st.Terminal; b != nil && !b.Stopped && b.SessionID == sid && b.Generation == generation && b.MaxWakes > 0 && ownTerminalTarget(b, b.Target) {
		if b.Target.Runtime != "claude" && b.Target.Runtime != "codex" {
			return errDoorbellScope
		}
		identity, err := tmuxwake.SocketID(b.Target.Socket)
		if err != nil || identity != b.Target.SocketID {
			return errDoorbellScope
		}
		birth, err := bindingProcessBirth(ctx, b.OwnerPID)
		if err == nil && birth == b.OwnerBirth {
			return nil
		}
	}
	return errDoorbellScope
}

func doorbellHeld(st config.State) bool {
	if st.HooksPaused || manualWakeGate(st) != "" {
		return true
	}
	if b := st.Runtime; b != nil && !b.Retired {
		return b.Gate != "ready" || (st.Wake.LastTurn != "" && st.Wake.LastTurnStatus != "completed")
	}
	return false
}

// A notice spends the existing enrollment's hourly transport budget, without
// creating a native Attempt or pretending stdout is a native accepted turn.
func doorbellBudget(st *config.State, now time.Time, spend bool) bool {
	var reservations *[]time.Time
	limit := 0
	if b := st.Runtime; b != nil && !b.Retired {
		reservations, limit = &st.Wake.Reservations, b.MaxPerHour
	} else if b := st.Terminal; b != nil {
		reservations, limit = &b.Reservations, b.MaxWakes
	}
	if reservations == nil || limit <= 0 {
		return false
	}
	active := 0
	for _, at := range *reservations {
		if at.After(now.Add(-time.Hour)) {
			active++
		}
	}
	if active >= limit {
		return false
	}
	if spend {
		*reservations = slices.DeleteFunc(*reservations, func(at time.Time) bool { return !at.After(now.Add(-time.Hour)) })
		*reservations = append(*reservations, now.UTC())
	}
	return true
}

func doorbellUncertain(st config.State) bool {
	return st.Notification.Reservation != nil || st.Wake.Attempt != nil || (st.Terminal != nil && len(st.Terminal.AttemptIDs) > 0)
}

func ownDoorbellReservation(st config.State, sid, generation, token string) bool {
	r := st.Notification.Reservation
	return r != nil && r.Token == token && r.Path == "doorbell" && r.Session == sid && r.Generation == generation
}

// Settle only a locally proven transport result, under the existing state
// lock and exact live scope/token guards. Run cancellation cannot invalidate
// these local proofs; a fresh finite budget bounds them after Write returns.
// This is neither native acceptance nor uncertain-attempt reconciliation.
func (a *App) settleDoorbell(ctx context.Context, s *session, sid, generation, token string, complete bool) error {
	settle, cancel := context.WithTimeout(context.WithoutCancel(ctx), nativeRPCTimeout)
	defer cancel()
	deadline, _ := settle.Deadline()
	_, err := s.lockedDeliver(settle, deadline, false, func(cur *config.State, _ *config.Mailbox) ([]string, error) {
		if a.doorbellScope(settle, s, *cur, sid, generation) != nil || !ownDoorbellReservation(*cur, sid, generation, token) {
			return nil, errDoorbellUncertain
		}
		if complete {
			return nil, a.completeNotification(s, cur, token)
		}
		return nil, cur.Notification.Cancel(token)
	})
	if err != nil {
		return errDoorbellUncertain
	}
	return nil
}

// emitDoorbell holds control through reserve/write/complete. announced is local
// coalescing only; durable pending mail, shown, hook/native Rung and intake are
// untouched. Restart can rediscover every pending obligation after spacing.
func (a *App) emitDoorbell(ctx context.Context, s *session, sid, generation string, announced map[string]bool) (bool, error) {
	deadline := time.Now().Add(nativeRPCTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	printed := false
	err := config.WithRuntimeLock(s.paths, s.name, "control", deadline, func() error {
		st, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return errDoorbellFailed
		}
		if err = a.doorbellScope(ctx, s, st, sid, generation); err != nil {
			return doorbellContextFailure(ctx, err)
		}
		if doorbellUncertain(st) {
			return errDoorbellUncertain
		}
		if doorbellHeld(st) || !doorbellBudget(&st, a.Now(), false) {
			return nil
		}
		var items []waitItem
		_, err = s.lockedDeliver(ctx, deadline, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
			items, err = doorbellItems(mb.Pending())
			return nil, err
		})
		if err != nil {
			return err
		}
		members := s.verifySenders(ctx, items)
		var out string
		var keys []string
		token, err := a.reserveNotification(ctx, s, deadline, "doorbell", sid, generation, false, func(cur *config.State, mb *config.Mailbox, _ string) (bool, error) {
			if err := a.doorbellScope(ctx, s, *cur, sid, generation); err != nil {
				return false, doorbellContextFailure(ctx, err)
			}
			if doorbellHeld(*cur) || !doorbellBudget(cur, a.Now(), false) {
				return false, nil
			}
			items, err := doorbellItems(mb.Pending())
			if err != nil {
				return false, err
			}
			keys = itemIDs(items)
			fresh := false
			for _, k := range keys {
				fresh = fresh || !announced[k]
			}
			if !fresh {
				return false, nil
			}
			out = doorbellText(doorbellOf(items, s.server, members)) + "\n"
			return doorbellBudget(cur, a.Now(), true), nil
		})
		if err != nil && token != "" {
			return errDoorbellUncertain
		}
		if err != nil || token == "" {
			return err
		}
		proof, cancelProof := context.WithTimeout(context.WithoutCancel(ctx), nativeRPCTimeout)
		defer cancelProof()
		current, loadErr := config.LoadState(s.paths, s.name)
		if loadErr != nil || a.doorbellScope(proof, s, current, sid, generation) != nil || !ownDoorbellReservation(current, sid, generation, token) {
			return errDoorbellUncertain
		}
		stopErr := doorbellContextFailure(ctx, proof.Err())
		if stopErr != nil || doorbellHeld(current) {
			if err := a.settleDoorbell(ctx, s, sid, generation, token, false); err != nil {
				return err
			}
			return stopErr // no Write was started; nil means a held session
		}
		if s.checkConnection() != nil {
			return errDoorbellUncertain
		}
		n, err := io.WriteString(a.Stdout, out)
		if err == nil && n != len(out) {
			err = io.ErrShortWrite
		}
		if err != nil {
			if n == 0 {
				if err := a.settleDoorbell(ctx, s, sid, generation, token, false); err != nil {
					return err
				}
				return errDoorbellFailed
			}
			return errDoorbellUncertain
		}
		if flush, ok := a.Stdout.(interface{ Flush() error }); ok && flush.Flush() != nil {
			return errDoorbellUncertain
		}
		if a.afterPrint != nil && a.afterPrint() != nil {
			return errDoorbellUncertain
		}
		if err := a.settleDoorbell(ctx, s, sid, generation, token, true); err != nil {
			return err
		}
		clear(announced)
		for _, k := range keys {
			announced[k] = true
		}
		printed = true
		return nil
	})
	return printed, err
}

func (a *App) runDoorbell(ctx context.Context, s *session, sid, generation string, once bool) error {
	st, err := config.LoadState(s.paths, s.name)
	if err != nil || a.doorbellScope(ctx, s, st, sid, generation) != nil {
		return doorbellContextFailure(ctx, errDoorbellScope)
	}
	who, err := s.client.Whoami(ctx)
	if err != nil || who.UserID != s.creds.UserID {
		return doorbellContextFailure(ctx, errDoorbellScope)
	}
	announced := map[string]bool{}
	first := true
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		st, err = config.LoadState(s.paths, s.name)
		if err != nil || a.doorbellScope(ctx, s, st, sid, generation) != nil {
			return doorbellContextFailure(ctx, errDoorbellScope)
		}
		if doorbellUncertain(st) {
			return errDoorbellUncertain
		}
		if !doorbellHeld(st) {
			list, err := s.getSubs(ctx)
			if err != nil {
				return doorbellContextFailure(ctx, errDoorbellFailed)
			}
			subs := slices.DeleteFunc(slices.Clone(list.Subs), func(sub Sub) bool { return sub.expiredAt(a.Now()) })
			deadline, _ := ctx.Deadline()
			if first {
				st, err = s.syncNow(ctx, st, subs, deadline)
				first = false
				if err != nil {
					return doorbellContextFailure(ctx, errDoorbellFailed)
				}
			}
			printed, err := a.emitDoorbell(ctx, s, sid, generation, announced)
			if err != nil {
				if errors.Is(err, errDoorbellUncertain) || errors.Is(err, errDoorbellScope) || errors.Is(err, errDoorbellMetadata) || errors.Is(err, errDoorbellFailed) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return err
				}
				return doorbellContextFailure(ctx, errDoorbellFailed)
			}
			if printed && once {
				return nil
			}
			st, err = config.LoadState(s.paths, s.name)
			if err != nil {
				return doorbellContextFailure(ctx, errDoorbellFailed)
			}
			poll := a.notificationWait(st, maxPoll)
			if d, ok := ctx.Deadline(); ok {
				poll = min(poll, time.Until(d))
			}
			if poll <= 0 {
				return errDoorbellTimeout
			}
			resp, err := s.client.Sync(ctx, st.NextBatch, poll, syncFilter)
			if err != nil {
				return doorbellContextFailure(ctx, errDoorbellFailed)
			}
			err = config.WithRuntimeLock(s.paths, s.name, "control", deadline, func() error {
				current, err := config.LoadState(s.paths, s.name)
				if err != nil || a.doorbellScope(ctx, s, current, sid, generation) != nil {
					return errDoorbellScope
				}
				_, err = s.ingest(ctx, resp, st.NextBatch, subs, nil, deadline)
				return err
			})
			if err != nil {
				return doorbellContextFailure(ctx, errDoorbellFailed)
			}
			continue
		}
		// Held sessions retain work. This is a deterministic local hold check,
		// not a model timer; independent owner supervision remains required.
		timer := time.NewTimer(min(time.Second, time.Until(deadlineOf(ctx))))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func deadlineOf(ctx context.Context) time.Time {
	d, _ := ctx.Deadline()
	return d
}
