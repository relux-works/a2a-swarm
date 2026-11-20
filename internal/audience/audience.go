// Package audience resolves notification audiences. It grants neither read
// access nor signature verification; ordinary selectors and effect fences
// still decide whether eligible mail may be announced.
package audience

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const (
	Version                 = 1
	Self                    = "self"
	Other                   = "other"
	General                 = "general"
	Invalid                 = "invalid"
	Unresolved              = "unresolved"
	Unavailable             = "unavailable"
	Header                  = "header"
	Mentions                = "mentions"
	Legacy                  = "general"
	AddressOther            = "ADDRESS_OTHER"
	AddressMetadataInvalid  = "ADDRESS_METADATA_INVALID"
	AddressTargetUnresolved = "ADDRESS_TARGET_UNRESOLVED"
	AddressStateUnavailable = "ADDRESS_STATE_UNAVAILABLE"
)

// Evidence contains no body, replacement content, recipient list or authority.
// Identity and room scope prevent reuse for a different notification consumer.
type Evidence struct {
	Version     int       `json:"version"`
	Room        string    `json:"room"`
	EventID     string    `json:"event_id"`
	UserID      string    `json:"user_id"`
	Name        string    `json:"name"`
	Sender      string    `json:"sender"`
	Source      string    `json:"source"`
	Decision    string    `json:"decision"`
	Target      string    `json:"target,omitempty"`
	Inherited   bool      `json:"inherited,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
	RetainUntil time.Time `json:"retain_until,omitempty"`
}

func (e Evidence) Allows(mandatory bool) bool {
	return e.Version == Version && (e.Decision == Self || e.Decision == General || (e.Decision == Invalid && mandatory))
}

func (e Evidence) Diagnostic() string {
	if e.Version != Version {
		return AddressStateUnavailable
	}
	switch e.Decision {
	case Self, General:
		return ""
	case Other:
		return AddressOther
	case Invalid:
		return AddressMetadataInvalid
	case Unresolved:
		return AddressTargetUnresolved
	default:
		return AddressStateUnavailable
	}
}

// object rejects duplicate keys rather than choosing an ambiguous recipient
// intent according to a JSON decoder's last-key behavior.
func object(raw []byte, relevant ...string) (map[string]json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	t, err := d.Token()
	if err != nil || t != json.Delim('{') {
		return nil, errors.New("object required")
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		t, err = d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := t.(string)
		if !ok {
			return nil, errors.New("key required")
		}
		if _, exists := out[key]; exists && (len(relevant) == 0 || slices.Contains(relevant, key)) {
			return nil, errors.New("duplicate key")
		}
		var v json.RawMessage
		if err = d.Decode(&v); err != nil {
			return nil, err
		}
		out[key] = v
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, errors.New("trailing value")
	}
	return out, nil
}

func absent(raw []byte) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func validRecipient(s string, full bool) bool {
	if s == "" || strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	if strings.HasPrefix(s, "@") {
		local, server, ok := strings.Cut(s[1:], ":")
		return ok && local != "" && server != "" && !strings.ContainsAny(local, "@:") && !strings.ContainsAny(server, "@/")
	}
	if full {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._=-/", r)) {
			return false
		}
	}
	return true
}

func recipientList(raw []byte, full bool) ([]string, bool) {
	if !utf8.Valid(raw) {
		return nil, false
	}
	if absent(raw) {
		return nil, true
	}
	var values []json.RawMessage
	if json.Unmarshal(raw, &values) != nil {
		return nil, false
	}
	var out []string
	for _, v := range values {
		var s string
		if json.Unmarshal(v, &s) != nil || !validRecipient(s, full) {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func addressed(ids []string, me, name string) string {
	for _, id := range ids {
		if id == "all" {
			return General
		}
	}
	for _, id := range ids {
		if id == me || id == name {
			return Self
		}
	}
	return Other
}

// Parse handles original messages and independent replies. Relations requiring
// an immutable target ceiling are resolved by Cycle, never guessed as general.
func Parse(ev matrix.Event, room, me, name string, now time.Time) Evidence {
	e := Evidence{Version: Version, Room: room, EventID: ev.EventID, UserID: me, Name: name, Sender: ev.Sender, Source: Legacy, Decision: General, ObservedAt: now.UTC()}
	if ev.IsRedacted() {
		e.Decision = Unresolved
		return e
	}
	c, err := object(ev.Content, "ai.relux.a2a", "m.mentions")
	if err != nil {
		e.Decision = Invalid
		return e
	}
	if !absent(c["ai.relux.a2a"]) {
		e.Source = Header
		h, err := object(c["ai.relux.a2a"], "to")
		if err != nil {
			e.Decision = Invalid
			return e
		}
		ids, ok := recipientList(h["to"], false)
		if !ok {
			e.Decision = Invalid
			return e
		}
		if len(ids) > 0 {
			e.Decision = addressed(ids, me, name)
			return e
		}
		e.Source = Legacy
	}
	if !absent(c["m.mentions"]) {
		e.Source = Mentions
		m, err := object(c["m.mentions"], "room", "user_ids")
		if err != nil {
			e.Decision = Invalid
			return e
		}
		var roomMention bool
		if raw := m["room"]; len(raw) > 0 && (absent(raw) || json.Unmarshal(raw, &roomMention) != nil) {
			e.Decision = Invalid
			return e
		}
		ids, ok := recipientList(m["user_ids"], true)
		if !ok {
			e.Decision = Invalid
			return e
		}
		if roomMention {
			return e
		}
		if len(ids) > 0 {
			e.Decision = addressed(ids, me, "")
			return e
		}
		e.Source = Legacy
	}
	return e
}
