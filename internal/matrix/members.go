package matrix

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"unicode/utf8"
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

// Received member events permit historical localparts, including empty and
// control characters (except NUL). This is not the registration policy:
// https://spec.matrix.org/v1.16/appendices/#historical-user-ids
func memberUserID(id string) bool {
	if !strings.HasPrefix(id, "@") || len(id) > 255 || !utf8.ValidString(id) {
		return false
	}
	local, server, ok := strings.Cut(id[1:], ":")
	return ok && !strings.ContainsRune(local, 0) && memberServerName(server)
}

// Validate the received server-name grammar without resolving, normalizing or
// applying new-account DNS label restrictions. Full MXIDs remain case sensitive.
// https://spec.matrix.org/v1.16/appendices/#server-name
func memberServerName(server string) bool {
	host, tail := server, ""
	if strings.HasPrefix(server, "[") {
		end := strings.IndexByte(server, ']')
		if end < 0 {
			return false
		}
		host, tail = server[1:end], server[end+1:]
		addr, err := netip.ParseAddr(host)
		if err != nil || !addr.Is6() || addr.Zone() != "" {
			return false
		}
	} else {
		if pos := strings.IndexByte(server, ':'); pos >= 0 {
			host, tail = server[:pos], server[pos:]
		}
		if len(host) == 0 || len(host) > 255 {
			return false
		}
		for _, b := range []byte(host) {
			if !(b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-' || b == '.') {
				return false
			}
		}
	}
	if tail == "" {
		return true
	}
	if tail[0] != ':' || len(tail) < 2 || len(tail) > 6 {
		return false
	}
	for _, b := range []byte(tail[1:]) {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
