package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// CardEventType is the state event type of an agent card.
const CardEventType = "ai.relux.a2a.card"

// LobbyRoom is where cards live.
const LobbyRoom = "#lobby"

// Card is the content of an ai.relux.a2a.card state event.
type Card struct {
	Name      string `json:"name"`
	Format    string `json:"format"`
	Body      string `json:"body"`
	UpdatedAt string `json:"updated_at"`
}

// cmdCard: a2a card set --file card.md | a2a card get <name>
func cmdCard(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a card set --file card.md | a2a card get <name>"
	if len(args) == 0 {
		return usagef(use)
	}
	fs := newFlagSet("card " + args[0])
	file := fs.String("file", "", "markdown card file (or - for stdin)")
	room := fs.String("room", LobbyRoom, "room that holds the cards")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	switch args[0] {
	case "set":
		if *file == "" || len(pos) != 0 {
			return usagef(use)
		}
		src := *file
		if src != "-" {
			src = "@" + src
		}
		body, err := a.readBody(src)
		if err != nil {
			return err
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		roomID, label, err := s.resolveRoom(ctx, *room)
		if err != nil {
			return err
		}
		card := Card{Name: s.name, Format: "text/markdown", Body: body, UpdatedAt: a.Now().UTC().Format(time.RFC3339)}
		id, err := s.client.PutState(ctx, roomID, CardEventType, s.creds.UserID, card)
		if err != nil {
			if matrix.IsCode(err, "M_FORBIDDEN") {
				return fmt.Errorf("card set: %s does not let you send %s state events; a room admin must set power_levels.events[%q] = 0 (or give you the state_default level)", label, CardEventType, CardEventType)
			}
			return fmt.Errorf("card set: %w", err)
		}
		a.printf("%s\n", id)
		return nil
	case "get":
		if len(pos) != 1 {
			return usagef(use)
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		user, err := normalizeUser(pos[0], s.server)
		if err != nil {
			return err
		}
		roomID, label, err := s.resolveRoom(ctx, *room)
		if err != nil {
			return err
		}
		var card Card
		if err := s.client.GetState(ctx, roomID, CardEventType, user, &card); err != nil {
			if isNotFound(err) {
				return fmt.Errorf("no card for %s in %s", user, label)
			}
			return err
		}
		a.printf("%s\n", strings.TrimRight(sanitizeText(card.Body), "\n"))
		return nil
	default:
		return usagef(use)
	}
}

// statePresence maps a2a status values to Matrix presence. The a2a value is
// also kept at the start of status_msg so that it round-trips exactly.
var statePresence = map[string]string{"active": "online", "idle": "unavailable", "away": "unavailable"}

// cmdStatus: a2a status set <active|idle|away> ["text"] | a2a status get [name]
func cmdStatus(ctx context.Context, a *App, args []string) error {
	if len(args) > 0 && args[0] == "init" {
		return cmdStatusInit(ctx, a, args[1:])
	}
	if typedStatusArgs(args) {
		return cmdStatusRecord(ctx, a, args[1:])
	}
	const use = `usage: a2a status set <active|idle|away> ["text"] | a2a status get [name]`
	if len(args) == 0 {
		return usagef(use)
	}
	switch args[0] {
	case "set":
		if len(args) < 2 {
			return usagef(use)
		}
		pres, ok := statePresence[args[1]]
		if !ok {
			return usagef("invalid status %q (one of: active, idle, away)", args[1])
		}
		msg := args[1]
		if text := sanitizeLine(strings.Join(args[2:], " ")); text != "" {
			if len(text) > 500 {
				return usagef("status text too long (max 500 bytes)")
			}
			msg += ": " + text
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		if err := s.client.SetPresence(ctx, s.creds.UserID, matrix.Presence{Presence: pres, StatusMsg: msg}); err != nil {
			return fmt.Errorf("status set: %w", err)
		}
		a.printf("%s %s\n", s.name, msg)
		return nil
	case "get":
		if len(args) > 2 {
			return usagef(use)
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		user := s.creds.UserID
		if len(args) == 2 {
			if user, err = normalizeUser(args[1], s.server); err != nil {
				return err
			}
		}
		p, err := s.client.GetPresence(ctx, user)
		if isNotFound(err) {
			p, err = matrix.Presence{Presence: "unknown"}, nil
		}
		if err != nil {
			return fmt.Errorf("status get: %w", err)
		}
		msg := p.StatusMsg
		if msg == "" {
			msg = "-"
		}
		a.printf("%s\t%s\t%s\n", localpartIfLocal(user, s.server), p.Presence, sanitizeLine(msg))
		return nil
	default:
		return usagef(use)
	}
}
