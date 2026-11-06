package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// privateRoomPolicy shares ownerAudience's one-snapshot, duplicate-state,
// invite-rule and no-encryption semantics, without its two-person identity pin.
// Workspace membership is caller-selected; owner DM admission is unchanged.
func (s *session) privateRoomPolicy(ctx context.Context, room string) error {
	events, err := s.client.GetRoomPolicyState(ctx, room)
	if err != nil {
		return fmt.Errorf("private policy unavailable: %w", err)
	}
	seen := map[string]map[string]bool{}
	policy := map[string]map[string]any{}
	for _, event := range events {
		if seen[event.Type] == nil {
			seen[event.Type] = map[string]bool{}
		}
		if seen[event.Type][event.StateKey] {
			return errors.New("private policy: duplicate state")
		}
		seen[event.Type][event.StateKey] = true
		if event.Type == "m.room.encryption" {
			return errors.New("private policy: unexpected encryption")
		}
		if event.StateKey == "" {
			policy[event.Type] = event.Content
		}
	}
	for _, want := range []struct{ typ, field, value string }{
		{"m.room.join_rules", "join_rule", "invite"},
		{"m.room.history_visibility", "history_visibility", "joined"},
		{"m.room.guest_access", "guest_access", "forbidden"},
	} {
		if policy[want.typ][want.field] != want.value {
			return fmt.Errorf("private policy: %s must be %s", want.field, want.value)
		}
	}
	return privateRoomPower(policy["m.room.power_levels"])
}

func privateRoomPower(power map[string]any) error {
	bad := errors.New("private policy: unsafe or malformed power levels")
	if power == nil {
		return bad
	}
	// Matrix's preset defaults, plus the existing room-new redact override.
	for field, expected := range map[string]int64{"users_default": 0, "events_default": 0, "state_default": 50, "invite": 0, "redact": 100, "ban": 50, "kick": 50} {
		value, exists := power[field]
		if !exists {
			if field == "redact" { // Explicitly requested; its default is only 50.
				return bad
			}
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return bad
		}
		level, err := number.Int64()
		if err != nil || level != expected {
			return bad
		}
	}
	if value, exists := power["users"]; exists {
		users, ok := value.(map[string]any)
		if !ok || len(users) != 0 { // No grants, including a listed v12 creator.
			return bad
		}
	}
	if value, exists := power["events"]; exists {
		events, ok := value.(map[string]any)
		if !ok {
			return bad
		}
		for event, value := range events {
			number, ok := value.(json.Number)
			if !ok {
				return bad
			}
			level, err := number.Int64()
			if err != nil || level < 0 {
				return bad
			}
			// A preset must not lower control-state writes below state_default.
			switch event {
			case "m.room.power_levels", "m.room.join_rules", "m.room.history_visibility", "m.room.guest_access", "m.room.encryption", "m.room.server_acl", "m.room.tombstone":
				if level < 50 {
					return bad
				}
			}
		}
	}
	return nil
}
