package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Owner evidence is stdin-only and exact: legacy hooks without an event name
// may still deliver, but never establish or refresh monitor ownership.
func monitorHookInput(in hookInput, ev hookEvent) bool {
	if !claudeSessionRE.MatchString(in.SessionID) || in.HookEventName != ev.name {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(in.Payload))
	if tok, err := d.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		tok, err := d.Token()
		key, ok := tok.(string)
		if err != nil || !ok {
			return false
		}
		for _, field := range []string{"session_id", "hook_event_name", "stop_reason"} {
			// EqualFold matches encoding/json, including Unicode aliases such
			// as long s. Only the exact ASCII evidence spelling is admitted.
			if strings.EqualFold(key, field) {
				if key != field || seen[field] {
					return false
				}
				seen[field] = true
			}
		}
		var raw json.RawMessage
		if d.Decode(&raw) != nil {
			return false
		}
	}
	return seen["session_id"] && seen["hook_event_name"]
}

func (a *App) recordHookOwner(ctx context.Context, rt hookRuntime, ev hookEvent, in hookInput) error {
	if a.as == "" || a.human || !monitorHookInput(in, ev) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name, err := a.identity()
	if err != nil {
		return err
	}
	p, err := a.pathsFor(name)
	if err != nil {
		return err
	}
	deadline, _ := ctx.Deadline()
	if a.daemonEngine {
		err = config.RecordMonitorOwnerHookContext(ctx, p, name, deadline, rt.name, in.SessionID, ev.name, ev.arg == "stop" && in.StopReason == "exit", a.Now())
	} else {
		err = config.RecordMonitorOwnerHook(p, name, deadline, rt.name, in.SessionID, ev.name, ev.arg == "stop" && in.StopReason == "exit", a.Now())
	}
	// A non-owner hook must not invent an owner. Existing hook delivery retains
	// its own lease guard and legacy behavior independently of this association.
	// Wrapped malformed owner State also falls through this shim for non-Start
	// hooks. Legacy delivery still performs its own State/lease checks; this
	// grants no monitor authority and does not repair malformed State.
	if errors.Is(err, config.ErrMonitorOwner) && ev.arg != "session-start" {
		return nil
	}
	return err
}
