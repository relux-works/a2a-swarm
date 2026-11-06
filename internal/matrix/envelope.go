package matrix

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
)

// Envelope carriage: a completed swarm-envelope/2 travels in Matrix event
// content as unchanged bytes. Matrix content is JSON, so the bytes are carried
// as one string in a reversible encoding: unpadded base64url (RFC 4648 §5, no
// '=', canonical trailing bits). Nothing here parses, canonicalizes or
// re-serializes the envelope JSON, and nothing derives authority from the
// sender, a display name or the envelope's own claims. Consumers verify.
const (
	// EnvelopeContentKey is the fixed event-content key from the envelope
	// specification's Matrix adapter rule.
	EnvelopeContentKey = "works.relux.swarm.envelope"
	// EnvelopeMaxBytes is the specification's per-envelope limit (65,536 bytes).
	// It is enforced here independently of any Matrix limit.
	EnvelopeMaxBytes = 65536
	// EnvelopeEventContentBudget is the published transport byte budget for the
	// serialized content of one event that carries an envelope. Matrix limits a
	// whole event, including server-added fields, to 65,536 bytes, and base64url
	// grows the envelope by a third, so this is tighter than EnvelopeMaxBytes.
	// An event over the budget is refused, never truncated. The homeserver stays
	// authoritative and may still refuse an event inside the budget.
	EnvelopeEventContentBudget = 60 << 10
)

// Stable refusal codes. A refusal never carries response data.
const (
	// Send side.
	RefuseEnvelopeEmpty      = "envelope_empty"
	RefuseEnvelopeUnreadable = "envelope_unreadable"
	RefuseEventTooLarge      = "event_too_large"
	RefuseTextInvalid        = "text_invalid"
	RefuseTextEmpty          = "text_empty"
	// Both sides.
	RefuseEnvelopeTooLarge = "envelope_too_large"
	// Read side.
	RefuseSelectionInvalid = "selection_invalid"
	RefuseResponseInvalid  = "response_invalid"
	RefuseEntityTooLarge   = "entity_too_large"
	RefuseContentTooLarge  = "content_too_large"
	RefuseInvalidUTF8      = "invalid_utf8"
	RefuseDuplicateKey     = "duplicate_key"
	RefuseTrailingData     = "trailing_data"
	RefuseKeyCaseAlias     = "key_case_alias"
	RefuseEventIDMismatch  = "event_id_mismatch"
	RefuseEventTypeInvalid = "event_type_invalid"
	RefuseSenderInvalid    = "sender_invalid"
	RefuseRoomIDMissing    = "room_id_missing"
	RefuseRoomIDMismatch   = "room_id_mismatch"
	RefuseContentInvalid   = "content_invalid"
	RefuseMsgtypeInvalid   = "msgtype_invalid"
	RefuseBodyMissing      = "body_missing"
	RefuseEnvelopeMissing  = "envelope_missing"
	RefuseEnvelopeBad      = "envelope_malformed"
	RefuseEventRedacted    = "event_redacted"
)

// EnvelopeRefusal is a deliberate refusal with a stable machine-readable code.
// Detail holds only local numbers (sizes), never data from a response.
type EnvelopeRefusal struct {
	Code   string
	Detail string
}

func (r *EnvelopeRefusal) Error() string {
	if r.Detail != "" {
		return "envelope refused: " + r.Code + " (" + r.Detail + ")"
	}
	return "envelope refused: " + r.Code
}

// Refuse builds an EnvelopeRefusal.
func Refuse(code, detail string) error { return &EnvelopeRefusal{Code: code, Detail: detail} }

// RefusalCode returns the code of a deliberate envelope refusal, or "".
func RefusalCode(err error) string {
	var r *EnvelopeRefusal
	if errors.As(err, &r) {
		return r.Code
	}
	return ""
}

// EncodeEnvelope returns the event-content value for the exact envelope bytes.
// An empty envelope and one over EnvelopeMaxBytes are refused, never truncated.
func EncodeEnvelope(envelope []byte) (string, error) {
	if len(envelope) == 0 {
		return "", Refuse(RefuseEnvelopeEmpty, "")
	}
	if len(envelope) > EnvelopeMaxBytes {
		return "", Refuse(RefuseEnvelopeTooLarge, strconv.Itoa(len(envelope))+" bytes exceeds the "+strconv.Itoa(EnvelopeMaxBytes)+"-byte limit")
	}
	return base64.RawURLEncoding.EncodeToString(envelope), nil
}

// DecodeEnvelope is the strict inverse of EncodeEnvelope. The encoded length is
// bounded before any allocation. Decoding must re-encode to the original text,
// which rejects padding, the standard alphabet, whitespace (Go's decoder skips
// CR and LF), and non-zero trailing bits.
func DecodeEnvelope(value string) ([]byte, error) {
	if value == "" {
		return nil, Refuse(RefuseEnvelopeBad, "")
	}
	if len(value) > base64.RawURLEncoding.EncodedLen(EnvelopeMaxBytes) {
		return nil, Refuse(RefuseEnvelopeTooLarge, "encoded value exceeds the limit")
	}
	out, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(out) == 0 || base64.RawURLEncoding.EncodeToString(out) != value {
		return nil, Refuse(RefuseEnvelopeBad, "")
	}
	return out, nil
}

// CheckEventContentBudget refuses content whose serialized form exceeds
// EnvelopeEventContentBudget. It measures what the sender would transmit.
func CheckEventContentBudget(content any) error {
	data, err := json.Marshal(content)
	if err != nil {
		return err
	}
	if len(data) > EnvelopeEventContentBudget {
		return Refuse(RefuseEventTooLarge, strconv.Itoa(len(data))+" bytes exceeds the "+strconv.Itoa(EnvelopeEventContentBudget)+"-byte event content budget")
	}
	return nil
}

// EnvelopeEvent is an original-endpoint observation of one event that carries
// an envelope. It is never an authority verdict: Sender is the event's
// authenticated sender spelling, Envelope and BodyText are the exact bytes.
type EnvelopeEvent struct {
	EventID  string
	Sender   string
	RoomID   string
	Envelope []byte
	BodyText []byte
	// Content is the original content value bytes, kept only so a caller can
	// consult local redaction tombstones (including explicit edit targets).
	Content json.RawMessage
}

// GetEnvelopeEvent performs one scoped, redirect-free GET of the requested event
// and projects its envelope and human-readable text with the same strictness as
// GetVerificationEvent: bounded entity and content, strict JSON and UTF-8,
// duplicate-key, case-alias and trailing-data refusals, exact requested and
// returned identifiers, and no fallback fetch. Every refusal returns the zero
// value. The returned room id is required (a server's event endpoint always
// reports it) because the envelope binds its room to the actual room id.
func (c *Client) GetEnvelopeEvent(ctx context.Context, roomID, eventID string) (EnvelopeEvent, error) {
	var zero EnvelopeEvent
	if !ValidVerificationID(roomID, '!') || !ValidVerificationID(eventID, '$') {
		return zero, Refuse(RefuseSelectionInvalid, "")
	}
	// Like GetVerificationEvent: the fetch carries the default deadline unless
	// the caller set one, and only an exact 200 response is an observation.
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, "/_matrix/client/v3/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, "", -1)
	if err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return zero, Refuse(RefuseResponseInvalid, "")
	}
	v, err := readControl(ctx, resp, VerificationEntityLimit+1)
	if err != nil {
		return zero, classifyControl(ctx, err)
	}
	if verificationKeys(v.value) != nil {
		return zero, Refuse(RefuseKeyCaseAlias, "")
	}
	if id, ok := v.value["event_id"].(string); !ok || id != eventID {
		return zero, Refuse(RefuseEventIDMismatch, "")
	}
	if typ, ok := v.value["type"].(string); !ok || typ != "m.room.message" {
		return zero, Refuse(RefuseEventTypeInvalid, "")
	}
	sender, ok := v.value["sender"].(string)
	if !ok || !ValidVerificationID(sender, '@') {
		return zero, Refuse(RefuseSenderInvalid, "")
	}
	room, present := v.value["room_id"]
	if !present {
		return zero, Refuse(RefuseRoomIDMissing, "")
	}
	if s, ok := room.(string); !ok || s != roomID {
		return zero, Refuse(RefuseRoomIDMismatch, "")
	}
	// A redacted event is refused even when a server still returns content,
	// exactly as GetVerificationEvent refuses it.
	if unsigned, present := v.value["unsigned"]; present {
		fields, ok := unsigned.(map[string]any)
		if !ok {
			return zero, Refuse(RefuseResponseInvalid, "")
		}
		for k := range fields {
			if verificationFold(k) == verificationFold("redacted_because") {
				return zero, Refuse(RefuseEventRedacted, "")
			}
		}
	}
	content, ok := v.value["content"].(map[string]any)
	if !ok || len(content) == 0 {
		return zero, Refuse(RefuseContentInvalid, "")
	}
	// Spans are used ONLY to measure the original content value; every semantic
	// decision below uses the strict tree.
	var spans map[string]json.RawMessage
	if err = json.Unmarshal(v.raw, &spans); err != nil {
		return zero, Refuse(RefuseResponseInvalid, "")
	}
	if len(spans["content"]) > VerificationContentLimit {
		return zero, Refuse(RefuseContentTooLarge, "")
	}
	if envelopeContentKeysAmbiguous(content) {
		return zero, Refuse(RefuseKeyCaseAlias, "")
	}
	if msgtype, ok := content["msgtype"].(string); !ok || msgtype != "m.text" {
		return zero, Refuse(RefuseMsgtypeInvalid, "")
	}
	body, ok := content["body"].(string)
	if !ok {
		return zero, Refuse(RefuseBodyMissing, "")
	}
	raw, present := content[EnvelopeContentKey]
	if !present {
		return zero, Refuse(RefuseEnvelopeMissing, "")
	}
	encoded, ok := raw.(string)
	if !ok {
		return zero, Refuse(RefuseEnvelopeBad, "")
	}
	envelope, err := DecodeEnvelope(encoded)
	if err != nil {
		return zero, err
	}
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	return EnvelopeEvent{EventID: eventID, Sender: sender, RoomID: roomID, Envelope: envelope, BodyText: []byte(body), Content: append(json.RawMessage(nil), spans["content"]...)}, nil
}

// classifyControl names the strict reader's refusals. Context failures stay
// context failures: they are not a statement about the response.
func classifyControl(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	switch {
	case errors.Is(err, errControlTooLarge):
		return Refuse(RefuseEntityTooLarge, "")
	case errors.Is(err, errWireNotUTF8), errors.Is(err, errWireSurrogate):
		return Refuse(RefuseInvalidUTF8, "")
	case errors.Is(err, errWireDuplicate):
		return Refuse(RefuseDuplicateKey, "")
	case errors.Is(err, errWireTrailing):
		return Refuse(RefuseTrailingData, "")
	}
	return Refuse(RefuseResponseInvalid, "")
}

// envelopeContentKeysAmbiguous reports content member names that collide under
// Unicode simple case folding, and any case variant of the three names this
// projection reads. A lenient decoder (Go's struct unmarshal) would silently
// pick one of them.
func envelopeContentKeysAmbiguous(content map[string]any) bool {
	seen := make(map[string]bool, len(content))
	for k := range content {
		fold := verificationFold(k)
		if seen[fold] {
			return true
		}
		seen[fold] = true
		for _, name := range []string{EnvelopeContentKey, "body", "msgtype"} {
			if k != name && strings.EqualFold(k, name) {
				return true
			}
		}
	}
	return false
}
