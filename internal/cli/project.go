package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// DefaultProjectKinds are the kinds the projector mirrors into the projection room (protocol rule 24).
var DefaultProjectKinds = []string{"system", "owner-needed"}

// projectFilter is syncFilter for the projector: messages only, no presence.
const projectFilter = syncFilter

// Projection is the ai.relux.a2a content of a projected message.
type Projection struct {
	V              int      `json:"v"`
	Type           string   `json:"type"` // "projection"
	Kind           string   `json:"kind"`
	From           string   `json:"from"`
	To             []string `json:"to,omitempty"`
	At             string   `json:"at"`
	Slug           string   `json:"slug,omitempty"`
	Subtype        string   `json:"subtype,omitempty"`
	Subject        string   `json:"subject,omitempty"`
	OriginRoom     string   `json:"origin_room"`
	OriginAlias    string   `json:"origin_alias,omitempty"`
	OriginEvent    string   `json:"origin_event"`
	Product        string   `json:"product,omitempty"`
	ProductAlias   string   `json:"product_alias,omitempty"`
	TopicRoot      string   `json:"topic_root,omitempty"`
	AuthorMXID     string   `json:"author_mxid"`
	Author         string   `json:"author"`
	AuthorOperator string   `json:"author_operator"`
	AuthorHost     string   `json:"author_host"`
}

// projectionTxn is deterministic per target and origin event, so a retried or
// restarted projector reuses the transaction id independently in each target.
func projectionTxn(targetRoom, originRoom, eventID string) string {
	sum := sha256.Sum256([]byte(targetRoom + "\x00" + originRoom + "\x00" + eventID))
	return "a2aproj" + hex.EncodeToString(sum[:12])
}

const productMembershipTTL = 10 * time.Minute

type productMembership struct {
	space, alias string
	expires      time.Time
}

// productForOrigin finds a room's product from its parent state or a product
// space's child state. Failed discovery is treated as no known product so the
// global projection can still proceed.
func (s *session) productForOrigin(ctx context.Context, roomID string, cache map[string]productMembership, now time.Time) (string, string) {
	if found, ok := cache[roomID]; ok && now.Before(found.expires) {
		return found.space, found.alias
	}
	store := func(space, alias string) (string, string) {
		cache[roomID] = productMembership{space: space, alias: alias, expires: now.Add(productMembershipTTL)}
		return space, alias
	}
	if parents, err := s.client.GetStateEvents(ctx, roomID, "m.space.parent"); err == nil {
		for _, parent := range parents {
			if parent.StateKey == nil {
				continue
			}
			if space, alias, ok := s.productSpaceForID(ctx, *parent.StateKey); ok {
				return store(space, alias)
			}
		}
	}
	rooms, err := s.client.JoinedRooms(ctx)
	if err == nil {
		for _, space := range rooms {
			var create map[string]any
			if s.client.GetState(ctx, space, "m.room.create", "", &create) != nil || create["type"] != "m.space" {
				continue
			}
			children, err := s.client.GetStateEvents(ctx, space, "m.space.child")
			if err != nil {
				continue
			}
			for _, child := range children {
				if child.StateKey != nil && *child.StateKey == roomID {
					if id, alias, ok := s.productSpaceForID(ctx, space); ok {
						return store(id, alias)
					}
				}
			}
		}
	}
	return store("", "")
}

func (s *session) productSpaceForID(ctx context.Context, space string) (string, string, bool) {
	var create map[string]any
	if s.client.GetState(ctx, space, "m.room.create", "", &create) != nil || create["type"] != "m.space" {
		return "", "", false
	}
	var canonical struct {
		Alias string `json:"alias"`
	}
	if s.client.GetState(ctx, space, "m.room.canonical_alias", "", &canonical) != nil || canonical.Alias == "" {
		return "", "", false
	}
	local := strings.TrimPrefix(s.shortAlias(canonical.Alias), "#")
	if !productNameRE.MatchString(local) {
		return "", "", false
	}
	resolved, err := s.client.ResolveAlias(ctx, "#"+local+":"+s.server)
	if err != nil || resolved != space {
		return "", "", false
	}
	return space, "#" + local, true
}

// projectable returns the header of an event the projector must mirror, or false.
func projectable(ev matrix.Event, roomID, systemRoom, me string, kinds map[string]bool) (Header, bool) {
	if ev.Type != "m.room.message" || roomID == systemRoom || ev.Sender == me {
		return Header{}, false
	}
	h, ok := decodeHeader(ev.Message().A2A)
	if !ok || h.Type != "message" || !kinds[h.Kind] {
		return Header{}, false
	}
	return h, true
}

// projection builds the mirrored message for ev.
func projection(ev matrix.Event, h Header, roomID, roomLabel, server, product, productAlias string) map[string]any {
	from := localpartIfLocal(ev.Sender, server)
	author := from
	agent := from
	if agent == "" {
		agent = localpart(ev.Sender)
	}
	identityParts := strings.Split(localpart(ev.Sender), "_")
	authorHost := ""
	if len(identityParts) == 3 && validIdentityPart(identityParts[0]) && validIdentityPart(identityParts[1]) && validIdentityPart(identityParts[2]) {
		authorHost = identityParts[0]
	}
	text := stripHeaderBody(ev.Message().Body)
	at := time.UnixMilli(ev.OriginServerTS).UTC().Format(time.RFC3339)
	var b strings.Builder
	// Header fields are sender-controlled: each is one sanitized line here.
	kind := sanitizeLine(h.Kind)
	if h.Subtype != "" {
		kind += "/" + sanitizeLine(h.Subtype)
	}
	fmt.Fprintf(&b, "[%s] from %s", kind, agent)
	if productAlias != "" {
		fmt.Fprintf(&b, " · %s ›", productAlias)
	} else {
		b.WriteString(" ·")
	}
	fmt.Fprintf(&b, " %s", roomLabel)
	if h.Slug != "" {
		fmt.Fprintf(&b, " › topic %s", sanitizeLine(h.Slug))
	}
	fmt.Fprintf(&b, " › %s", ev.EventID)
	if h.Subject != "" {
		fmt.Fprintf(&b, " · subject %s", sanitizeLine(h.Subject))
	}
	fmt.Fprintf(&b, "\n%s\n\norigin: https://matrix.to/#/%s/%s", sanitizeText(text), roomID, ev.EventID)
	topicRoot := ev.ThreadRoot()
	return map[string]any{
		"msgtype": "m.text",
		"body":    b.String(),
		// Projections never ping anyone: subscribers watch the room.
		"m.mentions": map[string]any{},
		ContentKey: Projection{V: 1, Type: "projection", Kind: h.Kind, From: from, To: h.To, At: at, Slug: h.Slug,
			Subtype: h.Subtype, Subject: h.Subject, OriginRoom: roomID, OriginAlias: roomLabel, OriginEvent: ev.EventID,
			Product: product, ProductAlias: productAlias, TopicRoot: topicRoot, AuthorMXID: ev.Sender, Author: author, AuthorHost: authorHost},
	}
}

func validIdentityPart(part string) bool { return productNameRE.MatchString(part) }

// cmdProject: a2a project [--system R] [--kind k1,k2] [--once] [--poll 30s]
// Runs the projector of protocol rule 24 as the current identity: it accepts
// every invite, and mirrors each message of a projected kind from any joined
// room into the projection room, with a link to the original.
func cmdProject(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("project")
	systemArg := fs.String("system", "system", "projection room")
	kindArg := fs.String("kind", strings.Join(DefaultProjectKinds, ","), "kinds to project")
	once := fs.Bool("once", false, "process one sync batch and exit (for tests and cron)")
	poll := fs.Duration("poll", 30*time.Second, "server-side long-poll per sync")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef("usage: a2a project [--system R] [--kind k1,k2] [--once] [--poll 30s]")
	}
	kinds := map[string]bool{}
	for _, k := range splitList(*kindArg) {
		if !validKind(k) {
			return usagef("invalid --kind %q", sanitizeLine(k))
		}
		kinds[k] = true
	}
	if len(kinds) == 0 {
		return usagef("--kind must name at least one kind")
	}
	if *poll <= 0 || *poll > maxPoll {
		return usagef("--poll must be between 0 and %s", maxPoll)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	systemRoom, systemLabel, err := s.resolveRoom(ctx, *systemArg)
	if err != nil {
		return err
	}
	me := s.creds.UserID
	labels := map[string]string{}
	productCache := map[string]productMembership{}

	st, err := config.LoadState(s.paths, s.name)
	if err != nil {
		return err
	}
	if st.NextBatch == "" {
		// First run: take a position; history before the projector started is not replayed.
		resp, err := s.client.Sync(ctx, "", 0, initialFilter)
		if err != nil {
			return err
		}
		if st, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error {
			cur.NextBatch = resp.NextBatch
			return nil
		}); err != nil {
			return err
		}
		a.printf("projector %s -> %s: position taken\n", s.name, systemLabel)
		if err := s.acceptInvites(ctx, a, resp); err != nil {
			return err
		}
	}
	for {
		resp, err := s.client.Sync(ctx, st.NextBatch, *poll, projectFilter)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := s.acceptInvites(ctx, a, resp); err != nil {
			return err
		}
		for _, roomID := range sortedKeys(resp.Rooms.Join) {
			tl := resp.Rooms.Join[roomID].Timeline
			events, gap, err := s.timelineEvents(ctx, roomID, st.NextBatch, tl.Events, tl.Limited, tl.PrevBatch, func(ev matrix.Event) bool {
				_, ok := projectable(ev, roomID, systemRoom, me, kinds)
				return ok
			})
			if err != nil {
				// Keep the position: the next run retries the whole batch.
				return fmt.Errorf("backfill %s: %w", s.roomLabel(ctx, roomID, labels), err)
			}
			if gap != nil {
				fmt.Fprintf(a.Stderr, "a2a: projector: %s: gap: events between %s and %s could not be checked; they were not projected\n", s.roomLabel(ctx, roomID, labels), sanitizeLine(gap.From), sanitizeLine(gap.To))
			}
			for _, ev := range events {
				h, _ := projectable(ev, roomID, systemRoom, me, kinds)
				label := s.roomLabel(ctx, roomID, labels)
				product, productAlias := s.productForOrigin(ctx, roomID, productCache, a.Now())
				content := projection(ev, h, roomID, label, s.server, product, productAlias)
				id, err := s.client.SendMessageTxn(ctx, systemRoom, projectionTxn(systemRoom, roomID, ev.EventID), content)
				if err != nil {
					// Keep the position: the next run retries with the same txn id.
					return fmt.Errorf("project %s from %s: %w", sanitizeLine(ev.EventID), label, err)
				}
				a.printf("projected %s %s %s -> %s\n", label, ev.EventID, h.Kind, id)
				if productAlias != "" {
					productSystem, resolveErr := s.client.ResolveAlias(ctx, productAlias+"-system:"+s.server)
					if resolveErr != nil {
						fmt.Fprintf(a.Stderr, "a2a: projector: could not project %s to %s-system: %s\n", sanitizeLine(ev.EventID), productAlias, sanitizeLine(resolveErr.Error()))
						continue
					}
					if _, err := s.client.SendMessageTxn(ctx, productSystem, projectionTxn(productSystem, roomID, ev.EventID), content); err != nil {
						fmt.Fprintf(a.Stderr, "a2a: projector: could not project %s to %s-system: %s\n", sanitizeLine(ev.EventID), productAlias, sanitizeLine(err.Error()))
					}
				}
			}
		}
		// Save the position only after every projection of the batch was sent.
		if st, err = config.UpdateState(s.paths, s.name, func(cur *config.State) error {
			cur.NextBatch = resp.NextBatch
			return nil
		}); err != nil {
			return fmt.Errorf("save projector state: %w", err)
		}
		if *once {
			return nil
		}
	}
}

// acceptInvites joins every room the projector is invited to.
func (s *session) acceptInvites(ctx context.Context, a *App, resp matrix.SyncResponse) error {
	for _, roomID := range sortedKeys(resp.Rooms.Invite) {
		if _, err := s.client.Join(ctx, roomID); err != nil {
			return fmt.Errorf("join %s: %w", sanitizeLine(roomID), err)
		}
		a.printf("joined %s\n", sanitizeLine(roomID))
	}
	return nil
}
