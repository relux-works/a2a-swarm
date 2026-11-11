package cli

import (
	"context"
	"errors"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

var errPrivateSpace = errors.New("private Space could not be verified; no existing room was changed")

func (s *session) newPrivateProduct(ctx context.Context, name string) error {
	client := s.client.WithoutRedirects()
	alias := "#" + name + ":" + s.server
	room, err := client.ResolveAlias(ctx, alias)
	if errors.Is(err, matrix.ErrUnknownAlias) {
		room, err = client.CreateRoom(ctx, matrix.CreateRoomRequest{
			RoomAliasName: name, Preset: "private_chat", Visibility: "private",
			CreationContent: map[string]any{"type": "m.space"},
			InitialState: []matrix.InitialStateEvent{
				{Type: "m.room.join_rules", StateKey: "", Content: []byte(`{"join_rule":"invite"}`)},
				{Type: "m.room.guest_access", StateKey: "", Content: []byte(`{"guest_access":"forbidden"}`)},
				{Type: "m.room.history_visibility", StateKey: "", Content: []byte(`{"history_visibility":"joined"}`)},
			},
		})
		if err == nil {
			if !matrix.ValidVerificationID(room, '!') {
				return errors.New("private Space creation returned an invalid room ID; outcome requires inspection")
			}
			s.app.printf("created private space #%s %s\n", name, room)
			return nil
		}
		if !matrix.IsCode(err, "M_ROOM_IN_USE") {
			return errors.New("private Space creation failed; outcome may be unknown, no automatic retry was made")
		}
		room, err = client.ResolveAlias(ctx, alias)
	}
	if err != nil {
		return errPrivateSpace
	}
	events, err := client.GetRoomPolicyState(ctx, room)
	if err != nil || !privateSpacePolicy(events, s.creds.UserID) {
		return errPrivateSpace
	}
	visibility, err := client.GetRoomDirectoryVisibility(ctx, room)
	if err != nil || visibility != "private" {
		return errPrivateSpace
	}
	s.app.printf("already private space #%s %s\n", name, room)
	return nil
}

// privateSpacePolicy qualifies the observed Space and caller, not directory
// visibility, encryption or the future state of any federated server.
func privateSpacePolicy(events []matrix.RoomPolicyEvent, caller string) bool {
	type tuple struct{ typ, key string }
	seen := map[tuple]bool{}
	create, invite, guest, history, joined := false, false, false, false, false
	value := func(content map[string]any, key, want string) bool {
		for k := range content {
			if k != key && strings.EqualFold(k, key) {
				return false
			}
		}
		return content[key] == want
	}
	for _, event := range events {
		key := tuple{event.Type, event.StateKey}
		if seen[key] {
			return false
		}
		seen[key] = true
		switch event.Type {
		case "m.room.create":
			if event.StateKey == "" {
				create = value(event.Content, "type", "m.space")
			}
		case "m.room.join_rules":
			if event.StateKey == "" {
				invite = value(event.Content, "join_rule", "invite")
			}
		case "m.room.guest_access":
			if event.StateKey == "" {
				guest = value(event.Content, "guest_access", "forbidden")
			}
		case "m.room.history_visibility":
			if event.StateKey == "" {
				history = value(event.Content, "history_visibility", "joined")
			}
		case "m.room.member":
			if event.StateKey == caller {
				joined = value(event.Content, "membership", "join")
			}
		}
	}
	return create && invite && guest && history && joined
}
