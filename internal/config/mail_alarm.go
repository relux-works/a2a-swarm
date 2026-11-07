package config

import (
	"bytes"
	"encoding/json"
	"time"
)

// MailAlarmConfig is local per-identity policy, independent of binding generations.
type MailAlarmConfig struct {
	Lead      string `json:"lead,omitempty"`
	LeadRoom  string `json:"lead_room,omitempty"`
	LeadTopic string `json:"lead_topic,omitempty"`
	Minutes   int    `json:"minutes"`
	Owner     string `json:"owner"` // auto, off, or an explicit room
	Operator  string `json:"operator,omitempty"`
	Room      string `json:"room,omitempty"`
	Topic     string `json:"topic,omitempty"`
	Invalid   bool   `json:"-"`
}

func (c *MailAlarmConfig) UnmarshalJSON(b []byte) error {
	type plain MailAlarmConfig
	var p plain
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) || json.Unmarshal(b, &p) != nil {
		*c = MailAlarmConfig{Invalid: true}
		return nil
	}
	*c = MailAlarmConfig(p)
	return nil
}

// MailAlarm is a durable episode fence and outbox, never a mailbox receipt.
type MailAlarm struct {
	EpisodeMinutes int            `json:"episode_minutes,omitempty"`
	Active         bool           `json:"active"`
	LastObserved   time.Time      `json:"last_observed,omitempty"`
	IdleSince      time.Time      `json:"idle_since,omitempty"`
	LastAlert      time.Time      `json:"last_alert,omitempty"`
	Jobs           []MailAlarmJob `json:"jobs,omitempty"`
	Invalid        bool           `json:"invalid,omitempty"`
}

func (a *MailAlarm) UnmarshalJSON(b []byte) error {
	type plain MailAlarm
	var p plain
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("{")) || json.Unmarshal(b, &p) != nil {
		*a = MailAlarm{Active: true, Invalid: true}
		return nil
	}
	*a = MailAlarm(p)
	return nil
}

type MailAlarmJob struct {
	Kind         string        `json:"kind,omitempty"`
	Since        string        `json:"since,omitempty"`
	Failure      string        `json:"failure,omitempty"`
	AutoOwner    string        `json:"auto_owner,omitempty"`
	ID           string        `json:"id"`
	Target       string        `json:"target,omitempty"`
	Room         string        `json:"room"`
	Topic        string        `json:"topic,omitempty"`
	ResolvedRoom string        `json:"resolved_room,omitempty"`
	Root         string        `json:"root,omitempty"`
	Identity     string        `json:"identity"`
	Host         string        `json:"host"`
	Count        int           `json:"count"`
	Minutes      int64         `json:"minutes"`
	Reason       string        `json:"reason"`
	At           time.Time     `json:"at"`
	Next         time.Time     `json:"next"`
	Delay        time.Duration `json:"delay"`
	// Reserved before sending: crash or ambiguous transport spends one attempt.
	Uncertain int `json:"uncertain"`
}
