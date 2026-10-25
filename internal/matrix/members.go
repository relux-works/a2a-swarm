package matrix

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"unicode"
)

var (
	ErrMembersMalformed  = errors.New("matrix: malformed membership response")
	ErrMembersIncomplete = errors.New("matrix: incomplete membership response")
	ErrMembersNotCurrent = errors.New("matrix: caller is not joined in membership response; current roster unverified")
)

const membersResponseLimit = 16 << 20

// RoomMember projects only the principal and its current membership. Peer
// profile fields, reasons, event history and unsigned data are never returned.
type RoomMember struct {
	UserID     string `json:"user_id"`
	Membership string `json:"membership"`
}

// CurrentRoomMembers reads the complete, unfiltered /members response using
// this client's authority. A left caller can receive historical state from
// Matrix, so its own join must be present in the SAME response. Completeness
// attests transport/schema, not server honesty or a cross-room global roster.
func (c *Client) CurrentRoomMembers(ctx context.Context, roomID, caller string) ([]RoomMember, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/members", nil, nil, "", -1)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, ErrMembersIncomplete
	}
	v, err := readControl(ctx, resp, membersResponseLimit+1)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if v != nil && v.complete {
			return nil, ErrMembersMalformed
		}
		return nil, ErrMembersIncomplete
	}
	// /members has no pagination. Never silently treat a partial-page extension
	// or a lazy/limited-state envelope as this command's complete response.
	for _, key := range []string{"next_batch", "limited", "partial_state"} {
		if _, exists := v.value[key]; exists {
			return nil, ErrMembersIncomplete
		}
	}
	chunk, ok := v.value["chunk"].([]any)
	if !ok {
		return nil, ErrMembersMalformed
	}
	members := make([]RoomMember, 0, len(chunk))
	seen := make(map[string]bool, len(chunk))
	joined := false
	for _, raw := range chunk {
		ev, ok := raw.(map[string]any)
		if !ok || ev["type"] != "m.room.member" || ev["room_id"] != roomID {
			return nil, ErrMembersMalformed
		}
		id, ok := ev["state_key"].(string)
		if !ok || !memberUserID(id) || seen[id] {
			return nil, ErrMembersMalformed
		}
		seen[id] = true
		content, ok := ev["content"].(map[string]any)
		if !ok {
			return nil, ErrMembersMalformed
		}
		membership, ok := content["membership"].(string)
		if !ok {
			return nil, ErrMembersMalformed
		}
		switch membership {
		case "join", "invite", "leave", "ban", "knock":
		default:
			return nil, ErrMembersMalformed
		}
		if id == caller && membership == "join" {
			joined = true
		}
		if membership == "join" || membership == "invite" {
			members = append(members, RoomMember{UserID: id, Membership: membership})
		}
	}
	if !joined {
		return nil, ErrMembersNotCurrent
	}
	sort.Slice(members, func(i, j int) bool { return members[i].UserID < members[j].UserID })
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return members, nil
}

func memberUserID(id string) bool {
	if !strings.HasPrefix(id, "@") || len(id) > 255 || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return false
	}
	local, server, ok := strings.Cut(id[1:], ":")
	return ok && local != "" && server != ""
}
