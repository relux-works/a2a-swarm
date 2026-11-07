package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
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

// Proves the status-room override preserves each protected Matrix state
// event at power 100 alongside the power-zero self-key telemetry event.
func TestLivenessProtectedRoomPolicy(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	s, err := eApp(e).sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	room, _, err := s.resolveRoom(context.Background(), "status")
	if err != nil {
		t.Fatal(err)
	}
	var pl struct {
		Events map[string]int `json:"events"`
	}
	if err := s.client.GetState(context.Background(), room, "m.room.power_levels", "", &pl); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"m.room.power_levels", "m.room.tombstone", "m.room.server_acl", "m.room.encryption", "m.room.history_visibility"} {
		if pl.Events[key] != 100 {
			t.Fatalf("protected event %s demoted: %v", key, pl.Events)
		}
	}
	if pl.Events[StatusEventType] != 0 {
		t.Fatal("self-key positive control unavailable")
	}
}

// Proves hostile sender/key pairs, mismatched names and non-user state keys
// cannot create or refresh a row; actual membership remains visible as down.
func TestLivenessReaderRejectsForgedState(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	e.as("bob", "join", "status").ok(t)
	e.as("carol", "join", "status").ok(t)
	e.hs.mu.Lock()
	room := e.hs.rooms[e.hs.aliases["#status:test.local"]]
	var record config.AgentStatus
	_ = json.Unmarshal(room.state[StatusEventType+"|@alice:test.local"], &record)
	for _, p := range []struct{ key, sender, name string }{{"@bob:test.local", "@alice:test.local", "bob"}, {"@carol:test.local", "@carol:test.local", "alice"}, {"alien", "@alien:test.local", "alien"}, {"@forged:test.local", "@alice:test.local", "forged"}} {
		record.Name = p.name
		room.state[StatusEventType+"|"+p.key], _ = json.Marshal(record)
		room.stateSenders[StatusEventType+"|"+p.key] = p.sender
	}
	e.hs.mu.Unlock()
	r := e.as("alice", "who", "--stale-after", "3m").ok(t)
	if !strings.Contains(r.stdout, "alice\tworking\tfresh") || !strings.Contains(r.stdout, "bob\tunknown\tunknown") || !strings.Contains(r.stdout, "carol\tunknown\tunknown") || strings.Contains(r.stdout, "alien") || strings.Contains(r.stdout, "forged") {
		t.Fatalf("forged telemetry admitted: %s", r.stdout)
	}
}

// Proves power-zero members cannot overwrite protected room policy; an
// existing demoted policy is refused without attempting an automatic repair.
func TestLivenessProtectedPolicyRejectsMutation(t *testing.T) {
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
	_, err = s.client.PutState(context.Background(), room, "m.room.power_levels", "", map[string]any{"events_default": 0})
	if !matrix.IsCode(err, "M_FORBIDDEN") {
		t.Fatalf("protected write not rejected: %v", err)
	}
	var levels struct {
		Events map[string]int `json:"events"`
	}
	if err := s.client.GetState(context.Background(), room, "m.room.power_levels", "", &levels); err != nil || levels.Events["m.room.power_levels"] != 100 {
		t.Fatal("rejected write changed room policy")
	}
	e.hs.mu.Lock()
	e.hs.rooms[room].state["m.room.power_levels|"] = json.RawMessage(`{"state_default":50,"events":{"ai.relux.a2a.status":0}}`)
	e.hs.mu.Unlock()
	before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
	e.as("alice", "status", "init").failsWith(t, ExitError, "protected event m.room.power_levels")
	if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != before {
		t.Fatal("existing policy was silently rewritten")
	}
}

type livenessDeadlineTransport struct {
	seen bool
	t    *testing.T
}

func (d *livenessDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/state/"+StatusEventType) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > nativeRPCTimeout {
			d.t.Error("locked network write lacks the control deadline")
		}
		d.seen = true
	}
	return http.DefaultTransport.RoundTrip(r)
}

// Proves the actual status PUT carries the bounded control deadline and a
// failed publication does not commit a replacement local status profile.
func TestLivenessLockedWriteDeadlineAndFailureState(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	prior := e.state("alice").Liveness.State
	probe := &livenessDeadlineTransport{t: t}
	e.configure = func(a *App) { a.HTTP = &http.Client{Transport: probe} }
	e.hs.fail("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}", 500, "M_UNKNOWN", "status unavailable")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	e.runCtx(ctx, append([]string{"--as", "alice"}, statusArgs("blocked")...)...).failsWith(t, ExitError, "HTTP 500")
	if !probe.seen || e.state("alice").Liveness.State != prior {
		t.Fatal("deadline probe not applied or failed PUT mutated local state")
	}
}
