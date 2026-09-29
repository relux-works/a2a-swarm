package cli

import (
	"encoding/json"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// EventRecord is the machine-readable form of one event, printed as one JSON
// object per line by `read --json` and `wait --json`. Text fields are
// sanitized exactly like the human-readable output.
type EventRecord struct {
	Type    string   `json:"type"` // read: "topic" or "message"; wait: "to", "mention", "thread" or "invite"
	Room    string   `json:"room,omitempty"`
	RoomID  string   `json:"room_id"`
	EventID string   `json:"event_id,omitempty"`
	Sender  string   `json:"sender,omitempty"`
	At      string   `json:"at,omitempty"`
	Kind    string   `json:"kind,omitempty"`
	Topic   string   `json:"topic,omitempty"` // topic slug
	Title   string   `json:"title,omitempty"` // topic roots only
	Root    string   `json:"root,omitempty"`  // thread root event id
	To      []string `json:"to,omitempty"`
	Re      string   `json:"re,omitempty"`
	ReplyBy string   `json:"reply_by,omitempty"`
	Trunk   string   `json:"trunk,omitempty"`
	Excerpt string   `json:"excerpt,omitempty"` // wait: at most SnippetMax characters
	Body    string   `json:"body,omitempty"`    // read: the full text without the header lines
	// Attachment is set for file messages (a2a attach).
	Attachment *Attachment `json:"attachment,omitempty"`
}

// eventRecord builds the record for a message event. typ is the record type;
// the text goes to Body (read) or, bounded, to Excerpt (wait).
func eventRecord(typ, room, roomID string, ev matrix.Event, server string, excerpt bool) EventRecord {
	m := ev.Message()
	r := EventRecord{
		Type:    typ,
		Room:    room,
		RoomID:  roomID,
		EventID: ev.EventID,
		Sender:  localpartIfLocal(ev.Sender, server),
		At:      time.UnixMilli(ev.OriginServerTS).UTC().Format(time.RFC3339),
		Root:    ev.ThreadRoot(),
	}
	text := m.Body
	if h, ok := decodeHeader(m.A2A); ok {
		r.Kind, r.Topic, r.To, r.Re, r.ReplyBy, r.Trunk = h.Kind, h.Slug, h.To, h.Re, h.ReplyBy, h.Trunk
		if at := h.Attachment; at != nil {
			r.Attachment = &Attachment{Name: sanitizeLine(at.Name), Size: at.Size, SHA256: sanitizeLine(at.SHA256),
				MXC: sanitizeLine(at.MXC), MimeType: sanitizeLine(at.MimeType)}
		}
		if h.Type == "topic" {
			r.Title = h.Title
		} else {
			text = stripHeaderBody(text)
		}
	}
	if r.Topic == "" {
		if slug, title, ok := parseTopicBody(m.Body); ok && r.Root == "" {
			r.Topic, r.Title = slug, title
		}
	}
	for i, t := range r.To {
		r.To[i] = sanitizeLine(t)
	}
	if excerpt {
		r.Excerpt = snippet(text, SnippetMax)
	} else {
		r.Body = sanitizeText(text)
	}
	return r
}

func (a *App) printJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	a.printf("%s\n", data)
	return nil
}
