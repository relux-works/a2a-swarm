package config

import (
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
	"time"
)

// TerminalBinding contains only local process metadata and event IDs. A wake
// reservation is independent of inbox receipts and survives an uncertain send.
type TerminalBinding struct {
	Generation   string          `json:"generation"`
	SessionID    string          `json:"session_id"`
	Host         string          `json:"host"`
	OwnerPID     int             `json:"owner_pid"`
	OwnerBirth   string          `json:"owner_birth"`
	DaemonPID    int             `json:"daemon_pid"`
	DaemonBirth  string          `json:"daemon_birth"`
	Target       tmuxwake.Target `json:"target"`
	Interval     time.Duration   `json:"interval"`
	RetryMax     time.Duration   `json:"retry_max"`
	MaxWakes     int             `json:"max_wakes_per_hour"`
	Stopped      bool            `json:"stopped"`
	Outcome      string          `json:"outcome"`
	AttemptIDs   []string        `json:"attempt_ids,omitempty"`
	Notice       string          `json:"notice,omitempty"`
	NoticeCount  int             `json:"notice_count,omitempty"`
	AttemptAt    time.Time       `json:"attempt_at,omitempty"`
	Retried      bool            `json:"retried,omitempty"`
	Typed        bool            `json:"typed,omitempty"` // known not to have sent Enter
	Rung         []string        `json:"rung,omitempty"`
	Reservations []time.Time     `json:"reservations,omitempty"`
}
