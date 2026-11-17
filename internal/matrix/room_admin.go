package matrix

import (
	"context"
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidRoomName preserves literal spacing and case; the bound is in bytes.
func ValidRoomName(name string) bool {
	if len(name) == 0 || len(name) > 255 || !utf8.ValidString(name) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// RoomChildViaError returns a safe failing index, or -1 for a count/aggregate
// failure. Existing server spelling, order and duplicates remain accepted.
func RoomChildViaError(via []string) (int, bool) {
	if len(via) < 1 || len(via) > 16 {
		return -1, true
	}
	total := 0
	for i, s := range via {
		if len(s) > 261 || !utf8.ValidString(s) || !ValidateParentVia([]string{s}) {
			return i, true
		}
		total += len(s)
	}
	return -1, total > 4096
}

// RoomName is only a projection of one bounded exact-key read.
// A nil content with present=false means validated original M_NOT_FOUND.
func (c *Client) RoomName(ctx context.Context, room string) (map[string]any, bool, error) {
	return c.readAdminProperty(ctx, room, "", false)
}

func (c *Client) SpaceChild(ctx context.Context, space, child string) (map[string]any, bool, error) {
	if !ValidVerificationID(child, '!') || space == child {
		return nil, false, errors.New("matrix: invalid child selection")
	}
	return c.readAdminProperty(ctx, space, child, true)
}

func (c *Client) readAdminProperty(ctx context.Context, room, key string, child bool) (map[string]any, bool, error) {
	if !ValidVerificationID(room, '!') {
		return nil, false, errors.New("matrix: invalid property selection")
	}
	typ, field := "m.room.name", "name"
	if child {
		typ, field = "m.space.child", "via"
	}
	content, present, err := c.propertyContent(ctx, room, typ, key)
	if err != nil || !present {
		return nil, present, err
	}
	for name := range content {
		if name != field && strings.EqualFold(name, field) {
			return nil, true, errors.New("matrix: ambiguous property field")
		}
	}
	out := map[string]any{}
	v, exists := content[field]
	if !exists {
		return out, true, nil
	}
	if !child {
		s, ok := v.(string)
		if !ok || s != "" && !ValidRoomName(s) {
			return nil, true, errors.New("matrix: malformed name")
		}
		out[field] = s
	} else {
		list, ok := v.([]any)
		if !ok {
			return nil, true, errors.New("matrix: malformed child via")
		}
		via := make([]string, len(list))
		for i, item := range list {
			var valid bool
			via[i], valid = item.(string)
			if !valid {
				return nil, true, errors.New("matrix: malformed child server")
			}
		}
		if _, bad := RoomChildViaError(via); len(via) != 0 && bad {
			return nil, true, errors.New("matrix: invalid child via")
		}
		out[field] = via
	}
	return out, true, nil
}

func (c *Client) SetRoomName(ctx context.Context, room, name string) (StateWriteResult, error) {
	if !ValidVerificationID(room, '!') || !ValidRoomName(name) {
		return StateWriteResult{}, errors.New("matrix: invalid name selection")
	}
	return c.writePropertyChecked(ctx, room, "m.room.name", "", map[string]any{"name": name}, nil)
}

// SetSpaceChild replaces only the selected Space-side event. A nil via means
// the explicit empty tombstone; no reciprocal mutation or retry is performed.
func (c *Client) SetSpaceChild(ctx context.Context, space, child string, via []string) (StateWriteResult, error) {
	if !ValidVerificationID(space, '!') || !ValidVerificationID(child, '!') || space == child {
		return StateWriteResult{}, errors.New("matrix: invalid child selection")
	}
	content := map[string]any{}
	if via != nil {
		if _, bad := RoomChildViaError(via); bad {
			return StateWriteResult{}, errors.New("matrix: invalid child via")
		}
		content["via"] = append([]string(nil), via...)
	}
	return c.writePropertyChecked(ctx, space, "m.space.child", child, content, nil)
}
