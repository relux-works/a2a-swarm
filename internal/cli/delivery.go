package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/audience"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
	"github.com/relux-works/a2a-swarm/internal/deliverypolicy"
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
		rec := EventRecord{Type: "invite", RoomID: sanitizeLine(e.Room), Sender: e.Sender}
		if e.Label != e.Room {
			rec.Room = sanitizeLine(e.Label)
		}
		// Bound room metadata before appending the reversible, unclipped sender.
		line := snippet("invite "+e.Label+" "+e.Room, 400)
		if e.Sender != "" {
			line += " from=" + senderText(e.Sender)
		}
		return waitItem{line: line, rec: rec, id: e.ID}
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
	agentHeld    []agentDMHeld
	agentStamp   string
	audiences    []audience.Evidence
	selections   []config.MailSelection
	entries      []config.MailEntry
	redactions   []config.Redaction
	learned      []string             // thread roots learned from my own events
	threadEvents []config.ThreadEvent // room-scoped server-derived membership
	close        []string             // --until-kind subscriptions closed by a persisted event
	muted        []string             // mute snapshot used for classification
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
	gate, err := s.refreshAgentGate(ctx)
	if err != nil {
		return batch{}, err
	}
	s.agentDelivery = &gate
	held, err := s.readAgentHeld()
	if err != nil {
		// Existing held bytes stay untouched. The cursor is retained if the
		// later persistence retry still fails; other rooms remain deliverable.
		s.warnAgentDelivery()
		held = agentDMHeldStore{}
	}
	subs, err = s.principalLiveSubscriptions(subs)
	if err != nil {
		return batch{}, err
	}
	b := batch{muted: slices.Clone(st.Muted), agentStamp: gate.Stamp}
	st.ThreadEvents = slices.Clone(st.ThreadEvents)
	me := s.creds.UserID
	mailbox, err := config.OpenMailbox(s.paths, s.name)
	if err != nil {
		s.addressDiagnostic(audience.AddressStateUnavailable)
		return b, err
	}
	audiences := s.audienceCycle(ctx, mailbox, true)
	for room, joined := range resp.Rooms.Join {
		if !s.adminRoomExcluded(room) {
			audiences.Add(room, joined.Timeline.Events)
		}
	}
	// Deferred retained references get a bounded retry even on an empty sync.
	deferred := map[string][]matrix.Event{}
	rooms := map[string]bool{}
	for room := range resp.Rooms.Join {
		rooms[room] = true
	}
	for _, entry := range mailbox.RawPending() {
		if prior, ok := mailbox.Audience(audience.Key{Room: entry.Room, EventID: entry.ID}); ok && prior.Decision == audience.Unresolved {
			var ev matrix.Event
			if json.Unmarshal(entry.Event, &ev) == nil {
				deferred[entry.Room] = append(deferred[entry.Room], ev)
				rooms[entry.Room] = true
			}
		}
	}
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
	firstPages := firstSyncMaxPages
	// Iterate rooms in a stable order so output is deterministic.
	for _, entry := range held.Entries {
		rooms[entry.Room] = true
	}
	for _, roomID := range sortedKeys(rooms) {
		if s.adminRoomExcluded(roomID) {
			continue
		}
		jr := resp.Rooms.Join[roomID]
		keep := func(ev matrix.Event) bool {
			if gate.decision(ev, roomID, me, configured) != "allow" {
				return true
			}
			if ev.Type == "m.room.redaction" || ev.IsRedacted() {
				return true
			}
			if ev.Type != "m.room.message" {
				return false
			}
			// Excluded mail still crosses the durable cursor boundary. Retain
			// it without granting any subscription or notification side effect.
			e := audience.Parse(ev, roomID, me, s.name, s.app.Now())
			if !e.Allows(slices.Contains(configured, ev.Sender)) {
				return true
			}
			// Keep plain replies before chronological classification, even when
			// their targets will only be learned on an older backfill page.
			if ev.Sender == me || ev.Message().RelatesTo != nil || all[roomID] {
				return true
			}
			rel, _, _ := classifyWithPrincipals(ev, roomID, me, s.name, st, subs, configured)
			return rel != ""
		}
		var events []matrix.Event
		var gap *backfillGap
		var err error
		if since == "" {
			events, gap, err = s.firstTimeline(ctx, roomID, jr.Timeline.Events, jr.Timeline.Limited, jr.Timeline.PrevBatch, resp.NextBatch, &firstPages)
			if err == nil {
				events, _, err = s.timelineEvents(ctx, roomID, "", events, false, "", keep)
			}
		} else {
			events, gap, err = s.timelineEvents(ctx, roomID, since, jr.Timeline.Events, jr.Timeline.Limited, jr.Timeline.PrevBatch, keep)
		}
		if err != nil {
			return b, fmt.Errorf("backfill %s: %w", s.roomLabel(ctx, roomID, labels), err)
		}
		if gap != nil {
			b.entries = append(b.entries, config.MailEntry{ID: "gap:" + roomID + ":" + since, Kind: config.MailGap, Room: roomID,
				Label: s.roomLabel(ctx, roomID, labels), From: gap.From, To: gap.To})
		}
		b.redactions = append(b.redactions, eventRedactions(roomID, events)...)
		for _, entry := range held.Entries {
			if entry.Room != roomID {
				continue
			}
			var ev matrix.Event
			if json.Unmarshal(entry.Event, &ev) == nil && ev.EventID == entry.ID && gate.decision(ev, roomID, me, configured) == "allow" {
				events = append(events, ev)
			}
		}
		events = append(events, deferred[roomID]...)
		audiences.Add(roomID, events)
		for _, ev := range events {
			evidence := audiences.Resolve(roomID, ev)
			s.addressDiagnostic(evidence.Diagnostic())
			allowed := evidence.Allows(slices.Contains(configured, ev.Sender))
			decision := gate.decision(ev, roomID, me, configured)
			if decision != "allow" {
				if decision == "hold" && ev.Type == "m.room.message" && !ev.IsRedacted() {
					raw, err := json.Marshal(ev)
					if err != nil {
						return b, err
					}
					if _, err := matrix.ParseStrictJSON(raw); err != nil {
						continue
					}
					b.agentHeld = append(b.agentHeld, agentDMHeld{Room: roomID, ID: ev.EventID, Event: raw})
				}
				continue
			}
			if ev.Type == "m.room.message" && !ev.IsRedacted() {
				root := deliveryTopicRoot(ev, roomID, st)
				if root != "" {
					for _, id := range []string{root, ev.EventID} {
						membership := config.ThreadEvent{Room: roomID, EventID: id, Root: root}
						st.RememberThreadEvent(membership)
						b.threadEvents = append(b.threadEvents, membership)
					}
				}
			}
			active := subs
			if len(closed) > 0 {
				active = nil
				for _, sub := range subs {
					if !closed[sub.ID] {
						active = append(active, sub)
					}
				}
			}
			rel, root, matched := audienceClassify(ev, roomID, me, s.name, st, active, configured, evidence)
			if root != "" {
				b.learned = append(b.learned, root)
				st.AddThread(root)
			}
			if allowed && rel == "" && all[roomID] && ev.Type == "m.room.message" && ev.Sender != me {
				rel = relAll
			}
			if allowed && rel == "" && ev.Type == "m.room.message" && ev.IsRedacted() && ev.Sender != me {
				rel = relevance("redacted")
			}
			if (rel == "" && (allowed || ev.Type != "m.room.message" || ev.Sender == me)) || seen[ev.EventID] {
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
			if allowed && rel != "" && mailbox.Has(ev.EventID) {
				b.selections = append(b.selections, config.MailSelection{Room: roomID, EventID: ev.EventID, Rel: string(rel), Sub: subID})
			}
			raw, err := json.Marshal(ev)
			if err != nil {
				return b, err
			}
			b.entries = append(b.entries, config.MailEntry{ID: ev.EventID, Kind: config.MailEvent, Room: roomID,
				Label: s.roomLabel(ctx, roomID, labels), Rel: string(rel), Sub: subID, Sender: ev.Sender, TS: ev.OriginServerTS, Event: raw})
		}
	}
	// Persist evidence for retained mail and its target chain, not an extra
	// metadata log for unrelated general traffic that has no mailbox record.
	resolved := map[audience.Key]audience.Evidence{}
	for _, e := range audiences.Resolved() {
		resolved[audience.Key{Room: e.Room, EventID: e.EventID}] = e
	}
	var roots []audience.Key
	for _, e := range b.entries {
		if e.Kind == config.MailEvent {
			roots = append(roots, audience.Key{Room: e.Room, EventID: e.ID})
		}
	}
	for _, r := range b.redactions {
		roots = append(roots, audience.Key{Room: r.Room, EventID: r.EventID})
	}
	kept := map[audience.Key]bool{}
	for len(roots) > 0 {
		key := roots[len(roots)-1]
		roots = roots[:len(roots)-1]
		if kept[key] {
			continue
		}
		kept[key] = true
		if e, ok := resolved[key]; ok {
			b.audiences = append(b.audiences, e)
			if e.Target != "" {
				roots = append(roots, audience.Key{Room: e.Room, EventID: e.Target})
			}
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
	// Every Pending consumer enters here, including ack, notices and binding
	// wakes without a preceding sync. Revalidate remote policy even when the
	// local admission stamp did not change; stale snapshots grant no authority.
	gate, err := s.refreshAgentGate(ctx)
	if err != nil {
		return config.State{}, err
	}
	s.agentDelivery = &gate
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
		s.agentMailboxView(mb)
		view := s.installAudienceView(mb)
		// Resolve lazy retained entries before persisting metadata. The view
		// never performs network I/O under the reservation/state lock.
		_ = mb.Pending()
		if err := mb.SaveAudiences(view.Resolved()); err != nil {
			return nil, err
		}
		return fn(cur, mb)
	}
	if !withSubs {
		return config.UpdateStateContextIfChanged(ctx, s.paths, s.name, deadline, func(cur *config.State) error {
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

var errDeliveryMutesChanged = errors.New("topic mutes changed during delivery; retry sync")

func (s *session) beginBindCursor(ctx context.Context, state config.State, kind string, deadline time.Time) (string, error) {
	return s.beginBindCursorView(ctx, config.BindIdleDeliveryStateFromState(state), kind, deadline)
}

func (s *session) beginBindCursorView(ctx context.Context, state config.BindIdleDeliveryState, kind string, deadline time.Time) (string, error) {
	if !state.Marked {
		return "", nil
	}
	if !state.Active {
		return "", config.ErrBindPartOperation
	}
	owner, err := s.app.bindOperationOwner(ctx, s, deadline)
	if err != nil {
		return "", err
	}
	token, err := config.NewBindToken()
	if err != nil {
		return "", err
	}
	generation := ""
	if state.Generation != "" && !state.Stopped {
		generation = state.Generation
		kind = "bind"
	}
	if _, err := config.BeginBindCursor(s.paths, s.name, token, kind, state.NextBatch, generation, owner, s.app.bindOperationOwnerDead(ctx)); err != nil {
		return "", err
	}
	return token, nil
}

// beginControlledCursor adds the existing control lock ahead of the State
// lock for consumers whose contract requires consumer -> control -> state.
func (s *session) beginControlledCursor(ctx context.Context, kind string, deadline time.Time, check func(config.State) error) (config.State, string, error) {
	var state config.State
	var token string
	err := s.app.withRuntimeLock(s.paths, s.name, "control", deadline, func() error {
		current, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return err
		}
		state = current
		if check != nil {
			if err := check(current); err != nil {
				return err
			}
		}
		if !config.BindPartMarked(current) {
			return nil
		}
		if !config.BindPartActive(current) {
			return config.ErrBindPartOperation
		}
		owner, err := s.app.bindOperationOwner(ctx, s, deadline)
		if err != nil {
			return err
		}
		token, err = config.NewBindToken()
		if err != nil {
			return err
		}
		_, err = config.BeginBindCursor(s.paths, s.name, token, kind, current.NextBatch, "", owner, s.app.bindOperationOwnerDead(ctx))
		return err
	})
	if err != nil {
		return config.State{}, "", err
	}
	return state, token, nil
}

func (s *session) abortBindCursor(token string) error {
	if token == "" {
		return nil
	}
	return config.AbortBindCursor(s.paths, s.name, token)
}

func (s *session) commitBindCursor(token, next string) (config.State, error) {
	if token == "" {
		return config.State{}, config.ErrBindPartOperation
	}
	if s.app.beforeBindCursorCommit != nil {
		s.app.beforeBindCursorCommit()
	}
	if _, err := config.CommitBindCursor(s.paths, s.name, token, next); err != nil {
		return config.State{}, err
	}
	return config.LoadState(s.paths, s.name)
}

// persist appends a collected batch to the mailbox (fsynced) and only then
// moves the cursor from since to next; a cursor another consumer moved
// meanwhile is left alone (its batch is in the mailbox too).
func (s *session) persist(ctx context.Context, b batch, since, next string, deadline time.Time) (config.State, error) {
	return s.persistReserved(ctx, b, since, next, deadline, "")
}

func (s *session) persistReserved(ctx context.Context, b batch, since, next string, deadline time.Time, cursorToken string) (config.State, error) {
	a := s.app
	state, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return config.State{}, err
	}
	marked := config.BindPartMarked(state)
	if marked && !config.BindPartActive(state) {
		return state, config.ErrBindPartOperation
	}
	if marked && cursorToken == "" {
		return state, config.ErrBindPartOperation
	}
	if marked && state.NextBatch != since {
		return state, config.ErrBindPartStale
	}
	deferBindCursor := false
	saved, err := s.lockedDeliver(ctx, deadline, len(b.close) > 0, func(cur *config.State, mb *config.Mailbox) ([]string, error) {
		deferBindCursor = false
		// Mute and admission share the state lock. A policy change invalidates
		// the whole batch, including one-shot closures and learned threads.
		if !slices.Equal(b.muted, cur.Muted) {
			return nil, errDeliveryMutesChanged
		}
		if b.agentStamp != "" && b.agentStamp != s.agentGateStamp() {
			return nil, errDeliveryMutesChanged
		}
		if err := migrateLegacyDelivery(cur, mb); err != nil {
			return nil, err
		}
		if a.beforeAppend != nil {
			if err := a.beforeAppend(); err != nil {
				return nil, err
			}
		}
		if err := mb.SaveAudiences(b.audiences); err != nil {
			return nil, err
		}
		if err := mb.ResolveSelections(b.selections); err != nil {
			return nil, err
		}
		if err := mb.RedactAt(b.redactions, a.Now()); err != nil {
			return nil, err
		}
		if err := s.appendAdmitted(ctx, mb, b.entries); err != nil {
			return nil, err
		}
		heldErr := s.persistAgentHeld(ctx, mb, b.agentHeld)
		if heldErr != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.agentReplayDeferred = heldErr != nil
		if heldErr != nil {
			deferBindCursor = true
			s.warnAgentDelivery()
		}
		if a.afterAppend != nil {
			if err := a.afterAppend(); err != nil {
				return nil, err
			}
		}
		for _, e := range b.threadEvents {
			cur.RememberThreadEvent(e)
		}
		for _, r := range b.learned {
			cur.AddThread(r)
		}
		if marked && cur.NextBatch != since {
			return nil, config.ErrBindPartStale
		}
		// Capacity defers cursor advancement so held peer mail can be replayed,
		// while already appended principal/ordinary mail remains deliverable.
		if heldErr == nil && cur.NextBatch == since && !marked {
			cur.NextBatch = next
		}
		return b.close, nil
	})
	if err != nil {
		if marked && !errors.Is(err, errDeliveryMutesChanged) {
			if abortErr := s.abortBindCursor(cursorToken); abortErr != nil {
				return saved, errors.Join(err, abortErr)
			}
		}
		return saved, err
	}
	if marked {
		if deferBindCursor {
			if abortErr := s.abortBindCursor(cursorToken); abortErr != nil {
				return saved, abortErr
			}
			return saved, nil
		}
		committed, err := s.commitBindCursor(cursorToken, next)
		if err != nil {
			if abortErr := s.abortBindCursor(cursorToken); abortErr != nil {
				return saved, errors.Join(err, abortErr)
			}
		}
		return committed, err
	}
	return saved, nil
}

// ingest classifies one sync response from since and persists it.
func (s *session) ingest(ctx context.Context, resp matrix.SyncResponse, since string, subs []Sub, all map[string]bool, deadline time.Time) (config.State, error) {
	return s.ingestReserved(ctx, resp, since, subs, all, deadline, "")
}

func (s *session) ingestReserved(ctx context.Context, resp matrix.SyncResponse, since string, subs []Sub, all map[string]bool, deadline time.Time, cursorToken string) (config.State, error) {
	if since == "" {
		return s.savePositionAllReserved(ctx, resp, deadline, all, cursorToken)
	}
	return s.ingestWithRedactions(ctx, resp, since, subs, all, nil, deadline, cursorToken)
}

// Recollect outside the state lock when a concurrent mute/follow invalidates
// admission. Bound contention; refusal leaves the cursor and mailbox untouched
// so the consumer's next cycle can retry. Already pending mail is preserved.
func (s *session) ingestWithRedactions(ctx context.Context, resp matrix.SyncResponse, since string, subs []Sub, all map[string]bool, redactions []config.Redaction, deadline time.Time, cursorToken string) (config.State, error) {
	abort := func(state config.State, err error) (config.State, error) {
		if cursorToken != "" {
			if abortErr := s.abortBindCursor(cursorToken); abortErr != nil {
				return state, errors.Join(err, abortErr)
			}
		}
		return state, err
	}
	var st config.State
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return abort(st, err)
		}
		snap, err := config.LoadState(s.paths, s.name)
		if err != nil {
			return abort(snap, err)
		}
		b, err := s.collectBatch(ctx, resp, since, snap, subs, all, map[string]string{})
		if err != nil {
			return abort(snap, err)
		}
		if err := ctx.Err(); err != nil {
			return abort(snap, err)
		}
		b.redactions = append(b.redactions, redactions...)
		if s.app.beforeDeliver != nil {
			s.app.beforeDeliver()
		}
		st, err = s.persistReserved(ctx, b, since, resp.NextBatch, deadline, cursorToken)
		if !errors.Is(err, errDeliveryMutesChanged) {
			return st, err
		}
	}
	// Deferral is a normal cycle outcome, including during first sync. Return
	// fresh persisted state: never the zero State returned by a refused update.
	// Consumers keep their cursor and ownership and poll again normally.
	if cursorToken != "" {
		if abortErr := s.abortBindCursor(cursorToken); abortErr != nil {
			return config.State{}, abortErr
		}
	}
	return config.LoadState(s.paths, s.name)
}

// A deferred first sync must still locate our join on the next normal cycle.
func deliverySyncFilter(since string) string {
	if since == "" {
		return firstDeliveryFilter
	}
	return syncFilter
}

// takePosition is the very first run. Only history after the identity's own
// join is eligible; the initial sync and recovery share one HTTP deadline.
func (s *session) takePosition(ctx context.Context, deadline time.Time) (config.State, error) {
	return s.takePositionAll(ctx, deadline, nil)
}

func (s *session) takePositionAll(ctx context.Context, deadline time.Time, all map[string]bool) (config.State, error) {
	return s.takePositionAllKind(ctx, deadline, all, "delivery")
}

func (s *session) takePositionAllKind(ctx context.Context, deadline time.Time, all map[string]bool, kind string) (config.State, error) {
	snap, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return config.State{}, err
	}
	token, err := s.beginBindCursor(ctx, snap, kind, deadline)
	if err != nil {
		return snap, err
	}
	ctx, cancel := context.WithTimeout(ctx, firstSyncTimeout)
	defer cancel()
	resp, err := s.client.Sync(ctx, "", 0, firstDeliveryFilter)
	if err != nil {
		_ = s.abortBindCursor(token)
		return config.State{}, err
	}
	return s.savePositionAllReserved(ctx, resp, deadline, all, token)
}

func (s *session) savePosition(ctx context.Context, resp matrix.SyncResponse, deadline time.Time) (config.State, error) {
	return s.savePositionAll(ctx, resp, deadline, nil)
}

func (s *session) savePositionAll(ctx context.Context, resp matrix.SyncResponse, deadline time.Time, all map[string]bool) (config.State, error) {
	return s.savePositionAllReserved(ctx, resp, deadline, all, "")
}

func (s *session) savePositionAllReserved(ctx context.Context, resp matrix.SyncResponse, deadline time.Time, all map[string]bool, cursorToken string) (config.State, error) {
	ctx, cancel := context.WithTimeout(ctx, firstSyncTimeout)
	defer cancel()
	snap, err := config.LoadState(s.paths, s.name)
	if err != nil || snap.NextBatch != "" {
		if cursorToken != "" {
			_ = s.abortBindCursor(cursorToken)
		}
		return snap, err // Another consumer already established this identity's cursor.
	}
	marked := config.BindPartMarked(snap)
	if marked && !config.BindPartActive(snap) {
		return snap, config.ErrBindPartOperation
	}
	if marked && cursorToken == "" {
		return snap, config.ErrBindPartOperation
	}
	// Preserve initial redaction learning, including for already retained mail,
	// without treating pre-join messages as delivery candidates.
	var redactions []config.Redaction
	if s.client.FilterEvents != nil {
		// Scope the callback to this scan without mutating the caller's client.
		bounded := *s
		client := *s.client
		bounded.client = &client
		s = &bounded
		s.installRedactionCacheContext(ctx)
	}
	for _, roomID := range sortedKeys(resp.Rooms.Join) {
		if s.adminRoomExcluded(roomID) {
			continue
		}
		jr := resp.Rooms.Join[roomID]
		if len(jr.Timeline.Events) > backfillPageSize {
			continue // The bounded collector will report this malformed tail as a gap.
		}
		if s.client.FilterEvents != nil {
			jr.Timeline.Events, err = s.client.FilterEvents(roomID, jr.Timeline.Events)
			if err != nil {
				if cursorToken != "" {
					_ = s.abortBindCursor(cursorToken)
				}
				return snap, err
			}
			resp.Rooms.Join[roomID] = jr
		}
		redactions = append(redactions, eventRedactions(roomID, jr.Timeline.Events)...)
	}
	s.principals = s.readDeliveryPrincipals()
	list, err := s.getSubs(ctx)
	if err != nil {
		if cursorToken != "" {
			_ = s.abortBindCursor(cursorToken)
		}
		return snap, err
	}
	var subs []Sub
	for _, sub := range list.Subs {
		if !sub.expiredAt(s.app.Now()) {
			subs = append(subs, sub)
		}
	}
	return s.ingestWithRedactions(ctx, resp, "", subs, all, redactions, deadline, cursorToken)
}

// syncNow is the "bring the mailbox up to date" step of inbox and the hooks:
// one /sync from the cursor with timeout 0 (no long poll), persisted like
// wait's; the first run recovers only bounded post-join message history.
func (s *session) syncNow(ctx context.Context, st config.State, subs []Sub, deadline time.Time) (config.State, error) {
	s.principals = s.readDeliveryPrincipals()
	if st.NextBatch == "" {
		return s.takePosition(ctx, deadline)
	}
	token, err := s.beginBindCursor(ctx, st, "delivery", deadline)
	if err != nil {
		return st, err
	}
	resp, err := s.client.Sync(ctx, st.NextBatch, 0, syncFilter)
	if err != nil {
		_ = s.abortBindCursor(token)
		return st, err
	}
	return s.ingestReserved(ctx, resp, st.NextBatch, subs, nil, deadline, token)
}

type bindIdleSyncResult struct {
	mailboxProvenEmpty bool
}

func (s *session) syncIdleBindNow(ctx context.Context, state config.BindIdleDeliveryState, subs []Sub, deadline time.Time) (bindIdleSyncResult, error) {
	s.principals = s.readDeliveryPrincipals()
	if !state.Marked || !state.Active {
		return bindIdleSyncResult{}, config.ErrBindPartOperation
	}
	if state.NextBatch == "" {
		return bindIdleSyncResult{}, errors.New("idle bind cursor is empty; ordinary position initialization required")
	}
	token, err := s.beginBindCursorView(ctx, state, "delivery", deadline)
	if err != nil {
		return bindIdleSyncResult{}, err
	}
	resp, err := s.client.Sync(ctx, state.NextBatch, 0, syncFilter)
	if err != nil {
		if abortErr := s.abortBindCursor(token); abortErr != nil {
			return bindIdleSyncResult{}, errors.Join(err, abortErr)
		}
		return bindIdleSyncResult{}, err
	}
	return s.finishIdleBindSync(ctx, state, token, resp, subs, deadline)
}

// finishIdleBindSync persists one sync response of a marked binding under the
// cursor reservation token, which the caller took for state.NextBatch. A
// response without events advances the sidecar cursor alone when the mailbox
// and the legacy delivery fields are empty; everything else goes through the
// ordinary delivery persistence, which commits the cursor through the same
// reservation. The reservation is released on every refusal.
func (s *session) finishIdleBindSync(ctx context.Context, state config.BindIdleDeliveryState, token string, resp matrix.SyncResponse, subs []Sub, deadline time.Time) (bindIdleSyncResult, error) {
	if resp.NextBatch != "" && len(resp.Rooms.Join) == 0 && len(resp.Rooms.Invite) == 0 {
		if _, err := s.principalLiveSubscriptions(subs); err != nil {
			if abortErr := s.abortBindCursor(token); abortErr != nil {
				return bindIdleSyncResult{}, errors.Join(err, abortErr)
			}
			return bindIdleSyncResult{}, err
		}
		_, committed, err := config.TryCommitEmptyBindCursor(s.paths, s.name, token, resp.NextBatch, state)
		if err != nil {
			if abortErr := s.abortBindCursor(token); abortErr != nil {
				return bindIdleSyncResult{}, errors.Join(err, abortErr)
			}
			return bindIdleSyncResult{}, err
		} else if committed {
			return bindIdleSyncResult{mailboxProvenEmpty: true}, nil
		}
	}
	_, err := s.ingestReserved(ctx, resp, state.NextBatch, subs, nil, deadline, token)
	return bindIdleSyncResult{}, err
}

// startDelivery removes expired subscriptions and returns the state to
// deliver from and the live subscriptions. It acknowledges nothing.
func (s *session) startDelivery(ctx context.Context, now time.Time) (config.State, SubList, error) {
	return s.updateSubs(ctx, func(st *config.State, list *SubList) (bool, error) {
		return len(list.pruneExpired(now)) > 0, nil
	})
}

// startBindDelivery avoids opening a no-op State writer on a stable marked
// bind. Legacy subscription migration and expiry pruning still use the normal
// locked writer path whenever the cached provider view cannot prove they are
// unnecessary.
type bindDeliveryStart struct {
	state  config.State
	view   config.BindIdleDeliveryState
	cached bool
}

func (s *session) startBindDelivery(ctx context.Context, now time.Time) (bindDeliveryStart, SubList, error) {
	if view, hit, err := config.TryLoadCachedBindIdleDeliveryState(s.paths, s.name); hit {
		if err != nil {
			return bindDeliveryStart{}, SubList{}, err
		}
		if !view.Active {
			return bindDeliveryStart{}, SubList{}, config.ErrBindPartOperation
		}
		if view.Stopped || view.Generation == "" {
			return bindDeliveryStart{}, SubList{}, errors.New("binding retired")
		}
		if view.SealValid && !view.HasMigration && !view.HasLegacySubscriptions {
			if err := s.checkConnection(); err != nil {
				return bindDeliveryStart{}, SubList{}, err
			}
			list, err := s.getSubs(ctx)
			if err != nil {
				return bindDeliveryStart{}, SubList{}, err
			}
			if len(list.pruneExpired(now)) == 0 {
				if err := s.checkConnection(); err != nil {
					return bindDeliveryStart{}, SubList{}, err
				}
				return bindDeliveryStart{view: view, cached: true}, list, nil
			}
		}
	}
	state, list, err := s.startDelivery(ctx, now)
	if err != nil {
		return bindDeliveryStart{}, SubList{}, err
	}
	return bindDeliveryStart{state: state}, list, nil
}

// shownHint tells a wait/watch consumer how its entries leave the mailbox.
const shownHint = "a2a: printed entries stay in the mailbox until received: run `a2a inbox`, or `a2a ack <ids>` / `a2a ack all-shown`"

// showNew prints the mailbox entries that wait/watch has not shown yet and f
// selects, one flushed write each, and marks them shown after the write, under
// the state lock (so concurrent waits never print the same entry). It receipts
// nothing. It returns how many it printed.
func (a *App) showNew(ctx context.Context, s *session, f deliveryFilter, asJSON bool) (int, error) {
	gate, err := s.refreshAgentGate(ctx)
	if err != nil {
		return 0, err
	}
	s.agentDelivery = &gate
	f.predicates.principals = s.principals
	printed := 0
	_, err = s.lockedDeliver(ctx, time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
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
	if err := s.creds.ValidateWaitKeywords(); err != nil {
		return err
	}
	if err := refuseBoundWait(ctx, s); err != nil {
		return err
	}
	releaseConsumer, err := config.AcquireRawConsumerLock(s.paths, s.name, time.Now().Add(100*time.Millisecond))
	if err != nil {
		return err
	}
	releaseOnce := releaseConsumer
	var consumerReleased bool
	releaseConsumer = func() {
		if !consumerReleased {
			consumerReleased = true
			releaseOnce()
		}
	}
	defer releaseConsumer()
	if err := refuseBoundWait(ctx, s); err != nil {
		return err
	}
	s.longConsumer = deliverypolicy.LongConsumer("wait", *timeout)
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
			if !errors.Is(err, ErrOwnStatusPending) {
				return err
			}
		} else {
			lastHeartbeat = a.Now()
		}
		defer func() {
			releaseConsumer()
			stopCtx, cancel := context.WithTimeout(context.Background(), daemon.LockReleaseBound)
			defer cancel()
			profile.Doorbell = "none"
			if err := s.retryLivenessSource(stopCtx, profile, "wait-disarm"); err != nil {
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
			base, err := config.LoadState(s.paths, s.name)
			if err != nil {
				return config.State{}, err
			}
			token, err := s.beginBindCursor(ctx, base, "delivery", deadline)
			if err != nil {
				return base, err
			}
			initialCtx, cancel := context.WithTimeout(ctx, firstSyncTimeout)
			defer cancel()
			resp, err := s.waitSync(initialCtx, "", 0, firstDeliveryFilter)
			if err != nil {
				_ = s.abortBindCursor(token)
				return config.State{}, err
			}
			return s.savePositionAllReserved(initialCtx, resp, time.Time{}, filter.allRooms(), token)
		}); err != nil {
			return err
		}
	}
	for {
		s.principals = s.readDeliveryPrincipals()
		if *status != "" && a.Now().Sub(lastHeartbeat) >= *heartbeat {
			if err := s.writeLivenessSource(ctx, profile, "wait-heartbeat"); err != nil {
				if !errors.Is(err, ErrOwnStatusPending) {
					return err
				}
			} else {
				lastHeartbeat = a.Now()
			}
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
		if err := s.pauseAgentReplay(ctx, deadline); err != nil {
			if errors.Is(err, errWaitTimeout) {
				return printTimeout()
			}
			return err
		}
		type reservedWaitBatch struct {
			response matrix.SyncResponse
			token    string
		}
		batch, err := waitRetry(ctx, s, deadline, func() (reservedWaitBatch, error) {
			token, err := s.beginBindCursor(ctx, st, "delivery", deadline)
			if err != nil {
				return reservedWaitBatch{}, err
			}
			resp, err := s.waitSyncUntil(ctx, st.NextBatch, poll, deliverySyncFilter(st.NextBatch), deadline)
			if err != nil {
				if abortErr := s.abortBindCursor(token); abortErr != nil {
					return reservedWaitBatch{}, fmt.Errorf("wait sync failed (%v); cursor reservation cleanup failed: %w", err, abortErr)
				}
				return reservedWaitBatch{}, err
			}
			return reservedWaitBatch{response: resp, token: token}, nil
		})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		readSubscriptions := waitRetry[SubList]
		if batch.token != "" {
			readSubscriptions = waitRetryCursorOwned[SubList]
		}
		subs, err := readSubscriptions(ctx, s, deadline, func() (SubList, error) { return s.getSubs(ctx) })
		if err != nil {
			if abortErr := s.abortBindCursor(batch.token); abortErr != nil {
				return fmt.Errorf("wait subscriptions failed (%v); cursor reservation cleanup failed: %w", err, abortErr)
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if st, err = s.ingestReserved(ctx, batch.response, st.NextBatch, subs.Subs, filter.allRooms(), time.Time{}, batch.token); err != nil {
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
		// An ack receives without printing: the entries never reached the
		// session in full, so they hold back the room's read receipt. They are
		// received in timeline order, the order the ledger's checkpoint is
		// derived from, whatever order the arguments name them in.
		live := mb.InOrder(ids)
		ordered := make([]string, 0, len(live))
		for _, e := range live {
			ordered = append(ordered, e.ID)
		}
		// The count keeps its meaning: every argument naming a pending entry
		// (a repeated id counts each time, as before).
		pending := map[string]bool{}
		for _, id := range ordered {
			pending[id] = true
		}
		named := 0
		for _, id := range ids {
			if pending[id] {
				named++
			}
		}
		s.noteDelivered(mb, live, nil)
		if _, err := mb.Receive(ordered); err != nil {
			return nil, err
		}
		n = named
		return nil, nil
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
	o, err := parseWatchOptions(args)
	if err != nil {
		return err
	}
	rooms, predicates := o.rooms, o.predicates
	all, asJSON := &o.all, &o.json
	if o.notices {
		return a.watchNotices(ctx, rooms, *all, *asJSON, o.listen, predicates)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	if err := s.creds.ValidateWaitKeywords(); err != nil {
		return err
	}
	if err := refuseBoundWait(ctx, s); err != nil {
		return err
	}
	releaseConsumer, err := config.AcquireRawConsumerLock(s.paths, s.name, time.Now().Add(100*time.Millisecond))
	if err != nil {
		return err
	}
	defer releaseConsumer()
	if err := refuseBoundWait(ctx, s); err != nil {
		return err
	}
	s.longConsumer = deliverypolicy.LongConsumer("watch", 0)
	stopped := func(err error) error {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	filter, err := waitRetry(ctx, s, time.Time{}, func() (deliveryFilter, error) {
		return s.resolveFilter(ctx, rooms, *all)
	})
	if err != nil {
		return stopped(err)
	}
	filter.predicates = predicates.forSession(s)
	st, _, err := s.startDelivery(ctx, a.Now())
	if err != nil {
		return stopped(err)
	}
	if st.NextBatch == "" {
		// Reserve inside the retried operation, after its lease check, so a retry
		// never attempts to recover its own live cursor reservation.
		type initialWatchBatch struct {
			response matrix.SyncResponse
			deadline time.Time
			token    string
		}
		initial, readErr := waitRetry(ctx, s, time.Time{}, func() (initialWatchBatch, error) {
			initialCtx, cancel := context.WithTimeout(ctx, firstSyncTimeout)
			defer cancel()
			initialDeadline, _ := initialCtx.Deadline()
			token, err := s.beginBindCursor(ctx, st, "delivery", time.Time{})
			if err != nil {
				return initialWatchBatch{}, err
			}
			resp, err := s.waitSync(initialCtx, "", 0, firstDeliveryFilter)
			if err != nil {
				if abortErr := s.abortBindCursor(token); abortErr != nil {
					return initialWatchBatch{}, fmt.Errorf("watch initial sync failed (%v); cursor reservation cleanup failed: %w", err, abortErr)
				}
				return initialWatchBatch{}, err
			}
			return initialWatchBatch{response: resp, deadline: initialDeadline, token: token}, nil
		})
		if readErr != nil {
			return stopped(readErr)
		}
		recoveryCtx, cancel := context.WithDeadline(ctx, initial.deadline)
		st, err = s.savePositionAllReserved(recoveryCtx, initial.response, time.Time{}, filter.allRooms(), initial.token)
		cancel()
		if err != nil {
			return stopped(err)
		}
	}
	for {
		s.principals = s.readDeliveryPrincipals()
		if _, err := a.showNew(ctx, s, filter, *asJSON); err != nil {
			return err
		}
		type reservedWatchBatch struct {
			response matrix.SyncResponse
			token    string
		}
		if err := s.pauseAgentReplay(ctx, time.Time{}); err != nil {
			return stopped(err)
		}
		batch, err := waitRetry(ctx, s, time.Time{}, func() (reservedWatchBatch, error) {
			token, err := s.beginBindCursor(ctx, st, "delivery", time.Time{})
			if err != nil {
				return reservedWatchBatch{}, err
			}
			resp, err := s.waitSync(ctx, st.NextBatch, maxPoll, deliverySyncFilter(st.NextBatch))
			if err != nil {
				if abortErr := s.abortBindCursor(token); abortErr != nil {
					return reservedWatchBatch{}, fmt.Errorf("watch sync failed (%v); cursor reservation cleanup failed: %w", err, abortErr)
				}
				return reservedWatchBatch{}, err
			}
			return reservedWatchBatch{response: resp, token: token}, nil
		})
		if err != nil {
			return stopped(err)
		}
		readSubscriptions := waitRetry[SubList]
		if batch.token != "" {
			readSubscriptions = waitRetryCursorOwned[SubList]
		}
		subs, err := readSubscriptions(ctx, s, time.Time{}, func() (SubList, error) { return s.getSubs(ctx) })
		if err != nil {
			if abortErr := s.abortBindCursor(batch.token); abortErr != nil {
				return stopped(fmt.Errorf("watch subscriptions failed (%v); cursor reservation cleanup failed: %w", err, abortErr))
			}
			return stopped(err)
		}
		if st, err = s.ingestReserved(ctx, batch.response, st.NextBatch, subs.Subs, filter.allRooms(), time.Time{}, batch.token); err != nil {
			return stopped(err)
		}
	}
}
