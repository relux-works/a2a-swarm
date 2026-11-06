package matrix

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

// RequireSpace checks the selected room's creation content at its original
// state endpoint. It never joins the room or accepts an event envelope in place
// of content. A successful observation is not a transaction with later writes.
func (c *Client) RequireSpace(ctx context.Context, roomID string) error {
	if !ValidVerificationID(roomID, '!') {
		return errors.New("matrix: invalid space selection")
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/state/m.room.create/", nil, nil, "", -1)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return errors.New("matrix: invalid space response")
	}
	v, err := readControl(ctx, resp, VerificationEntityLimit+1)
	if err != nil {
		return err
	}
	if v.value["type"] != "m.space" {
		return errors.New("matrix: creation type is not m.space")
	}
	for key := range v.value {
		// These are event metadata, not m.room.create content. In particular,
		// nested content or a claimed room_id cannot authorize the selection.
		for _, field := range []string{"content", "room_id", "event_id", "state_key"} {
			if strings.EqualFold(key, field) {
				return errors.New("matrix: event envelope is not creation content")
			}
		}
		if key != "type" && strings.EqualFold(key, "type") {
			return errors.New("matrix: ambiguous creation type")
		}
	}
	return ctx.Err()
}
