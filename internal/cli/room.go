package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Room policy (owner decision, 2026-09-29; protocol rule 24): every new room
// is created with preset public_chat and directory visibility private (joinable
// by alias, not listed), redactions need power 100, the meta-orchestrator has
// power 100 next to the creator, and the meta-orchestrator and the projector
// are invited. Room version 12 gives the creator its power implicitly and
// refuses a creator listed in power_levels.users, so the caller is never listed.
const (
	// MetaOrchestrator is the owner's single inbox (rule 24).
	MetaOrchestrator = "local-models-e9"
	// ProjectorAgent mirrors system and owner-needed events into #system.
	ProjectorAgent = "a2a-projector"

	roomPreset     = "public_chat"
	roomVisibility = "private"
)

const roomUsage = "usage: a2a room new <alias> [--name N] [--topic T] [--invite a,b]"

// roomSpec is one room to create with the standard policy.
type roomSpec struct {
	local       string   // alias local part, without # and :server
	name, topic string   // optional m.room.name / m.room.topic
	invite      []string // extra invitees (full user ids)
}

// cmdRoom: a2a room new <alias> [--name N] [--topic T] [--invite a,b]
func cmdRoom(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 || args[0] != "new" {
		return usagef("%s", roomUsage)
	}
	fs := newFlagSet("room new")
	name := fs.String("name", "", "room name (m.room.name)")
	topic := fs.String("topic", "", "room topic (m.room.topic)")
	invite := fs.String("invite", "", "comma-separated extra invitees, besides "+MetaOrchestrator+" and "+ProjectorAgent)
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("%s", roomUsage)
	}
	local, err := roomLocal(pos[0])
	if err != nil {
		return err
	}
	for _, u := range splitList(*invite) {
		if _, err := normalizeUser(u, "x"); err != nil {
			return usagef("invalid --invite %q: use name, @name, or @name:server", sanitizeLine(u))
		}
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	spec := roomSpec{local: local, name: *name, topic: *topic}
	for _, u := range splitList(*invite) {
		id, _ := normalizeUser(u, s.server)
		spec.invite = append(spec.invite, id)
	}
	_, _, err = s.newRoom(ctx, spec)
	return err
}

// roomLocal validates a new room's alias: #alias or alias on this server.
func roomLocal(arg string) (string, error) {
	local := strings.TrimPrefix(arg, "#")
	if strings.Contains(local, ":") || strings.HasPrefix(arg, "!") || !aliasLocalRE.MatchString(local) {
		return "", usagef("invalid alias %q: room new takes #alias or alias on this server (no :server, no room id)", sanitizeLine(arg))
	}
	return local, nil
}

// newRoom creates spec's room with the standard policy and invites, printing
// every step. An alias that already resolves is left untouched: it prints
// "exists <alias> <id>" and reports created=false.
func (s *session) newRoom(ctx context.Context, spec roomSpec) (id string, created bool, err error) {
	a := s.app
	full := "#" + spec.local + ":" + s.server
	label := s.shortAlias(full)
	if id, err := s.client.ResolveAlias(ctx, full); err == nil {
		a.printf("exists %s %s\n", label, id)
		return id, false, nil
	} else if !errors.Is(err, matrix.ErrUnknownAlias) {
		return "", false, fmt.Errorf("resolve %s: %w", label, err)
	}
	me := strings.ToLower(s.creds.UserID)
	meta := "@" + MetaOrchestrator + ":" + s.server
	users := map[string]int{}
	if meta != me {
		users[meta] = 100
	}
	req := matrix.CreateRoomRequest{
		Preset: roomPreset, Visibility: roomVisibility, RoomAliasName: spec.local, Name: spec.name, Topic: spec.topic,
		PowerLevelContentOverride: map[string]any{"redact": 100, "users": users},
	}
	id, err = s.client.CreateRoom(ctx, req)
	if matrix.IsCode(err, "M_ROOM_IN_USE") {
		if id, rerr := s.client.ResolveAlias(ctx, full); rerr == nil {
			a.printf("exists %s %s\n", label, id)
			return id, false, nil
		}
	}
	if err != nil {
		return "", false, fmt.Errorf("create %s: %w", label, err)
	}
	power := "redact 100"
	if len(users) > 0 {
		power += ", " + meta + " 100"
	}
	a.printf("created %s %s (preset %s, visibility %s: not in the room directory; power: %s, creator %s implicit)\n",
		label, id, roomPreset, roomVisibility, power, sanitizeLine(s.creds.UserID))
	seen := map[string]bool{me: true}
	for _, u := range append([]string{meta, "@" + ProjectorAgent + ":" + s.server}, spec.invite...) {
		if seen[strings.ToLower(u)] {
			continue
		}
		seen[strings.ToLower(u)] = true
		err := s.client.Invite(ctx, id, u)
		switch {
		case err == nil:
			a.printf("invited %s to %s\n", u, label)
		case isNotFound(err):
			a.printf("not invited %s to %s: no such account yet (invite it later: a2a invite %s %s)\n", u, label, label, u)
		default:
			return id, true, fmt.Errorf("%s was created, but inviting %s failed: %w (invite by hand: a2a invite %s %s)", label, u, err, label, u)
		}
	}
	return id, true, nil
}
