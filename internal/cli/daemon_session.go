package cli

import (
	"encoding/json"
	"errors"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/daemon"
)

// Resolve only invocation-owned evidence. Absence preserves local PID fallback
// in the engine; a PID holder is never synthesized on the station.
func stationSession(plan daemon.Plan, input []byte, getenv func(string) string) (string, error) {
	if plan.Command == "hook" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(input, &fields) != nil || fields["session_id"] == nil {
			return "", nil // existing hook input validation remains engine-owned
		}
		var id string
		if json.Unmarshal(fields["session_id"], &id) != nil || !daemon.ValidSession(id) || hookSessionKey(id) != id {
			return "", daemon.NotAllowed
		}
		return id, nil
	}
	if id := getenv(EnvSessionID); id != "" {
		if !daemon.ValidSession(id) || hookSessionKey(id) != id {
			return "", daemon.NotAllowed
		}
		return id, nil
	}
	if plan.LongConsumer {
		return "", nil
	}
	if id := getenv("CODEX_THREAD_ID"); claudeSessionRE.MatchString(id) && daemon.ValidSession(id) {
		return id, nil
	}
	if getenv("CODEX_THREAD_ID") == "" {
		if id := getenv("CLAUDE_CODE_SESSION_ID"); claudeSessionRE.MatchString(id) && daemon.ValidSession(id) {
			return id, nil
		}
	}
	return "", nil
}

func (a *App) noteDaemonError(err error) {
	if a.daemonEngine && errors.Is(err, config.ErrMonitorConflict) {
		a.daemonLeaseConflict = true
	}
}
