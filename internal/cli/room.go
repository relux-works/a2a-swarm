package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Room policy: public_chat and directory visibility private (joinable by
// alias, not listed), redactions need power 100, and no automatic principals
// receive power or invitations. Only caller-supplied invitees are invited.
// Room version 12 gives the creator its power implicitly and refuses a creator
// listed in power_levels.users, so the caller is never listed.
const (
	roomPreset     = "public_chat"
	roomVisibility = "private"
)

// Bound filtered history reads; reaching the cap leaves the room untouched.
const roomRetireMaxPages = 100

const roomUsage = "usage: a2a room new <alias> [--name N] [--topic T] [--invite a,b] [--product X] [--private] | a2a room retire <alias> | a2a room members <room> [--membership join|invite|join,invite] [--json]"

// roomSpec is one room to create with the standard policy.
type roomSpec struct {
	local       string   // alias local part, without # and :server
	name, topic string   // optional m.room.name / m.room.topic
	invite      []string // explicit invitees (full user ids), without power grants
	private     bool     // require invite-only policy readback before follow-up writes
}

// cmdRoom: a2a room new <alias> [--name N] [--topic T] [--invite a,b]
func cmdRoom(ctx context.Context, a *App, args []string) error {
	if len(args) > 0 && args[0] == "members" {
		return cmdRoomMembers(ctx, a, args[1:])
	}
	if len(args) == 2 && args[0] == "retire" {
		return cmdRoomRetire(ctx, a, args[1])
	}
	if len(args) == 0 || args[0] != "new" {
		return usagef("%s", roomUsage)
	}
	fs := newFlagSet("room new")
	name := fs.String("name", "", "room name (m.room.name)")
	topic := fs.String("topic", "", "room topic (m.room.topic)")
	invite := fs.String("invite", "", "comma-separated invitees (no power grants; no automatic invitees)")
	product := fs.String("product", "", "product space to adopt the new room into")
	private := fs.Bool("private", false, "invite-only, joined history, guests forbidden; verify server policy")
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
	if *product != "" && !productNameRE.MatchString(*product) {
		return usagef("invalid product name %q: use [a-z0-9-]{1,40}", sanitizeLine(*product))
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
	spec := roomSpec{local: local, name: *name, topic: *topic, private: *private}
	for _, u := range splitList(*invite) {
		id, _ := normalizeUser(u, s.server)
		spec.invite = append(spec.invite, id)
	}
	if *product != "" {
		if _, _, err := s.productSpace(ctx, *product); err != nil {
			return err
		}
	}
	id, created, err := s.newRoom(ctx, spec)
	if err != nil {
		return err
	}
	if created {
		// The creator is joined by the creation; like `join`, it gets the
		// room subscription, so it hears what is posted there (a parent keeper
		// that created a child's room heard nothing before, 2026-10-05).
		if err := s.subscribeRoom(ctx, a, id, s.shortAlias("#"+local+":"+s.server), a.Now()); err != nil {
			return err
		}
	}
	if *product == "" {
		return nil
	}
	// Adopt by alias, so every message names the room as the caller typed it.
	return s.adoptProductRoom(ctx, *product, "#"+local, false)
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
		if spec.private {
			if err := s.privateRoomPolicy(ctx, id); err != nil {
				return id, false, fmt.Errorf("existing room %s %s not qualified as private; room unchanged: %w", label, id, err)
			}
		}
		a.printf("exists %s %s\n", label, id)
		return id, false, nil
	} else if !errors.Is(err, matrix.ErrUnknownAlias) {
		return "", false, fmt.Errorf("resolve %s: %w", label, err)
	}
	me := strings.ToLower(s.creds.UserID)
	req := matrix.CreateRoomRequest{
		Preset: roomPreset, Visibility: roomVisibility, RoomAliasName: spec.local, Name: spec.name, Topic: spec.topic,
		PowerLevelContentOverride: map[string]any{"redact": 100, "users": map[string]int{}},
	}
	if spec.private {
		req.Preset = "private_chat"
		req.InitialState = []matrix.InitialStateEvent{
			{Type: "m.room.join_rules", StateKey: "", Content: json.RawMessage(`{"join_rule":"invite"}`)},
			{Type: "m.room.history_visibility", StateKey: "", Content: json.RawMessage(`{"history_visibility":"joined"}`)},
			{Type: "m.room.guest_access", StateKey: "", Content: json.RawMessage(`{"guest_access":"forbidden"}`)},
		}
	}
	id, err = s.client.CreateRoom(ctx, req)
	if matrix.IsCode(err, "M_ROOM_IN_USE") {
		if id, rerr := s.client.ResolveAlias(ctx, full); rerr == nil {
			if spec.private {
				if err := s.privateRoomPolicy(ctx, id); err != nil {
					return id, false, fmt.Errorf("existing room %s %s not qualified as private; room unchanged: %w", label, id, err)
				}
			}
			a.printf("exists %s %s\n", label, id)
			return id, false, nil
		}
	}
	if err != nil {
		return "", false, fmt.Errorf("create %s: %w", label, err)
	}
	if spec.private {
		if err := s.privateRoomPolicy(ctx, id); err != nil {
			return id, true, fmt.Errorf("created %s %s, but not qualified as private; room retained for inspection: %w", label, id, err)
		}
		a.printf("verified private policy %s %s (invite-only, history joined, guests forbidden, unencrypted)\n", label, id)
	}
	a.printf("created %s %s (preset %s, visibility %s: not in the room directory; power: redact 100, creator %s implicit)\n",
		label, id, req.Preset, roomVisibility, sanitizeLine(s.creds.UserID))
	seen := map[string]bool{me: true}
	for _, u := range spec.invite {
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

// cmdRoomRetire: a2a room retire <alias>. It removes an EMPTY room created by
// mistake (Matrix cannot delete a room): it unlinks the room from every parent
// space where the caller has the power, deletes the alias, and leaves. A room
// with any message is refused: history is archived with `a2a archive` and the
// room is retired through the move checklist instead, never by this command.
func cmdRoomRetire(ctx context.Context, a *App, arg string) error {
	local, err := roomLocal(arg)
	if err != nil {
		return err
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	full := "#" + local + ":" + s.server
	label := s.shortAlias(full)
	id, err := s.client.ResolveAlias(ctx, full)
	if err != nil {
		if errors.Is(err, matrix.ErrUnknownAlias) {
			return fmt.Errorf("room %s does not exist", label)
		}
		return err
	}
	// An empty filtered chunk can still have older messages. Only a page
	// without an end token proves the visible history has been exhausted.
	from := ""
	seen := map[string]bool{}
	for n := 0; ; n++ {
		if n == roomRetireMaxPages {
			return fmt.Errorf("cannot prove %s is empty after %d pages; room unchanged", label, roomRetireMaxPages)
		}
		page, err := s.client.Messages(ctx, id, from, "b", 1)
		if err != nil {
			return fmt.Errorf("cannot prove %s is empty (join it first); room unchanged: %w", label, err)
		}
		if len(page.Chunk) > 0 {
			return fmt.Errorf("%s has messages: it is not retired by this command; archive it (a2a archive %s) and follow the room-move checklist", label, local)
		}
		if page.End == "" {
			break
		}
		if seen[page.End] {
			return fmt.Errorf("cannot prove %s is empty: pagination repeated a token; room unchanged", label)
		}
		seen[page.End] = true
		from = page.End
	}
	parents, err := s.client.GetStateEvents(ctx, id, "m.space.parent")
	if err != nil {
		return fmt.Errorf("read the parent spaces of %s: %w", label, err)
	}
	for _, ev := range parents {
		if ev.StateKey == nil || *ev.StateKey == "" {
			continue
		}
		space := *ev.StateKey
		if _, err := s.client.PutState(ctx, space, "m.space.child", id, map[string]any{}); err != nil {
			a.printf("not unlinked from %s: %v (%s can unlink it)\n", space, err, s.spaceLinkers(ctx, space))
			continue
		}
		a.printf("unlinked from %s\n", space)
	}
	if err := s.client.DeleteAlias(ctx, full); err != nil {
		return fmt.Errorf("delete alias %s: %w", label, err)
	}
	a.printf("deleted alias %s\n", label)
	if err := s.client.Leave(ctx, id); err != nil {
		return fmt.Errorf("leave %s: %w", id, err)
	}
	a.printf("left %s (the empty room stays on the server without an alias)\n", id)
	return s.unsubscribeRoom(ctx, a, id, label)
}
