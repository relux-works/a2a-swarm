package cli

import (
	"encoding/json"
	"errors"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/courier"
)

// Resolve only invocation-owned evidence. Absence preserves local PID fallback
// in the engine; a PID holder is never synthesized on the station.
func stationSession(plan courier.Plan, input []byte, getenv func(string) string) (string, error) {
	if plan.Command == "hook" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(input, &fields) != nil || fields["session_id"] == nil {
			return "", nil // existing hook input validation remains engine-owned
		}
		var id string
		if json.Unmarshal(fields["session_id"], &id) != nil || !courier.ValidSession(id) || hookSessionKey(id) != id {
			return "", courier.NotAllowed
		}
		return id, nil
	}
	if id := getenv(EnvSessionID); id != "" {
		if !courier.ValidSession(id) || hookSessionKey(id) != id {
			return "", courier.NotAllowed
		}
		return id, nil
	}
	if plan.LongConsumer {
		return "", nil
	}
	if id := getenv("CODEX_THREAD_ID"); claudeSessionRE.MatchString(id) && courier.ValidSession(id) {
		return id, nil
	}
	if getenv("CODEX_THREAD_ID") == "" {
		if id := getenv("CLAUDE_CODE_SESSION_ID"); claudeSessionRE.MatchString(id) && courier.ValidSession(id) {
			return id, nil
		}
	}
	return "", nil
}

func (a *App) noteCourierError(err error) {
	if a.courierEngine && errors.Is(err, config.ErrMonitorConflict) {
		a.courierLeaseConflict = true
	}
}
