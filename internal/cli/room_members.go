package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const membersUsage = "usage: a2a room members <room> [--membership join|invite|join,invite] [--json]"

func cmdRoomMembers(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("room members")
	filter := fs.String("membership", "join,invite", "project join, invite, or join,invite")
	asJSON := fs.Bool("json", false, "print one minimal current-room membership record")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || (*filter != "join" && *filter != "invite" && *filter != "join,invite") {
		return usagef("%s", membersUsage)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	id, _, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return fmt.Errorf("room members: resolve room: %w", err)
	}
	members, err := s.client.CurrentRoomMembers(ctx, id, s.creds.UserID)
	if err != nil {
		if matrix.IsStatus(err, 403) {
			return fmt.Errorf("room members: permission denied: %w", err)
		}
		if errors.Is(err, matrix.ErrMembersMalformed) || errors.Is(err, matrix.ErrMembersIncomplete) || errors.Is(err, matrix.ErrMembersNotCurrent) {
			return fmt.Errorf("room members: %w; no roster printed", err)
		}
		return fmt.Errorf("room members: request failed: %w", err)
	}
	projected := make([]matrix.RoomMember, 0, len(members))
	for _, m := range members {
		if *filter == "join,invite" || m.Membership == *filter {
			projected = append(projected, m)
		}
	}
	if err := s.checkConnection(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(a.Stdout).Encode(struct {
			Type         string              `json:"type"`
			RoomID       string              `json:"room_id"`
			Caller       string              `json:"caller"`
			Filter       string              `json:"membership_filter"`
			Completeness string              `json:"completeness"`
			Members      []matrix.RoomMember `json:"members"`
		}{"room-members", id, s.creds.UserID, *filter, "complete-response", projected})
	}
	a.printf("room-members\t%s\tcomplete-response\tfilter=%s\tcount=%d\n", strconv.QuoteToASCII(id), *filter, len(projected))
	for _, m := range projected {
		a.printf("%s\t%s\n", m.Membership, strconv.QuoteToASCII(m.UserID))
	}
	return nil
}
