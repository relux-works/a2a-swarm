package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	VerificationEntityLimit  = 1 << 20
	VerificationContentLimit = 64 << 10
)

// VerificationEvent is an original-endpoint observation, never an authority
// verdict. Content retains the returned entity's exact JSON value spelling.
type VerificationEvent struct {
	EventID       string
	Sender        string
	Type          string
	RoomIDPresent bool
	RoomID        string
	Content       json.RawMessage
}

// ValidVerificationID preserves opaque historical IDs without registration's
// normalization policy. It validates syntax, not existence or authority.
func ValidVerificationID(s string, sigil byte) bool {
	if len(s) < 2 || len(s) > 255 || s[0] != sigil || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	if sigil == '@' {
		local, server, ok := strings.Cut(s[1:], ":")
		return ok && local != "" && server != ""
	}
	return true
}

// GetVerificationEvent is separate from ordinary bulk/display decoding. Its
// scoped transport retains the caller's immutable connection guard and never
// changes another user's redirect policy or performs a fallback fetch.
func (c *Client) GetVerificationEvent(ctx context.Context, roomID, eventID string) (VerificationEvent, error) {
	var zero VerificationEvent
	if !ValidVerificationID(roomID, '!') || !ValidVerificationID(eventID, '$') {
		return zero, errors.New("matrix: invalid verification selection")
	}
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, "/_matrix/client/v3/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, "", -1)
	if err != nil {
		return zero, err
	}
	// The existing strict reader owns EOF/Close/context checks. Adding one to
	// its exclusive read bound admits exactly the inclusive entity limit.
	v, err := readControl(ctx, resp, VerificationEntityLimit+1)
	if err != nil {
		return zero, errors.New("matrix: invalid verification response")
	}
	if err = verificationKeys(v.value); err != nil {
		return zero, err
	}
	id, idOK := v.value["event_id"].(string)
	sender, senderOK := v.value["sender"].(string)
	typ, typeOK := v.value["type"].(string)
	content, contentOK := v.value["content"].(map[string]any)
	if !idOK || id != eventID || !senderOK || !ValidVerificationID(sender, '@') || !typeOK || typ != "m.room.message" || !contentOK || len(content) == 0 {
		return zero, errors.New("matrix: invalid verification event")
	}
	result := VerificationEvent{EventID: id, Sender: sender, Type: typ}
	if room, present := v.value["room_id"]; present {
		s, ok := room.(string)
		if !ok || s != roomID {
			return zero, errors.New("matrix: verification room mismatch")
		}
		result.RoomIDPresent, result.RoomID = true, s
	}
	// All syntax, Unicode, key uniqueness and authoritative semantics were
	// decided by the strict tree above. RawMessage is used ONLY to capture
	// original value spans, never to reinterpret a semantic field or repair it.
	var spans map[string]json.RawMessage
	if err = json.Unmarshal(v.raw, &spans); err != nil {
		return zero, errors.New("matrix: invalid verification content")
	}
	if len(spans["content"]) > VerificationContentLimit {
		return zero, errors.New("matrix: verification content exceeds limit")
	}
	result.Content = append(json.RawMessage(nil), spans["content"]...)
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	return result, nil
}

func verificationKeys(object map[string]any) error {
	seen := make(map[string]bool, len(object))
	for k := range object {
		fold := verificationFold(k)
		if seen[fold] {
			return errors.New("matrix: ambiguous verification keys")
		}
		seen[fold] = true
		for _, name := range []string{"event_id", "room_id", "sender", "type", "content"} {
			if k != name && strings.EqualFold(k, name) {
				return errors.New("matrix: ambiguous verification field")
			}
		}
	}
	return nil
}

// Canonicalize each Unicode simple-fold cycle, without altering field values.
// This avoids a quadratic pairwise comparison for a bounded but large object.
func verificationFold(s string) string {
	var b strings.Builder
	for _, r := range s {
		best := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < best {
				best = next
			}
		}
		b.WriteRune(best)
	}
	return b.String()
}
