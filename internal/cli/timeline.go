package cli

import (
	"context"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// maxBackfill bounds how many events one limited /sync timeline is backfilled
// with, so a flood cannot keep a doorbell busy forever.
const maxBackfill = 2000

// timelineEvents returns every event a /sync batch covers for one room, oldest
// first. When the server cut the timeline (limited), the skipped events between
// the previous position `since` and the batch are fetched with /messages. gap
// reports that more than maxBackfill events were skipped and some were dropped.
func (s *session) timelineEvents(ctx context.Context, roomID, since string, events []matrix.Event, limited bool, prevBatch string) (out []matrix.Event, gap bool, err error) {
	if !limited || prevBatch == "" || since == "" {
		return events, false, nil
	}
	var older []matrix.Event // newest first
	from := prevBatch
	for len(older) < maxBackfill {
		p, err := s.client.MessagesTo(ctx, roomID, from, since, 100)
		if err != nil {
			return nil, false, err
		}
		older = append(older, p.Chunk...)
		next := firstNonEmpty(p.End, p.NextBatch)
		if next == "" || next == from || len(p.Chunk) == 0 {
			break
		}
		from = next
	}
	if len(older) >= maxBackfill {
		gap = true
		older = older[:maxBackfill]
	}
	out = make([]matrix.Event, 0, len(older)+len(events))
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	return append(out, events...), gap, nil
}
