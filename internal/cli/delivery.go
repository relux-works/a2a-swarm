package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Delivery is at least once (protocol rule 10), with one durable boundary per
// agent: the mailbox (config.Mailbox, state/<name>.mailbox.jsonl).
//
//   - Persist before advance. Every consumer (wait, watch, inbox, the hooks)
//     syncs from the one cursor, classifies the batch (backfill pages drop
//     irrelevant events as they arrive), appends the relevant events, invites
//     and gap records to the mailbox, fsyncs it, and only then saves the
//     cursor. A crash in between re-syncs the batch; the mailbox deduplicates
//     it by event id.
//   - Receipts on pull. An entry leaves the mailbox only by a receipt: `a2a
//     inbox` records one for exactly the entries it printed, after the write;
//     `a2a ack <id...>|all-shown` records one explicitly. wait and watch mark
//     what they printed as shown (so the next wait blocks for something new)
//     and receipt nothing; the hooks ring a doorbell and receipt nothing.
//   - Gaps are surfaced. A backfill that cannot reach the previous position
//     appends a gap record, which inbox, wait, watch and the doorbell report.

// waitItem is one mailbox entry in both output forms. id is the mailbox id
// (the event id, or invite:/gap: ids); ev is the raw event (nil for an invite
// or a gap), which `a2a inbox` and the hook doorbell read.
type waitItem struct {
	line string
	rec  EventRecord
	id   string
	ev   *matrix.Event
	gap  *config.MailEntry
}

// gapText is the a2a-authored report of a gap entry (after "gap: ").
func gapText(e config.MailEntry) string {
	return "events between " + sanitizeLine(e.From) + " and " + sanitizeLine(e.To) + " could not be checked; read them with a2a read " + sanitizeLine(e.Label)
}

// itemOf renders one mailbox entry.
func itemOf(e config.MailEntry, server string) waitItem {
	switch e.Kind {
	case config.MailInvite:
		rec := EventRecord{Type: "invite", RoomID: sanitizeLine(e.Room), Sender: sanitizeLine(e.Sender)}
		if e.Label != e.Room {
			rec.Room = sanitizeLine(e.Label)
		}
		line := "invite " + e.Label + " " + e.Room
		if e.Sender != "" {
			line += " from=" + e.Sender
		}
		return waitItem{line: snippet(line, 400), rec: rec, id: e.ID}
	case config.MailGap:
		g := e
		rec := EventRecord{Type: "gap", Room: sanitizeLine(e.Label), RoomID: sanitizeLine(e.Room), EventID: sanitizeLine(e.ID), Excerpt: "gap: " + gapText(e)}
		return waitItem{line: sanitizeLine("gap "+e.Label+" "+e.ID) + ": " + gapText(e), rec: rec, id: e.ID, gap: &g}
	}
	var ev matrix.Event
	_ = json.Unmarshal(e.Event, &ev)
	rec := eventRecord(e.Rel, e.Label, e.Room, ev, server, true)
	rec.Sub = sanitizeLine(e.Sub)
	return waitItem{line: eventLine(relevance(e.Rel), e.Label, ev, server, sanitizeLine(e.Sub)), rec: rec, id: e.ID, ev: &ev}
}

// itemsOf renders mailbox entries.
func itemsOf(entries []config.MailEntry, server string) []waitItem {
	out := make([]waitItem, 0, len(entries))
	for _, e := range entries {
		out = append(out, itemOf(e, server))
	}
	return out
}

func itemIDs(items []waitItem) []string {
	ids := make([]string, 0, len(items))
	for _, it := range items {
		ids = append(ids, it.id)
	}
	return ids
}

// emit writes items (one line or one JSON object each) and flushes the
// output. Any write error is returned: the caller must then record nothing.
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

// batch is what one sync response adds to the mailbox.
type batch struct {
	entries    []config.MailEntry
	redactions []config.Redaction
	learned    []string // thread roots learned from my own events
	close      []string // --until-kind subscriptions closed by a persisted event
}

// deliveryFilter is the --room/--all selection of wait and watch.
type deliveryFilter struct {
	rooms      map[string]bool // empty = every joined room
	all        bool
	predicates deliveryPredicates
}

// allRooms is the set of rooms whose every message is relevant (--all).
func (f deliveryFilter) allRooms() map[string]bool {
	if !f.all {
		return nil
	}
	return f.rooms
}

// shows reports whether wait/watch with this filter prints e.
func (f deliveryFilter) shows(e config.MailEntry) bool {
	if principalMail(e, f.predicates.principals) {
		return true // A room or message predicate cannot suppress principal wake.
	}
	if e.Kind == config.MailInvite {
		return true
	}
	if e.Rel == string(relAll) && !f.all {
		return false
	}
	return (len(f.rooms) == 0 || f.rooms[e.Room]) && f.predicates.matches(e)
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

// inviteEntry is the mailbox entry of a pending invite. Its id includes the
// position the sync started from, so a re-synced batch deduplicates it.
func inviteEntry(roomID, since string, state []matrix.Event, me, server string) config.MailEntry {
	room, inviter := inviteInfo(roomID, state, me, server)
	return config.MailEntry{ID: "invite:" + roomID + ":" + since, Kind: config.MailInvite, Room: roomID, Label: room, Sender: inviter}
}

// collectBatch classifies one sync response into mailbox entries. Backfill
// keeps only candidates (my own events, thread replies, and what classify or
// --all accepts) while it pages; they are classified oldest first. all is the
// set of rooms whose every message counts (wait/watch --all).
func (s *session) collectBatch(ctx context.Context, resp matrix.SyncResponse, since string, st config.State, subs []Sub, all map[string]bool, labels map[string]string) (batch, error) {
	return s.collectBatchWithPrincipals(ctx, resp, since, st, subs, all, labels, s.principals)
}

func (s *session) collectBatchWithPrincipals(ctx context.Context, resp matrix.SyncResponse, since string, st config.State, subs []Sub, all map[string]bool, labels map[string]string, configured []string) (batch, error) {
	var b batch
	me := s.creds.UserID
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
		if s.adminRoomExcluded(roomID) {
			continue
		}
		b.entries = append(b.entries, inviteEntry(roomID, since, resp.Rooms.Invite[roomID].InviteState.Events, me, s.server))
	}
	byID := map[string]Sub{}
	for _, sub := range subs {
		byID[sub.ID] = sub
	}
	closed := map[string]bool{}
	seen := map[string]bool{}
	// Iterate rooms in a stable order so output is deterministic.
	for _, roomID := range sortedKeys(resp.Rooms.Join) {
		if s.adminRoomExcluded(roomID) {
			continue
		}
		jr := resp.Rooms.Join[roomID]
		keep := func(ev matrix.Event) bool {
			if ev.Type == "m.room.redaction" || ev.IsRedacted() {
				return true
			}
			if ev.Type != "m.room.message" {
				return false
			}
			if ev.Sender == me || ev.ThreadRoot() != "" || all[roomID] {
				return true
			}
			rel, _, _ := classifyWithPrincipals(ev, roomID, me, s.name, st, subs, configured)
			return rel != ""
		}
		events, gap, err := s.timelineEvents(ctx, roomID, since, jr.Timeline.Events, jr.Timeline.Limited, jr.Timeline.PrevBatch, keep)
		if err != nil {
			return b, fmt.Errorf("backfill %s: %w", s.roomLabel(ctx, roomID, labels), err)
		}
		if gap != nil {
			b.entries = append(b.entries, config.MailEntry{ID: "gap:" + roomID + ":" + since, Kind: config.MailGap, Room: roomID,
				Label: s.roomLabel(ctx, roomID, labels), From: gap.From, To: gap.To})
		}
		b.redactions = append(b.redactions, eventRedactions(roomID, events)...)
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
			rel, root, matched := classifyWithPrincipals(ev, roomID, me, s.name, st, active, configured)
			if root != "" {
				b.learned = append(b.learned, root)
				st.AddThread(root)
			}
			if rel == "" && all[roomID] && ev.Type == "m.room.message" && ev.Sender != me {
				rel = relAll
			}
			if rel == "" && ev.Type == "m.room.message" && ev.IsRedacted() && ev.Sender != me {
				rel = relevance("redacted")
			}
			if rel == "" || seen[ev.EventID] {
				continue
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
			if rel == relSub && len(matched) > 0 && !mandatoryPrincipal(ev, configured) {
				subID = matched[0]
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				return b, err
			}
			b.entries = append(b.entries, config.MailEntry{ID: ev.EventID, Kind: config.MailEvent, Room: roomID,
				Label: s.roomLabel(ctx, roomID, labels), Rel: string(rel), Sub: subID, Sender: ev.Sender, TS: ev.OriginServerTS, Event: raw})
		}
	}
	return b, nil
}

// lockedDeliver runs fn under the state lock with the mailbox open, and saves
// the state only when fn succeeds. When withSubs is set the subscription list
// is held as well and the ids fn returns are removed from it (account data is
// written before the state). A set deadline bounds the wait for the lock.
func (s *session) lockedDeliver(ctx context.Context, deadline time.Time, withSubs bool, fn func(cur *config.State, mb *config.Mailbox) (closeSubs []string, err error)) (config.State, error) {
	if err := s.checkConnection(); err != nil {
		return config.State{}, err
	}
	run := func(cur *config.State) ([]string, error) {
		if err := s.checkConnection(); err != nil {
			return nil, err
		}
		mb, err := config.OpenMailbox(s.paths, s.name)
		if err != nil {
			return nil, err
		}
		for _, entry := range mb.Pending() {
			if s.adminRoomExcluded(entry.Room) {
				return nil, errors.New("admin-room retained mailbox refused; private owner recovery required")
			}
		}
		return fn(cur, mb)
	}
	if !withSubs {
		return config.UpdateStateUntil(s.paths, s.name, deadline, func(cur *config.State) error {
			_, err := run(cur)
			return err
		})
	}
	st, _, err := s.updateSubsBy(ctx, deadline, func(cur *config.State, list *SubList) (bool, error) {
		ids, err := run(cur)
		if err != nil {
			return false, err
		}
		return list.remove(ids...), nil
	})
	return st, err
}

// migrateLegacyDelivery moves a pre-mailbox acked ring into the receipt index
// and drops a pre-mailbox pending batch (its events are behind the cursor, so
// this sync puts them into the mailbox).
func migrateLegacyDelivery(cur *config.State, mb *config.Mailbox) error {
	if len(cur.LegacyAcked) > 0 {
		if err := mb.AddReceived(cur.LegacyAcked); err != nil {
			return err
		}
	}
	cur.LegacyAcked, cur.LegacyPending = nil, nil
	return nil
}

// persist appends a collected batch to the mailbox (fsynced) and only then
// moves the cursor from since to next; a cursor another consumer moved
// meanwhile is left alone (its batch is in the mailbox too).
func (s *session) persist(ctx context.Context, b batch, since, next string, deadline time.Time) (config.State, error) {
	a := s.app
	return s.lockedDeliver(ctx, deadline, len(b.close) > 0, func(cur *config.State, mb *config.Mailbox) ([]string, error) {
		if err := migrateLegacyDelivery(cur, mb); err != nil {
			return nil, err
		}
		if a.beforeAppend != nil {
			if err := a.beforeAppend(); err != nil {
				return nil, err
			}
		}
		if err := mb.Redact(b.redactions); err != nil {
			return nil, err
		}
		if _, err := mb.Append(b.entries); err != nil {
			return nil, err
		}
		if a.afterAppend != nil {
			if err := a.afterAppend(); err != nil {
				return nil, err
			}
		}
		for _, r := range b.learned {
			cur.AddThread(r)
		}
		if cur.NextBatch == since {
			cur.NextBatch = next
		}
		return b.close, nil
	})
}

// ingest classifies one sync response from since and persists it.
func (s *session) ingest(ctx context.Context, resp matrix.SyncResponse, since string, subs []Sub, all map[string]bool, deadline time.Time) (config.State, error) {
	snap, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return snap, err
	}
	b, err := s.collectBatch(ctx, resp, since, snap, subs, all, map[string]string{})
	if err != nil {
		return snap, err
	}
	if s.app.beforeDeliver != nil {
		s.app.beforeDeliver()
	}
	return s.persist(ctx, b, since, resp.NextBatch, deadline)
}

// takePosition is the very first run. No message history is replayed;
// pending invites precede the first cursor.
func (s *session) takePosition(ctx context.Context, deadline time.Time) (config.State, error) {
	resp, err := s.client.Sync(ctx, "", 0, initialFilter)
	if err != nil {
		return config.State{}, err
	}
	return s.savePosition(ctx, resp, deadline)
}

func (s *session) savePosition(ctx context.Context, resp matrix.SyncResponse, deadline time.Time) (config.State, error) {
	var b batch
	for roomID, jr := range resp.Rooms.Join {
		if s.client.FilterEvents != nil {
			events, err := s.client.FilterEvents(roomID, jr.Timeline.Events)
			if err != nil {
				return config.State{}, err
			}
			jr.Timeline.Events = events
		}
		b.redactions = append(b.redactions, eventRedactions(roomID, jr.Timeline.Events)...)
	}
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
		if s.adminRoomExcluded(roomID) {
			continue
		}
		b.entries = append(b.entries, inviteEntry(roomID, "", resp.Rooms.Invite[roomID].InviteState.Events, s.creds.UserID, s.server))
	}
	return s.persist(ctx, b, "", resp.NextBatch, deadline)
}

// syncNow is the "bring the mailbox up to date" step of inbox and the hooks:
// one /sync from the cursor with timeout 0 (no long poll), persisted like
// wait's; the first run takes a position without replaying message history.
func (s *session) syncNow(ctx context.Context, st config.State, subs []Sub, deadline time.Time) (config.State, error) {
	s.principals = s.readDeliveryPrincipals()
	if st.NextBatch == "" {
		return s.takePosition(ctx, deadline)
	}
	resp, err := s.client.Sync(ctx, st.NextBatch, 0, syncFilter)
	if err != nil {
		return st, err
	}
	return s.ingest(ctx, resp, st.NextBatch, subs, nil, deadline)
}

// startDelivery removes expired subscriptions and returns the state to
// deliver from and the live subscriptions. It acknowledges nothing.
func (s *session) startDelivery(ctx context.Context, now time.Time) (config.State, SubList, error) {
	return s.updateSubs(ctx, func(st *config.State, list *SubList) (bool, error) {
		return len(list.pruneExpired(now)) > 0, nil
	})
}

// shownHint tells a wait/watch consumer how its entries leave the mailbox.
const shownHint = "a2a: printed entries stay in the mailbox until received: run `a2a inbox`, or `a2a ack <ids>` / `a2a ack all-shown`"

// showNew prints the mailbox entries that wait/watch has not shown yet and f
// selects, one flushed write each, and marks them shown after the write, under
// the state lock (so concurrent waits never print the same entry). It receipts
// nothing. It returns how many it printed.
func (a *App) showNew(ctx context.Context, s *session, f deliveryFilter, asJSON bool) (int, error) {
	f.predicates.principals = s.principals
	printed := 0
	_, err := s.lockedDeliver(ctx, time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		var items []waitItem
		for _, e := range mb.Pending() {
			if !e.Shown && f.shows(e) {
				items = append(items, itemOf(e, s.server))
			}
		}
		for _, it := range items {
			if err := a.emit([]waitItem{it}, asJSON); err != nil {
				return nil, fmt.Errorf("%w; nothing was marked shown, the entries stay in the mailbox and are printed again", err)
			}
			printed++
		}
		if len(items) == 0 {
			return nil, nil
		}
		if a.afterPrint != nil {
			if err := a.afterPrint(); err != nil {
				return nil, fmt.Errorf("%w; nothing was marked shown, the entries stay in the mailbox and are printed again", err)
			}
		}
		return nil, mb.MarkShown(itemIDs(items))
	})
	return printed, err
}

// cmdWait: a2a wait [--room R ...] [--all] [--timeout 25m|0] [--json]
func cmdWait(ctx context.Context, a *App, args []string) (result error) {
	fs := newFlagSet("wait")
	predicateFlags := addDeliveryPredicateFlags(fs)
	var rooms stringList
	fs.Var(&rooms, "room", "room to watch (repeatable; default: all joined rooms)")
	timeout := fs.Duration("timeout", DefaultWaitTimeout, "how long to wait (0 = until something arrives)")
	asJSON := fs.Bool("json", false, "one JSON object per event")
	all := fs.Bool("all", false, "wake on every new message in the --room rooms, not only on relevant ones")
	status := fs.String("status", "", "opt-in liveness state while waiting")
	heartbeat := fs.Duration("heartbeat", 0, "explicit liveness heartbeat interval")
	runtime := fs.String("runtime", "", "declared runtime: claude, codex or none")
	hooks := fs.String("hooks", "", "declared hooks: installed or none")
	product := fs.String("product", "", "self-recorded product scope")
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
	predicates, err := predicateFlags.check()
	if err != nil {
		return err
	}
	profile := config.AgentStatus{State: *status, HeartbeatEvery: heartbeat.String(), Runtime: *runtime, Hooks: *hooks, Doorbell: "armed", Product: *product}
	if *status != "" {
		if err := statusValid(profile); err != nil {
			return usagef("%s", err)
		}
	} else if *heartbeat != 0 || *runtime != "" || *hooks != "" || *product != "" {
		return usagef("liveness flags require --status and explicit --heartbeat, --runtime, --hooks")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if err := refuseBoundWait(ctx, s); err != nil {
		return err
	}
	if err := a.acquireLease(s, "", time.Time{}); err != nil {
		return err
	}
	var deadline time.Time
	if *timeout > 0 {
		deadline = a.Now().Add(*timeout)
	}
	printTimeout := func() error {
		if *asJSON {
			return a.printJSON(map[string]string{"type": "timeout"})
		}
		a.printf("timeout\n")
		return nil
	}
	defer func() {
		if errors.Is(result, errWaitTimeout) {
			if ctx.Err() != nil {
				result = ctx.Err()
			} else {
				result = printTimeout()
			}
		}
	}()
	var lastHeartbeat time.Time
	if *status != "" {
		if err := s.writeLivenessSource(ctx, profile, "wait"); err != nil {
			return err
		}
		lastHeartbeat = a.Now()
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), nativeRPCTimeout)
			defer cancel()
			profile.Doorbell = "none"
			if err := s.writeLivenessSource(stopCtx, profile, "wait-disarm"); err != nil {
				fmt.Fprintln(a.Stderr, "wait liveness disarm:", a.secrets.Scrub(sanitizeLine(err.Error())))
			}
		}()
	}
	filter, err := waitRetry(ctx, s, deadline, func() (deliveryFilter, error) {
		return s.resolveFilter(ctx, rooms, *all)
	})
	if err != nil {
		return err
	}
	filter.predicates = predicates.forSession(s)
	st, err := waitRetry(ctx, s, deadline, func() (config.State, error) {
		st, _, err := s.startDelivery(ctx, a.Now())
		return st, err
	})
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		if st, err = waitRetry(ctx, s, deadline, func() (config.State, error) {
			resp, err := s.waitSync(ctx, "", 0, initialFilter)
			if err != nil {
				return config.State{}, err
			}
			return s.savePosition(ctx, resp, time.Time{})
		}); err != nil {
			return err
		}
	}
	for {
		s.principals = s.readDeliveryPrincipals()
		if *status != "" && a.Now().Sub(lastHeartbeat) >= *heartbeat {
			if err := s.writeLivenessSource(ctx, profile, "wait-heartbeat"); err != nil {
				return err
			}
			lastHeartbeat = a.Now()
		}
		n, err := a.showNew(ctx, s, filter, *asJSON)
		if err != nil {
			return err
		}
		if n > 0 {
			fmt.Fprintln(a.Stderr, shownHint)
			return nil
		}
		poll := maxPoll
		if *status != "" {
			poll = min(poll, *heartbeat)
		}
		if !deadline.IsZero() {
			remaining := deadline.Sub(a.Now())
			if remaining <= 0 {
				return printTimeout()
			}
			if remaining < poll {
				poll = remaining
			}
		}
		resp, err := waitRetry(ctx, s, deadline, func() (matrix.SyncResponse, error) {
			return s.waitSync(ctx, st.NextBatch, poll, syncFilter)
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		subs, err := waitRetry(ctx, s, deadline, func() (SubList, error) { return s.getSubs(ctx) })
		if err != nil {
			return err
		}
		if st, err = s.ingest(ctx, resp, st.NextBatch, subs.Subs, filter.allRooms(), time.Time{}); err != nil {
			return err
		}
	}
}

const ackUsage = "usage: a2a ack <id...>|all-shown  (ids as wait/watch/inbox print them; all-shown: every entry wait or watch printed)"

// cmdAck: a2a ack <id...>|all-shown records receipts in the mailbox.
func cmdAck(ctx context.Context, a *App, args []string) error {
	allShown := len(args) == 1 && args[0] == "all-shown"
	if len(args) == 0 {
		return usagef("%s", ackUsage)
	}
	if !allShown {
		for _, id := range args {
			if !strings.HasPrefix(id, "$") && !strings.HasPrefix(id, "invite:") && !strings.HasPrefix(id, "gap:") {
				return usagef("%q is not a mailbox id; %s", sanitizeLine(id), ackUsage)
			}
		}
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	n, asked := 0, 0
	if _, err := s.lockedDeliver(ctx, time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		ids := args
		if allShown {
			ids = nil
			for _, e := range mb.Pending() {
				if e.Shown {
					ids = append(ids, e.ID)
				}
			}
		}
		asked = len(ids)
		var err error
		n, err = mb.Receive(ids)
		return nil, err
	}); err != nil {
		return err
	}
	switch {
	case asked == 0:
		a.printf("nothing pending\n")
	case n < asked:
		a.printf("acked %d events (%d not pending)\n", n, asked-n)
	default:
		a.printf("acked %d events\n", n)
	}
	return nil
}

// cmdWatch: a2a watch [--room R ...] [--all] [--json] streams new mailbox
// entries until interrupted: one line (or JSON object) per entry, flushed
// each, marked shown after the write. It receipts nothing: a reader that dies
// after the write loses nothing, `a2a inbox` still prints the entries.
func cmdWatch(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("watch")
	predicateFlags := addDeliveryPredicateFlags(fs)
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
	predicates, err := predicateFlags.check()
	if err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if err := refuseBoundWait(ctx, s); err != nil {
		return err
	}
	if err := a.acquireLease(s, "", time.Time{}); err != nil {
		return err
	}
	filter, err := s.resolveFilter(ctx, rooms, *all)
	if err != nil {
		return err
	}
	filter.predicates = predicates.forSession(s)
	stopped := func(err error) error {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	st, _, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return stopped(err)
	}
	if st.NextBatch == "" {
		if st, err = s.takePosition(ctx, time.Time{}); err != nil {
			return stopped(err)
		}
	}
	for {
		s.principals = s.readDeliveryPrincipals()
		if _, err := a.showNew(ctx, s, filter, *asJSON); err != nil {
			return err
		}
		if err := a.acquireLease(s, "", time.Time{}); err != nil {
			return stopped(err)
		}
		resp, err := s.client.Sync(ctx, st.NextBatch, maxPoll, syncFilter)
		if err != nil {
			return stopped(err)
		}
		subs, err := s.getSubs(ctx)
		if err != nil {
			return stopped(err)
		}
		if st, err = s.ingest(ctx, resp, st.NextBatch, subs.Subs, filter.allRooms(), time.Time{}); err != nil {
			return stopped(err)
		}
	}
}
