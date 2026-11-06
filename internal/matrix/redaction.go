package matrix

import (
	"context"
	"encoding/json"
	"net/http"
)

// RedactionEventType is a bounded, direct target-type observation, not an
// authority check. No fetched content is returned to the caller.
func (c *Client) RedactionEventType(ctx context.Context, roomID, eventID string) (string, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	var out struct {
		Type string `json:"type"`
	}
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, "", -1)
	if err != nil {
		return "", err
	}
	// This lookup caps the complete client JSON at 64 KiB, including unsigned
	// data (which can exceed the room-event limit). One byte detects overflow;
	// no arbitrary fetched body is ever returned as a diagnostic.
	v, err := readControl(ctx, resp, (64<<10)+1)
	if err != nil {
		return "", err
	}
	err = v.consume(ctx, &out, "type")
	return out.Type, err
}

// RedactionTarget supports both pre-v11 and v11+ client event formats. It
// deliberately still reads the protected target of a redacted redaction;
// redacting a redaction never restores the original event.
func (e Event) RedactionTarget() string {
	if e.Type != "m.room.redaction" {
		return ""
	}
	var c struct {
		Redacts string `json:"redacts"`
	}
	_ = json.Unmarshal(e.Content, &c)
	if c.Redacts != "" {
		return c.Redacts
	}
	return e.Redacts
}

// IsRedacted recognises server redaction metadata without inspecting or
// returning the embedded redaction event (which can contain a sensitive reason).
func (e Event) IsRedacted() bool {
	var u map[string]json.RawMessage
	if json.Unmarshal(e.Unsigned, &u) == nil {
		if r := u["redacted_because"]; len(r) > 0 && string(r) != "null" {
			return true
		}
	}
	var c map[string]json.RawMessage
	return e.Type == "m.room.message" && (json.Unmarshal(e.Content, &c) != nil || len(c) == 0)
}

// ReplacementTarget identifies only an explicit message edit. Replies, thread
// relations and arbitrary quoted copies do not inherit their target's state.
func (e Event) ReplacementTarget() string {
	if e.Type != "m.room.message" {
		return ""
	}
	var c struct {
		RelatesTo struct {
			RelType string `json:"rel_type"`
			EventID string `json:"event_id"`
		} `json:"m.relates_to"`
	}
	if json.Unmarshal(e.Content, &c) != nil || c.RelatesTo.RelType != "m.replace" {
		return ""
	}
	return c.RelatesTo.EventID
}

// WithoutContent is a local tombstone, never the Matrix state-event redaction
// algorithm. Drop all content and unsigned data, including attachments, HTML,
// edits, envelopes, reply quotes and bundled events.
func (e Event) WithoutContent() Event {
	if e.Type == "m.room.redaction" {
		e.Redacts = e.RedactionTarget()
	}
	e.Content = json.RawMessage(`{}`)
	e.Unsigned = json.RawMessage(`{"redacted_because":{}}`)
	return e
}

// RedactEventTxn makes exactly one direct HTTP attempt with the supplied
// transaction id. The caller keeps that id for an explicit idempotent replay.
func (c *Client) RedactEventTxn(ctx context.Context, roomID, eventID, txnID, reason string) (string, error) {
	ack, err := c.WithoutRedirects().ack(ctx, v3+"/rooms/"+seg(roomID)+"/redact/"+seg(eventID)+"/"+seg(txnID), struct {
		Reason string `json:"reason"`
	}{reason})
	return ack.EventID(), err
}
