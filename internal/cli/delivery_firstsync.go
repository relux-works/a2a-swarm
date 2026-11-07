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

// ownJoin uses timeline order, never origin_server_ts. A join-to-join profile
// update does not establish a new membership boundary.
func ownJoin(ev matrix.Event, me string) bool {
	if ev.Type != "m.room.member" || ev.StateKey == nil || *ev.StateKey != me || ev.EventID == "" || ev.Message().Membership != "join" {
		return false
	}
	var unsigned struct {
		PrevContent struct {
			Membership string `json:"membership"`
		} `json:"prev_content"`
	}
	if len(ev.Unsigned) > 0 {
		if err := json.Unmarshal(ev.Unsigned, &unsigned); err != nil {
			return false
		}
	}
	return unsigned.PrevContent.Membership != "join"
}

// firstTimeline returns only the tail proven to follow our latest own join.
// Until that boundary is found, candidates are buffered but never classified:
// an exhausted budget/history yields a normal mailbox gap, not pre-join mail.
// /messages is newest first; stop scanning on the join, even mid-page, and
// never request its predecessor page. Existing-cursor/rejoin syncs bypass this.
func (s *session) firstTimeline(ctx context.Context, room string, events []matrix.Event, limited bool, prev string, pagesLeft *int) ([]matrix.Event, *backfillGap, error) {
	oldest := ""
	if len(events) > 0 {
		oldest = events[0].EventID
	}
	gap := func() ([]matrix.Event, *backfillGap, error) {
		return nil, &backfillGap{From: "own join", To: oldest}, nil
	}
	if len(events) > backfillPageSize {
		return gap()
	}
	for i := len(events) - 1; i >= 0; i-- {
		if ownJoin(events[i], s.creds.UserID) {
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
		if len(page.Chunk) > backfillPageSize {
			return gap()
		}
		for _, ev := range page.Chunk {
			oldest = ev.EventID
			if ownJoin(ev, s.creds.UserID) {
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
