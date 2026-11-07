package cli

import (
	"context"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// maxBackfillPages bounds how many /messages pages (of backfillPageSize
// events) one limited /sync timeline is backfilled with. Irrelevant events are
// dropped while paging (keep), so the bound is on work, never on how many
// relevant events fit; a timeline that cannot be paged back to the previous
// position within it is reported as a gap, never skipped silently.
const maxBackfillPages = 500

const backfillPageSize = 100

// backfillGap is a stretch of a room's timeline that could not be checked:
// the events between the position From and the event To (the oldest one reached).
type backfillGap struct {
	From, To string
}

// timelineEvents returns the events of one room that a /sync batch covers and
// keep accepts, oldest first. When the server cut the timeline (limited), the
// skipped events between the previous position `since` and the batch are
// fetched with /messages, and keep filters each page as it arrives, so a flood
// of unrelated events costs pages but never pushes a kept event out. gap is
// set when the pages could not reach `since` (the page bound, or a server that
// stops paging).
func (s *session) timelineEvents(ctx context.Context, roomID, since string, events []matrix.Event, limited bool, prevBatch string, keep func(matrix.Event) bool) (out []matrix.Event, gap *backfillGap, err error) {
	// Learn the entire live tail before paging/selection. A target redaction
	// can follow its edit, including on a different backfill page.
	if s.client.FilterEvents != nil {
		events, err = s.client.FilterEvents(roomID, events)
		if err != nil {
			return nil, nil, err
		}
	}
	filter := func(evs []matrix.Event) []matrix.Event {
		var kept []matrix.Event
		for _, ev := range evs {
			if keep(ev) {
				kept = append(kept, ev)
			}
		}
		return kept
	}
	if !limited || prevBatch == "" || since == "" {
		return filter(events), nil, nil
	}
	maxPages := maxBackfillPages
	if s.app != nil && s.app.backfillPages > 0 {
		maxPages = s.app.backfillPages
	}
	var older []matrix.Event // newest first
	oldest := ""
	if len(events) > 0 {
		oldest = events[0].EventID
	}
	from := prevBatch
	for pages := 0; ; pages++ {
		if pages >= maxPages {
			gap = &backfillGap{From: since, To: oldest}
			break
		}
		p, err := s.client.MessagesTo(ctx, roomID, from, since, backfillPageSize)
		if err != nil {
			return nil, nil, err
		}
		for _, ev := range p.Chunk {
			oldest = ev.EventID
			if keep(ev) {
				older = append(older, ev)
			}
		}
		next := firstNonEmpty(p.End, p.NextBatch)
		if next == "" {
			break // no more events: the previous position was reached
		}
		if next == from {
			gap = &backfillGap{From: since, To: oldest} // the server cannot page further back
			break
		}
		from = next
	}
	out = make([]matrix.Event, 0, len(older)+len(events))
	for i := len(older) - 1; i >= 0; i-- {
		out = append(out, older[i])
	}
	out = append(out, events...)
	if s.client.FilterEvents != nil {
		out, err = s.client.FilterEvents(roomID, out)
		if err != nil {
			return nil, nil, err
		}
	}
	return filter(out), gap, nil
}
