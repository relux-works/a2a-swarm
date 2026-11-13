package cli

import (
	"context"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// A terminal consumer with nothing to deliver, no attempt, reservation,
// cooldown, report or refresh candidate pending is quiescent. After a
// continuous quiescent streak it stops repeating full cycles every Interval
// and waits for a reason instead: new server events (a /sync long-poll), a
// change to its own state, mailbox or connection files, its owner's exit or
// replacement, a launch-alias change, or the bounded reconciliation deadline.
// Every reason leads to an ordinary full cycle, which keeps every guard,
// lease renewal and effect boundary of the active loop. Wake hints grant no
// authority. These values are package-private test seams; production code,
// flags and environment never change them.
var (
	terminalQuietAfter     = 30 * time.Second // continuous quiescence before waiting for reasons
	terminalReconcileEvery = 60 * time.Second // maximum interval between full cycles
	terminalLongPoll       = 25 * time.Second // server-side /sync wait
	terminalQuietTickMax   = time.Second      // owner liveness and local-change checks
	terminalQuiescence     = true             // matched comparisons: false keeps the active loop
)

// terminalQuiescent reports whether a freshly loaded State leaves the binding
// with no outstanding work for this consumer. Diagnostic history (recovery,
// send refusal, publication outcome) does not count as work; anything that
// carries its own deadline or effect does. Unknown or unsettled means active.
func terminalQuiescent(st config.State, generation string, pid int, now time.Time) bool {
	b := st.Terminal
	if b == nil || b.DaemonPID != pid {
		return false
	}
	if terminalRefreshBlockReason(st, generation, b.DaemonBirth, machineLabel(), pid, now) != "" {
		return false
	}
	if b.Outcome != "armed: no new mail" && b.Outcome != "suppressed: manual gate" {
		return false
	}
	if b.Idle != nil || b.NoticeOrigin != nil || b.NoticePosition != nil || b.TypingStarted {
		return false
	}
	if b.NoticeReport != nil || b.ConsumerReport != nil || b.OwnNotice.PossiblyOrphaned != nil && *b.OwnNotice.PossiblyOrphaned {
		return false
	}
	if st.NoticeRetry != nil || st.NoticeWriteUncertain {
		return false
	}
	return !st.Notification.NotBefore.After(now)
}

type quiescentResult int

const (
	quiescentReconcile   quiescentResult = iota // run a full cycle now
	quiescentCancelled                          // the consumer context ended
	quiescentUnavailable                        // wake sources unknown: take the ordinary wait
)

type quiescentPoll struct {
	resp matrix.SyncResponse
	err  error
}

// quiescentWatch is the set of local objects a quiescent consumer depends on
// but does not write: connection, credential, subscription policy and owner
// files, and the retained launch alias. They are compared by stamp only.
type quiescentWatch struct {
	paths   []string
	stamps  []config.FileStamp
	refresh terminalRefreshView
}

func newQuiescentWatch(s *session, refresh terminalRefreshView) quiescentWatch {
	owner := ownerFiles(s.paths, s.name)
	w := quiescentWatch{refresh: refresh, paths: []string{
		s.paths.AgentFile(s.name), s.paths.EnvFile(), s.paths.ConnectionsFile(), s.paths.HostFile(),
		s.paths.SubscriptionPolicyFile(s.name),
		owner.path("pin"), owner.path("json"), owner.path("principals.json"), owner.path("bindings"), owner.path("archive-exclude"),
	}}
	for _, p := range w.paths {
		w.stamps = append(w.stamps, config.StampFile(p))
	}
	if refresh != nil {
		alias := refresh.aliasStamps()
		w.stamps = append(w.stamps, alias[0], alias[1])
	}
	return w
}

func (w quiescentWatch) changed() bool {
	current := make([]config.FileStamp, 0, len(w.stamps))
	for _, p := range w.paths {
		current = append(current, config.StampFile(p))
	}
	if w.refresh != nil {
		alias := w.refresh.aliasStamps()
		current = append(current, alias[0], alias[1])
	}
	for i := range w.stamps {
		if !w.stamps[i].Equal(current[i]) {
			return true
		}
	}
	return false
}

// terminalRefreshView is the read-only refresh state a quiescent wait needs.
type terminalRefreshView interface {
	pending() bool
	aliasStamps() [2]config.FileStamp
}

func (a *App) traceTerminal(event string) {
	if a.terminalTrace != nil {
		a.terminalTrace(event)
	}
}

// waitQuiescent waits for the first reason to run a full cycle. It holds no
// lock and no decoded State across its waits. The owner is registered by its
// native start time between two canonical Birth checks, so PID reuse during
// registration is detected; afterwards a fork-free check runs every
// min(Interval, 1s). Any registration uncertainty returns quiescentUnavailable
// so the caller keeps the ordinary active wait and its owner checks.
func (a *App) waitQuiescent(ctx context.Context, s *session, policy config.TerminalBinding, lastFull time.Time, refresh terminalRefreshView) (quiescentResult, string) {
	scope := s.paths.StateDecodeScope()
	if !scope.OwnFilesRecorded() {
		return quiescentUnavailable, "own files unrecorded"
	}
	probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	before, beforeErr := tmuxwake.Birth(probe, policy.OwnerPID)
	start, startErr := processStart(policy.OwnerPID)
	after, afterErr := tmuxwake.Birth(probe, policy.OwnerPID)
	cancel()
	if beforeErr != nil || afterErr != nil || startErr != nil || before != after {
		return quiescentUnavailable, "owner registration"
	}
	if before != policy.OwnerBirth {
		return quiescentReconcile, "owner"
	}
	watch := newQuiescentWatch(s, refresh)
	tick := min(policy.Interval, terminalQuietTickMax)
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	deadline := lastFull.Add(terminalReconcileEvery)
	mailbox := s.paths.MailboxFile(s.name)

	polls := make(chan quiescentPoll, 1)
	polling := false
	pollCancel := context.CancelFunc(func() {})
	var since string
	var subs []Sub
	var mailboxBefore config.FileStamp
	nextPoll := time.Time{}
	stop := func() {
		pollCancel()
		if polling {
			<-polls
			polling = false
		}
	}
	defer stop()
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return quiescentReconcile, "reconcile"
		}
		if !polling && !now.Before(nextPoll) {
			op, opCancel := context.WithTimeout(ctx, nativeRPCTimeout)
			st, list, err := s.startDelivery(op, a.Now())
			opCancel()
			if err != nil {
				if ctx.Err() != nil {
					return quiescentCancelled, "cancelled"
				}
				return quiescentReconcile, "delivery"
			}
			if st.NextBatch == "" {
				return quiescentReconcile, "cursor"
			}
			s.principals = s.readDeliveryPrincipals()
			since, subs, mailboxBefore = st.NextBatch, list.Subs, config.StampFile(mailbox)
			var pctx context.Context
			// stop (deferred) cancels and joins the request on every return.
			pctx, pollCancel = cancellablePoll(ctx)
			polling = true
			// Rate bound: an immediately returning or empty response cannot
			// start the next request earlier than one Interval after this one.
			nextPoll = now.Add(policy.Interval)
			a.traceTerminal("poll")
			go func(since string) {
				resp, err := s.client.Sync(pctx, since, terminalLongPoll, syncFilter)
				polls <- quiescentPoll{resp, err}
			}(since)
		}
		select {
		case <-ctx.Done():
			return quiescentCancelled, "cancelled"
		case out := <-polls:
			polling = false
			pollCancel()
			if out.err != nil {
				if ctx.Err() != nil {
					return quiescentCancelled, "cancelled"
				}
				return quiescentReconcile, "sync"
			}
			// The authoritative ingestion path: mailbox before cursor,
			// mandatory principal classification, redactions and rewrites.
			op, opCancel := context.WithTimeout(ctx, nativeRPCTimeout)
			_, err := s.ingest(op, out.resp, since, subs, nil, time.Now().Add(nativeRPCTimeout))
			opCancel()
			if err != nil {
				if ctx.Err() != nil {
					return quiescentCancelled, "cancelled"
				}
				return quiescentReconcile, "ingest"
			}
			if !mailboxBefore.Equal(config.StampFile(mailbox)) {
				return quiescentReconcile, "mail"
			}
		case <-ticker.C:
			current, err := processStart(policy.OwnerPID)
			if err != nil || current != start {
				return quiescentReconcile, "owner"
			}
			if !scope.OwnFilesUnchanged(s.paths, s.name) {
				return quiescentReconcile, "state"
			}
			if watch.changed() {
				return quiescentReconcile, "local"
			}
		}
	}
}

func cancellablePoll(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}
