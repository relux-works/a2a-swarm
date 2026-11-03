package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

var ErrNotificationAttempt = errors.New("notification attempt unresolved or invalid")
var ErrNotificationAttemptCapacity = errors.New("notification attempt capacity exhausted")

const notificationAttemptLimit = 32
const notificationAttemptBytes = 65536

// HostNotificationBinding is the complete curator-host-notification/1 binding.
// Its references describe already admitted custody; decoding supplies no trust.
type HostNotificationBinding struct {
	Execution          string `json:"execution"`
	ControllerEpoch    string `json:"controller_epoch"`
	RunnerInstance     string `json:"runner_instance"`
	NativeThread       string `json:"native_thread"`
	MemberRef          string `json:"member_ref"`
	ConnectionRef      string `json:"connection_ref"`
	ConfigurationRef   string `json:"configuration_ref"`
	ProfileLockRef     string `json:"profile_lock_ref"`
	ConsumerGeneration string `json:"consumer_generation"`
	EnrollmentRef      string `json:"enrollment_ref"`
	WorkflowReference  string `json:"workflow_reference"`
	WorkflowSHA256     string `json:"workflow_sha256"`
}

func hostScalar(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, c := range s {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == ':' || c == '/' || c == '@' || c == '+' || c == '-') {
			return false
		}
	}
	return true
}

func lowerHex(s string, size int) bool {
	if len(s) != size {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func nonzeroDecimal(s string) bool {
	if len(s) == 0 || s[0] < '1' || s[0] > '9' {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	_, err := strconv.ParseUint(s, 10, 64)
	return err == nil
}

func (b HostNotificationBinding) valid() bool {
	return hostScalar(b.Execution) && nonzeroDecimal(b.ControllerEpoch) && hostScalar(b.RunnerInstance) && hostScalar(b.NativeThread) &&
		hostRef(b.MemberRef, 512) && hostRef(b.ConnectionRef, 128) && hostRef(b.ConfigurationRef, 512) && hostRef(b.ProfileLockRef, 512) &&
		hostRef(b.ConsumerGeneration, 128) && hostRef(b.EnrollmentRef, 512) && hostScalar(b.WorkflowReference) && lowerHex(b.WorkflowSHA256, 64)
}

// NotificationAttemptKey binds one source reservation to exact notice bytes and
// actual Host custody. Session is the source session, not the native thread.
type NotificationAttemptKey struct {
	Token        string                  `json:"token"`
	Session      string                  `json:"session"`
	Generation   string                  `json:"generation"`
	NoticeSHA256 string                  `json:"notice_sha256"`
	Binding      HostNotificationBinding `json:"binding"`
}

func (k NotificationAttemptKey) valid() bool {
	return lowerHex(k.Token, 32) && hostRef(k.Session, 200) && hostRef(k.Generation, 128) && lowerHex(k.NoticeSHA256, 64) && k.Binding.valid() && k.Generation == k.Binding.ConsumerGeneration
}

type NotificationAttempt struct {
	Key         NotificationAttemptKey `json:"key"`
	State       string                 `json:"state"`
	RequestID   string                 `json:"request_id,omitempty"`
	Sequence    string                 `json:"sequence,omitempty"`
	NativeStage string                 `json:"native_stage,omitempty"`
	Reason      string                 `json:"reason,omitempty"`
}

// NotificationAttempts retains every correlation, including terminal records.
// It has no clock, expiry, eviction, I/O, retry or mailbox-receipt operation.
// Mutations must be integrated with the shared reservation in one durable state
// transaction before this ledger can authorize an adapter operation.
type NotificationAttempts struct {
	Version int                   `json:"version"`
	Entries []NotificationAttempt `json:"entries"`
}

func nativeStage(s string) int {
	switch s {
	case "not-evidenced":
		return 1
	case "write-unresolved":
		return 2
	case "native-accepted":
		return 3
	case "native-completed":
		return 4
	}
	return 0
}

func triggerRefusal(s string) bool {
	switch s {
	case "unsupported-contract", "attachment-unavailable", "enrollment-unverified", "binding-conflict", "stale-binding", "input-not-ready", "external-held", "security-stopped", "capacity-full", "invalid-attempt", "workflow-conflict":
		return true
	}
	return false
}

func (l NotificationAttempts) Validate() error {
	if l.Version != 1 || l.Entries == nil || len(l.Entries) > notificationAttemptLimit {
		return ErrNotificationAttempt
	}
	active := 0
	tokens, requests := map[string]bool{}, map[string]bool{}
	type hostSequence struct {
		binding  HostNotificationBinding
		sequence string
	}
	sequences := map[hostSequence]bool{}
	for _, a := range l.Entries {
		if !a.Key.valid() || tokens[a.Key.Token] {
			return ErrNotificationAttempt
		}
		tokens[a.Key.Token] = true
		if a.RequestID != "" {
			if !hostRef(a.RequestID, 128) || requests[a.RequestID] {
				return ErrNotificationAttempt
			}
			requests[a.RequestID] = true
		}
		switch a.State {
		case "prepared":
			active++
			if a.RequestID != "" || a.Sequence != "" || a.NativeStage != "" || a.Reason != "" {
				return ErrNotificationAttempt
			}
		case "unknown":
			active++
			if a.RequestID == "" || a.Sequence != "" || a.NativeStage != "" || a.Reason != "" {
				return ErrNotificationAttempt
			}
		case "accepted":
			if a.RequestID == "" || !nonzeroDecimal(a.Sequence) || nativeStage(a.NativeStage) == 0 || a.Reason != "" {
				return ErrNotificationAttempt
			}
			key := hostSequence{a.Key.Binding, a.Sequence}
			if sequences[key] {
				return ErrNotificationAttempt
			}
			sequences[key] = true
		case "not-applied":
			if a.Sequence != "" || a.NativeStage != "" || !(a.RequestID == "" && a.Reason == "abandoned-before-io" || a.RequestID != "" && triggerRefusal(a.Reason)) {
				return ErrNotificationAttempt
			}
		default:
			return ErrNotificationAttempt
		}
	}
	if active > 1 {
		return ErrNotificationAttempt
	}
	data, err := json.Marshal(l)
	if err != nil || len(data) > notificationAttemptBytes {
		return ErrNotificationAttemptCapacity
	}
	return nil
}

// Strict decoding keeps future or malformed state from being silently dropped.
// Size is checked before tokenization or allocations proportional to its fields.
func (l *NotificationAttempts) UnmarshalJSON(data []byte) error {
	if len(data) > notificationAttemptBytes || !utf8.Valid(data) {
		return ErrNotificationAttempt
	}
	d := json.NewDecoder(bytes.NewReader(data))
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil || token == nil {
			return ErrNotificationAttempt
		}
	}
	type plain NotificationAttempts
	var next plain
	if err := DecodeConnectionJSON(data, &next); err != nil {
		return ErrNotificationAttempt
	}
	// encoding/json matches struct tags case-insensitively. This closed schema
	// permits only its literal ASCII field names, including inside the binding.
	var fields any
	if err := json.Unmarshal(data, &fields); err != nil || !literalAttemptFields(fields) {
		return ErrNotificationAttempt
	}
	if err := NotificationAttempts(next).Validate(); err != nil {
		return err
	}
	*l = NotificationAttempts(next)
	return nil
}

func literalAttemptFields(value any) bool {
	switch v := value.(type) {
	case map[string]any:
		for key, child := range v {
			for _, c := range key {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
					return false
				}
			}
			if !literalAttemptFields(child) {
				return false
			}
		}
	case []any:
		for _, child := range v {
			if !literalAttemptFields(child) {
				return false
			}
		}
	}
	return true
}

func (l *NotificationAttempts) Prepare(k NotificationAttemptKey) error {
	if !k.valid() {
		return ErrNotificationAttempt
	}
	next := *l
	if next.Version == 0 && next.Entries == nil {
		next = NotificationAttempts{Version: 1, Entries: []NotificationAttempt{}}
	}
	if err := next.Validate(); err != nil {
		return err
	}
	for _, a := range next.Entries {
		if a.Key.Token == k.Token || a.State == "prepared" || a.State == "unknown" {
			return ErrNotificationAttempt
		}
	}
	if len(next.Entries) == notificationAttemptLimit {
		return ErrNotificationAttemptCapacity
	}
	next.Entries = append(append([]NotificationAttempt{}, next.Entries...), NotificationAttempt{Key: k, State: "prepared"})
	if err := next.Validate(); err != nil {
		return err
	}
	*l = next
	return nil
}

// change uses a detached slice so every refused mutation preserves old bytes.
func (l *NotificationAttempts) change(k NotificationAttemptKey, fn func(*NotificationAttempt) error) error {
	if err := l.Validate(); err != nil {
		return err
	}
	for i, a := range l.Entries {
		if a.Key != k {
			continue
		}
		if err := fn(&a); err != nil {
			return err
		}
		next := NotificationAttempts{Version: l.Version, Entries: append([]NotificationAttempt{}, l.Entries...)}
		next.Entries[i] = a
		if err := next.Validate(); err != nil {
			return err
		}
		*l = next
		return nil
	}
	return ErrNotificationAttempt
}

// Begin must be durably saved before the one trigger call. UNKNOWN immediately
// fences crashes and lost responses; the request ID never authorizes a resend.
func (l *NotificationAttempts) Begin(k NotificationAttemptKey, requestID string) error {
	return l.change(k, func(a *NotificationAttempt) error {
		if a.State != "prepared" || !hostRef(requestID, 128) {
			return ErrNotificationAttempt
		}
		a.State, a.RequestID = "unknown", requestID
		return nil
	})
}

// Accept consumes ONLY an authoritative matching retained Host observation from
// the admitted transport. An API caller or parsed JSON alone supplies no proof.
func (l *NotificationAttempts) Accept(k NotificationAttemptKey, sequence, stage string) error {
	return l.change(k, func(a *NotificationAttempt) error {
		if !nonzeroDecimal(sequence) || nativeStage(stage) == 0 || !(a.State == "unknown" || a.State == "accepted") {
			return ErrNotificationAttempt
		}
		if a.State == "accepted" && (a.Sequence != sequence || nativeStage(stage) < nativeStage(a.NativeStage)) {
			return ErrNotificationAttempt
		}
		a.State, a.Sequence, a.NativeStage = "accepted", sequence, stage
		return nil
	})
}

// Refuse applies only the conclusive response to the original trigger call.
// A lookup miss, refusal of a later call, EOF or timeout must never call this.
func (l *NotificationAttempts) Refuse(k NotificationAttemptKey, requestID, reason string) error {
	return l.change(k, func(a *NotificationAttempt) error {
		if a.State != "unknown" || a.RequestID != requestID || !triggerRefusal(reason) {
			return ErrNotificationAttempt
		}
		a.State, a.Reason = "not-applied", reason
		return nil
	})
}

// Abandon is permitted only while no trigger I/O has been authorized.
func (l *NotificationAttempts) Abandon(k NotificationAttemptKey) error {
	return l.change(k, func(a *NotificationAttempt) error {
		if a.State != "prepared" {
			return ErrNotificationAttempt
		}
		a.State, a.Reason = "not-applied", "abandoned-before-io"
		return nil
	})
}
