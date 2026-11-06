package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// RoomPolicyEvent is a strict current-state observation, not an authority verdict.
// Content is the tree interpreted by the shared strict control JSON reader.
type RoomPolicyEvent struct {
	Type, StateKey string
	Content        map[string]any
}

// GetRoomPolicyState is separate from display/bulk decoding. Like verification
// input, it requires a complete bounded original-endpoint response, exact field
// spelling and strict JSON. It does not change GetRoomState or owner-DM behavior.
func (c *Client) GetRoomPolicyState(ctx context.Context, roomID string) ([]RoomPolicyEvent, error) {
	if !ValidVerificationID(roomID, '!') {
		return nil, errors.New("matrix: invalid policy room selection")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/state", nil, nil, "", -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, errors.New("matrix: policy readback requires HTTP 200")
	}
	// Same inclusive bound and EOF/Close/context requirements as control reads.
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, VerificationEntityLimit+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil || ctx.Err() != nil || len(raw) > VerificationEntityLimit ||
		(resp.ContentLength >= 0 && int64(len(raw)) != resp.ContentLength) {
		return nil, errors.New("matrix: incomplete or oversized policy readback")
	}
	tree, err := strictJSON(raw)
	if err != nil {
		return nil, err
	}
	array, ok := tree.([]any)
	if !ok {
		return nil, errors.New("matrix: policy state must be an array")
	}
	result := make([]RoomPolicyEvent, 0, len(array))
	for _, item := range array {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, errors.New("matrix: invalid policy state event")
		}
		for key := range object {
			for _, name := range []string{"type", "state_key", "content", "room_id", "unsigned"} {
				if key != name && strings.EqualFold(key, name) {
					return nil, errors.New("matrix: ambiguous policy state field")
				}
			}
		}
		typ, typeOK := object["type"].(string)
		key, keyOK := object["state_key"].(string)
		content, contentOK := object["content"].(map[string]any)
		if !typeOK || typ == "" || !keyOK || !contentOK {
			return nil, errors.New("matrix: invalid policy state event")
		}
		if room, present := object["room_id"]; present && room != roomID {
			return nil, errors.New("matrix: policy room mismatch")
		}
		if value, present := object["unsigned"]; present {
			unsigned, ok := value.(map[string]any)
			if !ok {
				return nil, errors.New("matrix: invalid policy unsigned state")
			}
			for key := range unsigned {
				if strings.EqualFold(key, "redacted_because") {
					return nil, errors.New("matrix: redacted policy state")
				}
			}
		}
		result = append(result, RoomPolicyEvent{Type: typ, StateKey: key, Content: content})
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
