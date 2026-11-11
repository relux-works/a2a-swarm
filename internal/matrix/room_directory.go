package matrix

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
)

// GetRoomDirectoryVisibility observes this server's directory only. Join rules
// and history visibility do not establish whether a room is listed there.
func (c *Client) GetRoomDirectoryVisibility(ctx context.Context, roomID string) (string, error) {
	bad := errors.New("matrix: incomplete or invalid room directory visibility")
	if !ValidVerificationID(roomID, '!') {
		return "", bad
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	response, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/directory/list/room/"+seg(roomID), nil, nil, "", -1)
	if err != nil {
		return "", bad
	}
	if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return "", bad
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, VerificationEntityLimit+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || ctx.Err() != nil || len(raw) > VerificationEntityLimit ||
		(response.ContentLength >= 0 && int64(len(raw)) != response.ContentLength) {
		return "", bad
	}
	tree, err := strictJSON(raw)
	object, ok := tree.(map[string]any)
	if err != nil || !ok {
		return "", bad
	}
	for key := range object {
		if key != "visibility" && strings.EqualFold(key, "visibility") {
			return "", bad
		}
	}
	visibility, ok := object["visibility"].(string)
	if !ok || (visibility != "private" && visibility != "public") || ctx.Err() != nil {
		return "", bad
	}
	return visibility, nil
}
