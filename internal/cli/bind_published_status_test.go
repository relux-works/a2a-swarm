package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/tmuxwake"
)

// Both readers use acknowledged self-publication: presence-only is unknown;
// legacy notice-only events can be suspected/clear/unknown without liveness;
// a genuine old heartbeat stays stale when notice telemetry changes.
func TestPublishedNoticeStatusReaders(t *testing.T) {
	e, a, s, _ := strandedFallbackFixture(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", "status", "set", "active").ok(t) // Deliberately leaves Matrix presence online.
	check := func(state, health, notice string) {
		t.Helper()
		for _, args := range [][]string{{"who", "--stale-after", "3m"}, {"status", "get", "alice"}} {
			out := e.as("alice", args...).ok(t).stdout
			if !strings.Contains(out, "alice\t"+state+"\t"+health) || !strings.Contains(out, notice) || strings.Contains(out, "online") {
				t.Fatalf("%v: %s", args, out)
			}
		}
	}
	check("unknown", "unknown", "own notice unknown")
	publishLegacyNoticeForReader(t, s, e.state("alice").Terminal.OwnNotice)
	check("unknown", "unknown", "SUSPECTED-DEAF")
	st := e.state("alice")
	if st.Liveness != nil || st.NoticeStatus != nil {
		t.Fatal("notice publication invented liveness")
	}
	b := *st.Terminal
	if err := a.observeTerminalNotice(s, "gen", b, "empty"); err != nil {
		t.Fatal(err)
	}
	publishLegacyNoticeForReader(t, s, e.state("alice").Terminal.OwnNotice)
	check("unknown", "unknown", "own notice clear")
	if err := a.observeTerminalNotice(s, "gen", *e.state("alice").Terminal, "unknown"); err != nil {
		t.Fatal(err)
	}
	publishLegacyNoticeForReader(t, s, e.state("alice").Terminal.OwnNotice)
	check("unknown", "unknown", "own notice unknown")
	for _, state := range []string{"idle", "blocked", "waiting-human", "working"} {
		e.as("alice", statusArgs(state)...).ok(t)
		check(state, "fresh", "own notice unknown")
	}
	heartbeat := e.state("alice").Liveness.HeartbeatAt
	now := a.Now().Add(11 * time.Hour)
	a.Now = func() time.Time { return now }
	e.now = a.Now
	notificationState(t, e, func(st *config.State) { st.Terminal.OwnNotice = b.OwnNotice })
	if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
		t.Fatal(err)
	}
	check("working", "stale", "SUSPECTED-DEAF")
	if !e.state("alice").Liveness.HeartbeatAt.Equal(heartbeat) {
		t.Fatal("consumer refreshed heartbeat")
	}
	e.as("alice", statusArgs("stopping")...).ok(t)
	check("stopping", "down", "SUSPECTED-DEAF")
	now = now.Add(-time.Minute)
	check("stopping", "clock", "SUSPECTED-DEAF")
}

// Seed a self-authored legacy notice-only event to exercise reader compatibility;
// automatic writers now require a locally opted-in liveness record.
func publishLegacyNoticeForReader(t *testing.T, s *session, notice config.OwnNoticeObservation) {
	t.Helper()
	room, _, err := s.resolveRoom(context.Background(), StatusRoom)
	if err != nil {
		t.Fatal(err)
	}
	record := config.AgentStatus{Name: s.name, State: "unknown", Host: "host-a", Version: 1, OwnNotice: notice}
	if _, err := s.client.PutState(context.Background(), room, StatusEventType, s.creds.UserID, record); err != nil {
		t.Fatal(err)
	}
}

// Without liveness, fresh observations, suspicion and clears never resolve/PUT
// status. Hooks preserve that opt-out. Opting in is the adjacent valid control.
func TestNoticeWithoutLivenessMakesNoRequests(t *testing.T) {
	e, a, s, _ := strandedFallbackFixture(t)
	requests := 0
	base := s.client.HTTP.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	s.client.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		return base.RoundTrip(r)
	})}
	for _, input := range []string{"nonempty", "empty", "unknown"} {
		for i := 0; i < 3; i++ {
			now := a.Now().Add(time.Second)
			a.Now = func() time.Time { return now }
			if err := a.observeTerminalNotice(s, "gen", *e.state("alice").Terminal, input); err != nil {
				t.Fatal(err)
			}
			if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
				t.Fatal(err)
			}
		}
	}
	before := e.state("alice")
	if err := a.hookLiveness(context.Background(), hookEvent{arg: "prompt"}, hookInput{SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	st := e.state("alice")
	if !reflect.DeepEqual(before, st) {
		t.Fatal("hook changed an unconfigured lane")
	}
	if requests != 0 || st.Liveness != nil || st.NoticeStatus != nil || st.NoticeRetry != nil {
		t.Fatal("unconfigured lane gained publication/retry work", requests)
	}
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	if err := a.observeTerminalNotice(s, "gen", *e.state("alice").Terminal, "empty"); err != nil {
		t.Fatal(err)
	}
	if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
		t.Fatal(err)
	}
	if requests != 2 || e.state("alice").Terminal.NoticePublication != "published" {
		t.Fatal("opted-in neighbor did not publish")
	}
}

// Prompt/Stop for this session and a submitted notice clear the published
// episode. Foreign sessions/hosts and paused hooks cannot clear it. No tmux
// invocation or actual keypress is exercised by the supplied SendResult.
func TestPublishedNoticeRecovery(t *testing.T) {
	for _, mode := range []string{"prompt", "stop", "submitted", "foreign", "foreign-host", "paused"} {
		t.Run(mode, func(t *testing.T) {
			e, a, s, _ := strandedFallbackFixture(t)
			e.as("alice", "status", "init").ok(t)
			e.as("alice", statusArgs("blocked")...).ok(t)
			var expected config.TerminalBinding
			notificationState(t, e, func(st *config.State) {
				b := st.Terminal
				b.Host = machineLabel()
				b.SessionID = "session-a"
				b.Notice = tmuxwake.Notice(1)
				b.NoticeCount = 1
				b.AttemptIDs = []string{"$pending"}
				b.AttemptAt = a.Now().Add(-11 * time.Minute)
				b.Typed = true
				token, err := st.Notification.Reserve("terminal", b.SessionID, b.Generation, a.Now(), false)
				if err != nil {
					t.Fatal(err)
				}
				b.NotificationToken = token
				expected = *b
			})
			if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
				t.Fatal(err)
			}
			mb := e.mailbox("alice")
			if _, err := mb.Append([]config.MailEntry{{ID: "$pending", Kind: config.MailEvent, Room: "!synthetic:test.local", Rel: "to", TS: a.Now().UnixMilli(), Event: json.RawMessage(`{"content":{"body":"synthetic mail"}}`)}}); err != nil {
				t.Fatal(err)
			}
			if err := a.reportStrandedTerminalNotice(context.Background(), s, "gen"); err != nil {
				t.Fatal(err)
			}
			before := mb.Pending()
			if mode == "submitted" {
				result := tmuxwake.SendResult{Disposition: tmuxwake.SendSubmitted, Outcome: tmuxwake.SendWakeSubmitted}
				if err := a.finishTerminal(s, "gen", result, nil, false, expected); err != nil {
					t.Fatal(err)
				}
				if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
					t.Fatal(err)
				}
			} else {
				sessionID := "session-a"
				event := "prompt"
				if mode == "stop" {
					event = "stop"
				}
				if mode == "foreign" {
					sessionID = "other-session"
				}
				if mode == "paused" {
					notificationState(t, e, func(st *config.State) { st.HooksPaused = true })
				}
				if mode == "foreign-host" {
					notificationState(t, e, func(st *config.State) { st.Terminal.Host = "other-host" })
				}
				if err := a.hookLiveness(context.Background(), hookEvent{arg: event}, hookInput{SessionID: sessionID}); err != nil {
					t.Fatal(err)
				}
			}
			forbidden := mode == "foreign" || mode == "foreign-host" || mode == "paused"
			for _, args := range [][]string{{"who", "--stale-after", "3m"}, {"status", "get", "alice"}} {
				out := e.as("alice", args...).ok(t).stdout
				if strings.Contains(out, "SUSPECTED-DEAF") != forbidden || !forbidden && !strings.Contains(out, "own notice clear") {
					t.Fatal("wrong recovery output", out)
				}
			}
			after := e.state("alice")
			if after.Liveness == nil || !reflect.DeepEqual(before, e.mailbox("alice").Pending()) {
				t.Fatal("recovery lost liveness or changed receipts")
			}
			if !forbidden {
				a.sendMailAlarm(context.Background(), s, "gen")
				if len(after.MailAlarm.Jobs) != 0 || len(strandedAlerts(e)) != 0 {
					t.Fatal("recovery retained/sent old incident jobs")
				}
			}
			if !forbidden && (after.Terminal.NoticeReport != nil || after.Terminal.Notice != "" || after.Terminal.NoticeOrigin != nil) {
				t.Fatal("old episode/typing authority retained")
			}
		})
	}
}

// Readers refuse a foreign sender, mismatched name and malformed notice;
// restoring the same self-published event is the valid neighboring control.
func TestPublishedNoticeReaderRefusals(t *testing.T) {
	e, _, s, _ := strandedFallbackFixture(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
		t.Fatal(err)
	}
	room, _, err := s.resolveRoom(context.Background(), StatusRoom)
	if err != nil {
		t.Fatal(err)
	}
	key := StatusEventType + "|" + s.creds.UserID
	e.hs.mu.Lock()
	original := e.hs.rooms[room].state[key]
	e.hs.mu.Unlock()
	for _, mode := range []string{"sender", "name", "notice", "valid"} {
		var record config.AgentStatus
		if err := json.Unmarshal(original, &record); err != nil {
			t.Fatal(err)
		}
		sender := s.creds.UserID
		switch mode {
		case "sender":
			sender = "@other:test.local"
		case "name":
			record.Name = "other"
		case "notice":
			record.OwnNotice.Reason = "mail-content"
		}
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		e.hs.mu.Lock()
		e.hs.rooms[room].state[key] = raw
		e.hs.rooms[room].stateSenders[key] = sender
		e.hs.mu.Unlock()
		for _, args := range [][]string{{"who", "--stale-after", "3m"}, {"status", "get", "alice"}} {
			out := e.as("alice", args...).ok(t).stdout
			if strings.Contains(out, "SUSPECTED-DEAF") != (mode == "valid") {
				t.Fatal("untrusted observation admitted or valid refused", mode, out)
			}
			if mode == "notice" && !strings.Contains(out, "invalid") {
				t.Fatal("invalid observation hidden", out)
			}
		}
	}
}

// Notice-only status must not smuggle heartbeat/runtime claims. A normal
// typed liveness record and a metadata-only unknown record remain valid.
func TestNoticeOnlyStatusContract(t *testing.T) {
	for _, mode := range []string{"valid", "heartbeat", "period", "since", "runtime", "hooks", "doorbell", "detail", "wake", "product"} {
		record := config.AgentStatus{State: "unknown"}
		switch mode {
		case "heartbeat":
			record.HeartbeatAt = time.Now()
		case "period":
			record.HeartbeatEvery = "1m"
		case "since":
			record.Since = time.Now()
		case "runtime":
			record.Runtime = "codex"
		case "hooks":
			record.Hooks = "installed"
		case "doorbell":
			record.Doorbell = "armed"
		case "detail":
			record.Detail = "active"
		case "wake":
			record.Wake = "submitted"
		case "product":
			record.Product = "project"
		}
		if (statusValid(record) == nil) != (mode == "valid") {
			t.Fatal("wrong notice-only verdict", mode)
		}
	}
	if err := statusValid(config.AgentStatus{State: "working", HeartbeatEvery: "1m", Runtime: "none", Hooks: "none", Doorbell: "none"}); err != nil {
		t.Fatal("typed neighbor refused", err)
	}
}

// Failed opted-in publication cannot create visible suspicion or a local
// ACK. A failed recovery keeps the old remote suspicion until a later matching
// hook retries the clear; the manual blocked heartbeat remains unchanged.
func TestPublishedNoticeFailureAndRecoveryRetry(t *testing.T) {
	e, a, s, _ := strandedFallbackFixture(t)
	e.as("alice", "status", "init").ok(t)
	notice := e.state("alice").Terminal.OwnNotice
	notificationState(t, e, func(st *config.State) { st.Terminal.OwnNotice = config.OwnNoticeObservation{} })
	e.as("alice", statusArgs("blocked")...).ok(t)
	notificationState(t, e, func(st *config.State) { st.Terminal.OwnNotice = notice })
	heartbeat := e.state("alice").Liveness.HeartbeatAt
	stateRoute := "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}"
	e.hs.fail(stateRoute, 500, "M_UNKNOWN", "synthetic unavailable")
	if err := s.publishTerminalNotice(context.Background(), "gen"); err == nil {
		t.Fatal("failed PUT claimed success")
	}
	st := e.state("alice")
	if st.NoticeStatus != nil || st.Liveness == nil || !st.Liveness.HeartbeatAt.Equal(heartbeat) || !st.NoticeWriteUncertain || st.NoticeRetry == nil {
		t.Fatal("failed PUT fabricated ACK/liveness or lost retry")
	}
	for _, args := range [][]string{{"who", "--stale-after", "3m"}, {"status", "get", "alice"}} {
		out := e.as("alice", args...).ok(t).stdout
		if strings.Contains(out, "SUSPECTED-DEAF") || !strings.Contains(out, "unknown") {
			t.Fatal("unpublished suspicion visible", out)
		}
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, stateRoute)
	e.hs.mu.Unlock()
	now := st.NoticeRetry.Next
	a.Now = func() time.Time { return now }
	e.now = a.Now
	if err := s.publishTerminalNotice(context.Background(), "gen"); err != nil {
		t.Fatal(err)
	}
	notificationState(t, e, func(st *config.State) { st.Terminal.SessionID = "session-a"; st.Terminal.Host = machineLabel() })
	e.hs.fail(stateRoute, 500, "M_UNKNOWN", "synthetic recovery unavailable")
	if err := a.hookLiveness(context.Background(), hookEvent{arg: "prompt"}, hookInput{SessionID: "session-a"}); err == nil {
		t.Fatal("failed recovery publication claimed success")
	}
	st = e.state("alice")
	if st.Terminal.OwnNotice.PossiblyOrphaned == nil || *st.Terminal.OwnNotice.PossiblyOrphaned || st.NoticeRetry == nil {
		t.Fatal("failed publication lost local clear/retry")
	}
	if out := e.as("alice", "status", "get", "alice").ok(t).stdout; !strings.Contains(out, "SUSPECTED-DEAF") {
		t.Fatal("failed clear falsely visible", out)
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, stateRoute)
	e.hs.mu.Unlock()
	now = st.NoticeRetry.Next
	if err := a.hookLiveness(context.Background(), hookEvent{arg: "prompt"}, hookInput{SessionID: "session-a"}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"who", "--stale-after", "3m"}, {"status", "get", "alice"}} {
		out := e.as("alice", args...).ok(t).stdout
		if strings.Contains(out, "SUSPECTED-DEAF") || !strings.Contains(out, "own notice clear") {
			t.Fatal("clear retry lost", out)
		}
	}
	if !e.state("alice").Liveness.HeartbeatAt.Equal(heartbeat) {
		t.Fatal("retry refreshed blocked heartbeat")
	}
}
