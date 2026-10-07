package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// History checkpoints are stream event IDs, never timestamps: a server may
// return old timestamps before newer ones. The cache belongs to one watch run.
type livenessHistory struct {
	rooms map[string]*livenessRoomHistory
}
type livenessRoomHistory struct {
	from, anchor, nextAnchor string
	complete                 bool
	tokens                   map[string]bool
	events                   map[string]observedEvent
	redacted                 map[string]bool
}

func observeLivenessEvent(ev matrix.Event, room string, since time.Time, now time.Time, own string) (observedEvent, bool, error) {
	if ev.EventID == "" || ev.OriginServerTS <= 0 || ev.OriginServerTS > now.UnixMilli() {
		return observedEvent{}, false, errors.New("unanswered history has an invalid/future timestamp")
	}
	if ev.OriginServerTS < since.UnixMilli() || ev.Type != "m.room.message" || isRedacted(ev) {
		return observedEvent{}, false, nil
	}
	if _, edit := isEdit(ev); edit {
		return observedEvent{}, false, nil
	}
	h, ok := decodeHeader(ev.Message().A2A)
	if !ok || h.Type == "projection" {
		return observedEvent{}, false, nil
	}
	// Only this identity's marked monitor observations are excluded. A peer's
	// body or a forged From cannot suppress an ordinary owner-needed request.
	if ev.Sender == own && isLivenessObservation(ev) {
		return observedEvent{}, false, nil
	}
	return observedEvent{room: room, event: ev, header: h}, true, nil
}

// step resumes a bounded historical backfill or follows new stream events to
// its previous anchor. Each joined room receives the explicit page budget.
// Incomplete/failed scans never claim absence or clear unanswered incidents.
func (h *livenessHistory) step(ctx context.Context, a *App, s *session, rows []whoRow, since time.Time, policy replyPolicy, pages int) error {
	rooms, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	if h.rooms == nil {
		h.rooms = map[string]*livenessRoomHistory{}
	}
	joined := map[string]bool{}
	complete := true
	var firstErr error
	for _, room := range rooms {
		joined[room] = true
		st := h.rooms[room]
		if st == nil {
			st = &livenessRoomHistory{tokens: map[string]bool{}, events: map[string]observedEvent{}, redacted: map[string]bool{}}
			h.rooms[room] = st
		}
		// A completed pass starts a new tail pass; an unfinished pass resumes.
		if st.complete {
			st.complete = false
			st.from = ""
			st.tokens = map[string]bool{}
			st.nextAnchor = ""
		}
		continued := st.from != "" || st.nextAnchor != ""
		for n := 0; n < pages && !st.complete; n++ {
			page, readErr := s.client.MessagesWithRedactions(ctx, room, st.from, "b", 100)
			if readErr != nil {
				if firstErr == nil {
					firstErr = readErr
				}
				break
			}
			additions := map[string]observedEvent{}
			redacted := map[string]bool{}
			reached := false
			nextAnchor := st.nextAnchor
			for _, ev := range page.Chunk {
				if st.anchor != "" && ev.EventID == st.anchor {
					reached = true
					break
				}
				if nextAnchor == "" {
					nextAnchor = ev.EventID
				}
				event, keep, err := observeLivenessEvent(ev, room, since, a.Now(), s.creds.UserID)
				if err != nil {
					readErr = err
					break
				}
				if ev.Type == "m.room.redaction" {
					var c struct {
						Redacts string `json:"redacts"`
					}
					_ = json.Unmarshal(ev.Content, &c)
					if target := firstNonEmpty(c.Redacts, ev.Redacts); target != "" {
						redacted[target] = true
					}
				}
				if keep {
					additions[ev.EventID] = event
				}
			}
			next := firstNonEmpty(page.End, page.NextBatch)
			if readErr == nil && !reached && next != "" && (next == st.from || st.tokens[next]) {
				readErr = errors.New("unanswered history pagination cycle")
			}
			if readErr == nil && !reached && next == "" && st.anchor != "" {
				readErr = errors.New("unanswered history stream checkpoint missing; restart to recheck visible history")
			}
			if readErr == nil && h.tracked()+len(additions)+len(redacted) >= 100000 {
				readErr = errors.New("unanswered history exceeds relevant-event bound")
			}
			if readErr != nil {
				if firstErr == nil {
					firstErr = readErr
				}
				break
			}
			for id := range redacted {
				st.redacted[id] = true
				delete(st.events, id)
			}
			for id, event := range additions {
				if !st.redacted[id] {
					st.events[id] = event
				}
			}
			st.nextAnchor = nextAnchor
			if reached || next == "" {
				st.complete = true
				st.anchor = st.nextAnchor
				st.from = ""
				if continued {
					// Replies may have arrived during a multi-poll backfill.
					// Refresh the head before asserting any unanswered age.
					st.complete = false
					st.nextAnchor = ""
					st.tokens = map[string]bool{}
					continued = false
					continue
				}
				break
			}
			st.tokens[next] = true
			st.from = next
		}
		complete = complete && st.complete
	}
	for room := range h.rooms {
		if !joined[room] {
			delete(h.rooms, room)
		}
	}
	if !complete {
		for i := range rows {
			rows[i].Unanswered = "unknown (history incomplete)"
			rows[i].UnansweredSince = time.Time{}
		}
		if firstErr != nil {
			return firstErr
		}
		return nil
	}
	var events []observedEvent
	for _, st := range h.rooms {
		for _, event := range st.events {
			events = append(events, event)
			if len(events) > 100000 {
				return fmt.Errorf("unanswered history exceeds scan bound")
			}
		}
	}
	applyUnanswered(rows, unanswered(events, s.server, policy), a.Now())
	return nil
}

func applyUnanswered(rows []whoRow, ages map[string]time.Time, now time.Time) {
	for i := range rows {
		rows[i].Unanswered = "-"
		rows[i].UnansweredSince = time.Time{}
		if at := ages[rows[i].Name]; !at.IsZero() {
			rows[i].UnansweredSince = at
			rows[i].Unanswered = now.Sub(at).Round(time.Second).String()
		}
	}
}

// Bound retained evidence and checkpoint metadata across the whole watch.
func (h *livenessHistory) tracked() int {
	total := 0
	for _, st := range h.rooms {
		total += len(st.events) + len(st.redacted) + len(st.tokens)
	}
	return total
}
