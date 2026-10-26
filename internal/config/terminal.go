package config

import (
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"time"
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
	NotificationToken string               `json:"notification_token,omitempty"`
	OwnNotice         OwnNoticeObservation `json:"own_notice"`
	NoticePublication string               `json:"notice_publication"`
	Generation        string               `json:"generation"`
	SessionID         string               `json:"session_id"`
	Host              string               `json:"host"`
	OwnerPID          int                  `json:"owner_pid"`
	OwnerBirth        string               `json:"owner_birth"`
	DaemonPID         int                  `json:"daemon_pid"`
	DaemonBirth       string               `json:"daemon_birth"`
	Target            tmuxwake.Target      `json:"target"`
	Interval          time.Duration        `json:"interval"`
	RetryMax          time.Duration        `json:"retry_max"`
	MaxWakes          int                  `json:"max_wakes_per_hour"`
	Stopped           bool                 `json:"stopped"`
	Outcome           string               `json:"outcome"`
	AttemptIDs        []string             `json:"attempt_ids,omitempty"`
	Notice            string               `json:"notice,omitempty"`
	NoticeCount       int                  `json:"notice_count,omitempty"`
	AttemptAt         time.Time            `json:"attempt_at,omitempty"`
	Retried           bool                 `json:"retried,omitempty"`
	Typed             bool                 `json:"typed,omitempty"` // known not to have sent Enter
	Rung              []string             `json:"rung,omitempty"`
	Reservations      []time.Time          `json:"reservations,omitempty"`
	Idle              *TerminalIdle        `json:"idle_observation,omitempty"`
}

// Qualification is scoped to the exact consumer, policy and target. It is not
// carried into a replacement daemon or a session handover.
type TerminalIdle struct {
	At          time.Time       `json:"at"`
	Generation  string          `json:"generation"`
	SessionID   string          `json:"session_id"`
	Host        string          `json:"host"`
	DaemonPID   int             `json:"daemon_pid"`
	DaemonBirth string          `json:"daemon_birth"`
	Target      tmuxwake.Target `json:"target"`
	Interval    time.Duration   `json:"interval"`
}

// OwnNoticeObservation records rendered evidence, never editor ownership.
// Nil is unknown. Unknown retains the last suspicion's reason/since until fresh clear.
type OwnNoticeObservation struct {
	PossiblyOrphaned *bool  `json:"own_notice_possibly_orphaned"`
	Reason           string `json:"reason,omitempty"`
	Since            string `json:"since,omitempty"`
	ObservedAt       string `json:"observed_at,omitempty"`
}
