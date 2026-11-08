package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func validateMessageKind(kind string) error {
	if !validKind(kind) {
		return usagef("invalid --kind %q (one of: %s)", kind, strings.Join(Kinds, ", "))
	}
	return nil
}

func defaultReplyBy(kind string, now time.Time) string {
	if kind == "question" {
		return now.Add(DefaultReplyBy).Format(time.RFC3339)
	}
	return ""
}

// replyTopicEvent uses the original authenticated room-scoped endpoint. The
// strict reader binds the returned event ID and any room ID to that request,
// rejects redirects/redactions/ambiguous JSON, and retains the connection guard.
func (s *session) replyTopicEvent(ctx context.Context, roomID, id string) (matrix.MessageContent, error) {
	if err := s.checkReplyRedaction(roomID, matrix.Event{EventID: id}); err != nil {
		return matrix.MessageContent{}, err
	}
	ev, err := s.client.GetVerificationEvent(ctx, roomID, id)
	// Recheck even on a historical refusal: a tombstone learned during the
	// request must not be mistaken for permission to infer from the child.
	if filterErr := s.checkReplyRedaction(roomID, matrix.Event{EventID: id, Sender: ev.Sender, Type: ev.Type, Content: ev.Content}); filterErr != nil {
		return matrix.MessageContent{}, filterErr
	}
	if err != nil {
		return matrix.MessageContent{}, fmt.Errorf("cannot resolve --re event in the selected room: %w", err)
	}
	var m matrix.MessageContent
	if err := json.Unmarshal(ev.Content, &m); err != nil {
		return m, fmt.Errorf("invalid --re message content")
	}
	// Decode routing fields only under their exact wire spelling. Go's usual
	// struct decoder otherwise accepts case aliases, including inside relations.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(ev.Content, &fields); err != nil {
		return m, fmt.Errorf("invalid --re message content")
	}
	for k := range fields {
		for _, key := range []string{"m.relates_to", "body", ContentKey} {
			if k != key && strings.EqualFold(k, key) {
				return m, fmt.Errorf("ambiguous --re message content")
			}
		}
	}
	if raw, present := fields["m.relates_to"]; present {
		var relation map[string]json.RawMessage
		if json.Unmarshal(raw, &relation) != nil || relation == nil {
			return m, fmt.Errorf("invalid --re thread relation")
		}
		for k := range relation {
			for _, key := range []string{"rel_type", "event_id", "m.in_reply_to", "is_falling_back"} {
				if k != key && strings.EqualFold(k, key) {
					return m, fmt.Errorf("ambiguous --re thread relation")
				}
			}
		}
		for _, key := range []string{"rel_type", "event_id"} {
			if raw, present := relation[key]; present && string(raw) == "null" {
				return m, fmt.Errorf("invalid --re thread relation")
			}
		}
		if m.RelatesTo == nil || (m.RelatesTo.RelType != "m.thread" && m.RelatesTo.RelType != "") ||
			(m.RelatesTo.RelType == "m.thread" && !matrix.ValidVerificationID(m.RelatesTo.EventID, '$')) ||
			(m.RelatesTo.RelType == "" && (m.RelatesTo.EventID != "" || m.RelatesTo.InReplyTo == nil)) {
			return m, fmt.Errorf("--re target is not in a valid thread")
		}
		if raw, present := relation["is_falling_back"]; present {
			var fallback bool
			if string(raw) == "null" || json.Unmarshal(raw, &fallback) != nil {
				return m, fmt.Errorf("invalid --re fallback relation")
			}
		}
		if in, present := relation["m.in_reply_to"]; present {
			var reply map[string]json.RawMessage
			var id string
			if json.Unmarshal(in, &reply) != nil || reply == nil ||
				json.Unmarshal(reply["event_id"], &id) != nil || !matrix.ValidVerificationID(id, '$') {
				return m, fmt.Errorf("invalid --re reply relation")
			}
			for k := range reply {
				if k != "event_id" && strings.EqualFold(k, "event_id") {
					return m, fmt.Errorf("ambiguous --re reply relation")
				}
			}
		}
	}
	return m, nil
}

// The strict event reader deliberately bypasses display filtering. Consult
// the session's room-scoped suppression cache without relaxing that reader's
// binding or using locally retained payloads to establish thread membership.
func (s *session) checkReplyRedaction(roomID string, ev matrix.Event) error {
	if s.client.FilterEvents == nil {
		return nil
	}
	events, err := s.client.FilterEvents(roomID, []matrix.Event{ev})
	if err != nil {
		return err
	}
	if len(events) != 1 || events[0].IsRedacted() {
		return errors.New("--re event is locally redacted; refusing reply routing")
	}
	return nil
}

// resolveReplyTopic returns an empty root/slug for main-timeline replies. Plain
// targets stay in the main timeline; replyMain forces that form for any target.
// Otherwise follow exactly one m.thread relation, never a native reply chain or
// an a2a header's claimed topic. A child's authenticated relation can identify
// an ordinary Matrix root even when historical access prevents reading it.
func (s *session) resolveReplyTopic(ctx context.Context, roomID, label, target string, replyMain bool) (string, string, error) {
	m, err := s.replyTopicEvent(ctx, roomID, target)
	if err != nil {
		return "", "", err
	}
	threaded := m.RelatesTo != nil && m.RelatesTo.RelType == "m.thread"
	_, _, topicBody := parseTopicBody(m.Body)
	h, header := decodeHeader(m.A2A)
	// A native reply does not disqualify a topic marker from being a root,
	// matching listTopics/resolveTopic. An explicit m.thread still wins.
	topicRoot := !threaded && (topicBody || (header && h.Type == "topic" && slugRE.MatchString(h.Slug)))
	if threaded && m.RelatesTo.EventID == target {
		return "", "", fmt.Errorf("--re target has a cyclic thread relation")
	}
	if !threaded && !topicRoot && header && h.Topic != "" {
		return "", "", fmt.Errorf("--re target has only a header topic claim in %s, not server thread membership", label)
	}
	if replyMain || (!threaded && !topicRoot) {
		return "", "", nil
	}
	root := target
	if threaded {
		root = m.RelatesTo.EventID
		child := m
		m, err = s.replyTopicEvent(ctx, roomID, root)
		if err != nil {
			// Only an original-endpoint historical refusal permits routing from
			// the already validated child. Never mask transport or content errors.
			var me *matrix.Error
			if errors.As(err, &me) && me.OriginalRequest &&
				((me.Status == http.StatusNotFound && me.ErrCode == "M_NOT_FOUND") ||
					(me.Status == http.StatusForbidden && me.ErrCode == "M_FORBIDDEN")) {
				slug := ""
				if h, ok := decodeHeader(child.A2A); ok && slugRE.MatchString(h.Slug) {
					slug = h.Slug
				}
				return root, slug, nil
			}
			return "", "", err
		}
		if m.RelatesTo != nil {
			return "", "", fmt.Errorf("--re thread root is itself related to another event")
		}
	}
	slug, _, _ := parseTopicBody(m.Body)
	if slug == "" {
		if h, ok := decodeHeader(m.A2A); ok && h.Type == "topic" && slugRE.MatchString(h.Slug) {
			slug = h.Slug
		}
	}
	return root, slug, nil
}
