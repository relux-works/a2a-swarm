package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

var errRoomPowerPolicy = errors.New("room power: malformed or unsupported policy; no rows printed")

type roomPowerCreator struct {
	MXID   string `json:"mxid"`
	Origin string `json:"origin"`
}

type roomPowerAuthority struct {
	Kind  string `json:"kind"`
	Value *int64 `json:"value"`
}

type roomPowerUser struct {
	MXID          string             `json:"mxid"`
	Membership    *string            `json:"membership"`
	ExplicitLevel *int64             `json:"explicit_level"`
	Effective     roomPowerAuthority `json:"effective"`
	Origin        string             `json:"origin"`
}

type roomPowerView struct {
	Type               string             `json:"type"`
	V                  int                `json:"v"`
	ServerName         string             `json:"server_name"`
	RoomID             string             `json:"room_id"`
	Caller             string             `json:"caller"`
	RoomVersion        string             `json:"room_version"`
	Completeness       string             `json:"completeness"`
	Observation        string             `json:"observation"`
	Creators           []roomPowerCreator `json:"creators"`
	PowerLevelsPresent bool               `json:"power_levels_present"`
	Defaults           map[string]int64   `json:"defaults"`
	Required           map[string]int64   `json:"required"`
	Users              []roomPowerUser    `json:"users"`
}

const roomPowerUsage = "usage: a2a room power <room> [--json]"

func cmdRoomPower(ctx context.Context, a *App, args []string) error {
	fs := newFlagSet("room power")
	asJSON := fs.Bool("json", false, "print versioned current power and creator facts")
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("%s", roomPowerUsage)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	id, _, err := s.resolveRoom(ctx, pos[0])
	if err != nil {
		return fmt.Errorf("room power: resolve room: %w", err)
	}
	events, err := s.client.GetRoomPolicyState(ctx, id)
	if err != nil {
		if matrix.IsStatus(err, 403) {
			return errors.New("room power: access denied; no rows printed")
		}
		if errors.Is(err, matrix.ErrRoomPolicyIncomplete) || ctx.Err() != nil {
			return errors.New("room power: incomplete observation; no rows printed")
		}
		return errRoomPowerPolicy
	}
	view, err := projectRoomPower(events, s.server, id, s.creds.UserID)
	if err != nil {
		return err
	}
	if err = s.checkConnection(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return errors.New("room power: incomplete observation; no rows printed")
	}
	// Construct every row before writing stdout: policy failures expose no partial roster.
	if *asJSON {
		return json.NewEncoder(a.Stdout).Encode(view)
	}
	var out strings.Builder
	fmt.Fprintf(&out, "room-power\t%s\tv1\troom-version=%s\tcomplete-response\tpower-levels-present=%t\n", strconv.QuoteToASCII(id), view.RoomVersion, view.PowerLevelsPresent)
	for _, c := range view.Creators {
		fmt.Fprintf(&out, "creator\t%s\t%s\n", strconv.QuoteToASCII(c.MXID), c.Origin)
	}
	for _, key := range []string{"users_default", "events_default", "state_default"} {
		fmt.Fprintf(&out, "default\t%s\t%d\n", key, view.Defaults[key])
	}
	for _, key := range []string{"ban", "invite", "kick", "redact", "power_levels", "notifications_room"} {
		fmt.Fprintf(&out, "required\t%s\t%d\n", key, view.Required[key])
	}
	for _, u := range view.Users {
		membership, explicit, effective := "unknown", "absent", "creator-infinite"
		if u.Membership != nil {
			membership = *u.Membership
		}
		if u.ExplicitLevel != nil {
			explicit = strconv.FormatInt(*u.ExplicitLevel, 10)
		}
		if u.Effective.Value != nil {
			effective = strconv.FormatInt(*u.Effective.Value, 10)
		}
		fmt.Fprintf(&out, "user\t%s\tmembership=%s\texplicit=%s\teffective=%s\torigin=%s\n", strconv.QuoteToASCII(u.MXID), membership, explicit, effective, u.Origin)
	}
	_, err = fmt.Fprint(a.Stdout, out.String())
	return err
}

// projectRoomPower interprets a complete current-state observation. It does not
// attest to server honesty, exclusivity over unlisted users, or future writes.
func projectRoomPower(events []matrix.RoomPolicyEvent, server, room, caller string) (*roomPowerView, error) {
	bad := errRoomPowerPolicy
	if !matrix.ValidVerificationID(caller, '@') || !matrix.ValidVerificationID(room, '!') {
		return nil, bad
	}
	seen := map[[2]string]bool{}
	var create, power map[string]any
	sender := ""
	members := map[string]string{}
	for _, event := range events {
		key := [2]string{event.Type, event.StateKey}
		if seen[key] {
			return nil, bad
		}
		seen[key] = true
		switch event.Type {
		case "m.room.create":
			if event.StateKey != "" || event.Content == nil {
				return nil, bad
			}
			create = event.Content
			sender = event.Sender
		case "m.room.power_levels":
			if event.StateKey != "" || event.Content == nil {
				return nil, bad
			}
			power = event.Content
		case "m.room.member":
			if !matrix.ValidVerificationID(event.StateKey, '@') || !powerExactFields(event.Content, "membership") {
				return nil, bad
			}
			m, ok := event.Content["membership"].(string)
			if !ok {
				return nil, bad
			}
			switch m {
			case "join", "invite", "leave", "ban", "knock":
			default:
				return nil, bad
			}
			members[event.StateKey] = m
		}
	}
	if create == nil || members[caller] != "join" || !powerExactFields(create, "creator", "room_version", "additional_creators") {
		return nil, bad
	}
	version := "1"
	if v, ok := create["room_version"]; ok {
		var valid bool
		version, valid = v.(string)
		if !valid {
			return nil, bad
		}
	}
	n, err := strconv.Atoi(version)
	if err != nil || n < 1 || n > 12 || strconv.Itoa(n) != version {
		return nil, bad
	}
	creator, origin := sender, "create-sender"
	if n <= 10 {
		var ok bool
		creator, ok = create["creator"].(string)
		if !ok {
			return nil, bad
		}
		origin = "legacy-create-content"
	}
	if !matrix.ValidVerificationID(creator, '@') {
		return nil, bad
	}
	creators := []roomPowerCreator{{creator, origin}}
	creatorSet := map[string]bool{creator: true}
	if n == 12 {
		if raw, present := create["additional_creators"]; present {
			list, ok := raw.([]any)
			if !ok {
				return nil, bad
			}
			for _, v := range list {
				id, ok := v.(string)
				if !ok || !matrix.ValidVerificationID(id, '@') || creatorSet[id] {
					return nil, bad
				}
				creatorSet[id] = true
				creators = append(creators, roomPowerCreator{id, "additional-creator"})
			}
		}
	}
	sort.Slice(creators, func(i, j int) bool { return creators[i].MXID < creators[j].MXID })
	number := func(v any) (int64, bool) {
		switch v := v.(type) {
		case json.Number:
			i, e := v.Int64()
			return i, e == nil
		case string:
			if n < 10 {
				i, e := strconv.ParseInt(v, 10, 64)
				return i, e == nil
			}
		}
		return 0, false
	}
	field := func(object map[string]any, key string, fallback int64) (int64, bool) {
		if v, ok := object[key]; ok {
			return number(v)
		}
		return fallback, true
	}
	levels := map[string]int64{}
	defaults := map[string]int64{"users_default": 0, "events_default": 0, "state_default": 50}
	required := map[string]int64{"ban": 50, "invite": 0, "kick": 50, "redact": 50, "notifications_room": 50}
	if power != nil {
		if !powerExactFields(power, "users", "users_default", "events", "events_default", "state_default", "ban", "invite", "kick", "redact", "notifications") {
			return nil, bad
		}
		for k, fallback := range defaults {
			v, ok := field(power, k, fallback)
			if !ok {
				return nil, bad
			}
			defaults[k] = v
		}
		for _, k := range []string{"ban", "invite", "kick", "redact"} {
			v, ok := field(power, k, required[k])
			if !ok {
				return nil, bad
			}
			required[k] = v
		}
		if raw, present := power["users"]; present {
			users, ok := raw.(map[string]any)
			if !ok {
				return nil, bad
			}
			for id, raw := range users {
				v, ok := number(raw)
				if !ok || !matrix.ValidVerificationID(id, '@') || (n == 12 && creatorSet[id]) {
					return nil, bad
				}
				levels[id] = v
			}
		}
		if raw, present := power["events"]; present {
			events, ok := raw.(map[string]any)
			if !ok {
				return nil, bad
			}
			for name, raw := range events {
				if name == "" {
					return nil, bad
				}
				if _, ok := number(raw); !ok {
					return nil, bad
				}
			}
			v, ok := field(events, "m.room.power_levels", defaults["state_default"])
			if !ok {
				return nil, bad
			}
			required["power_levels"] = v
		}
		if raw, present := power["notifications"]; present {
			notifications, ok := raw.(map[string]any)
			if !ok || !powerExactFields(notifications, "room") {
				return nil, bad
			}
			for _, v := range notifications {
				if _, ok := number(v); !ok {
					return nil, bad
				}
			}
			v, ok := field(notifications, "room", 50)
			if !ok {
				return nil, bad
			}
			required["notifications_room"] = v
		}
	}
	if _, ok := required["power_levels"]; !ok {
		required["power_levels"] = defaults["state_default"]
	}
	union := map[string]bool{caller: true}
	for id, m := range members {
		if m == "join" || m == "invite" {
			union[id] = true
		}
	}
	for id := range levels {
		union[id] = true
	}
	for id := range creatorSet {
		union[id] = true
	}
	ids := make([]string, 0, len(union))
	for id := range union {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	users := make([]roomPowerUser, 0, len(ids))
	for _, id := range ids {
		u := roomPowerUser{MXID: id, Origin: "users_default"}
		if m, ok := members[id]; ok {
			u.Membership = &m
		}
		value := defaults["users_default"]
		if explicit, ok := levels[id]; ok {
			u.ExplicitLevel = &explicit
			value = explicit
			u.Origin = "explicit-user"
		}
		if power == nil && n < 12 && creatorSet[id] {
			value = 100
			u.Origin = "initial-creator"
		}
		u.Effective = roomPowerAuthority{Kind: "finite", Value: &value}
		if n == 12 && creatorSet[id] {
			u.Effective = roomPowerAuthority{Kind: "creator-infinite"}
			u.Origin = "creator"
		}
		users = append(users, u)
	}
	return &roomPowerView{Type: "room-power", V: 1, ServerName: server, RoomID: room, Caller: caller, RoomVersion: version, Completeness: "complete-response", Observation: "current-state-response", Creators: creators, PowerLevelsPresent: power != nil, Defaults: defaults, Required: required, Users: users}, nil
}

// Unknown extension fields remain opaque; near-miss spellings of interpreted
// fields are rejected rather than silently applying a lower default.
func powerExactFields(object map[string]any, names ...string) bool {
	for key := range object {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return false
			}
		}
	}
	return true
}
