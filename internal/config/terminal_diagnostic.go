package config

import (
	"slices"
	"time"
)

// TerminalDiagnostic is metadata only. Safe projects untrusted/legacy state to
// closed codes; neither error text nor terminal captures belong in this record.
type TerminalDiagnostic struct {
	Phase  string     `json:"phase"`
	Class  string     `json:"class"`
	Result string     `json:"result"`
	At     *time.Time `json:"observed_at,omitempty"`
}

func (d TerminalDiagnostic) Safe() TerminalDiagnostic {
	if !slices.Contains([]string{"state", "connection", "authority", "delivery_start", "delivery_position", "delivery_sync", "qualification", "dispatch_lock", "mailbox", "dispatch", "unknown"}, d.Phase) ||
		!slices.Contains([]string{"none", "unavailable", "invalid", "timeout", "cancelled", "locked", "permission", "lease_conflict", "authentication", "remote", "transport", "target_gone", "owner_unavailable", "target_unavailable", "hook_pause", "manual_gate", "native_binding", "binding_authority", "lease_authority", "no_new_mail", "input_unqualified", "idle_sample", "hourly_rate", "pending_input", "legacy_attempt", "partial_attempt", "retry_held", "draft_retained", "target_or_width", "submitted"}, d.Class) ||
		!slices.Contains([]string{"error", "suppressed", "qualifying", "submitted", "uncertain", "idle", "unknown"}, d.Result) || d.At != nil && (d.At.IsZero() || d.At.Year() < 1 || d.At.Year() > 9999) {
		return TerminalDiagnostic{Phase: "unknown", Class: "invalid", Result: "unknown"}
	}
	return d
}
