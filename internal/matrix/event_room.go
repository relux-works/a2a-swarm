package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// ErrEventIdentity means the returned event_id does not bind to the selection.
var ErrEventIdentity = errors.New("matrix: event identity mismatch")

// EventRoomMismatchError carries only valid room IDs, never peer content. The
// default diagnostic is deliberately opaque; display callers may name the IDs.
type EventRoomMismatchError struct {
	SelectedRoomID string
	ReturnedRoomID string
}

func (*EventRoomMismatchError) Error() string { return "matrix: event room mismatch" }

// CheckEventRoom is the common event/room binding check for display and strict
// verification consumers. Callers must reject duplicate identity keys before
// constructing envelope. Identity values must be interpreted without Unicode
// repair; case aliases are refused here. Content and event type are not policy
// inputs to this check.
//
// A present room_id must be a valid room ID equal to roomID. With allowAbsentRoom
// true, an absent room_id supplies endpoint context ONLY, not an observed room;
// this preserves GetVerificationEvent's explicit RoomIDPresent contract. Display
// uses false: the ClientEvent returned by /rooms/{roomId}/event/{eventId} requires
// room_id (Matrix Client-Server API). The request path never overrides a payload.
func CheckEventRoom(roomID, eventID string, envelope map[string]any, allowAbsentRoom bool) error {
	for key := range envelope {
		if key != "event_id" && strings.EqualFold(key, "event_id") {
			return ErrEventIdentity
		}
		if key != "room_id" && strings.EqualFold(key, "room_id") {
			return errors.New("matrix: ambiguous event room")
		}
	}
	id, ok := envelope["event_id"].(string)
	if !ok || id != eventID {
		return ErrEventIdentity
	}
	room, present := envelope["room_id"]
	if !present && allowAbsentRoom {
		return nil
	}
	if !present {
		return errors.New("matrix: event response has no room_id")
	}
	returned, ok := room.(string)
	if !ok || !ValidVerificationID(returned, '!') {
		return errors.New("matrix: invalid event room")
	}
	if returned != roomID {
		return &EventRoomMismatchError{SelectedRoomID: roomID, ReturnedRoomID: returned}
	}
	return nil
}

// GetRoomEvent is a display read bound to the selected event and returned room.
// It accepts any event type and preserves ordinary peer-content decoding, unlike
// GetVerificationEvent's strict message/content/redaction policy. Unscoped bulk
// GetEvent and timeline readers retain their historical compatibility behavior.
func (c *Client) GetRoomEvent(ctx context.Context, roomID, eventID string) (Event, error) {
	var zero Event
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, "", -1)
	if err != nil {
		return zero, err
	}
	raw, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if resp.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || (resp.ContentLength >= 0 && resp.ContentLength != int64(len(raw))) {
		return zero, errors.New("matrix: incomplete event response")
	}
	// Interpret only envelope identity strictly. Message content remains opaque
	// here: duplicates and legacy scalar repair inside it remain display data.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return zero, errors.New("matrix: invalid event envelope")
	}
	envelope := map[string]any{}
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return zero, errors.New("matrix: invalid event envelope")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return zero, errors.New("matrix: invalid event envelope")
		}
		name := key.(string)
		if !strings.EqualFold(name, "event_id") && !strings.EqualFold(name, "room_id") {
			continue
		}
		if _, exists := envelope[name]; exists {
			return zero, errors.New("matrix: duplicate event identity field")
		}
		parsed, err := strictJSON(value)
		if err != nil {
			return zero, errors.New("matrix: invalid event identity field")
		}
		envelope[name] = parsed
	}
	if _, err = decoder.Token(); err != nil {
		return zero, errors.New("matrix: invalid event envelope")
	}
	var trailing any
	if err = decoder.Decode(&trailing); err != io.EOF {
		return zero, errors.New("matrix: invalid event envelope")
	}
	if err = CheckEventRoom(roomID, eventID, envelope, false); err != nil {
		return zero, err
	}
	var event Event
	if err = json.Unmarshal(raw, &event); err != nil {
		return zero, errors.New("matrix: invalid display event")
	}
	if err = ctx.Err(); err != nil {
		return zero, err
	}
	return event, nil
}
