package matrixtransport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const MaxResponseBytes = 16 << 20
const MaxBatchEvents = 1000

type capture struct {
	body       []byte
	underlying http.RoundTripper
}

// readClient scopes capture to ONE operation. It bounds the complete entity
// before the existing bulk decoder runs, preserving the exact event JSON and
// leaving the supplied client's policy and transport unchanged.
func readClient(c *matrix.Client) (*matrix.Client, *capture) {
	copy := *c
	hc := http.DefaultClient
	if c.HTTP != nil {
		hc = c.HTTP
	}
	httpCopy := *hc
	underlying := httpCopy.Transport
	if underlying == nil {
		underlying = http.DefaultTransport
	}
	cap := &capture{underlying: underlying}
	httpCopy.Transport = cap
	copy.HTTP = &httpCopy
	return &copy, cap
}
func (c *capture) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, e := c.underlying.RoundTrip(r)
	if e != nil {
		return resp, e
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp, nil
	}
	raw, e := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	closeErr := resp.Body.Close()
	if e != nil || closeErr != nil || r.Context().Err() != nil || len(raw) > MaxResponseBytes || resp.ContentLength >= 0 && resp.ContentLength != int64(len(raw)) {
		return nil, errors.New("transport: incomplete or oversized response")
	}
	c.body = raw
	resp.Body = io.NopCloser(bytes.NewReader(raw))
	return resp, nil
}
func rawIndex(raw []byte, room, mode string) map[string]json.RawMessage {
	index := make(map[string]json.RawMessage)
	add := func(room string, list []json.RawMessage) {
		for _, b := range list {
			var id struct {
				ID string `json:"event_id"`
			}
			if json.Unmarshal(b, &id) == nil {
				index[room+"\x00"+id.ID] = b
			}
		}
	}
	switch mode {
	case "event":
		add(room, []json.RawMessage{raw})
	case "page":
		var page struct {
			Chunk []json.RawMessage `json:"chunk"`
		}
		if json.Unmarshal(raw, &page) == nil {
			add(room, page.Chunk)
		}
	case "sync":
		var sync struct {
			Rooms struct {
				Join map[string]struct {
					Timeline struct {
						Events []json.RawMessage `json:"events"`
					} `json:"timeline"`
				} `json:"join"`
			} `json:"rooms"`
		}
		if json.Unmarshal(raw, &sync) == nil {
			for room, j := range sync.Rooms.Join {
				add(room, j.Timeline.Events)
			}
		}
	}
	return index
}
