package matrix

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
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

// MessagesTo pages /messages backwards from `from`, stopping at the sync
// token `to` (exclusive): it returns what a limited /sync timeline skipped.
func (c *Client) MessagesTo(ctx context.Context, roomID, from, to string, limit int) (Page, error) {
	q := url.Values{"dir": {"b"}, "limit": {strconv.Itoa(limit)}}
	if from != "" {
		q.Set("from", from)
	}
	if to != "" {
		q.Set("to", to)
	}
	var out Page
	err := c.call(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/messages", q, nil, &out)
	return out, err
}
