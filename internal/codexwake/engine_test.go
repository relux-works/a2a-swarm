package codexwake

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func validBinding() Binding {
	return Binding{Identity: "agent", ThreadID: "thread", SessionID: "thread", Endpoint: "unix:///tmp/rpc.sock", EndpointID: "socket", ServerVersion: "0.160.1", Generation: "generation", Host: "host", Cwd: "/project", OwnerPID: 123, OwnerStart: "birth", Gate: "ready", MaxPerHour: 2, HeartbeatEvery: time.Minute}
}

// Proves valid admission beside neighboring lifecycle/identity failures.
// These are snapshot gates; the runtime remains responsible for its own
// concurrent turn/approval scheduling after a submitted native request.
func TestAdmissionPositiveAndGates(t *testing.T) {
	b := validBinding()
	remote := Snapshot{ID: "thread", Cwd: "/project", Status: Status{Type: "idle"}}
	if got := Admission(b, "host", true, false, remote); got != "ready" {
		t.Fatal(got)
	}
	for _, gate := range []string{"paused", "budget", "stopping", "unknown", "invalid"} {
		copy := b
		copy.Gate = gate
		if got := Admission(copy, "host", true, false, remote); got == "ready" {
			t.Fatalf("gate %s admitted", gate)
		}
	}
	cases := []struct {
		name          string
		b             Binding
		host          string
		alive, paused bool
		remote        Snapshot
		want          string
	}{
		{"host", b, "replacement", true, false, remote, "unavailable: host"},
		{"owner", b, "host", false, false, remote, "unavailable: launcher"},
		{"hook pause", b, "host", true, true, remote, "wake suppressed: paused"},
		{"thread", b, "host", true, false, Snapshot{ID: "foreign", Cwd: "/project", Status: Status{Type: "idle"}}, "unavailable: thread"},
		{"cwd", b, "host", true, false, Snapshot{ID: "thread", Cwd: "/foreign", Status: Status{Type: "idle"}}, "unavailable: cwd"},
		{"not loaded", b, "host", true, false, Snapshot{ID: "thread", Cwd: "/project", Status: Status{Type: "notLoaded"}}, "unavailable: notLoaded"},
		{"busy", b, "host", true, false, Snapshot{ID: "thread", Cwd: "/project", Status: Status{Type: "active"}}, "wake suppressed: busy"},
		{"approval", b, "host", true, false, Snapshot{ID: "thread", Cwd: "/project", Status: Status{Type: "active", ActiveFlags: []string{"waitingOnApproval"}}}, "wake suppressed: human"},
		{"input", b, "host", true, false, Snapshot{ID: "thread", Cwd: "/project", Status: Status{Type: "active", ActiveFlags: []string{"waitingOnUserInput"}}}, "wake suppressed: human"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Admission(tc.b, tc.host, tc.alive, tc.paused, tc.remote); got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
		})
	}
	invalid := b
	invalid.Generation = ""
	if err := invalid.Validate(); err == nil {
		t.Fatal("missing generation accepted")
	}
	invalid = b
	invalid.MaxPerHour = 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("implicit rate accepted")
	}
	invalid = b
	invalid.SessionID = "other"
	if err := invalid.Validate(); err == nil {
		t.Fatal("foreign session accepted")
	}
}

// Proves only new mail admits an explicitly capped attempt, retries cannot
// replace a durable uncertain attempt, and exact native history can reconcile
// it without any mailbox receipt. Attempts reserve rate before the RPC.
func TestJournalRateCoalescingAndAmbiguity(t *testing.T) {
	b := validBinding()
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	var ledger Ledger
	if _, err := ledger.Begin(b, nil, "host notice", now); err != nil || ledger.Attempt != nil {
		t.Fatal("empty mail started a turn")
	}
	attempt, err := ledger.Begin(b, []string{"$one", "$two"}, "metadata only", now)
	if err != nil || attempt == nil || !strings.Contains(attempt.Output, "metadata only") {
		t.Fatalf("begin: %+v %v", attempt, err)
	}
	if _, err := ledger.Begin(b, []string{"$one"}, "again", now); err == nil || ledger.Attempt.ID != attempt.ID {
		t.Fatal("uncertain attempt replaced")
	}
	wrong := b
	wrong.Generation = "replacement"
	if err := ledger.Accept(wrong, "turn"); err == nil {
		t.Fatal("replacement generation accepted old attempt")
	}
	output, _ := json.Marshal(attempt.Output)
	forged := Snapshot{Turns: []Turn{{ID: "turn", Items: []Item{{Type: "agentMessage", Name: ToolName, Namespace: ToolNamespace, Output: output}}}}}
	if _, ok := Accepted(forged, attempt.Output); ok {
		t.Fatal("assistant text reconciled tool authority")
	}
	forged.Turns[0].Items[0].Type = "functionCallOutput"
	if turn, ok := Accepted(forged, attempt.Output); !ok || turn != "turn" {
		t.Fatal("valid native history refused")
	}
	if err := ledger.Accept(b, "turn"); err != nil {
		t.Fatal(err)
	}
	if got := ledger.NewIDs([]string{"$one", "$two", "$three"}); len(got) != 1 || got[0] != "$three" {
		t.Fatal(got)
	}
	if _, err := ledger.Begin(b, []string{"$three"}, "metadata", now); err != nil {
		t.Fatal(err)
	}
	if err := ledger.Accept(b, "second"); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Begin(b, []string{"$four"}, "metadata", now); err != ErrRate {
		t.Fatalf("rate not held: %v", err)
	}
	if _, err := ledger.Begin(b, []string{"$four"}, "metadata", now.Add(-time.Hour)); err != ErrRate {
		t.Fatal("clock reversal bypassed rate")
	}
	if _, err := ledger.Begin(b, []string{"$four"}, "metadata", now.Add(time.Hour)); err != nil {
		t.Fatal("hour-boundary positive control refused")
	}
}
