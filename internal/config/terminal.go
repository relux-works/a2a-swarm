package config

import (
	"encoding/json"
	"time"

	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// NoticeRetry belongs to the identity, not a terminal generation. Only an
// acknowledged own-status PUT clears it. Times are wall clock across restart;
// a rollback waits for the stored eligibility time, without a freshness claim.
type NoticeRetry struct {
	FailedAt time.Time     `json:"failed_at"`
	Next     time.Time     `json:"next"`
	Delay    time.Duration `json:"delay"`
}

// TerminalBinding contains only local process metadata and event IDs. A wake
// reservation is independent of inbox receipts and survives an uncertain send.
type TerminalBinding struct {
	NotificationToken  string                     `json:"notification_token,omitempty"`
	OwnNotice          OwnNoticeObservation       `json:"own_notice"`
	NoticePublication  string                     `json:"notice_publication"`
	NoticeReport       *NoticeReport              `json:"notice_report,omitempty"`
	Generation         string                     `json:"generation"`
	SessionID          string                     `json:"session_id"`
	Host               string                     `json:"host"`
	OwnerPID           int                        `json:"owner_pid"`
	OwnerBirth         string                     `json:"owner_birth"`
	DaemonPID          int                        `json:"daemon_pid"`
	DaemonBirth        string                     `json:"daemon_birth"`
	Target             tmuxwake.Target            `json:"target"`
	ClaudeRegistryRoot string                     `json:"claude_registry_root,omitempty"`
	NoticeOrigin       *TerminalIdle              `json:"notice_origin,omitempty"`
	NoticePosition     *tmuxwake.ComposerPosition `json:"notice_position,omitempty"`
	TypingStarted      bool                       `json:"typing_started,omitempty"`
	Recovery           *TerminalRecovery          `json:"recovery,omitempty"`
	SendRefusal        *TerminalSendRefusal       `json:"send_refusal,omitempty"`
	Readiness          *TerminalReadiness         `json:"readiness,omitempty"`
	Interval           time.Duration              `json:"interval"`
	RetryMax           time.Duration              `json:"retry_max"`
	MaxWakes           int                        `json:"max_wakes_per_hour"`
	Stopped            bool                       `json:"stopped"`
	Outcome            string                     `json:"outcome"`
	AttemptIDs         []string                   `json:"attempt_ids,omitempty"`
	Notice             string                     `json:"notice,omitempty"`
	NoticeCount        int                        `json:"notice_count,omitempty"`
	AttemptAt          time.Time                  `json:"attempt_at,omitempty"`
	Retried            bool                       `json:"retried,omitempty"`
	Typed              bool                       `json:"typed,omitempty"` // known not to have sent Enter
	Rung               []string                   `json:"rung,omitempty"`
	Reservations       []time.Time                `json:"reservations,omitempty"`
	Idle               *TerminalIdle              `json:"idle_observation,omitempty"`
}

// NoticeReport pins an incident transaction before its network effect.
// Retries after an ambiguous ACK or restart reuse the same room and content.
type NoticeReport struct {
	Alarm             bool                 `json:"alarm,omitempty"`
	AlarmOwner        string               `json:"alarm_owner,omitempty"`
	AlarmLead         string               `json:"alarm_lead,omitempty"`
	AlarmLeadRequired bool                 `json:"alarm_lead_required,omitempty"`
	LoggedOutcome     string               `json:"logged_outcome,omitempty"`
	Since             string               `json:"since"`
	Message           LivenessAlertMessage `json:"message"`
	Sent              bool                 `json:"sent"`
	RetryAt           time.Time            `json:"retry_at"`
	Outcome           string               `json:"outcome"`
}

// Qualification is scoped to the exact consumer, policy and target. It is not
// carried into a replacement daemon or a session handover.
type TerminalIdle struct {
	At                 time.Time               `json:"at"`
	Generation         string                  `json:"generation"`
	SessionID          string                  `json:"session_id"`
	Host               string                  `json:"host"`
	DaemonPID          int                     `json:"daemon_pid"`
	DaemonBirth        string                  `json:"daemon_birth"`
	Target             tmuxwake.Target         `json:"target"`
	Interval           time.Duration           `json:"interval"`
	ClaudeRegistryRoot string                  `json:"claude_registry_root,omitempty"`
	RegistrySessionID  string                  `json:"registry_session_id,omitempty"`
	RegistryVersion    string                  `json:"registry_version,omitempty"`
	RegistryStatus     string                  `json:"registry_status,omitempty"`
	RegistryStatusAt   int64                   `json:"registry_status_at,omitempty"`
	ReadinessRoute     tmuxwake.ReadinessRoute `json:"readiness_route,omitempty"`
	ComposerRow        int                     `json:"composer_row"`
	ComposerColumn     int                     `json:"composer_column"`
	PaneWidth          int                     `json:"pane_width"`
}

// Diagnostic only. A persisted record never supplies a live observation key.
type TerminalReadiness struct {
	UnknownKeys string                 `json:"unknown_keys,omitempty"`
	Reason      tmuxwake.RuntimeReason `json:"reason"`
	Version     string                 `json:"version,omitempty"`
	Since       time.Time              `json:"since"`
}

// Corrupt diagnostic fields do not become authority or make a raw time/parser
// error the status output. The live observation supplies the replacement view.
func (r *TerminalReadiness) UnmarshalJSON(data []byte) error {
	*r = TerminalReadiness{}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	_ = json.Unmarshal(fields["reason"], &r.Reason)
	_ = json.Unmarshal(fields["version"], &r.Version)
	_ = json.Unmarshal(fields["since"], &r.Since)
	_ = json.Unmarshal(fields["unknown_keys"], &r.UnknownKeys)
	r.UnknownKeys = tmuxwake.SafeRegistryUnknownKeys(r.UnknownKeys)
	return nil
}

// OwnNoticeObservation records rendered evidence or aged retained-journal
// suspicion, never editor ownership.
// Nil is unknown. Unknown retains the last suspicion's reason/since until fresh clear.
type OwnNoticeObservation struct {
	PossiblyOrphaned *bool  `json:"own_notice_possibly_orphaned"`
	Reason           string `json:"reason,omitempty"`
	Since            string `json:"since,omitempty"`
	ObservedAt       string `json:"observed_at,omitempty"`
}

// TerminalSendRefusal is the last refused Send observation, not permission.
// Optional for old journals; invalid diagnostics never block journal loading.
type TerminalSendRefusal struct {
	Reason tmuxwake.SendReason `json:"reason"`
	At     time.Time           `json:"at"`
}

func (r *TerminalSendRefusal) UnmarshalJSON(data []byte) error {
	*r = TerminalSendRefusal{Reason: tmuxwake.ReasonUnavailable}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	_ = json.Unmarshal(fields["reason"], &r.Reason)
	_ = json.Unmarshal(fields["at"], &r.At)
	r.Reason = tmuxwake.SafeSendReason(r.Reason)
	return nil
}

// TerminalRecovery is bounded diagnostic history, never input authority.
type TerminalRecovery struct {
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

const TerminalRecovered = "owned_notice_submitted"

func (r *TerminalRecovery) UnmarshalJSON(data []byte) error {
	*r = TerminalRecovery{}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return nil
	}
	var reason string
	_ = json.Unmarshal(fields["reason"], &reason)
	_ = json.Unmarshal(fields["at"], &r.At)
	if reason == TerminalRecovered {
		r.Reason = reason
	}
	return nil
}
