package e2ee

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Section 7 refusal reasons (docs/e2ee.md). Each names one deterministic gate
// and carries no value. Store/operation failures are separate codes returned
// as plain errors, never as a Refusal: they abort the batch, not the event.
const (
	ErrMalformedEvent            Code = "e2ee_malformed_event"
	ErrContextUnknown            Code = "e2ee_context_unknown"
	ErrRoomScopeUnsupported      Code = "e2ee_room_scope_unsupported"
	ErrCiphertextInPlaintextRoom Code = "e2ee_ciphertext_in_plaintext_room"
	ErrPlaintextInEncryptedRoom  Code = "e2ee_plaintext_in_encrypted_room"
	ErrUnsupportedAlgorithm      Code = "e2ee_unsupported_algorithm"
	ErrDeviceChanged             Code = "e2ee_device_changed"
	ErrDeviceUnverified          Code = "e2ee_device_unverified"
	ErrDeviceUnknown             Code = "e2ee_device_unknown"
	ErrCiphertextInvalid         Code = "e2ee_ciphertext_invalid"
	ErrSelfSigningMissing        Code = "e2ee_self_signing_missing"
	ErrUndecryptable             Code = "e2ee_undecryptable"
	ErrKeyProvenanceUnverified   Code = "e2ee_key_provenance_unverified"
	ErrSenderMismatch            Code = "e2ee_sender_mismatch"
	ErrContentUnsupported        Code = "e2ee_content_unsupported"
	ErrSenderNotEligible         Code = "e2ee_sender_not_eligible"
	ErrRateLimited               Code = "e2ee_rate_limited"
	ErrKeyQueryUnavailable       Code = "e2ee_key_query_unavailable" // Batch abort, never an event verdict.
)

// Wire constants this verifier recognizes. Anything else is refused by name.
const (
	AlgorithmMegolm   = "m.megolm.v1.aes-sha2"
	AlgorithmOlm      = "m.olm.v1.curve25519-aes-sha2"
	TypeEncrypted     = "m.room.encrypted"
	TypeRoomKey       = "m.room_key"
	TypeRoomMessage   = "m.room.message"
	MsgTypeText       = "m.text"
	RelationReplace   = "m.replace"
	SourceRoom        = "room"
	SourceToDevice    = "to-device"
	MaxWireEvent      = 1 << 20
	MaxAttemptsPeer   = 20
	MaxAttemptsLocal  = 100
	attemptWindowMS   = int64(60 * 1000)
	maxCiphertextKeys = 8
)

// KeySource is the authenticated origin of a Megolm session (section 3 check 5).
// Only these two values are provenance; a record with any other source refuses.
type KeySource string

const (
	KeySourceDirectOlm   KeySource = "direct-olm"
	KeySourceOwnOutbound KeySource = "own-outbound-session"
)

// Room context states (section 5). Only a complete state response may set
// plaintext-confirmed; this library consumes the label, it never derives it.
const (
	ContextUnknown   = "unknown"
	ContextPlaintext = "plaintext-confirmed"
	ContextEncrypted = "encrypted-sticky"
)

// RoomContext is the reconciled joined-member view of one room at the
// revision the caller admits events under. Members are joined MXIDs only.
type RoomContext struct {
	State   string   `json:"state"`
	Members []string `json:"members"`
}

// dmWith reports whether the room is the encrypted two-member DM of exactly
// local and peer (section 1 scope). Order and duplicates do not matter.
func (c RoomContext) dmWith(local, peer string) bool {
	if c.State != ContextEncrypted || len(c.Members) != 2 || local == peer {
		return false
	}
	seen := map[string]bool{}
	for _, m := range c.Members {
		seen[m] = true
	}
	return len(seen) == 2 && seen[local] && seen[peer]
}

// LocalDevice is this account's public identity: the MXID and device ID the
// bootstrap bound (section 3) and the account's published Ed25519/Curve25519.
type LocalDevice struct {
	MXID, Device, Ed25519, Curve25519 string
}

func (d LocalDevice) valid() bool {
	if !fullMXID(d.MXID) || !text(d.Device, 255) {
		return false
	}
	_, e1 := publicBase64(d.Ed25519, 32)
	_, e2 := publicBase64(d.Curve25519, 32)
	return e1 == nil && e2 == nil
}

// EligibilityStatus is the stage 3 input the caller derives from a completed
// bounded key query plus the pinned roots and the selected Q4 policy. Q4 is
// out of scope here: this library consumes the label and never decides it.
type EligibilityStatus string

const (
	EligibilityEligible   EligibilityStatus = "eligible"
	EligibilityChanged    EligibilityStatus = "changed"    // Known changed pinned/root/device key.
	EligibilityTombstoned EligibilityStatus = "tombstoned" // Known deletion/block; wins over absence.
	EligibilityUnverified EligibilityStatus = "unverified" // Root unanchored or disallowed by Q4.
)

// DeviceObservation is one device from a completed key query with its
// caller-derived eligibility. Keys is the exact queried device-key object.
type DeviceObservation struct {
	Status EligibilityStatus
	Keys   DeviceKeys
}

// PeerObservation is the observed public chain of one MXID: the host-anchored
// master public key, the queried master and self-signing objects, the device
// list and the DM rooms this identity shares with that MXID. QueryComplete
// false means the required query failed or was partial: a batch abort.
type PeerObservation struct {
	AnchoredMaster string
	QueryComplete  bool
	Master         SignedKey
	SelfSigning    *SignedKey
	Devices        map[string]DeviceObservation
	Rooms          map[string]RoomContext
}

// SessionRecord is the authenticated provenance persisted beside an admitted
// inbound Megolm session. It is internal sealed state: wire input never
// produces one, only a successful to-device admission or own outbound does.
type SessionRecord struct {
	SessionID  string    `json:"session_id"`
	RoomID     string    `json:"room_id"`
	Originator string    `json:"originator"`
	Device     string    `json:"device_id"`
	Ed25519    string    `json:"ed25519"`
	Curve25519 string    `json:"curve25519"`
	Source     KeySource `json:"source"`
}

func (r SessionRecord) valid() bool {
	if !text(r.SessionID, 255) || !text(r.RoomID, 255) || !fullMXID(r.Originator) || !text(r.Device, 255) {
		return false
	}
	_, e1 := publicBase64(r.Ed25519, 32)
	_, e2 := publicBase64(r.Curve25519, 32)
	return e1 == nil && e2 == nil
}

// AttemptBudget is the persisted per-minute Olm attempt accounting (section 4):
// 20 attempts per peer and 100 per identity. Counting a refused attempt is
// permitted safety accounting, not a forbidden effect of the refusal.
type AttemptBudget struct {
	WindowMS int64          `json:"window_ms"`
	Peers    map[string]int `json:"peers"`
	Identity int            `json:"identity"`
}

// take records one attempt by peer at nowMS and reports whether it is within
// budget. The window is the calendar minute of nowMS; a new minute resets it.
func (b *AttemptBudget) take(peer string, nowMS int64) bool {
	window := nowMS - nowMS%attemptWindowMS
	if b.WindowMS != window || b.Peers == nil {
		b.WindowMS, b.Peers, b.Identity = window, map[string]int{}, 0
	}
	b.Peers[peer]++
	b.Identity++
	return b.Peers[peer] <= MaxAttemptsPeer && b.Identity <= MaxAttemptsLocal
}

// Refusal is the typed final verdict of one event. It is value-free: no
// content, ciphertext, key or library text ever enters it. Error() is the
// reason code alone, so wrapping it in a diagnostic cannot leak anything.
type Refusal struct {
	Source string `json:"source"`
	Stage  string `json:"stage"`
	Reason Code   `json:"reason"`
	ID     string `json:"refusal_id"`
	RoomID string `json:"room_id,omitempty"`
	Event  string `json:"event_id,omitempty"`
	Sender string `json:"sender"`
	Device string `json:"claimed_device,omitempty"`
}

func (r Refusal) Error() string { return string(r.Reason) }

// RoomRefusalID is the section 7 room-event identity (room, event_id,
// ciphertext_digest); each component is length-prefixed, so a delimiter in one
// field cannot collide with another split.
func RoomRefusalID(room, event, ciphertextDigest string) string {
	return TupleHash(room, event, ciphertextDigest)
}

// ToDeviceRefusalID is SHA256 over length-prefixed (binding, observed sender,
// type, exact raw content bytes). Malformed events pass empty components for
// the fields that could not be read and the raw event digest as content.
func ToDeviceRefusalID(binding, sender, kind string, content []byte) string {
	return TupleHash(binding, sender, kind, string(content))
}

func rawDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// AcceptedEvent is the internal accepted-event type (section 7). Every field
// is unexported: encoding/json and any wire decoder produce only the zero
// value, which carries no content and no provenance. Only VerifyRoomEvent
// constructs a populated value.
type AcceptedEvent struct {
	content    []byte
	room       string
	event      string
	digest     string
	originator string
	device     string
	ed25519    string
	curve25519 string
	source     KeySource
	session    string
	index      uint
	reread     bool
}

// Content returns a copy of the exact decrypted inner content bytes.
func (e *AcceptedEvent) Content() []byte { return append([]byte(nil), e.content...) }

// Room and EventID identify the outer event; CiphertextDigest is the SHA256
// hex of the exact outer content bytes, the third refusal-identity component.
func (e *AcceptedEvent) Room() string             { return e.room }
func (e *AcceptedEvent) EventID() string          { return e.event }
func (e *AcceptedEvent) CiphertextDigest() string { return e.digest }

// Originator is the session-originator MXID and device the chain binds.
func (e *AcceptedEvent) Originator() (mxid, device string) { return e.originator, e.device }

// Fingerprints are the SHA256 hex digests of the originator device's Ed25519
// and Curve25519 public keys, the same digest form the host pin uses.
func (e *AcceptedEvent) Fingerprints() (ed25519, curve25519 string) {
	return e.ed25519, e.curve25519
}
func (e *AcceptedEvent) KeySource() KeySource { return e.source }
func (e *AcceptedEvent) Session() string      { return e.session }
func (e *AcceptedEvent) Index() uint          { return e.index }

// Reread reports an exact retained reread (same event, digest and index):
// the prior outcome applies and no duplicate mail may be produced.
func (e *AcceptedEvent) Reread() bool { return e.reread }

// Populated distinguishes a verifier result from a zero value a decoder made.
func (e *AcceptedEvent) Populated() bool {
	return e != nil && e.content != nil && e.originator != "" && e.source != ""
}

func (AcceptedEvent) String() string                     { return "<e2ee-accepted-event>" }
func (e AcceptedEvent) GoString() string                 { return e.String() }
func (e AcceptedEvent) Format(state fmt.State, _ rune)   { formatLabel(state, e.String()) }
func (SessionRecord) String() string                     { return "<e2ee-session-record>" }
func (r SessionRecord) GoString() string                 { return r.String() }
func (r SessionRecord) Format(state fmt.State, _ rune)   { formatLabel(state, r.String()) }
func (ToDeviceRequest) String() string                   { return "<e2ee-to-device-request>" }
func (r ToDeviceRequest) GoString() string               { return r.String() }
func (r ToDeviceRequest) Format(state fmt.State, _ rune) { formatLabel(state, r.String()) }
func (ToDeviceAdmission) String() string                 { return "<e2ee-to-device-admission>" }
func (a ToDeviceAdmission) GoString() string             { return a.String() }
func (a ToDeviceAdmission) Format(state fmt.State, _ rune) {
	formatLabel(state, a.String())
}
func (RoomEventRequest) String() string                   { return "<e2ee-room-event-request>" }
func (r RoomEventRequest) GoString() string               { return r.String() }
func (r RoomEventRequest) Format(state fmt.State, _ rune) { formatLabel(state, r.String()) }
