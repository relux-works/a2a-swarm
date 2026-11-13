package cli

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
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
	terminalExchangeInline = false            // calibration: the earlier synchronous exchange
)

// terminalQuiescent reports whether a freshly loaded State leaves the binding
// with no outstanding work for this consumer. Diagnostic history (recovery,
// send refusal, publication outcome) does not count as work; anything that
// carries its own deadline or effect does. Unknown or unsettled means active.
// The caller also requires the same cycle's fresh delivery view to have found
// no un-rung pending entry (session.terminalUndelivered == 0).
func terminalQuiescent(st config.State, generation string, pid int, now time.Time) bool {
	b := st.Terminal
	if b == nil || b.DaemonPID != pid {
		return false
	}
	if terminalRefreshBlockReason(st, generation, b.DaemonBirth, machineLabel(), pid, now) != "" {
		return false
	}
	// A hook pause or manual gate keeps the active loop: its retained mail and
	// gate changes are observed every Interval exactly as before.
	if b.Outcome != "armed: no new mail" || st.HooksPaused || manualWakeGate(st) != "" {
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

// Process inspection seams for PID-replacement fixtures; production code
// never changes them.
var (
	terminalBirth        = tmuxwake.Birth
	terminalProcessStart = processStart
	// Calibration only: false treats a known start-token change as unknown.
	terminalKnownReplacementStops = true
	// Calibration only: false waits out the post-wake Interval blind.
	terminalWatchOwnerWhileHeld = true
)

// errProcessGone is processStart's answer for a PID with no process.
var errProcessGone = errors.New("process gone")

type ownerToken int

const (
	ownerTokenSame     ownerToken = iota
	ownerTokenReplaced            // known: absent, or a different valid start token
	ownerTokenUnknown             // inspection failed; grants nothing, proves nothing
)

// classifyOwner compares the owner's current native start token with the one
// registered for this process instance. A different VALID token is a known
// replacement even when the second-resolution canonical Birth is unchanged.
func classifyOwner(pid int, start string) ownerToken {
	current, err := terminalProcessStart(pid)
	switch {
	case errors.Is(err, errProcessGone):
		return ownerTokenReplaced
	case err != nil:
		return ownerTokenUnknown
	case current == start:
		return ownerTokenSame
	case terminalKnownReplacementStops:
		return ownerTokenReplaced
	default:
		return ownerTokenUnknown
	}
}

// ownerReason maps a non-same owner state to its wake reason.
func ownerReason(state ownerToken) string {
	if state == ownerTokenReplaced {
		return "owner-replaced"
	}
	return "owner"
}

// quiescentWriting serialises the exchange worker's State/mailbox write
// phases with the waiting loop's own-file comparison, so an in-progress own
// write is never mistaken for another writer's change.
type quiescentWriting struct {
	mu      sync.Mutex
	writing bool
}

func (w *quiescentWriting) set(writing bool) {
	w.mu.Lock()
	w.writing = writing
	w.mu.Unlock()
}

// waitQuiescent waits for the first reason to run a full cycle. It holds no
// lock and no decoded State across its waits. The owner is registered by its
// native start time between two canonical Birth checks, so PID reuse during
// registration is detected. One exchange worker at a time runs startDelivery,
// the /sync long-poll and ingestion on a cancellable context, while this loop
// keeps its tick of min(Interval, 1s): a fork-free start-time comparison and
// metadata stamp comparisons only. Any reason cancels and joins the worker
// before returning; cancellation aborts lock waits and the request, and an
// ingestion that already holds the State lock completes its transaction.
// Registration uncertainty returns quiescentUnavailable so the caller keeps
// the ordinary active wait and its owner checks.
func (a *App) waitQuiescent(ctx context.Context, s *session, policy config.TerminalBinding, lastFull time.Time, refresh terminalRefreshView) (quiescentResult, string) {
	// wake records the reason at the moment it is detected (before any hold),
	// so observers see the cause, not the end of the Interval hold.
	wake := func(result quiescentResult, reason string) (quiescentResult, string) {
		a.traceTerminal("wake:" + reason)
		return result, reason
	}
	scope := s.paths.StateDecodeScope()
	if !scope.OwnFilesRecorded() {
		return wake(quiescentUnavailable, "own files unrecorded")
	}
	probe, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	before, beforeErr := terminalBirth(probe, policy.OwnerPID)
	start, startErr := terminalProcessStart(policy.OwnerPID)
	after, afterErr := terminalBirth(probe, policy.OwnerPID)
	cancel()
	if beforeErr != nil || afterErr != nil || startErr != nil || before != after {
		return wake(quiescentUnavailable, "owner registration")
	}
	if before != policy.OwnerBirth {
		return wake(quiescentReconcile, "owner")
	}
	a.traceTerminal("register")
	watch := newQuiescentWatch(s, refresh)
	ticker := time.NewTicker(min(policy.Interval, terminalQuietTickMax))
	defer ticker.Stop()
	deadline := lastFull.Add(terminalReconcileEvery)
	// hold keeps full cycles at least one Interval apart after a non-owner
	// reason without going blind: the owner tick continues until then.
	hold := func(reason string) (quiescentResult, string) {
		a.traceTerminal("wake:" + reason)
		until := lastFull.Add(policy.Interval)
		for terminalWatchOwnerWhileHeld && time.Now().Before(until) {
			timer := time.NewTimer(time.Until(until))
			select {
			case <-ctx.Done():
				timer.Stop()
				return wake(quiescentCancelled, "cancelled")
			case <-timer.C:
			case <-ticker.C:
				timer.Stop()
				a.traceTerminal("tick")
				if state := classifyOwner(policy.OwnerPID, start); state != ownerTokenSame {
					return wake(quiescentReconcile, ownerReason(state))
				}
			}
		}
		if !terminalWatchOwnerWhileHeld {
			if wait := time.Until(until); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-ctx.Done():
					timer.Stop()
					return wake(quiescentCancelled, "cancelled")
				case <-timer.C:
				}
			}
		}
		return quiescentReconcile, reason // traced when detected
	}

	writes := &quiescentWriting{}
	done := make(chan string, 1)
	running := false
	workerCancel := context.CancelFunc(func() {})
	stop := func() {
		workerCancel()
		if running {
			<-done
			running = false
		}
	}
	defer stop()
	var nextExchange time.Time
	for {
		now := time.Now()
		if !now.Before(deadline) {
			return wake(quiescentReconcile, "reconcile")
		}
		if !running && !now.Before(nextExchange) {
			// Rate bound: an immediately returning or empty response cannot
			// start the next exchange earlier than one Interval after this one.
			nextExchange = now.Add(policy.Interval)
			var wctx context.Context
			wctx, workerCancel = cancellablePoll(ctx)
			running = true
			if terminalExchangeInline {
				done <- a.quiescentExchange(wctx, s, writes)
			} else {
				go func() { done <- a.quiescentExchange(wctx, s, writes) }()
			}
		}
		select {
		case <-ctx.Done():
			return wake(quiescentCancelled, "cancelled")
		case reason := <-done:
			running = false
			workerCancel()
			if ctx.Err() != nil {
				return wake(quiescentCancelled, "cancelled")
			}
			if reason != "" {
				stop()
				return hold(reason)
			}
			a.traceTerminal("exchanged")
		case <-ticker.C:
			a.traceTerminal("tick")
			if state := classifyOwner(policy.OwnerPID, start); state != ownerTokenSame {
				return wake(quiescentReconcile, ownerReason(state))
			}
			writes.mu.Lock()
			changed := !writes.writing && !scope.OwnFilesUnchanged(s.paths, s.name)
			writes.mu.Unlock()
			if changed {
				stop()
				return hold("state")
			}
			if watch.changed() {
				stop()
				return hold("local")
			}
		}
	}
}

// quiescentExchange runs one long-poll exchange through the authoritative
// delivery path: startDelivery (subscription expiry), the /sync long-poll,
// then ingestion (mailbox before cursor, mandatory principal classification,
// redactions and rewrites). It returns "" to keep waiting or the reason for a
// full cycle. Its write phases are marked for the waiting loop.
func (a *App) quiescentExchange(ctx context.Context, s *session, writes *quiescentWriting) string {
	if a.quiescentExchangeHook != nil {
		a.quiescentExchangeHook()
	}
	a.traceTerminal("deliver")
	writes.set(true)
	op, cancel := context.WithTimeout(ctx, nativeRPCTimeout)
	st, list, err := s.startDelivery(op, a.Now())
	cancel()
	writes.set(false)
	if err != nil {
		return "delivery"
	}
	if st.NextBatch == "" {
		return "cursor"
	}
	s.principals = s.readDeliveryPrincipals()
	mailbox := s.paths.MailboxFile(s.name)
	before := config.StampFile(mailbox)
	a.traceTerminal("poll")
	resp, err := s.client.Sync(ctx, st.NextBatch, terminalLongPoll, syncFilter)
	if err != nil {
		return "sync"
	}
	a.traceTerminal("ingest")
	writes.set(true)
	op, cancel = context.WithTimeout(ctx, nativeRPCTimeout)
	_, err = s.ingest(op, resp, st.NextBatch, list.Subs, nil, time.Now().Add(nativeRPCTimeout))
	cancel()
	writes.set(false)
	if err != nil {
		return "ingest"
	}
	if !before.Equal(config.StampFile(mailbox)) {
		return "mail"
	}
	return ""
}

func cancellablePoll(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(ctx)
}

// quiescentOwnerTeardown decides an owner wake directly from the retained
// owner association, before any connection, State or network work of a full
// cycle could delay it. A KNOWN replacement (the owner is absent, or its
// registered native start token changed to another valid token) stops at
// once, even when the second-resolution canonical Birth still matches; it is
// never re-registered as a new baseline. For an UNKNOWN inspection result the
// canonical Birth or process absence decides; otherwise the caller runs the
// ordinary full cycle. The stop record is best effort with a 500ms
// lock deadline, as a held lock must not keep a dead owner's consumer alive.
func (a *App) quiescentOwnerTeardown(ctx context.Context, s *session, generation string, policy config.TerminalBinding, known bool) (string, bool) {
	if !known {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		live, err := terminalBirth(probe, policy.OwnerPID)
		cancel()
		if !(err == nil && live != policy.OwnerBirth || processGone(policy.OwnerPID)) {
			return "", false
		}
	}
	if s.checkConnection() == nil {
		_, _ = a.updateStateBy(s.paths, s.name, time.Now().Add(500*time.Millisecond), func(st *config.State) error {
			if b := st.Terminal; b != nil && b.Generation == generation && !b.Stopped {
				b.Stopped = true
				b.Outcome = "target exited or replaced"
				dropRetiredStrandedAlarms(st)
			}
			return nil
		})
	}
	return "owner gone or replaced", true
}
