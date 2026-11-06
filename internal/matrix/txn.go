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
	ack, err := c.SendMessageTxnReceipt(ctx, roomID, txnID, content)
	return ack.EventID(), err
}

// SendMessageTxnReceipt preserves the caller's transaction identity and returns
// the exact interpreted ACK and raw entity. Redirect policy is unchanged;
// WithoutRedirects can additionally require the original endpoint's response.
func (c *Client) SendMessageTxnReceipt(ctx context.Context, roomID, txnID string, content any) (ACKReceipt, error) {
	return c.ack(ctx, v3+"/rooms/"+seg(roomID)+"/send/m.room.message/"+seg(txnID), content)
}
func (c *Client) ack(ctx context.Context, path string, content any) (ACKReceipt, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out struct {
		EventID string `json:"event_id"`
	}
	v, err := c.doResponse(ctx, http.MethodPut, path, nil, content, &out, false, "event_id")
	if err != nil {
		return ACKReceipt{}, err
	}
	if err = ctx.Err(); err != nil {
		return ACKReceipt{}, err
	}
	return ACKReceipt{eventID: out.EventID, raw: v.raw, status: v.status, contentLength: v.length}, nil
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
	err := c.callBulk(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/messages", q, nil, &out)
	if err == nil && c.FilterEvents != nil {
		out.Chunk, err = c.FilterEvents(roomID, out.Chunk)
	}
	return out, err
}
