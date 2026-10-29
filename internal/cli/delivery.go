package cli

import (
	"context"
	"encoding/json"
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
	all      map[string]bool // explicit wait/watch --all scope, retained for append-time recheck
	entries  []config.MailEntry
	close    []string            // --until-kind subscriptions closed by a persisted event
	closeFor map[string][]string // event id -> scopes closed by that event
}

// deliveryFilter is the --room/--all selection of wait and watch.
type deliveryFilter struct {
	rooms map[string]bool // empty = every joined room
	all   bool
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
	if e.Kind == config.MailInvite {
		return true
	}
	if e.Rel == string(relAll) && !f.all {
		return false
	}
	return len(f.rooms) == 0 || f.rooms[e.Room]
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
	b := batch{all: all}
	me := s.creds.UserID
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
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
		jr := resp.Rooms.Join[roomID]
		keep := func(ev matrix.Event) bool {
			if ev.Type != "m.room.message" {
				return false
			}
			if ev.Sender == me || ev.ThreadRoot() != "" || all[roomID] || st.HasLegacyPendingEvent(ev.EventID) {
				return true
			}
			rel, _, _ := classify(ev, roomID, me, s.name, st, subs)
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
		for _, ev := range events {
			// Replayed legacy receipts must not deliver or consume until-kind scopes.
			if contains(st.LegacyAcked, ev.EventID) {
				continue
			}
			// Closure is committed only under the append lock. Keep later
			// candidates too: an earlier candidate may be receipted or lose
			// its selector match between collection and persistence.
			rel, _, matched := classify(ev, roomID, me, s.name, st, subs)
			if rel == "" && all[roomID] && ev.Type == "m.room.message" && ev.Sender != me {
				rel = relAll
			}
			if rel == "" || seen[ev.EventID] {
				continue
			}
			seen[ev.EventID] = true
			h, _ := decodeHeader(ev.Message().A2A)
			for _, id := range matched {
				if sub := byID[id]; sub.UntilKind != "" && sub.UntilKind == h.Kind {
					if !closed[id] {
						closed[id] = true
						b.close = append(b.close, id)
					}
					if b.closeFor == nil {
						b.closeFor = map[string][]string{}
					}
					b.closeFor[ev.EventID] = append(b.closeFor[ev.EventID], id)
				}
			}
			subID := ""
			if rel == relSub {
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
// the state only when fn succeeds. This local boundary does not require the
// network or subscription-policy migration when withSubs is false. When set, the list
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
		return fn(cur, mb)
	}
	if !withSubs {
		return config.UpdateStateUntil(s.paths, s.name, deadline, func(cur *config.State) error {
			_, err := run(cur)
			return err
		})
	}
	// Do not checkpoint state after the older subscription converter has edited it.
	if _, err := s.ensureThreadPolicy(ctx, deadline); err != nil {
		return config.State{}, err
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
// before the recovered batch is retired. The caller validates complete recovery
// first and clears the old fields only after all effects are durable.
func migrateLegacyDelivery(cur *config.State, mb *config.Mailbox) error {
	if len(cur.LegacyAcked) > 0 {
		if err := mb.AddReceived(cur.LegacyAcked); err != nil {
			return err
		}
	}
	return nil
}

// persist appends a collected batch to the mailbox (fsynced) and only then
// moves the cursor from since to next; a cursor another consumer moved
// meanwhile is left alone (its batch is in the mailbox too).
func (s *session) persist(ctx context.Context, b batch, since, next string, deadline time.Time) (config.State, error) {
	a := s.app
	// Checkpointing is a separate durable phase, before delivery callbacks. A
	// later delivery failure does not roll migration back or advance its cursor.
	if _, err := s.ensureThreadPolicy(ctx, deadline); err != nil {
		return config.State{}, err
	}
	return s.lockedDeliver(ctx, deadline, len(b.close) > 0, func(cur *config.State, mb *config.Mailbox) ([]string, error) {
		if a.beforeAppend != nil {
			if err := a.beforeAppend(); err != nil {
				return nil, err
			}
		}
		// Recheck EVERY event under the append lock, including direct/mention/all:
		// stale selector matches must not close an until-kind subscription.
		entries := make([]config.MailEntry, 0, len(b.entries))
		var currentSubs *SubList
		closed := map[string]bool{}
		var closeSubs []string
		for _, e := range b.entries {
			if e.Kind == config.MailEvent {
				// A receipt committed since collection wins over replay and
				// must not cause a second subscription-disposition effect.
				if mb.Received(e.ID) || contains(cur.LegacyAcked, e.ID) {
					continue
				}
				var ev matrix.Event
				if err := json.Unmarshal(e.Event, &ev); err != nil {
					return nil, err
				}
				if currentSubs == nil {
					list, err := s.getSubs(ctx)
					if err != nil {
						return nil, err
					}
					currentSubs = &list
				}
				var active []Sub
				for _, sub := range currentSubs.Subs {
					if !closed[sub.ID] {
						active = append(active, sub)
					}
				}
				rel, _, matched := classify(ev, e.Room, s.creds.UserID, s.name, *cur, active)
				if rel == "" && b.all[e.Room] && ev.Type == "m.room.message" && ev.Sender != s.creds.UserID {
					rel = relAll
				}
				if rel == "" {
					continue
				}
				e.Rel, e.Sub = string(rel), ""
				if rel == relSub {
					e.Sub = matched[0]
				}
				for _, id := range matched {
					if contains(b.closeFor[e.ID], id) && !closed[id] {
						closed[id] = true
						closeSubs = append(closeSubs, id)
					}
				}
			}
			entries = append(entries, e)
		}
		// Do not consume a legacy recovery cursor unless every exact pending
		// obligation is already durable/receipted or is in this accepted batch.
		if err := validateLegacyRecovery(*cur, mb, entries); err != nil {
			return nil, err
		}
		if err := migrateLegacyDelivery(cur, mb); err != nil {
			return nil, err
		}
		if _, err := mb.Append(entries); err != nil {
			return nil, err
		}
		if a.afterAppend != nil {
			if err := a.afterAppend(); err != nil {
				return nil, err
			}
		}
		// Both mailbox appends and legacy receipts are durable before retiring
		// their recovery fields. Failure before here leaves a retryable batch.
		cur.LegacyPending, cur.LegacyAcked = nil, nil
		if cur.NextBatch == since {
			cur.NextBatch = next
		}
		return closeSubs, nil
	})
}

// ingest classifies one sync response from since and persists it.
func (s *session) ingest(ctx context.Context, resp matrix.SyncResponse, since string, subs []Sub, all map[string]bool, deadline time.Time) (config.State, error) {
	snap, err := s.ensureThreadPolicy(ctx, deadline)
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

// takePosition is the very first run: it takes a sync position without
// replaying history; pending invites go into the mailbox first.
func (s *session) takePosition(ctx context.Context, deadline time.Time) (config.State, error) {
	if _, err := s.ensureThreadPolicy(ctx, deadline); err != nil {
		return config.State{}, err
	}
	resp, err := s.client.Sync(ctx, "", 0, initialFilter)
	if err != nil {
		return config.State{}, err
	}
	var b batch
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
		b.entries = append(b.entries, inviteEntry(roomID, "", resp.Rooms.Invite[roomID].InviteState.Events, s.creds.UserID, s.server))
	}
	return s.persist(ctx, b, "", resp.NextBatch, deadline)
}

// syncNow is the "bring the mailbox up to date" step of inbox and the hooks:
// one /sync from the cursor with timeout 0 (no long poll), persisted like
// wait's; the first run only takes a position.
func (s *session) syncNow(ctx context.Context, st config.State, subs []Sub, deadline time.Time) (config.State, error) {
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
	// Inventory before legacy subscription conversion or expiry can change state.
	if _, err := s.ensureThreadPolicy(ctx, time.Time{}); err != nil {
		return config.State{}, SubList{}, err
	}
	return s.updateSubs(ctx, func(st *config.State, list *SubList) (bool, error) {
		return len(list.pruneExpired(now)) > 0, nil
	})
}

// Avoid rewriting completed state just to take a classification snapshot.
func (s *session) ensureThreadPolicy(ctx context.Context, deadline time.Time) (config.State, error) {
	if err := s.checkConnection(); err != nil {
		return config.State{}, err
	}
	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return st, err
	}
	if _, err := config.LegacyPendingIDs(st.LegacyPending); err != nil {
		return st, err
	}
	if st.ThreadPolicyVersion != 0 {
		return st, config.MigrateThreadFollows(s.paths, s.name, &st, nil)
	}
	return config.UpdateStateUntil(s.paths, s.name, deadline, func(cur *config.State) error {
		if err := s.checkConnection(); err != nil {
			return err
		}
		return s.migrateThreadPolicy(ctx, cur)
	})
}

// Migration shares the delivery lock and never changes account data or mail.
func (s *session) migrateThreadPolicy(ctx context.Context, st *config.State) error {
	if st.ThreadPolicyVersion == config.ExplicitThreadPolicyVersion {
		return config.MigrateThreadFollows(s.paths, s.name, st, nil)
	}
	list, err := s.getSubs(ctx)
	if err != nil {
		return err
	}
	return s.migrateThreadPolicyWithSubs(st, list)
}

func (s *session) migrateThreadPolicyWithSubs(st *config.State, list SubList) error {
	raw, err := json.Marshal(list)
	if err != nil {
		return err
	}
	previous := st.ThreadPolicyVersion
	if err := config.MigrateThreadFollows(s.paths, s.name, st, raw); err != nil {
		return err
	}
	m := st.ThreadMigration
	if previous == 0 && m != nil && (len(m.Unknown) > 0 || len(m.Pending) > 0 || len(m.LegacyPending) > 0) {
		fmt.Fprintf(s.app.Stderr, "a2a: explicit thread policy v%d: %d explicit, %d participation-only, %d unknown legacy roots excluded, %d mutes, %d durable pending retained; inventory: %s (thread_migration); legacy pending retained for normal recovery\n", st.ThreadPolicyVersion, len(m.Explicit), len(m.Implicit), len(m.Unknown), len(m.Mutes), len(m.Pending), sanitizeLine(s.paths.StateFile(s.name)))
	}
	return nil
}

// shownHint tells a wait/watch consumer how its entries leave the mailbox.
const shownHint = "a2a: printed entries stay in the mailbox until received: run `a2a inbox`, or `a2a ack <ids>` / `a2a ack all-shown`"

// showNew prints the mailbox entries that wait/watch has not shown yet and f
// selects, one flushed write each, and marks them shown after the write, under
// the state lock (so concurrent waits never print the same entry). It receipts
// nothing. It returns how many it printed.
func (a *App) showNew(ctx context.Context, s *session, f deliveryFilter, asJSON bool) (int, error) {
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
func cmdWait(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("wait")
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
	filter, err := s.resolveFilter(ctx, rooms, *all)
	if err != nil {
		return err
	}
	var deadline time.Time
	if *timeout > 0 {
		deadline = a.Now().Add(*timeout)
	}
	st, _, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		if st, err = s.takePosition(ctx, time.Time{}); err != nil {
			return err
		}
	}
	for {
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
		if err := a.acquireLease(s, "", time.Time{}); err != nil {
			return err
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

// validateLegacyRecovery is read-only so an unrecoverable/malformed batch cannot
// write receipts, add partial recovery mail or advance the shared sync cursor.
func validateLegacyRecovery(st config.State, mb *config.Mailbox, entries []config.MailEntry) error {
	ids, err := config.LegacyPendingIDs(st.LegacyPending)
	if err != nil {
		return err
	}
	accepted := map[string]bool{}
	for _, e := range entries {
		accepted[e.ID] = true
	}
	for _, id := range ids {
		if !mb.Has(id) && !contains(st.LegacyAcked, id) && !accepted[id] {
			return fmt.Errorf("legacy pending recovery incomplete: an unresolved event was not recovered; state and cursor retained")
		}
	}
	return nil
}
