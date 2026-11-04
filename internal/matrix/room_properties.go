package matrix

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"
)

const roomPropertyLimit = 64 << 10

// AvatarState is the bounded projection of one empty-key m.room.avatar read.
// Present distinguishes M_NOT_FOUND from an existing cleared avatar event.
type AvatarState struct {
	URL     string `json:"url,omitempty"`
	Present bool   `json:"-"`
}

// ParentState preserves omitted canonical separately from an explicit false.
type ParentState struct {
	Via       []string `json:"via,omitempty"`
	Canonical *bool    `json:"canonical,omitempty"`
	Present   bool     `json:"-"`
}

// StateWriteResult separates an acknowledgement from observed current content.
// UNKNOWN permits an applied write; callers must never blindly retry it.
type StateWriteResult struct {
	Outcome      string `json:"outcome"`
	Acknowledged bool   `json:"acknowledged"`
	EventID      string `json:"event_id,omitempty"`
	Readback     string `json:"readback"`
	HTTPStatus   int    `json:"http_status,omitempty"`
	ErrCode      string `json:"errcode,omitempty"`
}

// RoomPropertyID follows the existing opaque room-ID contract, including IDs
// without a server suffix. It does not resolve, normalize or select a room.
func RoomPropertyID(id string) bool {
	return len(id) >= 2 && len(id) <= 255 && id[0] == '!' && utf8.ValidString(id)
}

func propertyPath(room, typ, key string) string {
	return v3 + "/rooms/" + seg(room) + "/state/" + seg(typ) + "/" + seg(key)
}

func originalCode(err error, status int, codes ...string) bool {
	var me *Error
	if !errors.As(err, &me) || !me.OriginalRequest || me.Status != status || me.control == nil {
		return false
	}
	for _, code := range codes {
		if me.ErrCode == code {
			return true
		}
	}
	return false
}

// propertyContent never follows redirects or treats a partial entity as state.
func (c *Client) propertyContent(ctx context.Context, room, typ, key string) (map[string]any, bool, error) {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ctx, http.MethodGet, propertyPath(room, typ, key), nil, nil, "", -1)
	if originalCode(err, 404, "M_NOT_FOUND") {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, false, errors.New("matrix: state response is not 200")
	}
	v, err := readControl(ctx, resp, roomPropertyLimit+1)
	if err != nil {
		return nil, false, err
	}
	return v.value, true, nil
}

func avatarProjection(m map[string]any, present bool) (AvatarState, error) {
	out := AvatarState{Present: present}
	if value, exists := m["url"]; exists {
		url, ok := value.(string)
		if !ok {
			return AvatarState{}, errors.New("matrix: malformed avatar content")
		}
		if url != "" {
			if _, err := ParseMXC(url); err != nil {
				return AvatarState{}, errors.New("matrix: malformed avatar MXC")
			}
		}
		out.URL = url
	}
	return out, nil
}

func parentProjection(m map[string]any, present bool) (ParentState, error) {
	out := ParentState{Present: present}
	if value, exists := m["via"]; exists {
		via, ok := value.([]any)
		if !ok {
			return ParentState{}, errors.New("matrix: malformed parent via")
		}
		out.Via = make([]string, 0, len(via))
		for _, value := range via {
			server, ok := value.(string)
			if !ok || !memberServerName(server) {
				return ParentState{}, errors.New("matrix: malformed parent server")
			}
			out.Via = append(out.Via, server)
		}
	}
	if value, exists := m["canonical"]; exists {
		b, ok := value.(bool)
		if !ok {
			return ParentState{}, errors.New("matrix: malformed parent canonical")
		}
		out.Canonical = &b
	}
	return out, nil
}

// RoomAvatar reads only the pinned room's empty-key avatar, not media/history.
func (c *Client) RoomAvatar(ctx context.Context, room string) (AvatarState, error) {
	if !RoomPropertyID(room) {
		return AvatarState{}, errors.New("matrix: invalid avatar room ID")
	}
	m, present, err := c.propertyContent(ctx, room, "m.room.avatar", "")
	if err != nil {
		return AvatarState{}, err
	}
	return avatarProjection(m, present)
}

// RoomParent reads only one child-side parent key; it never reads the parent.
func (c *Client) RoomParent(ctx context.Context, child, parent string) (ParentState, error) {
	if !RoomPropertyID(child) || !RoomPropertyID(parent) || child == parent {
		return ParentState{}, errors.New("matrix: invalid parent target")
	}
	m, present, err := c.propertyContent(ctx, child, "m.space.parent", parent)
	if err != nil {
		return ParentState{}, err
	}
	return parentProjection(m, present)
}

// ValidateParentVia uses the existing received Matrix server-name grammar.
// Order and case are retained, including DNS, IPv4 and bracketed IPv6/ports.
func ValidateParentVia(via []string) bool {
	if len(via) == 0 {
		return false
	}
	for _, server := range via {
		if !memberServerName(server) {
			return false
		}
	}
	return true
}

// SetRoomAvatar writes an already uploaded MXC to the fixed empty avatar key.
func (c *Client) SetRoomAvatar(ctx context.Context, room, mxc string) (StateWriteResult, error) {
	if !RoomPropertyID(room) {
		return StateWriteResult{}, errors.New("matrix: invalid avatar room ID")
	}
	if _, err := ParseMXC(mxc); err != nil {
		return StateWriteResult{}, err
	}
	return c.writeProperty(ctx, room, "m.room.avatar", "", map[string]any{"url": mxc})
}

// SetRoomParent writes only the child-side link; canonical is always explicit.
func (c *Client) SetRoomParent(ctx context.Context, child, parent string, via []string, canonical bool) (StateWriteResult, error) {
	if !RoomPropertyID(child) || !RoomPropertyID(parent) || child == parent || !ValidateParentVia(via) {
		return StateWriteResult{}, errors.New("matrix: invalid parent target or via")
	}
	return c.writeProperty(ctx, child, "m.space.parent", parent, map[string]any{"via": append([]string(nil), via...), "canonical": canonical})
}

func (c *Client) writeProperty(ctx context.Context, room, typ, key string, content map[string]any) (StateWriteResult, error) {
	out := StateWriteResult{Outcome: "UNKNOWN", Readback: "not-attempted"}
	// Encode before sending, then compare the same strict tree to the full
	// readback. A projection alone could hide unexpected or wrong-key content.
	raw, err := json.Marshal(content)
	if err != nil || len(raw) > roomPropertyLimit {
		return out, errors.New("matrix: invalid property content size")
	}
	want, err := strictJSON(raw)
	if err != nil {
		return out, err
	}
	ackCtx, cancel := withTimeout(ctx)
	defer cancel()
	resp, err := c.WithoutRedirects().send(ackCtx, http.MethodPut, propertyPath(room, typ, key), nil, bytes.NewReader(raw), "application/json", int64(len(raw)))
	if err != nil {
		// Only the existing validated original-response pairs are projected;
		// arbitrary server text or codes never become diagnostic output.
		switch {
		case originalCode(err, 403, "M_FORBIDDEN"):
			out.Outcome, out.HTTPStatus, out.ErrCode = "REFUSED", 403, "M_FORBIDDEN"
		case originalCode(err, 401, "M_UNKNOWN_TOKEN"):
			out.Outcome, out.HTTPStatus, out.ErrCode = "REFUSED", 401, "M_UNKNOWN_TOKEN"
		case originalCode(err, 401, "M_MISSING_TOKEN"):
			out.Outcome, out.HTTPStatus, out.ErrCode = "REFUSED", 401, "M_MISSING_TOKEN"
		}
		return out, errors.New("matrix: property write " + out.Outcome)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return out, errors.New("matrix: property acknowledgement unverified")
	}
	v, err := readControl(ackCtx, resp, roomPropertyLimit+1)
	var ack struct {
		EventID string `json:"event_id"`
	}
	if err == nil {
		err = v.consume(ackCtx, &ack, "event_id")
	}
	if err != nil {
		return out, errors.New("matrix: property acknowledgement unverified")
	}
	out.Acknowledged, out.EventID = true, ack.EventID
	got, present, err := c.propertyContent(ctx, room, typ, key)
	if err == nil && present {
		if typ == "m.room.avatar" {
			_, err = avatarProjection(got, true)
		} else {
			_, err = parentProjection(got, true)
		}
	}
	if err != nil {
		out.Readback = "unavailable"
		return out, errors.New("matrix: acknowledged property readback unavailable")
	}
	if !present {
		out.Readback = "absent"
		return out, errors.New("matrix: acknowledged property readback absent")
	}
	if !reflect.DeepEqual(want, got) {
		out.Readback = "mismatch"
		return out, errors.New("matrix: acknowledged property readback mismatch")
	}
	out.Outcome, out.Readback = "VERIFIED", "match"
	return out, nil
}
