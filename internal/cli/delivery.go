package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Delivery is at least once (protocol rule 10). The local state holds one
// committed cursor (next_batch) plus the pending batch: `wait` prints a batch
// and only then records it as pending; the cursor moves past it when the batch
// is acknowledged (by the next `wait`, `watch`, or `a2a ack`). A wait that dies
// before the pending batch was saved leaves the cursor where it was, so the
// batch is delivered again. Acknowledged event ids go into a bounded ring and
// are never delivered twice.

// waitItem is one doorbell event in both output forms.
type waitItem struct {
	line string
	rec  EventRecord
}

func inviteItem(roomID string, state []matrix.Event, me, server string) waitItem {
	room, inviter := inviteInfo(roomID, state, me, server)
	rec := EventRecord{Type: "invite", RoomID: sanitizeLine(roomID), Sender: sanitizeLine(inviter)}
	if room != roomID {
		rec.Room = sanitizeLine(room)
	}
	return waitItem{line: inviteLine(roomID, state, me, server), rec: rec}
}

// emit writes items (one line or one JSON object each) and flushes the
// output. Any write error is returned: the caller must then not acknowledge.
func (a *App) emit(items []waitItem, asJSON bool) error {
	for _, it := range items {
		line := it.line
		if asJSON {
			data, err := json.Marshal(it.rec)
			if err != nil {
				return err
			}
			line = string(data)
		}
		if _, err := io.WriteString(a.Stdout, line+"\n"); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
	}
	if f, ok := a.Stdout.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return fmt.Errorf("flush output: %w", err)
		}
	}
	return nil
}

// batch is what one /sync response delivers.
type batch struct {
	items   []waitItem
	events  []string // delivered event ids
	learned []string // thread roots learned from my own events
	close   []string // --until-kind subscriptions closed by a delivered event
}

// deliveryFilter is the --room/--all selection of wait and watch.
type deliveryFilter struct {
	rooms map[string]bool // empty = every joined room
	all   bool
}

func (s *session) resolveFilter(ctx context.Context, rooms []string, all bool) (deliveryFilter, error) {
	f := deliveryFilter{rooms: map[string]bool{}, all: all}
	for _, r := range rooms {
		id, _, err := s.resolveRoom(ctx, r)
		if err != nil {
			return f, err
		}
		f.rooms[id] = true
	}
	return f, nil
}

// collectBatch classifies one sync response. st gains learned threads in memory.
func (s *session) collectBatch(ctx context.Context, resp matrix.SyncResponse, since string, st *config.State, subs []Sub, f deliveryFilter, labels map[string]string) (batch, error) {
	var b batch
	me := s.creds.UserID
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
		b.items = append(b.items, inviteItem(roomID, resp.Rooms.Invite[roomID].InviteState.Events, me, s.server))
	}
	byID := map[string]Sub{}
	for _, sub := range subs {
		byID[sub.ID] = sub
	}
	closed := map[string]bool{}
	seen := map[string]bool{}
	// Iterate rooms in a stable order so output is deterministic.
	for _, roomID := range sortedKeys(resp.Rooms.Join) {
		jr := resp.Rooms.Join[roomID]
		events, gap, err := s.timelineEvents(ctx, roomID, since, jr.Timeline.Events, jr.Timeline.Limited, jr.Timeline.PrevBatch)
		if err != nil {
			return b, fmt.Errorf("backfill %s: %w", s.roomLabel(ctx, roomID, labels), err)
		}
		if gap {
			fmt.Fprintf(s.app.Stderr, "a2a: %s: more than %d events since the last wait; older ones were not checked\n", s.roomLabel(ctx, roomID, labels), maxBackfill)
		}
		for _, ev := range events {
			active := subs
			if len(closed) > 0 {
				active = nil
				for _, sub := range subs {
					if !closed[sub.ID] {
						active = append(active, sub)
					}
				}
			}
			rel, root, matched := classify(ev, roomID, me, s.name, *st, active)
			if root != "" {
				b.learned = append(b.learned, root)
				st.AddThread(root)
			}
			if rel == "" && f.all && f.rooms[roomID] && ev.Type == "m.room.message" && ev.Sender != me {
				rel = relAll
			}
			if rel == "" || (len(f.rooms) > 0 && !f.rooms[roomID]) || seen[ev.EventID] {
				continue
			}
			if st.IsAcked(ev.EventID) {
				continue // acknowledged before: never delivered twice
			}
			seen[ev.EventID] = true
			h, _ := decodeHeader(ev.Message().A2A)
			for _, id := range matched {
				if sub := byID[id]; sub.UntilKind != "" && sub.UntilKind == h.Kind && !closed[id] {
					closed[id] = true
					b.close = append(b.close, id)
				}
			}
			subID := ""
			if rel == relSub {
				subID = matched[0]
			}
			label := s.roomLabel(ctx, roomID, labels)
			rec := eventRecord(string(rel), label, roomID, ev, s.server, true)
			rec.Sub = sanitizeLine(subID)
			b.items = append(b.items, waitItem{line: eventLine(rel, label, ev, s.server, sanitizeLine(subID)), rec: rec})
			b.events = append(b.events, ev.EventID)
		}
	}
	return b, nil
}

// startDelivery acknowledges any pending batch, removes the subscriptions it
// closed and the expired ones, and returns the state to deliver from.
func (s *session) startDelivery(ctx context.Context, now time.Time) (config.State, error) {
	st, _, err := s.updateSubs(ctx, func(st *config.State, list *SubList) (bool, error) {
		changed := false
		if p := st.Ack(); p != nil && list.remove(p.CloseSubs...) {
			changed = true
		}
		if len(list.pruneExpired(now)) > 0 {
			changed = true
		}
		return changed, nil
	})
	return st, err
}

// takePosition is the very first run: it takes a sync position without
// replaying history and reports pending invites. The position is saved only
// after the invites were written. It reports whether anything was printed.
func (a *App) takePosition(ctx context.Context, s *session, asJSON bool) (config.State, bool, error) {
	resp, err := s.client.Sync(ctx, "", 0, initialFilter)
	if err != nil {
		return config.State{}, false, err
	}
	var items []waitItem
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
		items = append(items, inviteItem(roomID, resp.Rooms.Invite[roomID].InviteState.Events, s.creds.UserID, s.server))
	}
	if len(items) > 0 {
		if err := a.emit(items, asJSON); err != nil {
			return config.State{}, false, err
		}
	}
	st, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
		cur.NextBatch = resp.NextBatch
		return nil
	})
	return st, len(items) > 0, err
}

// cmdWait: a2a wait [--room R ...] [--all] [--timeout 25m|0] [--json]
func cmdWait(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("wait")
	var rooms stringList
	fs.Var(&rooms, "room", "room to watch (repeatable; default: all joined rooms)")
	timeout := fs.Duration("timeout", DefaultWaitTimeout, "how long to wait (0 = until something arrives)")
	asJSON := fs.Bool("json", false, "one JSON object per event")
	all := fs.Bool("all", false, "wake on every new message in the --room rooms, not only on relevant ones")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a wait [--room R ...] [--all] [--timeout 25m|0] [--json]")
	}
	if *all && len(rooms) == 0 {
		return usagef("--all needs at least one --room")
	}
	if *timeout < 0 {
		return usagef("--timeout must not be negative (0 waits until something arrives)")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	filter, err := s.resolveFilter(ctx, rooms, *all)
	if err != nil {
		return err
	}
	labels := map[string]string{}
	var deadline time.Time
	if *timeout > 0 {
		deadline = a.Now().Add(*timeout)
	}
	st, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		var printed bool
		if st, printed, err = a.takePosition(ctx, s, *asJSON); err != nil || printed {
			return err
		}
	}
	for {
		poll := maxPoll
		if !deadline.IsZero() {
			remaining := deadline.Sub(a.Now())
			if remaining <= 0 {
				if *asJSON {
					return a.printJSON(map[string]string{"type": "timeout"})
				}
				a.printf("timeout\n")
				return nil
			}
			if remaining < poll {
				poll = remaining
			}
		}
		resp, err := s.client.Sync(ctx, st.NextBatch, poll, syncFilter)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		subs, err := s.getSubs(ctx)
		if err != nil {
			return err
		}
		b, err := s.collectBatch(ctx, resp, st.NextBatch, &st, subs.Subs, filter, labels)
		if err != nil {
			return err
		}
		if len(b.items) == 0 {
			// Nothing to deliver: the cursor may move directly.
			if st, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error {
				cur.NextBatch = resp.NextBatch
				for _, r := range b.learned {
					cur.AddThread(r)
				}
				return nil
			}); err != nil {
				return fmt.Errorf("save wait state: %w", err)
			}
			continue
		}
		if err := a.emit(b.items, *asJSON); err != nil {
			return fmt.Errorf("%w; nothing was acknowledged, the next wait delivers this batch again", err)
		}
		if a.afterPrint != nil {
			if err := a.afterPrint(); err != nil {
				return err
			}
		}
		// Only now, after a successful write, is the batch pending; the cursor
		// moves past it when it is acknowledged.
		if _, err := config.UpdateState(s.paths, s.name, func(cur *config.State) error {
			cur.Pending = &config.Pending{NextBatch: resp.NextBatch, Events: b.events, CloseSubs: b.close}
			for _, r := range b.learned {
				cur.AddThread(r)
			}
			return nil
		}); err != nil {
			return fmt.Errorf("save pending batch: %w (it will be delivered again)", err)
		}
		return nil
	}
}

// cmdAck: a2a ack [all] commits the pending batch explicitly.
func cmdAck(ctx context.Context, a *App, args []string) error {
	if len(args) > 1 || (len(args) == 1 && args[0] != "all") {
		return usagef("usage: a2a ack [all]")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	var p *config.Pending
	if _, _, err := s.updateSubs(ctx, func(st *config.State, list *SubList) (bool, error) {
		p = st.Ack()
		return p != nil && list.remove(p.CloseSubs...), nil
	}); err != nil {
		return err
	}
	if p == nil {
		a.printf("nothing pending\n")
		return nil
	}
	a.printf("acked %d events\n", len(p.Events))
	return nil
}

// cmdWatch: a2a watch [--room R ...] [--all] [--json] streams deliveries until
// interrupted: one line (or JSON object) per event, flushed each, and the
// cursor is committed right after each batch was fully written.
func cmdWatch(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("watch")
	var rooms stringList
	fs.Var(&rooms, "room", "room to watch (repeatable; default: all joined rooms)")
	asJSON := fs.Bool("json", false, "one JSON object per event")
	all := fs.Bool("all", false, "every new message in the --room rooms, not only relevant ones")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a watch [--room R ...] [--all] [--json]")
	}
	if *all && len(rooms) == 0 {
		return usagef("--all needs at least one --room")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	filter, err := s.resolveFilter(ctx, rooms, *all)
	if err != nil {
		return err
	}
	stopped := func(err error) error {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	labels := map[string]string{}
	st, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return stopped(err)
	}
	if st.NextBatch == "" {
		if st, _, err = a.takePosition(ctx, s, *asJSON); err != nil {
			return stopped(err)
		}
	}
	for {
		resp, err := s.client.Sync(ctx, st.NextBatch, maxPoll, syncFilter)
		if err != nil {
			return stopped(err)
		}
		subs, err := s.getSubs(ctx)
		if err != nil {
			return stopped(err)
		}
		b, err := s.collectBatch(ctx, resp, st.NextBatch, &st, subs.Subs, filter, labels)
		if err != nil {
			return stopped(err)
		}
		for _, it := range b.items {
			if err := a.emit([]waitItem{it}, *asJSON); err != nil {
				return fmt.Errorf("%w; the batch was not committed and will be delivered again", err)
			}
		}
		commit := func(cur *config.State) {
			cur.NextBatch = resp.NextBatch
			cur.AddAcked(b.events...)
			for _, r := range b.learned {
				cur.AddThread(r)
			}
		}
		if len(b.close) > 0 {
			st, _, err = s.updateSubs(ctx, func(cur *config.State, list *SubList) (bool, error) {
				commit(cur)
				return list.remove(b.close...), nil
			})
		} else {
			st, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error {
				commit(cur)
				return nil
			})
		}
		if err != nil {
			return stopped(fmt.Errorf("commit watch cursor: %w", err))
		}
	}
}
