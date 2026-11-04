package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const avatarUsage = "usage: a2a room avatar get ROOM [--json] | room avatar set ROOM --expect-room-id !ID --mxc MXC [--json]"
const parentUsage = "usage: a2a room parent get CHILD --parent PARENT [--json] | room parent set CHILD --parent PARENT --expect-child-id !ID --expect-parent-id !ID --via SERVER [--via SERVER...] --canonical true|false [--json]"

type propertyScalar struct {
	value string
	seen  bool
}

func (v *propertyScalar) String() string { return v.value }
func (v *propertyScalar) Set(s string) error {
	if v.seen {
		return fmt.Errorf("scalar flag repeated")
	}
	v.value, v.seen = s, true
	return nil
}

type propertyBool struct{ propertyScalar }

func (v *propertyBool) IsBoolFlag() bool { return true }
func (v *propertyBool) Set(s string) error {
	if s != "true" && s != "false" {
		return fmt.Errorf("boolean must be true or false")
	}
	return v.propertyScalar.Set(s)
}

// Version 1 projects only the selected property, never raw room state or ACK
// entities. For sets, a nonzero exit may accompany an acknowledged UNKNOWN.
type roomPropertyResult struct {
	Type      string `json:"type"`
	Version   int    `json:"version"`
	Operation string `json:"operation"`
	Caller    string `json:"caller"`
	RoomID    string `json:"room_id"`
	StateType string `json:"state_type"`
	StateKey  string `json:"state_key"`
	matrix.StateWriteResult
	Content any `json:"content,omitempty"`
}

func cmdRoomProperty(ctx context.Context, a *App, kind string, args []string) error {
	usage := avatarUsage
	if kind == "parent" {
		usage = parentUsage
	}
	if len(args) == 0 || (args[0] != "get" && args[0] != "set") {
		return usagef("%s", usage)
	}
	op := args[0]
	fs := newFlagSet("room " + kind + " " + op)
	var asJSON propertyBool
	var parent, expectedRoom, expectedParent, mxc, canonical propertyScalar
	var via stringList
	fs.Var(&asJSON, "json", "print the version 1 typed property result")
	if kind == "parent" {
		fs.Var(&parent, "parent", "explicit parent room ID or alias")
	}
	if op == "set" {
		if kind == "avatar" {
			fs.Var(&expectedRoom, "expect-room-id", "required exact resolved room ID")
			fs.Var(&mxc, "mxc", "required already uploaded MXC URI")
		} else {
			fs.Var(&expectedRoom, "expect-child-id", "required exact resolved child ID")
			fs.Var(&expectedParent, "expect-parent-id", "required exact resolved parent ID")
			fs.Var(&via, "via", "required server name; repeat to preserve explicit order")
			fs.Var(&canonical, "canonical", "required literal true or false")
		}
	}
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("%s", usage)
	}
	if _, err := normalizeRoom(pos[0], "unused"); err != nil {
		return err
	}
	if kind == "parent" {
		if !parent.seen || parent.value == "" {
			return usagef("%s", usage)
		}
		if _, err := normalizeRoom(parent.value, "unused"); err != nil {
			return err
		}
	}
	if op == "set" {
		if !expectedRoom.seen || !matrix.RoomPropertyID(expectedRoom.value) {
			return usagef("required expected room ID is invalid")
		}
		if kind == "avatar" {
			if !mxc.seen {
				return usagef("%s", usage)
			}
			if _, err := matrix.ParseMXC(mxc.value); err != nil {
				return usagef("invalid --mxc: %v", err)
			}
		} else if !expectedParent.seen || !matrix.RoomPropertyID(expectedParent.value) || expectedRoom.value == expectedParent.value || !matrix.ValidateParentVia(via) || !canonical.seen || (canonical.value != "true" && canonical.value != "false") {
			return usagef("parent set requires distinct expected IDs, explicit valid --via and --canonical true|false")
		}
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	// Resolve aliases through the selected connection once, without following a
	// directory redirect. Subsequent state requests use only these pinned IDs.
	s.client = s.client.WithoutRedirects()
	id, _, err := s.resolveRoom(ctx, pos[0])
	if err != nil || !matrix.RoomPropertyID(id) {
		return fmt.Errorf("room %s: target resolution unverified", kind)
	}
	parentID := ""
	if kind == "parent" {
		parentID, _, err = s.resolveRoom(ctx, parent.value)
		if err != nil || !matrix.RoomPropertyID(parentID) || parentID == id {
			return fmt.Errorf("room parent: parent resolution unverified")
		}
	}
	if op == "set" && (id != expectedRoom.value || (kind == "parent" && parentID != expectedParent.value)) {
		return fmt.Errorf("room %s: resolved target differs from expected ID; no write attempted", kind)
	}
	out := roomPropertyResult{Type: "room-" + kind, Version: 1, Operation: op, Caller: s.creds.UserID, RoomID: id, StateType: "m.room.avatar", StateKey: ""}
	if kind == "parent" {
		out.StateType, out.StateKey = "m.space.parent", parentID
	}
	var operationErr error
	if op == "set" {
		if kind == "avatar" {
			out.StateWriteResult, operationErr = s.client.SetRoomAvatar(ctx, id, mxc.value)
			if operationErr == nil {
				out.Content = matrix.AvatarState{URL: mxc.value}
			}
		} else {
			b := canonical.value == "true"
			out.StateWriteResult, operationErr = s.client.SetRoomParent(ctx, id, parentID, via, b)
			if operationErr == nil {
				out.Content = matrix.ParentState{Via: via, Canonical: &b}
			}
		}
	} else if kind == "avatar" {
		state, e := s.client.RoomAvatar(ctx, id)
		operationErr = e
		out.Outcome, out.Readback = "ABSENT", "absent"
		if state.Present {
			out.Outcome, out.Readback, out.Content = "PRESENT", "present", state
			if state.URL == "" {
				out.Outcome = "EMPTY"
			}
		}
	} else {
		state, e := s.client.RoomParent(ctx, id, parentID)
		operationErr = e
		out.Outcome, out.Readback = "ABSENT", "absent"
		if state.Present {
			out.Outcome, out.Readback, out.Content = "PRESENT", "present", state
			if len(state.Via) == 0 {
				out.Outcome = "EMPTY"
			}
		}
	}
	if e := s.checkConnection(); e != nil {
		operationErr = e
	}
	if e := ctx.Err(); e != nil {
		operationErr = e
	}
	if operationErr != nil && out.Outcome != "REFUSED" {
		out.Outcome, out.Content = "UNKNOWN", nil
		if op == "get" || out.Readback == "match" {
			out.Readback = "unavailable"
		}
	}
	if asJSON.value == "true" {
		if err := json.NewEncoder(a.Stdout).Encode(out); err != nil {
			return err
		}
	} else {
		content, _ := json.Marshal(out.Content)
		diagnostic := ""
		if out.HTTPStatus != 0 {
			diagnostic = fmt.Sprintf(" http_status=%d errcode=%s", out.HTTPStatus, strconv.QuoteToASCII(out.ErrCode))
		}
		a.printf("%s v1 %s room=%s key=%s outcome=%s acknowledged=%t event_id=%s readback=%s content=%s%s\n", out.Type, op, strconv.QuoteToASCII(id), strconv.QuoteToASCII(out.StateKey), out.Outcome, out.Acknowledged, strconv.QuoteToASCII(out.EventID), out.Readback, content, diagnostic)
	}
	if operationErr != nil {
		return fmt.Errorf("room %s %s: %s (no automatic retry)", kind, op, out.Outcome)
	}
	return nil
}
