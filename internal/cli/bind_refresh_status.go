package cli

import (
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The diagnostic uses exactly the refresh safety predicates, without mutating
// the notification ledger or granting any delivery/re-exec authority.
func terminalRefreshBlockReason(st config.State, generation, birth, host string, pid int, now time.Time) string {
	ledger := st.Notification
	if ledger.Prepare(now, time.Time{}) != nil {
		return "changed_invalid_notification"
	}
	b := st.Terminal
	if st.HostEnrollment != nil || st.Runtime != nil && !st.Runtime.Retired ||
		b == nil || b.Stopped || b.Generation != generation || b.Host != host ||
		b.DaemonPID != pid || b.DaemonBirth != birth {
		return "changed_authority"
	}
	if len(b.AttemptIDs) != 0 || b.Typed || b.Notice != "" {
		return "changed_pending_attempt"
	}
	if st.Notification.Reservation != nil {
		return "changed_reservation"
	}
	if st.Lease == nil || st.Lease.Holder != b.SessionID || st.Lease.Host != b.Host || !st.Lease.Expires.After(now) {
		return "changed_lease"
	}
	return ""
}

// Keep the last failure visible while its check is throttled, so a persistent
// safety, probe, pinning or candidate failure does not cause periodic saves.
func terminalRefreshCooldownStatus(last string) string {
	switch last {
	case "changed_pending_attempt", "changed_reservation", "changed_lease",
		"changed_authority", "changed_invalid_notification", "changed_safety_unavailable",
		"changed_probe_unavailable", "changed_pin_unavailable", "changed_candidate_unavailable":
		return last
	default:
		return "changed_cooldown"
	}
}

func safeTerminalRefreshStatus(token string) string {
	switch token {
	case "startup_ready", "startup_keychain_cooldown", "startup_keychain_locked", "startup_keychain_unavailable", "startup_companion_unavailable",
		"current", "unavailable", "changed_stabilizing", "changed_cooldown",
		"changed_pending_attempt", "changed_reservation", "changed_lease",
		"changed_authority", "changed_invalid_notification", "changed_safety_unavailable",
		"changed_probe_unavailable", "changed_protocol_incompatible", "changed_exec_failed",
		"changed_candidate_unavailable", "changed_pin_unavailable", "changed_ready":
		return token
	default:
		return "unavailable"
	}
}

func terminalRefreshStatus(b *config.TerminalBinding) string {
	if b == nil || b.Stopped {
		return "unavailable"
	}
	return safeTerminalRefreshStatus(b.RefreshStatus)
}

// Only the owning live consumer can change this diagnostic. No journal,
// lease, heartbeat, mailbox or send state is changed by the projection.
func recordTerminalRefreshStatus(st *config.State, generation, host string, pid int, token string) bool {
	b := st.Terminal
	if b == nil || b.Stopped || b.Generation != generation || b.Host != host || b.DaemonPID != pid {
		return false
	}
	token = safeTerminalRefreshStatus(token)
	if b.RefreshStatus == token {
		return false
	}
	b.RefreshStatus = token
	return true
}
