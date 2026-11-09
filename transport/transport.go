// Package transport defines the versioned messaging boundary for external transports.
// Sender identity is authenticated by the transport; claimed time never orders delivery.
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const APIVersion = 1

type ConversationID string
type EventID string
type PrincipalID string
type Cursor []byte

type Principal struct {
	ID          PrincipalID
	DisplayName string
}
type Message struct {
	Kind        string
	Text        string
	To          []PrincipalID
	Re          EventID
	Thread      EventID
	Attachments []MediaRef
	Extensions  map[string]json.RawMessage
}
type SendRequest struct {
	Conversation   ConversationID
	Message        Message
	IdempotencyKey string
}
type Outcome string

const (
	ACK     Outcome = "ACK"
	UNKNOWN Outcome = "UNKNOWN"
	REFUSED Outcome = "REFUSED"
)

// Event is set only for ACK. A partial publication is UNKNOWN, even if its logical ID is known.
type SendResult struct {
	Outcome Outcome
	Event   EventID
	Refusal string
}
type Event struct {
	ID           EventID
	Conversation ConversationID
	Sender       PrincipalID
	Time         time.Time
	Message      Message
	Redacted     bool
	Raw          []byte
}
type Batch struct {
	Events  []Event
	Invites []Invite
	Gaps    []Gap
	Next    Cursor
	Dropped int
}
type GapReason string

const (
	TimelineLimited  GapReason = "timeline_limited"
	RelayUnreachable GapReason = "relay_unreachable"
	RelaySetChanged  GapReason = "relay_set_changed"
	WindowExceeded   GapReason = "window_exceeded"
)

func (r GapReason) Valid() bool {
	switch r {
	case TimelineLimited, RelayUnreachable, RelaySetChanged, WindowExceeded:
		return true
	}
	return false
}

type Gap struct {
	Conversation ConversationID
	Reason       GapReason
}
type Invite struct {
	Conversation ConversationID
	Sender       PrincipalID
}
type Page struct {
	Events  []Event
	Next    Cursor
	Dropped int
}
type Member struct {
	Principal  PrincipalID
	Membership string
}

// MediaRef is opaque to callers. Only a transport should interpret Opaque().
// All fmt verbs, including %#v and %q, redact its payload.
type MediaRef struct{ value string }

func NewMediaRef(value string) MediaRef          { return MediaRef{value: value} }
func (r MediaRef) Opaque() string                { return r.value }
func (r MediaRef) String() string                { return "MediaRef(redacted)" }
func (r MediaRef) GoString() string              { return "MediaRef(redacted)" }
func (r MediaRef) Format(s fmt.State, verb rune) { _, _ = io.WriteString(s, "MediaRef(redacted)") }

type MediaMeta struct {
	Name        string
	ContentType string
	Size        int64
	SHA256      string
}
type RedactionMode string

const (
	NoRedaction RedactionMode = "none"
	BestEffort  RedactionMode = "best_effort"
	Enforced    RedactionMode = "enforced"
)

type MembershipMode string

const (
	ServerEnforced MembershipMode = "server_enforced"
	SenderSide     MembershipMode = "sender_side"
)

type Capabilities struct {
	Redaction  RedactionMode
	Receipts   bool
	Threads    bool
	Membership MembershipMode
	// MaxContentBytes bounds the serialized transport content, including envelope overhead.
	// A harness supplies a MessageAtSize hook because encoding is transport-specific.
	MaxContentBytes int
	IdempotentRetry bool
}
type ConversationCaps struct{ E2EE bool }
type SyncOptions struct {
	Timeout time.Duration
	Initial bool
}

type Transport interface {
	Self(context.Context) (Principal, error)
	Caps() Capabilities
	ConversationCaps(context.Context, ConversationID) (ConversationCaps, error)
	Addressing() Addressing
	Send(context.Context, SendRequest) (SendResult, error)
	Sync(context.Context, Cursor, SyncOptions) (Batch, error)
	History(context.Context, ConversationID, []byte, int) (Page, error)
	Event(context.Context, ConversationID, EventID) (Event, error)
	Members(context.Context, ConversationID) ([]Member, error)
	Upload(context.Context, io.Reader, MediaMeta) (MediaRef, error)
	Download(context.Context, MediaRef, io.Writer, int64) (MediaMeta, error)
	Close() error
}
type Addressing interface {
	Principal(string) (PrincipalID, error)
	Conversation(context.Context, string) (ConversationID, error)
	Format(PrincipalID) string
}

// Optional interfaces are discovered by type assertion. Unsupported reports a
// fixed refusal when an interface is absent; callers must never fall back.
func Unsupported() error { return NewError(UnsupportedByTransport) }

type Rooms interface {
	Create(context.Context, string) (ConversationID, error)
	Join(context.Context, string) (ConversationID, error)
	Leave(context.Context, ConversationID) error
	Invite(context.Context, ConversationID, PrincipalID) error
	Alias(context.Context, ConversationID, string) error
}
type RoomState interface {
	GetState(context.Context, ConversationID, string, string) (json.RawMessage, error)
	PutState(context.Context, ConversationID, string, string, json.RawMessage) (EventID, error)
}
type AccountData interface {
	GetAccountData(context.Context, string) (json.RawMessage, error)
	PutAccountData(context.Context, string, json.RawMessage) error
}
type Presence interface {
	SetPresence(context.Context, string) error
}
type Registration interface {
	Register(context.Context, map[string]string) (Principal, error)
}
type Redactor interface {
	Redact(context.Context, ConversationID, EventID, string) (SendResult, error)
}
