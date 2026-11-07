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
	out, err := decodeRelationsPage(raw, roomID, root)
	if err == nil && c.FilterEvents != nil {
		out.Chunk, err = c.FilterEvents(roomID, out.Chunk)
	}
	return out, err
}

// Validate the pagination envelope while retaining the bulk decoder's peer
// content compatibility inside events (including duplicate message-body keys).
func decodeRelationsPage(raw []byte, roomID, root string) (Page, error) {
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
	var events []json.RawMessage
	if len(chunk) == 0 || chunk[0] != '[' || json.Unmarshal(chunk, &events) != nil {
		return Page{}, invalid
	}
	out.Chunk = make([]Event, 0, len(events))
	for _, rawEvent := range events {
		ev, err := decodeRelationsEvent(rawEvent, roomID, root)
		if err != nil {
			return Page{}, invalid
		}
		out.Chunk = append(out.Chunk, ev)
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

// relationsFields inspects only named policy members. Unknown peer fields,
// including message bodies, retain the ordinary decoder's compatibility.
// Protected names must be canonical and unique, including escaped spellings.
func relationsFields(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	invalid := errors.New("matrix: invalid relations event")
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return nil, invalid
	}
	fields := map[string]json.RawMessage{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return nil, invalid
		}
		key := token.(string)
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, invalid
		}
		for _, name := range names {
			if !strings.EqualFold(key, name) {
				continue
			}
			if _, duplicate := fields[name]; duplicate || key != name {
				return nil, invalid
			}
			fields[name] = value
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, invalid
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		return nil, invalid
	}
	return fields, nil
}

func relationsString(raw []byte) (string, bool) {
	// Only policy scalars use the strict Unicode parser, never the user body.
	v, err := strictJSON(raw)
	s, ok := v.(string)
	return s, err == nil && ok && s != ""
}

type relationsPolicy struct {
	present      bool
	root, parent string
}

func relationsContent(raw []byte) (relationsPolicy, error) {
	var out relationsPolicy
	invalid := errors.New("matrix: invalid thread relation")
	content, err := relationsFields(raw, "m.relates_to")
	if err != nil {
		return out, invalid
	}
	relation, present := content["m.relates_to"]
	if !present {
		return out, nil
	}
	out.present = true
	fields, err := relationsFields(relation, "rel_type", "event_id", "m.in_reply_to", "is_falling_back")
	if err != nil {
		return out, invalid
	}
	typ, ok := relationsString(fields["rel_type"])
	if !ok || typ != "m.thread" {
		return out, invalid
	}
	out.root, ok = relationsString(fields["event_id"])
	if !ok || !ValidVerificationID(out.root, '$') {
		return out, invalid
	}
	if value, present := fields["is_falling_back"]; present {
		v, err := strictJSON(value)
		if _, ok := v.(bool); err != nil || !ok {
			return out, invalid
		}
	}
	if value, present := fields["m.in_reply_to"]; present {
		parent, err := relationsFields(value, "event_id")
		if err != nil {
			return out, invalid
		}
		out.parent, ok = relationsString(parent["event_id"])
		if !ok || !ValidVerificationID(out.parent, '$') {
			return out, invalid
		}
	}
	return out, nil
}

func decodeRelationsEvent(raw []byte, roomID, root string) (Event, error) {
	var out Event
	invalid := errors.New("matrix: invalid relations event")
	fields, err := relationsFields(raw, "event_id", "room_id", "sender", "type", "content", "unsigned")
	if err != nil {
		return out, invalid
	}
	id, ok := relationsString(fields["event_id"])
	if !ok || !ValidVerificationID(id, '$') {
		return out, invalid
	}
	if _, ok := relationsString(fields["type"]); !ok {
		return out, invalid
	}
	for name, sigil := range map[string]byte{"room_id": '!', "sender": '@'} {
		if value, present := fields[name]; present {
			s, ok := relationsString(value)
			if !ok || !ValidVerificationID(s, sigil) {
				return out, invalid
			}
			if name == "room_id" && s != roomID {
				return out, invalid
			}
		}
	}
	// Bind original declarations before Event/Message decoding or FilterEvents
	// can lose evidence. An absent relation remains subject to the caller's
	// redaction policy; a declared contradiction is always refused here.
	policy, err := relationsContent(fields["content"])
	if err != nil {
		return out, err
	}
	if id == root || policy.parent == id || (policy.present && policy.root != root) {
		return out, invalid
	}
	if value, present := fields["unsigned"]; present && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		unsigned, err := relationsFields(value, "redacted_because")
		if err != nil {
			return out, invalid
		}
		if redaction, present := unsigned["redacted_because"]; present && !bytes.Equal(bytes.TrimSpace(redaction), []byte("null")) {
			if _, err := relationsFields(redaction); err != nil {
				return out, invalid
			}
		}
	}
	if json.Unmarshal(raw, &out) != nil {
		return Event{}, invalid
	}
	return out, nil
}
