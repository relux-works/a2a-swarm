package matrix

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// AdminRegistrationSync is the bounded, strict in-memory observation path for
// registration only. Ordinary Sync always excludes ProtectedRoom. No cursor or
// response from this method may be persisted by a delivery consumer.
type AdminRegistrationTimeline struct {
	Events  []Event
	Limited bool
}
type AdminRegistrationBatch struct {
	NextBatch string
	Join      map[string]AdminRegistrationTimeline
}

func (c *Client) AdminRegistrationSync(ctx context.Context, room, since string, wait time.Duration) (AdminRegistrationBatch, error) {
	limit := 16
	if since == "" {
		limit = 0
	}
	filter := `{"room":{"rooms":[` + strconv.Quote(room) + `],"timeline":{"limit":` + strconv.Itoa(limit) + `,"types":["m.room.message"]},"state":{"types":[]},"ephemeral":{"types":[]},"account_data":{"types":[]}},"presence":{"types":[]},"account_data":{"types":[]}}`
	q := url.Values{"timeout": {strconv.FormatInt(wait.Milliseconds(), 10)}, "filter": {filter}}
	if since != "" {
		q.Set("since", since)
	}
	var out AdminRegistrationBatch
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/sync", q, nil, "", -1)
	if err != nil {
		return out, err
	}
	v, err := readControl(ctx, resp, (256<<10)+1)
	if err != nil {
		return out, err
	}
	if err = v.consume(ctx, nil, "next_batch"); err != nil {
		return out, err
	}
	out.NextBatch = v.value["next_batch"].(string)
	rooms, ok := v.value["rooms"].(map[string]any)
	if !ok {
		return out, errors.New("admin sync rooms malformed")
	}
	out.Join = map[string]AdminRegistrationTimeline{}
	if raw, exists := rooms["join"]; exists {
		joined, ok := raw.(map[string]any)
		if !ok {
			return out, errors.New("admin sync join malformed")
		}
		for id, raw := range joined {
			object, ok := raw.(map[string]any)
			if !ok {
				return out, errors.New("admin sync room malformed")
			}
			timeline, ok := object["timeline"].(map[string]any)
			if !ok {
				return out, errors.New("admin sync timeline malformed")
			}
			events, ok := timeline["events"].([]any)
			if !ok {
				return out, errors.New("admin sync events malformed")
			}
			tl := AdminRegistrationTimeline{}
			if raw, exists := timeline["limited"]; exists {
				b, ok := raw.(bool)
				if !ok {
					return out, errors.New("admin sync limited malformed")
				}
				tl.Limited = b
			}
			for _, raw := range events {
				event, err := adminRegistrationEvent(raw)
				if err != nil {
					return out, err
				}
				tl.Events = append(tl.Events, event)
			}
			out.Join[id] = tl
		}
	}
	return out, nil
}

// RedactRegistrationEvent is the narrow best-effort cleanup used by account
// admission; it adds no general-purpose CLI redaction surface.
func (c *Client) RedactRegistrationEvent(ctx context.Context, room, event string) error {
	var out struct {
		EventID string `json:"event_id"`
	}
	return c.WithoutRedirects().call(ctx, http.MethodPut, v3+"/rooms/"+seg(room)+"/redact/"+seg(event)+"/"+NewTxnID(), nil, struct{}{}, &out, "event_id")
}

// AdminRegistrationRoomState validates the full bounded state array before
// any membership or federation safety decision. No loose bulk decoding.
func (c *Client) AdminRegistrationRoomState(ctx context.Context, room string) ([]Event, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/rooms/"+seg(room)+"/state", nil, nil, "", -1)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	closeErr := resp.Body.Close()
	if readErr != nil || closeErr != nil || len(raw) > 1<<20 || resp.StatusCode != 200 || resp.ContentLength >= 0 && int64(len(raw)) != resp.ContentLength {
		return nil, errors.New("admin room state incomplete")
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	tree, err := strictJSON(raw)
	if err != nil {
		return nil, errors.New("admin room state malformed")
	}
	array, ok := tree.([]any)
	if !ok {
		return nil, errors.New("admin room state malformed")
	}
	var out []Event
	for _, raw := range array {
		ev, err := adminRegistrationEvent(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, nil
}

// Map exact authoritative keys from the strict tree. Content is serialized from
// that same tree, never reparsed from the original response by a loose decoder.
func adminRegistrationEvent(raw any) (Event, error) {
	var out Event
	object, ok := raw.(map[string]any)
	if !ok {
		return out, errors.New("admin event malformed")
	}
	for name, dst := range map[string]*string{"event_id": &out.EventID, "room_id": &out.RoomID, "sender": &out.Sender, "type": &out.Type} {
		if raw, exists := object[name]; exists {
			s, ok := raw.(string)
			if !ok {
				return out, errors.New("admin event malformed")
			}
			*dst = s
		}
	}
	if raw, exists := object["state_key"]; exists {
		s, ok := raw.(string)
		if !ok {
			return out, errors.New("admin event malformed")
		}
		out.StateKey = &s
	}
	if raw, exists := object["origin_server_ts"]; exists {
		n, ok := raw.(json.Number)
		if !ok {
			return out, errors.New("admin event malformed")
		}
		n64, err := strconv.ParseInt(string(n), 10, 64)
		if err != nil {
			return out, errors.New("admin event malformed")
		}
		out.OriginServerTS = n64
	}
	content, ok := object["content"].(map[string]any)
	if !ok {
		return out, errors.New("admin event content malformed")
	}
	out.Content, _ = json.Marshal(content)
	return out, nil
}

// ForAdminRegistration retains the server's typed status/code for fixed stage
// diagnostics, including 401 session refusals. Other clients keep ErrAuth.
func (c *Client) ForAdminRegistration() *Client {
	copy := c.WithoutRedirects()
	copy.adminRegistration = true
	return copy
}
