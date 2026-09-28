package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// DefaultWaitTimeout is how long `a2a wait` blocks without news.
const DefaultWaitTimeout = 25 * time.Minute

// maxPoll bounds one /sync long-poll so a dead connection is noticed.
const maxPoll = 60 * time.Second

// syncFilter keeps /sync small: only messages in timelines, no presence,
// account data, or ephemeral events. Invites arrive regardless of the filter.
const syncFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"lazy_load_members":true,"types":[]},"timeline":{"limit":100,"types":["m.room.message"]}}}`

// initialFilter is used for the very first sync: we only need a next_batch.
const initialFilter = `{"presence":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"room":{"ephemeral":{"not_types":["*"]},"account_data":{"not_types":["*"]},` +
	`"state":{"types":[]},"timeline":{"limit":1,"types":["m.room.message"]}}}`

// relevance explains why an event is relevant to me; "" means not relevant.
type relevance string

const (
	relTo      relevance = "to"
	relMention relevance = "mention"
	relThread  relevance = "thread"
)

// classify decides whether a timeline event concerns this agent. It is pure:
// the caller updates the thread list from the returned learnedRoot.
func classify(ev matrix.Event, me, myName string, st config.State) (rel relevance, learnedRoot string) {
	if ev.Type != "m.room.message" {
		return "", ""
	}
	m := ev.Message()
	root := ev.ThreadRoot()
	if ev.Sender == me {
		// My own message: a reply joins me to its thread; a topic root I sent is a thread I own.
		if root != "" {
			return "", root
		}
		if strings.HasPrefix(m.Body, TopicPrefix) {
			return "", ev.EventID
		}
		return "", ""
	}
	if h, ok := decodeHeader(m.A2A); ok {
		for _, t := range h.To {
			if t == myName || t == me {
				return relTo, ""
			}
		}
	}
	if m.Mentions != nil {
		for _, u := range m.Mentions.UserIDs {
			if u == me {
				return relMention, ""
			}
		}
	}
	if mentionsUser(m.Body, me) {
		return relMention, ""
	}
	if root != "" && st.HasThread(root) {
		return relThread, ""
	}
	return "", ""
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
func eventLine(rel relevance, room string, ev matrix.Event, server string) string {
	m := ev.Message()
	parts := []string{string(rel), room, ev.EventID, "from=" + localpartIfLocal(ev.Sender, server)}
	text := m.Body
	if h, ok := decodeHeader(m.A2A); ok {
		if h.Kind != "" {
			parts = append(parts, "kind="+h.Kind)
		}
		if h.Slug != "" {
			parts = append(parts, "topic="+h.Slug)
		}
		text = stripHeaderBody(text)
	}
	if root := ev.ThreadRoot(); root != "" {
		parts = append(parts, "root="+root)
	}
	return sanitizeLine(strings.Join(parts, " ")) + ": " + snippet(text, SnippetMax)
}

// inviteLine renders a doorbell line for an invite.
func inviteLine(roomID string, state []matrix.Event, me, server string) string {
	room, inviter := roomID, ""
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
				inviter = ev.Sender
			}
		}
	}
	line := "invite " + room + " " + roomID
	if inviter != "" {
		line += " from=" + localpartIfLocal(inviter, server)
	}
	return snippet(line, 400)
}

// cmdWait: a2a wait [--room R ...] [--timeout 25m]
func cmdWait(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("wait")
	var rooms stringList
	fs.Var(&rooms, "room", "room to watch (repeatable; default: all joined rooms)")
	timeout := fs.Duration("timeout", DefaultWaitTimeout, "how long to wait")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a wait [--room R ...] [--timeout 25m]")
	}
	if *timeout <= 0 {
		return usagef("--timeout must be positive")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	filter := map[string]bool{}
	for _, r := range rooms {
		id, _, err := s.resolveRoom(ctx, r)
		if err != nil {
			return err
		}
		filter[id] = true
	}
	me := s.creds.UserID
	labels := map[string]string{}
	deadline := a.Now().Add(*timeout)

	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		// First run: take a position without replaying history, but report pending invites.
		resp, err := s.client.Sync(ctx, "", 0, initialFilter)
		if err != nil {
			return err
		}
		var lines []string
		for roomID, inv := range resp.Rooms.Invite {
			lines = append(lines, inviteLine(roomID, inv.InviteState.Events, me, s.server))
		}
		if _, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
			cur.NextBatch = resp.NextBatch
			return nil
		}); err != nil {
			return err
		}
		st.NextBatch = resp.NextBatch
		if len(lines) > 0 {
			for _, l := range lines {
				a.printf("%s\n", l)
			}
			return nil
		}
	}

	for {
		remaining := deadline.Sub(a.Now())
		if remaining <= 0 {
			a.printf("timeout\n")
			return nil
		}
		poll := remaining
		if poll > maxPoll {
			poll = maxPoll
		}
		resp, err := s.client.Sync(ctx, st.NextBatch, poll, syncFilter)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		var lines []string
		var learned []string
		for roomID, inv := range resp.Rooms.Invite {
			lines = append(lines, inviteLine(roomID, inv.InviteState.Events, me, s.server))
		}
		// Iterate rooms in a stable order so output is deterministic.
		for _, roomID := range sortedKeys(resp.Rooms.Join) {
			jr := resp.Rooms.Join[roomID]
			for _, ev := range jr.Timeline.Events {
				rel, root := classify(ev, me, s.name, st)
				if root != "" {
					learned = append(learned, root)
					st.AddThread(root)
				}
				if rel == "" || (len(filter) > 0 && !filter[roomID]) {
					continue
				}
				lines = append(lines, eventLine(rel, s.roomLabel(ctx, roomID, labels), ev, s.server))
			}
		}
		// Persist the position before printing, so a restart never re-emits these events.
		st, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error {
			cur.NextBatch = resp.NextBatch
			for _, r := range learned {
				cur.AddThread(r)
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("save wait state: %w", err)
		}
		if len(lines) > 0 {
			for _, l := range lines {
				a.printf("%s\n", l)
			}
			return nil
		}
	}
}
