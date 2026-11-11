package matrix

import (
	"context"
	"errors"
	"net/http"
)

// ResolvePairAlias distinguishes a complete direct 404/M_NOT_FOUND from an
// unreadable alias. Other failures must never authorize replacement creation.
func (c *Client) ResolvePairAlias(ctx context.Context, alias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.WithoutRedirects().call(ctx, http.MethodGet, v3+"/directory/room/"+seg(alias), nil, nil, &out, "room_id")
	var refusal *Error
	if errors.As(err, &refusal) && refusal.Status == http.StatusNotFound && refusal.ErrCode == "M_NOT_FOUND" && refusal.OriginalRequest && refusal.BodyComplete {
		return "", ErrUnknownAlias
	}
	if err == nil && !ValidVerificationID(out.RoomID, '!') {
		return "", errors.New("matrix: invalid pair alias room")
	}
	return out.RoomID, err
}
