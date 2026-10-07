package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func statusArgs(state string) []string {
	return []string{"status", "set", state, "--heartbeat", "1m", "--runtime", "none", "--hooks", "none", "--doorbell", "armed", "--product", "swarm"}
}

// Proves creation enables a power-zero member's own status and the server
// refuses a foreign @-key without changing that member's record. The isolated
// fake uses the previously live-verified Matrix authorization rule.
func TestLivenessPowerZeroAndForeignKey(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("bob", "join", "status").ok(t)
	e.as("bob", statusArgs("working")...).ok(t)
	s, err := eApp(e).sessionAs("bob")
	if err != nil {
		t.Fatal(err)
	}
	room, _, err := s.resolveRoom(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.client.PutState(context.Background(), room, StatusEventType, "@alice:test.local", config.AgentStatus{Name: "alice", State: "stopping"})
	if err == nil {
		t.Fatal("server accepted foreign status key")
	}
	var own config.AgentStatus
	if err := s.client.GetState(context.Background(), room, StatusEventType, "@bob:test.local", &own); err != nil || own.State != "working" {
		t.Fatalf("own status changed: %+v %v", own, err)
	}
	result := e.as("alice", "who", "--stale-after", "3m", "--product", "swarm").ok(t)
	tuitestkit.ContainsStr(t, result.stdout, "bob")
	tuitestkit.ContainsStr(t, result.stdout, "hooks=none")
}

// Proves missing and nonsensical explicit settings are rejected before any
// Matrix write, while a nearby valid record is accepted in the control above.
func TestLivenessSettingsRejectWithoutWrites(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "set", "working").failsWith(t, ExitUsage, "heartbeat")
	e.as("alice", "status", "set", "working", "--heartbeat", "0").failsWith(t, ExitUsage, "heartbeat")
	args := statusArgs("working")
	args[4] = "-1s"
	e.as("alice", args...).failsWith(t, ExitUsage, "heartbeat")
	args = statusArgs("working")
	args[6] = "unknown"
	e.as("alice", args...).failsWith(t, ExitUsage, "runtime")
	e.as("alice", "who").failsWith(t, ExitUsage, "stale-after")
	tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
}

// Proves invalid heartbeat/staleness combinations fail with the agreed code
// 64, and boundary/future/stopping records are visible rather than fresh.
func TestLivenessFreshnessBoundaries(t *testing.T) {
	e, _ := world(t)
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("idle")...).ok(t)
	e.as("alice", "who", "--stale-after", "1m").failsWith(t, 64, "alice")
	now = now.Add(3 * time.Minute)
	r := e.as("alice", "who", "--stale-after", "3m").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "stale")
	e.as("alice", statusArgs("stopping")...).ok(t)
	r = e.as("alice", "who", "--stale-after", "3m").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "down")
	// A planted future heartbeat must not be displayed as fresh.
	e.hs.mu.Lock()
	room := e.hs.rooms[e.hs.aliases["#status:test.local"]]
	key := StatusEventType + "|@alice:test.local"
	var record config.AgentStatus
	json.Unmarshal(room.state[key], &record)
	record.State = "working"
	record.HeartbeatAt = now.Add(time.Hour)
	room.state[key], _ = json.Marshal(record)
	e.hs.mu.Unlock()
	r = e.as("alice", "who", "--stale-after", "3m").ok(t)
	if !strings.Contains(r.stdout, "clock") {
		t.Fatalf("future heartbeat looked fresh: %s", r.stdout)
	}
}

func eApp(e *env) *App {
	return &App{Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, HTTP: e.hs.srv.Client(), Now: time.Now}
}
