package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const maxSpaceChildListEntries = 1024

type spaceChildListResult struct {
	Type      string              `json:"type"`
	Version   int                 `json:"version"`
	Operation string              `json:"operation"`
	Caller    string              `json:"caller"`
	RoomID    string              `json:"room_id"`
	StateType string              `json:"state_type"`
	Children  []spaceChildListRow `json:"children"`
}

type spaceChildListRow struct {
	ChildID      string   `json:"child_id"`
	Via          []string `json:"via,omitempty"`
	Order        *string  `json:"order,omitempty"`
	OrderInvalid *int     `json:"order_invalid,omitempty"`
	Status       string   `json:"status"`
	Reason       string   `json:"reason"`
}

// projectSpaceChildList treats each child state event as a row candidate.
// Empty content (the normal removal tombstone) is omitted; malformed explicit
// via declarations remain visible as invalid rows so they cannot disappear.
func projectSpaceChildList(events []matrix.RoomPolicyEvent) ([]spaceChildListRow, bool, bool) {
	rows := make([]spaceChildListRow, 0)
	invalid := false
	entries := 0
	for _, event := range events {
		if event.Type != "m.space.child" {
			continue
		}
		entries++
		if entries > maxSpaceChildListEntries {
			return nil, false, true
		}

		content := event.Content
		row := spaceChildListRow{ChildID: event.StateKey, Status: "valid"}
		via, present := content["via"]
		reason := ""
		for key := range content {
			if key != "via" && strings.EqualFold(key, "via") {
				reason = "ambiguous_property_field"
				break
			}
		}
		if reason == "" && !present {
			// Missing via is a removed link. Unknown extensions do not revive it.
			continue
		}
		if reason == "" {
			list, ok := via.([]any)
			if !ok {
				reason = "malformed_child_via"
			} else {
				servers := make([]string, len(list))
				for i, item := range list {
					server, ok := item.(string)
					if !ok {
						reason = "malformed_child_server"
						break
					}
					servers[i] = server
				}
				if reason == "" {
					if _, bad := matrix.RoomChildViaError(servers); bad {
						reason = "invalid_child_via"
					} else {
						row.Via = servers
					}
				}
			}
		}
		if reason != "" {
			row.Status, row.Reason = "invalid", reason
			invalid = true
		}
		setSpaceChildOrder(&row, content)
		rows = append(rows, row)
	}

	// Preserve the command's stable child-ID order. A value that cannot be
	// used as an order is explicitly placed after all valid or unset orders.
	sort.Slice(rows, func(i, j int) bool {
		badI, badJ := rows[i].OrderInvalid != nil, rows[j].OrderInvalid != nil
		if badI != badJ {
			return !badI
		}
		return rows[i].ChildID < rows[j].ChildID
	})
	return rows, invalid, false
}

func setSpaceChildOrder(row *spaceChildListRow, content map[string]any) {
	value, present := content["order"]
	if !present {
		return
	}
	order, ok := value.(string)
	if ok && validSpaceChildOrder(order) {
		row.Order = &order
		return
	}
	length := spaceChildOrderRawLength(value)
	row.OrderInvalid = &length
}

func validSpaceChildOrder(order string) bool {
	if len(order) > 50 {
		return false
	}
	for i := 0; i < len(order); i++ {
		if order[i] < 0x20 || order[i] > 0x7e {
			return false
		}
	}
	return true
}

func spaceChildOrderRawLength(value any) int {
	if order, ok := value.(string); ok {
		return len(order)
	}
	// Non-string JSON values are also out of spec. Report only the compact
	// encoded byte length, never their content.
	raw, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(raw)
}

func (a *App) writeSpaceChildList(room, caller string, rows []spaceChildListRow, hasInvalid, asJSON bool) int {
	result := spaceChildListResult{
		Type:      "space-child-list",
		Version:   1,
		Operation: "list",
		Caller:    caller,
		RoomID:    room,
		StateType: "m.space.child",
		Children:  rows,
	}
	var output []byte
	if asJSON {
		var err error
		output, err = json.Marshal(result)
		if err != nil {
			return a.adminFailure(adminError("admin_output", "$.output", ExitError))
		}
		output = append(output, '\n')
	} else {
		output = formatSpaceChildListText(room, rows)
	}
	if n, err := a.Stdout.Write(output); err != nil || n != len(output) {
		return a.adminFailure(adminError("admin_output", "$.output", ExitError))
	}
	if hasInvalid {
		return a.adminFailure(adminError("admin_child_invalid", "$.children", ExitInvalidRows))
	}
	return ExitOK
}

func formatSpaceChildListText(room string, rows []spaceChildListRow) []byte {
	var out strings.Builder
	fmt.Fprintf(&out, "space-child-list v1 room=%s state_type=%s rows=%d\n", strconv.QuoteToASCII(room), strconv.QuoteToASCII("m.space.child"), len(rows))
	for _, row := range rows {
		via := "invalid"
		if row.Via != nil {
			encoded, _ := json.Marshal(row.Via)
			via = string(encoded)
		}
		fmt.Fprintf(&out, "child=%s via=%s", strconv.QuoteToASCII(row.ChildID), via)
		switch {
		case row.OrderInvalid != nil:
			fmt.Fprintf(&out, " order_invalid=%d", *row.OrderInvalid)
		case row.Order != nil:
			fmt.Fprintf(&out, " order=%s", strconv.QuoteToASCII(*row.Order))
		default:
			out.WriteString(" order=unset")
		}
		fmt.Fprintf(&out, " status=%s reason=%s\n", row.Status, row.Reason)
	}
	return []byte(out.String())
}
