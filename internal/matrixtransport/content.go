package matrixtransport

import (
	"encoding/json"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	tr "github.com/relux-works/a2a-swarm/transport"
)

const ContentKey = "ai.relux.a2a"

// mediaRefsKey carries neutral opaque references without changing existing post bytes.
const mediaRefsKey = "ai.relux.a2a.media"

// header preserves the current CLI's field ordering as well as its optional fields.
type header struct {
	V            int             `json:"v"`
	Type         string          `json:"type"`
	Slug         string          `json:"slug,omitempty"`
	Title        string          `json:"title,omitempty"`
	Topic        string          `json:"topic,omitempty"`
	From         string          `json:"from"`
	To           []string        `json:"to,omitempty"`
	At           string          `json:"at"`
	Re           string          `json:"re,omitempty"`
	Kind         string          `json:"kind,omitempty"`
	ReplyBy      string          `json:"reply_by,omitempty"`
	Trunk        string          `json:"trunk,omitempty"`
	Subtype      string          `json:"subtype,omitempty"`
	Subject      string          `json:"subject,omitempty"`
	ProductAlias string          `json:"product_alias,omitempty"`
	SubRequest   json.RawMessage `json:"sub_request,omitempty"`
	Attachment   json.RawMessage `json:"attachment,omitempty"`
}

// Content builds deterministic wire JSON. Extensions carry optional a2a header
// fields and formatted text from the caller; transport-owned fields cannot be overridden.
func Content(m tr.Message) ([]byte, error) {
	invalid := func() ([]byte, error) { return nil, tr.NewError(tr.InvalidReference) }
	if m.Re != "" && !eventID(string(m.Re)) || m.Thread != "" && !eventID(string(m.Thread)) {
		return invalid()
	}
	to := make([]string, 0, len(m.To))
	for _, p := range m.To {
		if !canonical(string(p), '@') {
			return invalid()
		}
		to = append(to, string(p))
	}
	c := map[string]any{"msgtype": "m.text", "body": m.Text}
	if len(m.Attachments) > 0 {
		refs := make([]string, 0, len(m.Attachments))
		for _, ref := range m.Attachments {
			if _, e := matrix.ParseMXC(ref.Opaque()); e != nil {
				return invalid()
			}
			refs = append(refs, ref.Opaque())
		}
		c[mediaRefsKey] = refs
	}
	h := header{V: 1, Type: "message"}
	supplied := false
	for key, value := range m.Extensions {
		switch key {
		case ContentKey:
			if json.Unmarshal(value, &h) != nil {
				return invalid()
			}
			supplied = true
		case "format", "formatted_body":
			var s string
			if json.Unmarshal(value, &s) != nil {
				return invalid()
			}
			c[key] = s
		default:
			return invalid()
		}
	}
	if m.Kind != "" {
		h.Kind = m.Kind
	}
	if len(to) > 0 && len(h.To) == 0 {
		h.To = to
	}
	if m.Thread != "" {
		h.Topic = string(m.Thread)
	}
	if m.Re != "" {
		h.Re = string(m.Re)
	}
	if supplied || m.Kind != "" || len(to) > 0 || m.Thread != "" || m.Re != "" {
		c[ContentKey] = h
		c["m.mentions"] = matrix.Mentions{UserIDs: to}
	}
	if m.Thread != "" || m.Re != "" {
		target := m.Re
		if target == "" {
			target = m.Thread
		}
		rel := matrix.RelatesTo{InReplyTo: &matrix.InReplyTo{EventID: string(target)}}
		if m.Thread != "" {
			rel.RelType = "m.thread"
			rel.EventID = string(m.Thread)
			rel.IsFallingBack = m.Re == ""
		}
		c["m.relates_to"] = rel
	}
	b, e := json.Marshal(c)
	if e != nil {
		return invalid()
	}
	return b, nil
}
