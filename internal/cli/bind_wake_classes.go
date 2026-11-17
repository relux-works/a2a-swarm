package cli

import (
	"encoding/json"
	"slices"
	"strconv"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Priority classes of a pending mailbox entry at wake decision time. The
// classes only decide when a wake is due; any wake carries every pending
// entry, and an entry never moves to an earlier class because it waited.
type wakeClass int

const (
	wakeBatch wakeClass = iota
	wakePeer
	wakeOwner
)

func (c wakeClass) String() string {
	switch c {
	case wakeOwner:
		return "owner"
	case wakePeer:
		return "peer"
	default:
		return "batch"
	}
}

const (
	defaultOwnerWakesPerHour = 20
	defaultPeerWakesPerHour  = 20
	defaultBatchInterval     = 30 * time.Minute
	minBatchInterval         = time.Minute
	maxBatchInterval         = 6 * time.Hour
)

// peerWakeKinds are the self-declared kinds that may wake at once when the
// message is addressed to this identity. They are metadata only; the class
// is bounded solely by its own hourly cap.
var peerWakeKinds = []string{"question", "request", "decision", "owner-needed"}

// classifyWakeEntry computes the class of one pending entry. Owner requires
// the exact host-pinned principal sender (principalMail); nothing in the
// body, display name, header or mentions can grant it. Peer requires the
// structured header to address this identity (name or user ID) with a peer
// kind. Everything else is batch.
func classifyWakeEntry(e config.MailEntry, principals []string, name, me string) wakeClass {
	if principalMail(e, principals) {
		return wakeOwner
	}
	if e.Kind != config.MailEvent {
		return wakeBatch
	}
	var ev matrix.Event
	if json.Unmarshal(e.Event, &ev) != nil || ev.Type != "m.room.message" || me != "" && ev.Sender == me {
		return wakeBatch
	}
	h, ok := decodeHeader(ev.Message().A2A)
	if !ok || !slices.Contains(peerWakeKinds, h.Kind) {
		return wakeBatch
	}
	for _, t := range h.To {
		if t != "" && (t == name || t == me) {
			return wakePeer
		}
	}
	return wakeBatch
}

func wakeClassCounts(entries []config.MailEntry, principals []string, name, me string) (owner, peer, batch int) {
	for _, e := range entries {
		switch classifyWakeEntry(e, principals, name, me) {
		case wakeOwner:
			owner++
		case wakePeer:
			peer++
		default:
			batch++
		}
	}
	return owner, peer, batch
}

// wakeClassesEnabled reports whether the recorded policy selects economy.
// An absent policy is the legacy single-cap behaviour.
func wakeClassesEnabled(p *config.WakeClassPolicy) bool {
	return p != nil && p.Mode == config.WakeClassEconomy
}

func wakeClassCaps(p *config.WakeClassPolicy) (owner, peer int, batch time.Duration) {
	owner, peer, batch = defaultOwnerWakesPerHour, defaultPeerWakesPerHour, defaultBatchInterval
	if p == nil {
		return owner, peer, batch
	}
	if p.OwnerPerHour > 0 {
		owner = p.OwnerPerHour
	}
	if p.PeerPerHour > 0 {
		peer = p.PeerPerHour
	}
	if p.BatchInterval > 0 {
		batch = p.BatchInterval
	}
	return owner, peer, batch
}

func pruneWakeHour(res []time.Time, now time.Time) []time.Time {
	return slices.DeleteFunc(slices.Clone(res), func(t time.Time) bool { return now.Sub(t) >= time.Hour })
}

// nextBatchDue is the next batch wake time: the batch interval after the
// previous wake that carried batch mail, or after the binding start. A
// policy with neither recorded is due at once.
func nextBatchDue(p *config.WakeClassPolicy) time.Time {
	_, _, batch := wakeClassCaps(p)
	base := p.LastBatchWake
	if base.IsZero() {
		base = p.StartedAt
	}
	if base.IsZero() {
		return time.Time{}
	}
	return base.Add(batch)
}

type wakeClassDecision struct {
	Due        bool
	Trigger    wakeClass
	Suppressed string // binding outcome when not due
	CapClass   wakeClass
	CapBlocked bool // an hourly cap, not the batch timer, holds the wake
}

// decideWakeClasses is the pure economy wake decision over the classes of
// the un-rung pending entries. Owner and peer allowances are independent;
// the batch timer delivers everything, including cap-exhausted entries.
func decideWakeClasses(p *config.WakeClassPolicy, classes []wakeClass, now time.Time) wakeClassDecision {
	if len(classes) == 0 {
		return wakeClassDecision{}
	}
	ownerCap, peerCap, _ := wakeClassCaps(p)
	hasOwner, hasPeer := slices.Contains(classes, wakeOwner), slices.Contains(classes, wakePeer)
	ownerUsed := len(pruneWakeHour(p.OwnerReservations, now))
	peerUsed := len(pruneWakeHour(p.PeerReservations, now))
	if hasOwner && ownerUsed < ownerCap {
		return wakeClassDecision{Due: true, Trigger: wakeOwner}
	}
	if hasPeer && peerUsed < peerCap {
		return wakeClassDecision{Due: true, Trigger: wakePeer}
	}
	if due := nextBatchDue(p); due.IsZero() || !now.Before(due) {
		return wakeClassDecision{Due: true, Trigger: wakeBatch}
	}
	if hasOwner && ownerUsed >= ownerCap {
		return wakeClassDecision{Suppressed: "suppressed: owner hourly cap", CapClass: wakeOwner, CapBlocked: true}
	}
	if hasPeer && peerUsed >= peerCap {
		return wakeClassDecision{Suppressed: "suppressed: peer hourly cap", CapClass: wakePeer, CapBlocked: true}
	}
	return wakeClassDecision{Suppressed: "suppressed: batch interval", CapClass: wakeBatch}
}

// applyWakeClassReservation spends the triggering class allowance and marks
// this wake as the last one that carried batch mail. Call under the state
// lock, in the same transaction as the legacy hourly reservation.
func applyWakeClassReservation(p *config.WakeClassPolicy, trigger wakeClass, now time.Time) {
	p.OwnerReservations = pruneWakeHour(p.OwnerReservations, now)
	p.PeerReservations = pruneWakeHour(p.PeerReservations, now)
	switch trigger {
	case wakeOwner:
		p.OwnerReservations = append(p.OwnerReservations, now)
	case wakePeer:
		p.PeerReservations = append(p.PeerReservations, now)
	}
	p.LastBatchWake = now
	p.OwnerSuppressed = false
	p.PeerSuppressed = false
}

// wakeClassStatus is the bind status projection of the recorded policy and
// the pending mailbox, counts only.
type wakeClassStatus struct {
	Mode         string     `json:"mode"`
	Reason       string     `json:"reason,omitempty"`
	OwnerUsed    int        `json:"owner_used"`
	PeerUsed     int        `json:"peer_used"`
	LastBatch    *time.Time `json:"last_batch_wake,omitempty"`
	NextBatch    *time.Time `json:"next_batch_due,omitempty"`
	PendingOwner int        `json:"pending_owner"`
	PendingPeer  int        `json:"pending_peer"`
	PendingBatch int        `json:"pending_batch"`
}

func wakeClassStatusOf(s *session, st config.State, now time.Time) wakeClassStatus {
	out := wakeClassStatus{Mode: config.WakeClassStandard}
	if p := st.WakeClasses; p != nil {
		if p.Mode == config.WakeClassEconomy {
			out.Mode = config.WakeClassEconomy
		}
		out.Reason = p.Reason
		out.OwnerUsed = len(pruneWakeHour(p.OwnerReservations, now))
		out.PeerUsed = len(pruneWakeHour(p.PeerReservations, now))
		if !p.LastBatchWake.IsZero() {
			last := p.LastBatchWake.UTC()
			out.LastBatch = &last
		}
		if due := nextBatchDue(p); !due.IsZero() {
			due = due.UTC()
			out.NextBatch = &due
		}
	}
	principals, _ := ownerFiles(s.paths, s.name).principals()
	if mb, err := config.OpenMailbox(s.paths, s.name); err == nil {
		out.PendingOwner, out.PendingPeer, out.PendingBatch = wakeClassCounts(mb.Pending(), principals, s.name, s.creds.UserID)
	}
	return out
}

func wakeClassTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Format(time.RFC3339Nano)
}

// recordWakeClassSuppression records a not-due economy decision. An hourly
// cap writes its outcome once per exhaustion episode; the flag clears on the
// next reserved wake. The entry stays in the mailbox either way.
func (a *App) recordWakeClassSuppression(s *session, generation string, d wakeClassDecision) error {
	if err := s.checkConnection(); err != nil {
		return err
	}
	_, err := a.updateState(s.paths, s.name, func(st *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		b := st.Terminal
		if b == nil || b.Generation != generation || b.Stopped {
			return errTerminalBindingRetired
		}
		b.Idle = nil
		if d.CapBlocked && st.WakeClasses != nil {
			flag := &st.WakeClasses.PeerSuppressed
			if d.CapClass == wakeOwner {
				flag = &st.WakeClasses.OwnerSuppressed
			}
			if !*flag {
				*flag = true
				b.Outcome = d.Suppressed
			}
			return nil
		}
		b.Outcome = d.Suppressed
		return nil
	})
	return err
}

// validWakeClassReason bounds an opt-out reason to 1..200 printable bytes.
func validWakeClassReason(s string) bool {
	if len(s) < 1 || len(s) > 200 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// wakeClassSelection is the economy choice carried by one bind invocation.
type wakeClassSelection struct {
	economy   bool
	noEconomy bool
	reason    string
	owner     int
	peer      int
	batch     time.Duration
}

// resolveWakeClassPolicy maps a bind selection to the policy to record, or
// nil to keep the recorded policy exactly. Without an explicit flag an
// existing (or absent legacy) policy is untouched: the explicit bind path
// never enables classes on its own. Switching mode preserves the recorded
// reservations and batch clock.
func resolveWakeClassPolicy(existing *config.WakeClassPolicy, sel wakeClassSelection, now time.Time) *config.WakeClassPolicy {
	if !sel.economy && !sel.noEconomy {
		return nil
	}
	var next *config.WakeClassPolicy
	if sel.noEconomy {
		next = &config.WakeClassPolicy{Mode: config.WakeClassStandard, Reason: sel.reason}
	} else {
		next = newEconomyPolicy(sel.owner, sel.peer, sel.batch, now)
	}
	if existing != nil {
		if !existing.StartedAt.IsZero() {
			next.StartedAt = existing.StartedAt
		}
		next.LastBatchWake = existing.LastBatchWake
		next.OwnerReservations = slices.Clone(existing.OwnerReservations)
		next.PeerReservations = slices.Clone(existing.PeerReservations)
		next.OwnerSuppressed = existing.OwnerSuppressed
		next.PeerSuppressed = existing.PeerSuppressed
	}
	return next
}

// autoBindArgs builds the SessionStart bind invocation. A brand-new binding
// (no recorded binding or policy) started by SessionStart selects economy;
// recovery and any existing recorded mode are left to the recorded policy.
func autoBindArgs(runtime string, owner int, sessionID string, st config.State, mode bindingMode) []string {
	args := []string{"--runtime", runtime, "--owner-pid", strconv.Itoa(owner), "--session-id", sessionID, "--interval", defaultBindInterval.String(), "--retry-max", defaultBindRetryMax.String(), "--max-wakes-per-hour", strconv.Itoa(defaultBindMaxWakes)}
	if mode == bindingSessionStart && st.Terminal == nil && st.WakeClasses == nil {
		args = append(args, "--economy")
	}
	return args
}

// newEconomyPolicy is the recorded economy policy with the given or default
// values, started at now (the binding start for the batch timer).
func newEconomyPolicy(owner, peer int, batch time.Duration, now time.Time) *config.WakeClassPolicy {
	if owner <= 0 {
		owner = defaultOwnerWakesPerHour
	}
	if peer <= 0 {
		peer = defaultPeerWakesPerHour
	}
	if batch <= 0 {
		batch = defaultBatchInterval
	}
	return &config.WakeClassPolicy{Mode: config.WakeClassEconomy, OwnerPerHour: owner, PeerPerHour: peer, BatchInterval: batch, StartedAt: now.UTC()}
}
