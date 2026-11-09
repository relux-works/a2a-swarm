package matrixtransport

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	tr "github.com/relux-works/a2a-swarm/transport"
)

// These constants are pinned against the CLI copies until the read path moves.
const SyncFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"lazy_load_members":true,"types":[]},"timeline":{"limit":100,"types":["m.room.message","m.room.redaction"]}}}`
const InitialFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"types":[]},"timeline":{"limit":1,"types":["m.room.message","m.room.redaction"]}}}`
const FirstDeliveryFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"types":[]},"timeline":{"limit":100,"types":["m.room.message","m.room.redaction","m.room.member"]}}}`

func project(room string, e matrix.Event) (tr.Event, bool) { return projectRaw(room, e, nil) }
func projectRaw(room string, e matrix.Event, raw []byte) (tr.Event, bool) {
	if !receivedPrincipal(e.Sender) || !eventID(e.EventID) || e.RoomID != "" && e.RoomID != room || len(e.Content) > MaxContentBytes {
		return tr.Event{}, false
	}
	if raw != nil && !e.IsRedacted() && !e.RedactionVerified {
		var wire matrix.Event
		if json.Unmarshal(raw, &wire) != nil || wire.EventID != e.EventID || wire.Sender != e.Sender || wire.RoomID != e.RoomID || wire.Type != e.Type || wire.OriginServerTS != e.OriginServerTS || !bytes.Equal(wire.Content, e.Content) {
			return tr.Event{}, false
		}
	}
	if e.IsRedacted() || e.RedactionVerified {
		e = e.WithoutContent()
		raw = nil
	}
	if raw == nil {
		var err error
		raw, err = json.Marshal(e)
		if err != nil {
			return tr.Event{}, false
		}
	}
	if len(raw) > MaxRawBytes {
		return tr.Event{}, false
	}
	m := e.Message()
	v := tr.Event{ID: tr.EventID(e.EventID), Conversation: tr.ConversationID(room), Sender: tr.PrincipalID(e.Sender), Time: time.UnixMilli(e.OriginServerTS), Raw: append([]byte(nil), raw...), Redacted: e.IsRedacted(), Message: tr.Message{Text: m.Body}}
	if m.RelatesTo != nil {
		if m.RelatesTo.RelType == "m.thread" {
			v.Message.Thread = tr.EventID(m.RelatesTo.EventID)
		}
		if m.RelatesTo.InReplyTo != nil {
			v.Message.Re = tr.EventID(m.RelatesTo.InReplyTo.EventID)
		}
	}
	if m.Mentions != nil {
		for _, id := range m.Mentions.UserIDs {
			if receivedPrincipal(id) {
				v.Message.To = append(v.Message.To, tr.PrincipalID(id))
			}
		}
	}
	if len(m.A2A) > 0 {
		var h header
		if json.Unmarshal(m.A2A, &h) == nil {
			v.Message.Kind = h.Kind
			if m.Mentions == nil {
				for _, s := range h.To {
					if canonical(s, '@') {
						v.Message.To = append(v.Message.To, tr.PrincipalID(s))
					}
				}
			}
		}
		v.Message.Extensions = map[string]json.RawMessage{ContentKey: append(json.RawMessage(nil), m.A2A...)}
	}
	// Project admitted opaque references and optional text extensions without
	// exposing reference contents in diagnostics. Unknown wire keys stay in Raw.
	var fields map[string]json.RawMessage
	if json.Unmarshal(e.Content, &fields) != nil {
		return tr.Event{}, false
	}
	if refs, ok := fields[mediaRefsKey]; ok {
		var values []string
		if len(bytes.TrimSpace(refs)) == 0 || bytes.TrimSpace(refs)[0] != '[' || json.Unmarshal(refs, &values) != nil {
			return tr.Event{}, false
		}
		for _, value := range values {
			if _, e := matrix.ParseMXC(value); e != nil {
				return tr.Event{}, false
			}
			v.Message.Attachments = append(v.Message.Attachments, tr.NewMediaRef(value))
		}
	}
	for _, key := range []string{"format", "formatted_body"} {
		if raw, ok := fields[key]; ok {
			var text *string
			if json.Unmarshal(raw, &text) != nil || text == nil {
				return tr.Event{}, false
			}
			if v.Message.Extensions == nil {
				v.Message.Extensions = map[string]json.RawMessage{}
			}
			v.Message.Extensions[key] = append(json.RawMessage(nil), raw...)
		}
	}
	return v, true
}
