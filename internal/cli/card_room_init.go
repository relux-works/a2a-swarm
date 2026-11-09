package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

func cardRoomInitForbidden(err error) error {
	return fmt.Errorf("card_room_init_forbidden: %w", err)
}

// cmdCardRoomInit changes only the card event power in an existing room. The
// strict original-endpoint snapshot preserves unknown fields and exact numbers;
// the homeserver remains the final authorization authority at write time.
func cmdCardRoomInit(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a card room init [--room R]"
	if len(args) == 0 || args[0] != "init" {
		return usagef(use)
	}
	fs := newFlagSet("card room init")
	room := fs.String("room", "", "existing room (default configured home card_room)")
	pos, err := parseInterspersed(fs, args[1:])
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		return usagef(use)
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	var id, label string
	if *room == "" {
		id, label, err = s.resolveCardRoom(ctx, s.server)
	} else {
		id, label, err = s.resolveRoom(ctx, *room)
	}
	if err != nil {
		return cardRoomInitForbidden(err)
	}
	events, err := s.client.GetRoomPolicyState(ctx, id)
	if err != nil {
		return cardRoomInitForbidden(err)
	}
	var power, create map[string]any
	creator := ""
	joined := false
	seen := map[string]bool{}
	for _, event := range events {
		key := event.Type + "|" + event.StateKey
		if seen[key] {
			return cardRoomInitForbidden(errors.New("duplicate room state"))
		}
		seen[key] = true
		switch {
		case event.Type == "m.room.power_levels" && event.StateKey == "":
			power = event.Content
		case event.Type == "m.room.create" && event.StateKey == "":
			create = event.Content
			creator = event.Sender
		case event.Type == "m.room.member" && event.StateKey == s.creds.UserID:
			joined = event.Content["membership"] == "join"
		}
	}
	if !joined || power == nil || create == nil {
		return cardRoomInitForbidden(errors.New("joined membership, creation state and current power levels required"))
	}
	have, need, old, already, err := cardRoomInitPower(power, create, creator, s.creds.UserID)
	if err != nil {
		return cardRoomInitForbidden(err)
	}
	if have < need {
		return cardRoomInitForbidden(errors.New("caller cannot change m.room.power_levels"))
	}
	if already {
		a.printf("card room %s already initialized\n", sanitizeLine(label))
		return nil
	}
	if have < old || have < 0 {
		return cardRoomInitForbidden(errors.New("caller cannot lower the existing card event level"))
	}
	levels, _ := power["events"].(map[string]any)
	if levels == nil {
		levels = map[string]any{}
		power["events"] = levels
	}
	levels[CardEventType] = json.Number("0")
	if _, err := s.client.PutState(ctx, id, "m.room.power_levels", "", power); err != nil {
		return cardRoomInitForbidden(err)
	}
	a.printf("initialized card room %s %s\n", sanitizeLine(label), id)
	return nil
}

// Power values are parsed as integers without float round trips. Unknown room
// versions and malformed policy refuse. Room v12 creators have implicit power;
// earlier versions use the explicit users/users_default values in power_levels.
func cardRoomInitPower(power, create map[string]any, creator, user string) (have, need, old int64, already bool, err error) {
	bad := errors.New("unsupported or malformed card-room power policy")
	version := "1"
	if value, present := create["room_version"]; present {
		var ok bool
		version, ok = value.(string)
		if !ok {
			err = bad
			return
		}
	}
	n, e := strconv.Atoi(version)
	if e != nil || n < 1 || n > 12 || strconv.Itoa(n) != version {
		err = bad
		return
	}
	number := func(value any) (int64, bool) {
		switch v := value.(type) {
		case json.Number:
			value, e := v.Int64()
			return value, e == nil
		case string:
			// Old room versions admitted string-encoded power levels.
			if n < 10 {
				value, e := strconv.ParseInt(v, 10, 64)
				return value, e == nil
			}
		}
		return 0, false
	}
	field := func(object map[string]any, key string, fallback int64) (int64, bool) {
		if value, ok := object[key]; ok {
			return number(value)
		}
		return fallback, true
	}
	var ok bool
	have, ok = field(power, "users_default", 0)
	if !ok {
		err = bad
		return
	}
	if value, present := power["users"]; present {
		users, valid := value.(map[string]any)
		if !valid {
			err = bad
			return
		}
		have, ok = field(users, user, have)
		if !ok {
			err = bad
			return
		}
	}
	state, ok := field(power, "state_default", 50)
	if !ok {
		err = bad
		return
	}
	need, old = state, state
	if value, present := power["events"]; present {
		levels, valid := value.(map[string]any)
		if !valid {
			err = bad
			return
		}
		need, ok = field(levels, "m.room.power_levels", state)
		if !ok {
			err = bad
			return
		}
		old, ok = field(levels, CardEventType, state)
		if !ok {
			err = bad
			return
		}
		_, present := levels[CardEventType]
		already = present && old == 0
	}
	if n == 12 {
		// An unknown creator identity cannot establish v12's implicit authority.
		if creator == "" {
			err = bad
			return
		}
		implicit := creator == user
		if value, present := create["additional_creators"]; present {
			creators, valid := value.([]any)
			if !valid {
				err = bad
				return
			}
			for _, entry := range creators {
				extra, valid := entry.(string)
				if !valid {
					err = bad
					return
				}
				implicit = implicit || extra == user
			}
		}
		if implicit {
			have = int64(^uint64(0) >> 1)
		}
	}
	return
}
