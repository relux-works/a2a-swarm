package cli

import (
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// DefaultWaitTimeout is how long `a2a wait` blocks without news.
const DefaultWaitTimeout = 25 * time.Minute

// maxPoll bounds one /sync long-poll so a dead connection is noticed.
const maxPoll = 60 * time.Second

// syncFilter keeps /sync small: messages and redactions in timelines, no presence,
// account data, or ephemeral events. Invites arrive regardless of the filter.
const syncFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"lazy_load_members":true,"types":[]},"timeline":{"limit":100,"types":["m.room.message","m.room.redaction"]}}}`

// initialFilter is the projector's first-sync baseline: only next_batch is needed.
const initialFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"types":[]},"timeline":{"limit":1,"types":["m.room.message","m.room.redaction"]}}}`

// First delivery needs membership in timeline order to locate our own join.
// The projector retains its independent initialFilter/baseline behavior.
const firstDeliveryFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"types":[]},"timeline":{"limit":100,"types":["m.room.message","m.room.redaction","m.room.member"]}}}`

// relevance explains why an event is relevant to me; "" means not relevant.
type relevance string

const (
	relTo      relevance = "to"
	relMention relevance = "mention"
	relThread  relevance = "thread"
	relSub     relevance = "sub"
	relAll     relevance = "all"
)

// eventTopicRoot is shared by read records and delivery. An explicit Matrix
// thread takes precedence; plain replies may carry their topic only in the
// a2a header. Projection origin metadata does not create a local thread.
func eventTopicRoot(ev matrix.Event) string {
	if root := ev.ThreadRoot(); root != "" {
		return root
	}
	if h, ok := decodeHeader(ev.Message().A2A); ok && h.Type != "projection" {
		return h.Topic
	}
	return ""
}

// classify decides whether a timeline event concerns this agent. It is pure:
// the caller updates the thread list from the returned learnedRoot. matched
// lists the subscriptions that deliver the event, whatever its relevance, so
// that the caller can close --until-kind subscriptions.
func classify(ev matrix.Event, roomID, me, myName string, st config.State, subs []Sub) (rel relevance, learnedRoot string, matched []string) {
	if ev.Type != "m.room.message" {
		return "", "", nil
	}
	m := ev.Message()
	root := eventTopicRoot(ev)
	if ev.Sender == me {
		// My own message: a reply joins me to its thread; a topic root I sent is a thread I own.
		if root != "" {
			return "", root, nil
		}
		if strings.HasPrefix(m.Body, TopicPrefix) {
			return "", ev.EventID, nil
		}
		return "", "", nil
	}
	h, hasHeader := decodeHeader(m.A2A)
	// Mutes identify canonical thread roots, including the root event itself.
	// Explicit sender filters express delivery intent, even for headerless client
	// messages. They retain their normal matching gates; this grants no authority.
	// Roomless subscriptions and diagnostic capture also remain independent.
	mutedRoot := root
	if mutedRoot == "" {
		mutedRoot = ev.EventID
	}
	muted := st.IsMuted(mutedRoot)
	for _, sub := range subs {
		if muted && sub.Room != "" && sub.Room == roomID && sub.From == "" && !sub.AllEvents && !(hasHeader && h.Type == "projection") {
			continue
		}
		if sub.matches(ev, roomID, h, hasHeader) {
			matched = append(matched, sub.ID)
		}
	}
	if hasHeader && h.Type == "projection" {
		// A projector's mirror of an event that already rang in its own room:
		// only existing projection-eligible subscriptions or `wait --all` report it.
		// Address-like projection headers do not gain direct-address priority.
		if len(matched) > 0 {
			return relSub, "", matched
		}
		return "", "", nil
	}
	if hasHeader {
		for _, t := range h.To {
			if t == myName || t == me {
				return relTo, "", matched
			}
		}
	}
	if m.Mentions != nil {
		for _, u := range m.Mentions.UserIDs {
			if u == me {
				return relMention, "", matched
			}
		}
	}
	if mentionsUser(m.Body, me) {
		return relMention, "", matched
	}
	if len(matched) > 0 {
		return relSub, "", matched
	}
	if root != "" && st.HasThread(root) && !st.IsMuted(root) {
		return relThread, "", nil
	}
	return "", "", nil
}

// mentionsUser reports whether body contains userID as a whole token, so that
// "@alice:test.local" does not match inside "@alice:test.localhost".
func mentionsUser(body, userID string) bool {
	for i := 0; ; {
		j := strings.Index(body[i:], userID)
		if j < 0 {
			return false
		}
		end := i + j + len(userID)
		if !continuesServerName(body[end:]) {
			return true
		}
		i = end
	}
}

// continuesServerName reports whether rest extends the server name that just
// ended (a trailing "." before a space or end of text is punctuation, not a label).
func continuesServerName(rest string) bool {
	if rest == "" {
		return false
	}
	alnum := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' }
	switch b := rest[0]; {
	case alnum(b), b == '-', b == ':':
		return true
	case b == '.':
		return len(rest) > 1 && alnum(rest[1])
	}
	return false
}

// eventLine renders one doorbell line. The body excerpt is sanitized and
// never longer than SnippetMax characters.
func eventLine(rel relevance, room string, ev matrix.Event, server, subID string) string {
	m := ev.Message()
	parts := []string{string(rel), room, ev.EventID, "from=" + localpartIfLocal(ev.Sender, server)}
	if subID != "" {
		parts = append(parts, "sub="+subID)
	}
	text := m.Body
	if h, ok := decodeHeader(m.A2A); ok {
		if h.Kind != "" {
			parts = append(parts, "kind="+h.Kind)
		}
		if h.Subtype != "" {
			parts = append(parts, "subtype="+h.Subtype)
		}
		if h.Subject != "" {
			parts = append(parts, "subject="+h.Subject)
		}
		if h.Slug != "" {
			parts = append(parts, "topic="+h.Slug)
		}
		text = stripHeaderBody(text)
	}
	if root := eventTopicRoot(ev); root != "" {
		parts = append(parts, "root="+root)
	}
	return sanitizeLine(strings.Join(parts, " ")) + ": " + snippet(text, SnippetMax)
}

// inviteInfo extracts the room alias (or the id) and the inviter from invite state.
func inviteInfo(roomID string, state []matrix.Event, me, server string) (room, inviter string) {
	room = roomID
	for _, ev := range state {
		switch ev.Type {
		case "m.room.canonical_alias":
			var c struct {
				Alias string `json:"alias"`
			}
			if jsonUnmarshal(ev.Content, &c) == nil && c.Alias != "" {
				room = strings.TrimSuffix(c.Alias, ":"+server)
			}
		case "m.room.member":
			if ev.StateKey != nil && *ev.StateKey == me && ev.Message().Membership == "invite" {
				inviter = localpartIfLocal(ev.Sender, server)
			}
		}
	}
	return room, inviter
}

// inviteLine renders a doorbell line for an invite.
func inviteLine(roomID string, state []matrix.Event, me, server string) string {
	room, inviter := inviteInfo(roomID, state, me, server)
	line := "invite " + room + " " + roomID
	if inviter != "" {
		line += " from=" + inviter
	}
	return snippet(line, 400)
}
