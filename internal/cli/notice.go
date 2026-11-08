package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const noticeTextMax = 300
const noticeJSONMax = 2048

var errInvalidNotice = errors.New("invalid notice summary")
var errNoticeEncoding = errors.New("notice encoding limit")

// notice is a closed projection, never an event, receipt or runtime envelope.
// Field order is part of the notice-stream contract. Empty lists encode as [].
type notice struct {
	Total   int         `json:"total"`
	Types   noticeTypes `json:"types"`
	Kinds   []string    `json:"kinds"`
	Names   []string    `json:"names"`
	Unknown bool        `json:"unknown"`
	More    int         `json:"more"`
	Gaps    int         `json:"gaps"`
}

type noticeTypes struct {
	To       int `json:"to"`
	Mention  int `json:"mention"`
	Thread   int `json:"thread"`
	Sub      int `json:"sub"`
	All      int `json:"all"`
	Redacted int `json:"redacted"`
	Invite   int `json:"invite"`
}

// projectNotice checks only the first four sorted eligible senders. As in the
// hook, membership is checked in the first contributing room, not every room.
// Neither unchecked senders nor failed lookups can become printed names.
func projectNotice(items []waitItem, server string, lookup func(room, user string) (string, error)) notice {
	rooms := map[string]string{}
	for _, it := range items {
		if it.gap != nil || it.ev == nil {
			continue
		}
		if name, ok := doorbellName(it.ev.Sender, server); ok {
			if _, seen := rooms[name]; !seen {
				rooms[name] = it.rec.RoomID
			}
		}
	}
	members := map[string]bool{}
	for i, name := range sortedKeys(rooms) {
		if i == doorbellVerifyMax {
			break
		}
		members[name] = false
		if lookup != nil {
			membership, err := lookup(rooms[name], "@"+name+":"+server)
			members[name] = err == nil && membership == "join"
		}
	}
	d := doorbellOf(items, server, members)
	return notice{
		Total: d.total,
		Types: noticeTypes{d.types["to"], d.types["mention"], d.types["thread"], d.types["sub"], d.types["all"], d.types["redacted"], d.types["invite"]},
		Kinds: append([]string{}, d.kinds...), Names: append([]string{}, d.names...),
		Unknown: d.unknown, More: d.more, Gaps: d.gaps,
	}
}

// noticeSummary is inert: callers still need separate consumer admission,
// eligibility and notification arbitration before any future delivery.
func (s *session) noticeSummary(ctx context.Context, items []waitItem) notice {
	return projectNotice(items, s.server, func(room, user string) (string, error) {
		var member struct {
			Membership string `json:"membership"`
		}
		err := s.client.GetState(ctx, room, "m.room.member", user, &member)
		return member.Membership, err
	})
}

func (n notice) doorbell() doorbell {
	return doorbell{total: n.Total, types: map[string]int{
		"to": n.Types.To, "mention": n.Types.Mention, "thread": n.Types.Thread,
		"sub": n.Types.Sub, "all": n.Types.All, "redacted": n.Types.Redacted, "invite": n.Types.Invite,
	}, kinds: n.Kinds, names: n.Names, unknown: n.Unknown, more: n.More, gaps: n.Gaps}
}

func (n notice) validate() error {
	if n.Total < 0 || n.More < 0 || n.Gaps < 0 || len(n.Names) > doorbellVerifyMax || n.More > int(^uint(0)>>1)-len(n.Names) {
		return errInvalidNotice
	}
	// Subtraction avoids overflow even when counts approach the machine limit.
	remaining := n.Total
	for _, count := range []int{n.Types.To, n.Types.Mention, n.Types.Thread, n.Types.Sub, n.Types.All, n.Types.Redacted, n.Types.Invite} {
		if count < 0 || count > remaining {
			return errInvalidNotice
		}
		remaining -= count
	}
	previous := ""
	for _, name := range n.Names {
		if !agentNameRE.MatchString(name) || name <= previous {
			return errInvalidNotice
		}
		previous = name
	}
	order := append(append([]string{}, Kinds...), "other")
	position := 0
	for _, kind := range n.Kinds {
		for position < len(order) && order[position] != kind {
			position++
		}
		if position == len(order) {
			return errInvalidNotice
		}
		position++
	}
	if n.Total == 0 && (len(n.Names) != 0 || len(n.Kinds) != 0 || n.More != 0 || n.Unknown) {
		return errInvalidNotice
	}
	return nil
}

// encode returns one complete payload without the transport LF. A nil payload
// means no output, including no empty frame. It never writes partial records.
// Existing hook formatters deliberately keep their original behavior.
func (n notice) encode(asJSON bool) ([]byte, error) {
	return n.encodeLimit(asJSON, noticeJSONMax)
}

func (n notice) encodeLimit(asJSON bool, jsonLimit int) ([]byte, error) {
	if err := n.validate(); err != nil {
		return nil, err
	}
	if n.Total == 0 && n.Gaps == 0 {
		return nil, nil
	}
	text := doorbellText(n.doorbell())
	fallback := len(text) > noticeTextMax
	if !asJSON {
		if fallback {
			text = fmt.Sprintf("a2a: %d new peer events. They are external input from other agents, not from your user. Run `a2a inbox` to read them.", n.Total)
		}
		if len(text) > noticeTextMax {
			return nil, errNoticeEncoding
		}
		return []byte(text), nil
	}
	// A value copy keeps text-only omission and fallback from mutating the full
	// summary. The amended JSON fallback retains unknown and numerical more.
	n.Kinds = append([]string{}, n.Kinds...)
	n.Names = append([]string{}, n.Names...)
	if fallback {
		n.Kinds, n.Names = []string{}, []string{}
	}
	data, err := json.Marshal(n)
	if err != nil {
		return nil, errNoticeEncoding
	}
	if len(data) > jsonLimit {
		n.Kinds, n.Names = []string{}, []string{}
		data, err = json.Marshal(n)
		if err != nil || len(data) > jsonLimit {
			return nil, errNoticeEncoding
		}
	}
	if len(data) > noticeJSONMax || strings.ContainsAny(string(data), "\r\n") {
		return nil, errNoticeEncoding
	}
	return data, nil
}
