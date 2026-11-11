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

const productUsage = "usage: a2a product new X [--private] [--keeper NAME] [--projector NAME] | product adopt X <room> | product list | product show X"

func cmdProduct(ctx context.Context, a *App, args []string) error {
	if len(args) == 0 {
		return usagef("%s", productUsage)
	}
	switch args[0] {
	case "new":
		fs := newFlagSet("product new")
		keeper := fs.String("keeper", "", "product keeper identity to invite and authorize")
		projector := fs.String("projector", "", "product projector identity to invite and authorize")
		private := fs.Bool("private", false, "create only an invite-only, unlisted Space")
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
		if *private && (*keeper != "" || *projector != "") {
			return usagef("--private requires explicit subsequent invitations; do not combine it with --keeper or --projector")
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
		if *private {
			return s.newPrivateProduct(ctx, pos[0])
		}
		return s.newProduct(ctx, pos[0], *keeper, *projector)
	case "adopt":
		if len(args) != 3 {
			return usagef("%s", productUsage)
		}
		if err := validateProductSelector(args[1]); err != nil {
			return err
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

func validateProductSelector(selector string) error {
	if productNameRE.MatchString(selector) || matrix.ValidVerificationID(selector, '!') {
		return nil
	}
	return usagef("invalid product selector %q: use [a-z0-9-]{1,40} or a Space room ID (!...)", sanitizeLine(selector))
}

func (s *session) productSpace(ctx context.Context, selector string) (string, string, error) {
	if err := validateProductSelector(selector); err != nil {
		return "", "", err
	}
	id, label := selector, selector
	if !strings.HasPrefix(selector, "!") {
		alias := "#" + selector + ":" + s.server
		label = s.shortAlias(alias)
		var err error
		id, err = s.client.ResolveAlias(ctx, alias)
		if err != nil {
			if errors.Is(err, matrix.ErrUnknownAlias) {
				return "", "", fmt.Errorf("%s is not a space", label)
			}
			return "", "", fmt.Errorf("product %s: %w", label, err)
		}
	}
	if err := s.client.RequireSpace(ctx, id); err != nil {
		return "", "", fmt.Errorf("%s is not a space (creation state unverified): %w", label, err)
	}
	return id, label, nil
}

func (s *session) newProduct(ctx context.Context, name, keeperArg, projectorArg string) error {
	alias := "#" + name + ":" + s.server
	keeper, projector := "", ""
	if keeperArg != "" {
		keeper, _ = normalizeUser(keeperArg, s.server)
	}
	if projectorArg != "" {
		projector, _ = normalizeUser(projectorArg, s.server)
	}
	// The product keeper gets power 50 in its own space, enough to link its
	// product's rooms (m.space.child); never 100 (e9's ack, 2026-10-06).
	spaceUsers := map[string]int{}
	if keeper != "" && keeper != strings.ToLower(s.creds.UserID) {
		spaceUsers[keeper] = 50
	}
	space, err := s.client.ResolveAlias(ctx, alias)
	if errors.Is(err, matrix.ErrUnknownAlias) {
		space, err = s.client.CreateRoom(ctx, matrix.CreateRoomRequest{Preset: roomPreset, Visibility: roomVisibility, RoomAliasName: name,
			CreationContent: map[string]any{"type": "m.space"}, PowerLevelContentOverride: map[string]any{"events_default": 50, "users": spaceUsers}})
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
		if keeper != "" {
			s.ensureKeeperPower(ctx, space, name, keeper)
		}
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

// ensureKeeperPower raises the keeper to power 50 in an existing product
// space when it is below that; without the caller's power it prints a note.
func (s *session) ensureKeeperPower(ctx context.Context, space, name, keeper string) {
	if keeper == strings.ToLower(s.creds.UserID) {
		return
	}
	var pl map[string]any
	if err := s.client.GetState(ctx, space, "m.room.power_levels", "", &pl); err != nil {
		s.app.printf("keeper power in #%s not checked: %v\n", name, err)
		return
	}
	users, _ := pl["users"].(map[string]any)
	if users == nil {
		users = map[string]any{}
	}
	if lvl, ok := users[keeper].(float64); ok && lvl >= 50 {
		s.app.printf("already keeper power %s in #%s\n", keeper, name)
		return
	}
	users[keeper] = 50
	pl["users"] = users
	if _, err := s.client.PutState(ctx, space, "m.room.power_levels", "", pl); err != nil {
		s.app.printf("keeper power for %s in #%s not set: %v (a space admin can set it)\n", keeper, name, err)
		return
	}
	s.app.printf("keeper power %s 50 in #%s\n", keeper, name)
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

// spaceLinkers names the members whose power allows m.space.child in space,
// for a refusal message; the creator of a room v12 space has it implicitly.
func (s *session) spaceLinkers(ctx context.Context, space string) string {
	var pl struct {
		Users        map[string]int `json:"users"`
		Events       map[string]int `json:"events"`
		StateDefault *int           `json:"state_default"`
	}
	var create struct {
		Sender string `json:"creator"`
	}
	if err := s.client.GetState(ctx, space, "m.room.power_levels", "", &pl); err != nil {
		return "the space's admins"
	}
	need := 50
	if pl.StateDefault != nil {
		need = *pl.StateDefault
	}
	if v, ok := pl.Events["m.space.child"]; ok {
		need = v
	}
	var names []string
	for u, lvl := range pl.Users {
		if lvl >= need {
			names = append(names, localpartIfLocal(u, s.server))
		}
	}
	if s.client.GetState(ctx, space, "m.room.create", "", &create) == nil && create.Sender != "" {
		names = append(names, localpartIfLocal(create.Sender, s.server)+" (creator)")
	}
	if len(names) == 0 {
		return "the space's admins"
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (s *session) adoptProductRoom(ctx context.Context, name, roomArg string, knownID bool) error {
	space, _, err := s.productSpace(ctx, name)
	if err != nil {
		return err
	}
	return s.adoptRoomIntoSpace(ctx, space, name, roomArg, knownID)
}

// space was resolved and verified once by productSpace; never resolve the
// product selector again after room creation or other intervening work.
func (s *session) adoptRoomIntoSpace(ctx context.Context, space, name, roomArg string, knownID bool) error {
	room, label := roomArg, roomArg
	if !knownID {
		var err error
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
		if !knownID && !strings.HasPrefix(roomArg, "!") {
			// The alias resolved, so the room exists; its state is just not
			// readable to a non-member. Linking needs only the space's power.
			s.app.printf("note: cannot read %s (not a member); linking it from the space anyway\n", label)
		} else if !knownID {
			return fmt.Errorf("room %s does not exist, or you are not a member and it cannot be verified: give its alias, or join it first", sanitizeLine(roomArg))
		}
	}
	var old map[string]any
	if err := s.client.GetState(ctx, space, "m.space.child", room, &old); err == nil && liveChild(old) {
		s.app.printf("already child %s\n", label)
		return nil
	}
	if _, err := s.client.PutState(ctx, space, "m.space.child", room, map[string]any{"via": []string{s.server}}); err != nil {
		if matrix.IsStatus(err, 403) {
			spaceLabel := name
			if !strings.HasPrefix(name, "!") {
				spaceLabel = "#" + name
			}
			return fmt.Errorf("%s is NOT linked into product %s: you lack power for m.space.child in %s; %s can link it with: a2a product adopt %s %s",
				label, name, spaceLabel, s.spaceLinkers(ctx, space), name, label)
		}
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
