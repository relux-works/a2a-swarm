package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

var productNameRE = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

const productUsage = "usage: a2a product new X [--keeper NAME] [--projector NAME] | product adopt X <room> | product list | product show X"

func cmdProduct(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", productUsage)
	}
	switch args[0] {
	case "new":
		fs := newFlagSet("product new")
		keeper := fs.String("keeper", "", "product keeper identity to invite and authorize")
		projector := fs.String("projector", "", "product projector identity to invite and authorize")
		pos, err := parseInterspersed(fs, args[1:])
		if err != nil {
			return err
		}
		if len(pos) != 1 {
			return usagef("%s", productUsage)
		}
		if !productNameRE.MatchString(pos[0]) {
			return usagef("invalid product name %q: use [a-z0-9-]{1,40}", sanitizeLine(pos[0]))
		}
		for _, raw := range []string{*keeper, *projector} {
			if raw != "" {
				if _, err := normalizeUser(raw, "x"); err != nil {
					return usagef("invalid product identity %q", sanitizeLine(raw))
				}
			}
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		return s.newProduct(ctx, pos[0], *keeper, *projector)
	case "adopt":
		if len(args) != 3 || !productNameRE.MatchString(args[1]) {
			return usagef("%s", productUsage)
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		return s.adoptProductRoom(ctx, args[1], args[2], false)
	case "list":
		if len(args) != 1 {
			return usagef("%s", productUsage)
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		rooms, err := s.client.JoinedRooms(ctx)
		if err != nil {
			return err
		}
		sort.Strings(rooms)
		for _, id := range rooms {
			var c map[string]any
			if s.client.GetState(ctx, id, "m.room.create", "", &c) == nil && c["type"] == "m.space" {
				a.printf("%s\t%s\n", s.roomLabel(ctx, id, nil), id)
			}
		}
		return nil
	case "show":
		if len(args) != 2 || !productNameRE.MatchString(args[1]) {
			return usagef("%s", productUsage)
		}
		s, err := a.session()
		if err != nil {
			return err
		}
		space, _, err := s.productSpace(ctx, args[1])
		if err != nil {
			return err
		}
		children, err := s.spaceChildren(ctx, space)
		if err != nil {
			return err
		}
		for _, id := range children {
			a.printf("%s\t%s\n", s.roomLabel(ctx, id, nil), id)
		}
		return nil
	}
	return usagef("%s", productUsage)
}

func (s *session) productSpace(ctx context.Context, name string) (string, string, error) {
	alias := "#" + name + ":" + s.server
	id, err := s.client.ResolveAlias(ctx, alias)
	if err != nil {
		if errors.Is(err, matrix.ErrUnknownAlias) {
			return "", "", fmt.Errorf("#%s is not a space", name)
		}
		return "", "", fmt.Errorf("product #%s: %w", name, err)
	}
	var c map[string]any
	if err := s.client.GetState(ctx, id, "m.room.create", "", &c); err != nil || c["type"] != "m.space" {
		return "", "", fmt.Errorf("#%s is not a space", name)
	}
	return id, s.shortAlias(alias), nil
}

func (s *session) newProduct(ctx context.Context, name, keeperArg, projectorArg string) error {
	alias := "#" + name + ":" + s.server
	space, err := s.client.ResolveAlias(ctx, alias)
	if errors.Is(err, matrix.ErrUnknownAlias) {
		space, err = s.client.CreateRoom(ctx, matrix.CreateRoomRequest{Preset: roomPreset, Visibility: roomVisibility, RoomAliasName: name,
			CreationContent: map[string]any{"type": "m.space"}, PowerLevelContentOverride: map[string]any{"events_default": 50}})
		racedAlias := matrix.IsCode(err, "M_ROOM_IN_USE")
		if racedAlias {
			space, err = s.client.ResolveAlias(ctx, alias)
			if err == nil {
				s.app.printf("already space #%s %s\n", name, space)
			}
		}
		if err != nil {
			return fmt.Errorf("create space #%s: %w", name, err)
		}
		if !racedAlias {
			s.app.printf("created space #%s %s\n", name, space)
		}
	} else if err != nil {
		return err
	} else {
		var c map[string]any
		if err := s.client.GetState(ctx, space, "m.room.create", "", &c); err != nil || c["type"] != "m.space" {
			return fmt.Errorf("#%s already exists and is not a space", name)
		}
		s.app.printf("already space #%s %s\n", name, space)
	}
	keeper, projector := "", ""
	if keeperArg != "" {
		keeper, _ = normalizeUser(keeperArg, s.server)
	}
	if projectorArg != "" {
		projector, _ = normalizeUser(projectorArg, s.server)
	}
	rooms := []struct{ suffix, policy string }{{"lobby", "lobby"}, {"broadcast", "broadcast"}, {"system", "system"}}
	for _, r := range rooms {
		local := name + "-" + r.suffix
		id, e := s.client.ResolveAlias(ctx, "#"+local+":"+s.server)
		if errors.Is(e, matrix.ErrUnknownAlias) {
			if r.policy == "lobby" {
				id, _, e = s.newRoom(ctx, roomSpec{local: local})
				if e != nil {
					return e
				}
			} else {
				req := matrix.CreateRoomRequest{Preset: roomPreset, Visibility: roomVisibility, RoomAliasName: local}
				switch r.policy {
				case "broadcast":
					users := map[string]int{}
					if keeper != "" && keeper != strings.ToLower(s.creds.UserID) {
						users[keeper] = 50
					}
					req.PowerLevelContentOverride = map[string]any{"events_default": 50, "users": users}
				case "system":
					users := map[string]int{}
					if projector != "" && projector != strings.ToLower(s.creds.UserID) {
						users[projector] = 50
					}
					req.PowerLevelContentOverride = map[string]any{"events_default": 50, "users": users}
				}
				id, e = s.client.CreateRoom(ctx, req)
				racedAlias := matrix.IsCode(e, "M_ROOM_IN_USE")
				if racedAlias {
					id, e = s.client.ResolveAlias(ctx, "#"+local+":"+s.server)
					if e == nil {
						s.app.printf("already #%s %s\n", local, id)
					}
				}
				if e != nil {
					return fmt.Errorf("create #%s: %w", local, e)
				}
				if !racedAlias {
					s.app.printf("created #%s %s\n", local, id)
				}
			}
		} else if e != nil {
			return e
		} else {
			s.app.printf("already #%s %s\n", local, id)
		}
		if err := s.linkSpace(ctx, space, id); err != nil {
			return err
		}
		if r.policy == "broadcast" && keeper != "" {
			if err := s.inviteProductUser(ctx, id, local, keeper); err != nil {
				return err
			}
		}
		if r.policy == "system" && projector != "" {
			if err := s.inviteProductUser(ctx, id, local, projector); err != nil {
				return err
			}
		}
	}
	for _, u := range []string{keeper, projector} {
		if u != "" {
			if err := s.inviteProductUser(ctx, space, "#"+name, u); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *session) inviteProductUser(ctx context.Context, room, label, user string) error {
	if strings.EqualFold(user, s.creds.UserID) {
		return nil
	}
	var membership struct {
		Membership string `json:"membership"`
	}
	if err := s.client.GetState(ctx, room, "m.room.member", user, &membership); err == nil {
		s.app.printf("already invited %s to %s\n", user, label)
		return nil
	}
	if err := s.client.Invite(ctx, room, user); err != nil {
		if isNotFound(err) {
			s.app.printf("not invited %s to %s: no such account yet\n", user, label)
			return nil
		}
		return fmt.Errorf("invite %s to %s: %w", user, label, err)
	}
	s.app.printf("invited %s to %s\n", user, label)
	return nil
}

// liveChild reports whether m.space.child content links a child. A child event
// whose content has no non-empty "via" is a removed link (Matrix spec, spaces).
func liveChild(content map[string]any) bool {
	via, ok := content["via"].([]any)
	return ok && len(via) > 0
}

// spaceChildren lists the room ids a space links as live children.
func (s *session) spaceChildren(ctx context.Context, space string) ([]string, error) {
	events, err := s.client.GetStateEvents(ctx, space, "m.space.child")
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, ev := range events {
		var c map[string]any
		if ev.StateKey == nil || json.Unmarshal(ev.Content, &c) != nil || !liveChild(c) {
			continue
		}
		ids = append(ids, *ev.StateKey)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *session) linkSpace(ctx context.Context, space, room string) error {
	content := map[string]any{"via": []string{s.server}}
	var old map[string]any
	if err := s.client.GetState(ctx, space, "m.space.child", room, &old); err == nil && liveChild(old) {
		s.app.printf("already child %s\n", room)
	} else {
		if _, err := s.client.PutState(ctx, space, "m.space.child", room, content); err != nil {
			return fmt.Errorf("link child %s: %w", room, err)
		}
		s.app.printf("linked child %s\n", room)
	}
	var parent map[string]any
	if err := s.client.GetState(ctx, room, "m.space.parent", space, &parent); err == nil {
		return nil
	}
	content["canonical"] = true
	if _, err := s.client.PutState(ctx, room, "m.space.parent", space, content); err != nil {
		if matrix.IsStatus(err, 403) {
			s.app.printf("parent link skipped: no power in %s\n", s.roomLabel(ctx, room, nil))
			return nil
		}
		return fmt.Errorf("link parent in %s: %w", s.roomLabel(ctx, room, nil), err)
	}
	s.app.printf("linked parent in %s\n", s.roomLabel(ctx, room, nil))
	return nil
}

func (s *session) adoptProductRoom(ctx context.Context, name, roomArg string, knownID bool) error {
	space, _, err := s.productSpace(ctx, name)
	if err != nil {
		return err
	}
	room, label := roomArg, roomArg
	if !knownID {
		room, label, err = s.resolveRoom(ctx, roomArg)
		if err != nil {
			if errors.Is(err, matrix.ErrUnknownAlias) {
				return fmt.Errorf("room %s does not exist", sanitizeLine(roomArg))
			}
			return err
		}
	}
	var create map[string]any
	if err := s.client.GetState(ctx, room, "m.room.create", "", &create); err != nil {
		return fmt.Errorf("room %s does not exist", sanitizeLine(roomArg))
	}
	var old map[string]any
	if err := s.client.GetState(ctx, space, "m.space.child", room, &old); err == nil && liveChild(old) {
		s.app.printf("already child %s\n", label)
		return nil
	}
	if _, err := s.client.PutState(ctx, space, "m.space.child", room, map[string]any{"via": []string{s.server}}); err != nil {
		return fmt.Errorf("link child %s: %w", label, err)
	}
	s.app.printf("linked child %s\n", label)
	parent := map[string]any{"via": []string{s.server}, "canonical": true}
	if _, err := s.client.PutState(ctx, room, "m.space.parent", space, parent); err != nil {
		if matrix.IsStatus(err, 403) {
			s.app.printf("parent link skipped: no power in %s\n", label)
			return nil
		}
		return err
	}
	s.app.printf("linked parent in %s\n", label)
	return nil
}

func cmdBroadcastRoom(ctx context.Context, a *App, args []string) error {
	if len(args) != 1 || args[0] != "init" {
		return usagef("usage: a2a broadcast-room init")
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	id, err := s.client.ResolveAlias(ctx, "#broadcast:"+s.server)
	if err == nil {
		a.printf("already #broadcast %s\n", id)
		return nil
	}
	if !errors.Is(err, matrix.ErrUnknownAlias) {
		return err
	}
	id, err = s.client.CreateRoom(ctx, matrix.CreateRoomRequest{Preset: roomPreset, Visibility: roomVisibility, RoomAliasName: "broadcast", PowerLevelContentOverride: map[string]any{"events_default": 50}})
	if matrix.IsCode(err, "M_ROOM_IN_USE") {
		id, err = s.client.ResolveAlias(ctx, "#broadcast:"+s.server)
		if err == nil {
			a.printf("already #broadcast %s\n", id)
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("create #broadcast: %w", err)
	}
	a.printf("created #broadcast %s (events_default 50, creator %s admin)\n", id, sanitizeLine(s.creds.UserID))
	return nil
}
