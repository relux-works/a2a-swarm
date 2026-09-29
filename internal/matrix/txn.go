package matrix

import (
	"context"
	"net/http"
)

// SendMessageTxn sends an m.room.message with a caller-chosen transaction id,
// so that a retry with the same id is deduplicated by the homeserver.
func (c *Client) SendMessageTxn(ctx context.Context, roomID, txnID string, content any) (string, error) {
	var out struct {
		EventID string `json:"event_id"`
	}
	err := c.call(ctx, http.MethodPut, v3+"/rooms/"+seg(roomID)+"/send/m.room.message/"+seg(txnID), nil, content, &out)
	return out.EventID, err
}
