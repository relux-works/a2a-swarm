package cli

import (
	"context"
	"encoding/json"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// These are aggregate history limits per first sync, shared across rooms in
// sorted order: 20 requests, at most 100 events each (2000 total). Initial
// timelines request at most 100 events per room. HTTP shares a 30s deadline;
// a caller's shorter context (including the hook's 5s budget) wins.
const firstSyncMaxPages = 20
const firstSyncTimeout = 30 * time.Second

func ownMembership(ev matrix.Event, me string) bool {
	return ev.Type == "m.room.member" && ev.StateKey != nil && *ev.StateKey == me
}

// ownJoin recognizes an unredacted, self-sent join starting a membership epoch.
// Read membership directly: Message() suppresses it for redacted events.
func ownJoin(ev matrix.Event, me string) bool {
	if !ownMembership(ev, me) || ev.Sender != me || ev.EventID == "" || ev.IsRedacted() {
		return false
	}
	var content struct {
		Membership string `json:"membership"`
	}
	if json.Unmarshal(ev.Content, &content) != nil || content.Membership != "join" {
		return false
	}
	if len(ev.Unsigned) == 0 {
		return true
	}
	var unsigned map[string]json.RawMessage
	if json.Unmarshal(ev.Unsigned, &unsigned) != nil || unsigned == nil {
		return false
	}
	if raw, supplied := unsigned["prev_content"]; supplied {
		var previous struct {
			Membership string `json:"membership"`
		}
		if json.Unmarshal(raw, &previous) == nil {
			switch previous.Membership {
			case "invite", "leave", "ban", "knock":
				return true
			}
		}
		return false // A profile update or ambiguous previous membership proves no new join.
	}
	return true
}

// firstTimeline uses server timeline order, never timestamps. The latest own
// membership event is a barrier: only a recognized join proves its later tail.
// Any other barrier or exhausted budget/history gaps the whole unproven tail.
// /messages is newest first; never scan or page past an own membership barrier.
// Existing-cursor/rejoin syncs bypass this.
func (s *session) firstTimeline(ctx context.Context, room string, events []matrix.Event, limited bool, prev, position string, pagesLeft *int) ([]matrix.Event, *backfillGap, error) {
	newest := firstNonEmpty(position, "sync position")
	haveTail := len(events) > 0
	if haveTail {
		newest = firstNonEmpty(events[len(events)-1].EventID, newest)
	}
	gap := func() ([]matrix.Event, *backfillGap, error) {
		// No current join is proven, so conservatively name the entire room range.
		return nil, &backfillGap{From: "room start", To: newest}, nil
	}
	if len(events) > backfillPageSize {
		return gap()
	}
	for i := len(events) - 1; i >= 0; i-- {
		if ownMembership(events[i], s.creds.UserID) {
			if !ownJoin(events[i], s.creds.UserID) {
				return gap()
			}
			return events[i+1:], nil, nil
		}
	}
	if !limited || prev == "" {
		return gap()
	}
	var older []matrix.Event // newest first, bounded by firstSyncMaxPages * backfillPageSize
	from := prev
	for *pagesLeft > 0 {
		*pagesLeft--
		page, err := s.client.MessagesTo(ctx, room, from, "", backfillPageSize)
		if err != nil {
			return nil, nil, err
		}
		if !haveTail && len(page.Chunk) > 0 {
			newest = firstNonEmpty(page.Chunk[0].EventID, newest)
			haveTail = true
		}
		if len(page.Chunk) > backfillPageSize {
			return gap()
		}
		for _, ev := range page.Chunk {
			if ownMembership(ev, s.creds.UserID) {
				if !ownJoin(ev, s.creds.UserID) {
					return gap()
				}
				out := make([]matrix.Event, 0, len(older)+len(events))
				for i := len(older) - 1; i >= 0; i-- {
					out = append(out, older[i])
				}
				return append(out, events...), nil, nil
			}
			older = append(older, ev)
		}
		next := firstNonEmpty(page.End, page.NextBatch)
		if next == "" || next == from {
			return gap()
		}
		from = next
	}
	return gap()
}
