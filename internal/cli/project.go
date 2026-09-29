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
	V           int      `json:"v"`
	Type        string   `json:"type"` // "projection"
	Kind        string   `json:"kind"`
	From        string   `json:"from"`
	To          []string `json:"to,omitempty"`
	At          string   `json:"at"`
	Slug        string   `json:"slug,omitempty"`
	OriginRoom  string   `json:"origin_room"`
	OriginAlias string   `json:"origin_alias,omitempty"`
	OriginEvent string   `json:"origin_event"`
}

// projectionTxn is deterministic per origin event, so a retried or restarted
// projector reuses the transaction id and the homeserver deduplicates the send.
func projectionTxn(roomID, eventID string) string {
	sum := sha256.Sum256([]byte(roomID + "\x00" + eventID))
	return "a2aproj" + hex.EncodeToString(sum[:12])
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
func projection(ev matrix.Event, h Header, roomID, roomLabel, server string) map[string]any {
	from := localpartIfLocal(ev.Sender, server)
	text := stripHeaderBody(ev.Message().Body)
	at := time.UnixMilli(ev.OriginServerTS).UTC().Format(time.RFC3339)
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s from %s", h.Kind, roomLabel, from)
	if h.Slug != "" {
		fmt.Fprintf(&b, " (topic %s)", h.Slug)
	}
	fmt.Fprintf(&b, ":\n%s\n\norigin: https://matrix.to/#/%s/%s", sanitizeText(text), roomID, ev.EventID)
	return map[string]any{
		"msgtype": "m.text",
		"body":    b.String(),
		// Projections never ping anyone: subscribers watch the room.
		"m.mentions": map[string]any{},
		ContentKey: Projection{V: 1, Type: "projection", Kind: h.Kind, From: from, To: h.To, At: at, Slug: h.Slug,
			OriginRoom: roomID, OriginAlias: roomLabel, OriginEvent: ev.EventID},
	}
}

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
			for _, ev := range resp.Rooms.Join[roomID].Timeline.Events {
				h, ok := projectable(ev, roomID, systemRoom, me, kinds)
				if !ok {
					continue
				}
				label := s.roomLabel(ctx, roomID, labels)
				content := projection(ev, h, roomID, label, s.server)
				id, err := s.client.SendMessageTxn(ctx, systemRoom, projectionTxn(roomID, ev.EventID), content)
				if err != nil {
					// Keep the position: the next run retries with the same txn id.
					return fmt.Errorf("project %s from %s: %w", sanitizeLine(ev.EventID), label, err)
				}
				a.printf("projected %s %s %s -> %s\n", label, ev.EventID, h.Kind, id)
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
