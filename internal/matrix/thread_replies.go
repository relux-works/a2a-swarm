package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// maxRelationsPageBytes bounds one relations response, independently of the
// caller's event/page scan limits. Oversize pages are incomplete, never empty.
const maxRelationsPageBytes = 16 << 20

// ThreadReplies lists replies to root (v1 /relations/{root}/m.thread). dir "b"
// is newest first. An empty chunk with next_batch still has more pages.
func (c *Client) ThreadReplies(ctx context.Context, roomID, root, from, dir string, limit int) (Page, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	q := url.Values{"limit": {strconv.Itoa(limit)}, "dir": {dir}}
	if from != "" {
		q.Set("from", from)
	}
	resp, err := c.send(ctx, http.MethodGet, v1+"/rooms/"+seg(roomID)+"/relations/"+seg(root)+"/m.thread", q, nil, "", -1)
	if err != nil {
		return Page{}, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRelationsPageBytes+1))
	closeErr := resp.Body.Close()
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if resp.StatusCode != http.StatusOK || readErr != nil || closeErr != nil || len(raw) > maxRelationsPageBytes || (resp.ContentLength >= 0 && resp.ContentLength != int64(len(raw))) {
		return Page{}, errors.New("matrix: incomplete relations response")
	}
	out, err := decodeRelationsPage(raw)
	if err == nil && c.FilterEvents != nil {
		out.Chunk, err = c.FilterEvents(roomID, out.Chunk)
	}
	return out, err
}

// Validate the pagination envelope while retaining the bulk decoder's peer
// content compatibility inside events (including duplicate message-body keys).
func decodeRelationsPage(raw []byte) (Page, error) {
	invalid := errors.New("matrix: invalid relations page")
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return Page{}, invalid
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return Page{}, invalid
		}
		name := token.(string)
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return Page{}, invalid
		}
		switch strings.ToLower(name) {
		case "chunk", "next_batch", "end":
			if name != strings.ToLower(name) {
				return Page{}, invalid
			}
			if _, exists := fields[name]; exists {
				return Page{}, invalid
			}
			fields[name] = value
		}
	}
	if _, err = d.Token(); err != nil {
		return Page{}, invalid
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return Page{}, invalid
	}
	var out Page
	chunk := bytes.TrimSpace(fields["chunk"])
	if len(chunk) == 0 || chunk[0] != '[' || json.Unmarshal(chunk, &out.Chunk) != nil {
		return Page{}, invalid
	}
	for name, dst := range map[string]*string{"next_batch": &out.NextBatch, "end": &out.End} {
		if value, present := fields[name]; present {
			value = bytes.TrimSpace(value)
			if len(value) == 0 || value[0] != '"' || json.Unmarshal(value, dst) != nil || *dst == "" {
				return Page{}, invalid
			}
		}
	}
	return out, nil
}
