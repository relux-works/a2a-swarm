// Package transport defines the versioned messaging boundary for external transports.
// Sender identity is authenticated by the transport; claimed time never orders delivery.
// All methods, including Addressing and optional extensions, are safe for concurrent use.
// Methods with a context honor cancellation/deadlines and return closed error codes.
// Returned cursors, Raw, slices and maps belong to the caller and may be retained:
// transports never mutate them after return. Inputs are borrowed only for the call.
// After Close, operations that return an error return unavailable; Close is idempotent.
// Caps remains usable after Close. Within Addressing, only Addressing() and Format
// survive Close; Principal and Conversation return unavailable.
package transport

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"
)

const APIVersion = 1

// MaxRawBytes is the absolute ceiling for received Raw payloads. Each transport
// declares its own exposure bound in Caps().MaxRawBytes; oversized events are dropped.
const MaxRawBytes = 1 << 20

type ConversationID string
type EventID string
type PrincipalID string
type Cursor []byte

type Principal struct {
	ID          PrincipalID
	DisplayName string
}

// Message is the transport-independent payload. To names explicit recipients;
// an empty To addresses the conversation audience. To never suppresses the self
// copy required by the transport delivery rule. Duplicate recipients are one part.
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
	Refusal Refusal
}
type Event struct {
	ID           EventID
	Conversation ConversationID
	Sender       PrincipalID
	Time         time.Time
	Message      Message
	Redacted     bool
	// Raw is optional, opaque wire data. Core never parses it or depends on its
	// presence; the mailbox stores it only if present. Caps().MaxRawBytes bounds
	// every read path, with zero meaning Raw is never exposed.
	Raw []byte
}

// Batch is a delivery window. Dropped counts rejected events (for example an
// unverifiable sender or an oversized payload), never pending recipient copies.
// A cursor must not pass a pending deliverable event unless a Gap reports loss.
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

// Page owns its events and cursor. Dropped has the same meaning as Batch.Dropped.
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
	// MaxRawBytes is 0 when Raw is never exposed on any read path. Values in
	// 1..transport.MaxRawBytes bound exposed Raw; larger received payloads are
	// dropped, never truncated. Values outside 0..transport.MaxRawBytes are invalid.
	MaxRawBytes     int
	IdempotentRetry bool
}
type ConversationCaps struct{ E2EE bool }

// SyncOptions controls long polling. Timeout is a maximum server wait (zero
// means return available events immediately); the context deadline also applies.
// Initial permits an empty cursor to establish a fresh window. That window may
// contain recent history and must report known loss via Gap. It is never an
// implicit fallback for an invalid resume cursor.
type SyncOptions struct {
	Timeout time.Duration
	Initial bool
}

// Transport is a concurrently usable connection for one authenticated identity.
// Context, ownership and Close rules in the package documentation apply to every method.
type Transport interface {
	// Self returns the stable authenticated principal; unavailable after Close.
	Self(context.Context) (Principal, error)
	// Caps returns immutable transport-wide limits and delivery capabilities, even after Close.
	Caps() Capabilities
	// ConversationCaps returns known-conversation capabilities; unknown IDs give invalid_reference.
	ConversationCaps(context.Context, ConversationID) (ConversationCaps, error)
	// Addressing returns a concurrent parser/formatter; its pure metadata remains available after Close.
	Addressing() Addressing
	// Send scopes IdempotencyKey to this transport identity. A retry of the same
	// request preserves its logical EventID and sends only unanswered parts.
	// ACK requires every part, including any required self copy. Partial or
	// uncertain submission (including a timeout after submit) is UNKNOWN, without
	// Event or Refusal. REFUSED carries a closed Refusal only for a definite
	// rejection. Errors are exclusively pre-I/O failures. The key must not be
	// reused with a different request. To follows Message's recipient semantics.
	Send(context.Context, SendRequest) (SendResult, error)
	// Sync resumes from a caller-owned cursor. Initial alone permits an empty
	// cursor; invalid/foreign cursors give invalid_cursor, never a reset to now.
	// Timeout bounds polling, cancellation bounds the whole call. Dropped counts
	// rejected events; gaps use closed reasons. Time has no ordering significance.
	// Returned slices/Raw/Next may be retained and are never mutated by the transport.
	Sync(context.Context, Cursor, SyncOptions) (Batch, error)
	// History returns at most limit events; nonpositive limits give invalid_reference.
	// Large limits may be clamped. Empty page starts history; empty Next ends it.
	// Invalid/foreign page cursors give invalid_cursor and leave valid cursors usable.
	// Events/Raw/Next are caller-owned; Dropped counts rejected events as in Sync.
	History(context.Context, ConversationID, Cursor, int) (Page, error)
	// Event returns the authenticated event projection used by Sync, or invalid_reference.
	// Its Raw and payload slices/maps are caller-owned and never mutated after return.
	Event(context.Context, ConversationID, EventID) (Event, error)
	// Members returns a caller-owned membership snapshot for a known conversation.
	Members(context.Context, ConversationID) ([]Member, error)
	// Upload consumes r during the call and returns an opaque media reference.
	Upload(context.Context, io.Reader, MediaMeta) (MediaRef, error)
	// Download writes at most limit bytes during the call; overflow gives content_too_large.
	Download(context.Context, MediaRef, io.Writer, int64) (MediaMeta, error)
	// Close releases resources, is idempotent, and makes error-returning operations unavailable.
	Close() error
}

// Addressing methods are safe concurrently, including during Close.
type Addressing interface {
	// Principal parses a supported reference or gives invalid_reference; unavailable after Close.
	Principal(string) (PrincipalID, error)
	// Conversation resolves a supported reference or gives invalid_reference; honors ctx and Close.
	Conversation(context.Context, string) (ConversationID, error)
	// Format returns a principal reference accepted by Principal; pure metadata survives Close.
	Format(PrincipalID) string
}

// Optional interfaces are discovered by type assertion. Unsupported reports a
// fixed refusal when an interface is absent; callers must never fall back.
func Unsupported() error { return NewError(UnsupportedByTransport) }

type Rooms interface {
	// Create creates a conversation with the supplied name. Context, ownership, concurrency and Close rules apply.
	Create(context.Context, string) (ConversationID, error)
	// Join resolves and joins a supported conversation reference. Context, ownership, concurrency and Close rules apply.
	Join(context.Context, string) (ConversationID, error)
	// Leave leaves the named conversation. Context, ownership, concurrency and Close rules apply.
	Leave(context.Context, ConversationID) error
	// Invite invites the principal to the conversation. Context, ownership, concurrency and Close rules apply.
	Invite(context.Context, ConversationID, PrincipalID) error
	// Alias assigns the supplied alias. Context, ownership, concurrency and Close rules apply.
	Alias(context.Context, ConversationID, string) error
}
type RoomState interface {
	// GetState returns caller-owned state JSON. Context, ownership, concurrency and Close rules apply.
	GetState(context.Context, ConversationID, string, string) (json.RawMessage, error)
	// PutState writes state JSON borrowed for the call. Context, ownership, concurrency and Close rules apply.
	PutState(context.Context, ConversationID, string, string, json.RawMessage) (EventID, error)
}
type AccountData interface {
	// GetAccountData returns caller-owned account JSON. Context, ownership, concurrency and Close rules apply.
	GetAccountData(context.Context, string) (json.RawMessage, error)
	// PutAccountData writes account JSON borrowed for the call. Context, ownership, concurrency and Close rules apply.
	PutAccountData(context.Context, string, json.RawMessage) error
}
type Presence interface {
	// SetPresence updates the authenticated identity presence. Context, ownership, concurrency and Close rules apply.
	SetPresence(context.Context, string) error
}
type Registration interface {
	// Register creates an identity from explicit parameters borrowed for the call. Context, ownership, concurrency and Close rules apply.
	Register(context.Context, map[string]string) (Principal, error)
}
type Redactor interface {
	// Redact applies the transport redaction rule and returns Send outcome semantics. Context, ownership, concurrency and Close rules apply.
	Redact(context.Context, ConversationID, EventID, string) (SendResult, error)
}
