package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Proves any addressee, an explicitly recorded alias or a keeper clears the
// whole multi-addressee request; forged From, unrelated senders, earlier
// replies and assistant/wake-like notices without re do not clear it.
func TestLivenessUnansweredReplyAuthority(t *testing.T) {
	at := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	request := observedEvent{event: matrix.Event{EventID: "$q", Sender: "@requester:test.local", OriginServerTS: at.UnixMilli()}, header: Header{Kind: "request", To: []string{"prefixed-a", "b"}}}
	p, err := newReplyPolicy([]string{"keeper"}, []string{"prefixed-a=a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		sender, re, from string
		offset           time.Duration
		answered         bool
	}{
		{"b", "$q", "", time.Second, true}, {"a", "$q", "", time.Second, true}, {"keeper", "$q", "", time.Second, true},
		{"outsider", "$q", "b", time.Second, false}, {"b", "$other", "", time.Second, false}, {"b", "$q", "", -time.Second, false}, {"b", "", "", time.Second, false},
	} {
		t.Run(test.sender+test.re+test.offset.String(), func(t *testing.T) {
			reply := observedEvent{event: matrix.Event{EventID: "$r", Sender: "@" + test.sender + ":test.local", OriginServerTS: at.Add(test.offset).UnixMilli()}, header: Header{Kind: "answer", Re: test.re, From: test.from}}
			got := unanswered([]observedEvent{reply, request}, "test.local", p)
			if (len(got) == 0) != test.answered {
				t.Fatalf("reply authority mismatch: %+v", got)
			}
		})
	}
	withoutAlias, _ := newReplyPolicy(nil, nil)
	reply := observedEvent{event: matrix.Event{Sender: "@a:test.local", OriginServerTS: at.Add(time.Second).UnixMilli()}, header: Header{Re: "$q"}}
	if len(unanswered([]observedEvent{request, reply}, "test.local", withoutAlias)) != 2 {
		t.Fatal("unrecorded alias granted authority")
	}
}

// Proves opt-in waiter heartbeats advance through empty polls, status writes
// do not enter message delivery, cancellation disarms and mail stays unread.
func TestLivenessWaitHeartbeatAndHooks(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", "wait", "--status", "idle", "--heartbeat", "10ms", "--runtime", "codex", "--hooks", "none", "--timeout", "55ms").ok(t)
	st := e.state("alice")
	if st.Liveness == nil || st.Liveness.Doorbell != "none" || st.Liveness.HeartbeatAt.Sub(st.Liveness.Since) < 40*time.Millisecond {
		t.Fatalf("heartbeat not maintained/disarmed: %+v", st.Liveness)
	}
	if len(e.mailbox("alice").Pending()) != 0 {
		t.Fatal("status writes rang mailbox")
	}
	e.stdin = `{"session_id":"liveness-session"}`
	e.as("alice", "hook", "prompt", "--runtime", "codex").ok(t)
	if e.state("alice").Liveness.State != "working" {
		t.Fatal("prompt did not write working")
	}
	e.as("alice", "hook", "stop", "--runtime", "codex").ok(t)
	if e.state("alice").Liveness.State != "idle" {
		t.Fatal("unblocked stop did not write idle")
	}
	before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}")
	e.as("alice", "hook", "pause", "--agent", "alice").ok(t)
	e.as("alice", "hook", "prompt", "--runtime", "codex").ok(t)
	if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != before {
		t.Fatal("pause wrote status")
	}
	e.as("alice", "wait", "--heartbeat", "1m", "--timeout", "1ms").failsWith(t, ExitUsage, "--status")
}

// Proves a member without a record is down; two stale agents on one host
// produce one keeper-routed event per transition, retained across invocations.
func TestLivenessCorrelatedTransitionAlerts(t *testing.T) {
	e, _ := world(t)
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	e.as("alice", "status", "init").ok(t)
	e.as("alice", "invite", "proj", "carol").ok(t)
	e.as("carol", "join", "proj").ok(t)
	e.as("bob", "join", "status").ok(t)
	r := e.as("alice", "who", "--stale-after", "3m").ok(t)
	if !strings.Contains(r.stdout, "bob\tunknown\tdown") {
		t.Fatal("missing record looked live")
	}
	e.as("alice", statusArgs("idle")...).ok(t)
	e.as("bob", statusArgs("working")...).ok(t)
	now = now.Add(4 * time.Minute)
	args := []string{"who", "--stale-after", "3m", "--product", "swarm", "--alert", "--alert-room", "proj", "--keeper", "carol"}
	for i := 0; i < 3; i++ {
		e.as("alice", args...).ok(t)
	}
	e.hs.mu.Lock()
	events := append([]matrix.Event(nil), e.hs.events...)
	e.hs.mu.Unlock()
	var alerts []Header
	for _, ev := range events {
		if h, ok := decodeHeader(ev.Message().A2A); ok && h.Subtype == "incident" {
			alerts = append(alerts, h)
		}
	}
	if len(alerts) != 1 || strings.Join(alerts[0].To, ",") != "carol" {
		t.Fatalf("duplicate/unrouted incident: %+v", alerts)
	}
	e.as("alice", statusArgs("working")...).ok(t)
	e.as("bob", statusArgs("working")...).ok(t)
	for i := 0; i < 2; i++ {
		e.as("alice", args...).ok(t)
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	count := 0
	for _, ev := range e.hs.events {
		if h, ok := decodeHeader(ev.Message().A2A); ok && h.Subtype == "incident" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("recovery transition count %d", count)
	}
}

// Proves caller-visible history clears age only for an actual authorized
// reply, not inbox receipt; empty-page continuation and cycle rejection are
// exercised without publishing any alerts on an incomplete scan.
func TestLivenessUnansweredScanAndNoPartialAlerts(t *testing.T) {
	e, room := world(t)
	now := time.Now()
	e.now = func() time.Time { return now }
	e.as("alice", "status", "init").ok(t)
	e.as("bob", "join", "status").ok(t)
	e.as("bob", statusArgs("idle")...).ok(t)
	h := Header{V: 1, Type: "message", Kind: "request", To: []string{"bob"}}
	id := e.hs.inject(room, "@alice:test.local", msg("request", map[string]any{"ai.relux.a2a": h}))
	e.hs.mu.Lock()
	for i := range e.hs.events {
		if e.hs.events[i].EventID == id {
			e.hs.events[i].OriginServerTS = now.UnixMilli()
		}
	}
	e.hs.mu.Unlock()
	now = now.Add(time.Minute)
	args := []string{"who", "--stale-after", "3m", "--unanswered-since", now.Add(-time.Hour).UTC().Format(time.RFC3339)}
	r := e.as("alice", args...).ok(t)
	if !strings.Contains(r.stdout, "unanswered=1m0s") {
		t.Fatalf("missing request age: %s", r.stdout)
	}
	e.as("bob", "inbox").ok(t)
	r = e.as("alice", args...).ok(t)
	if !strings.Contains(r.stdout, "unanswered=1m0s") {
		t.Fatal("receipt counted as answer")
	}
	answer := e.hs.inject(room, "@bob:test.local", msg("answer", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "answer", Re: id}}))
	e.hs.mu.Lock()
	for i := range e.hs.events {
		if e.hs.events[i].EventID == answer {
			e.hs.events[i].OriginServerTS = now.UnixMilli()
		}
	}
	e.hs.mu.Unlock()
	r = e.as("alice", args...).ok(t)
	if strings.Contains(r.stdout, "unanswered=1m0s") {
		t.Fatal("answer did not clear age")
	}
	e.hs.mu.Lock()
	if e.hs.messagePages == nil {
		e.hs.messagePages = map[string]map[string]fakeMessagePage{}
	}
	e.hs.messagePages[room] = map[string]fakeMessagePage{"": {page: matrix.Page{End: "next"}}, "next": {page: matrix.Page{End: "next"}}}
	e.hs.mu.Unlock()
	e.as("alice", append(args, "--alert", "--alert-room", "proj", "--keeper", "carol")...).failsWith(t, ExitError, "pagination cycle")
	if len(e.state("alice").LivenessAlerts) != 0 {
		t.Fatal("partial scan wrote alert state")
	}
}

// Proves a missing keeper prevents both alert publication and journal mutation;
// joining the same keeper permits the nearby valid alert.
func TestLivenessAlertRequiresJoinedKeeper(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("waiting-human")...).ok(t)
	args := []string{"who", "--stale-after", "3m", "--product", "swarm", "--alert", "--alert-room", "proj", "--keeper", "bob", "--keeper", "carol"}
	before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
	e.as("alice", args...).failsWith(t, ExitError, "keeper carol is not a verified joined member")
	if len(e.state("alice").LivenessAlerts) != 0 || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != before {
		t.Fatal("missing keeper left a partial alert or journal")
	}
	e.as("alice", "invite", "proj", "carol").ok(t)
	e.as("carol", "join", "proj").ok(t)
	e.as("alice", args...).ok(t)
	if len(e.state("alice").LivenessAlerts) != 1 {
		t.Fatal("joined keeper did not permit the alert")
	}
}

// Proves a failed host recovery publication keeps its durable pending message
// even after the host disappears from the incident set, then retries it once.
func TestLivenessHostRecoveryRetry(t *testing.T) {
	e, _ := world(t)
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	e.as("alice", "status", "init").ok(t)
	e.as("bob", "join", "status").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	e.as("bob", statusArgs("working")...).ok(t)
	now = now.Add(4 * time.Minute)
	args := []string{"who", "--stale-after", "3m", "--product", "swarm", "--alert", "--alert-room", "proj", "--keeper", "bob"}
	e.as("alice", args...).ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	e.as("bob", statusArgs("working")...).ok(t)
	const route = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
	e.hs.fail(route, 500, "M_UNKNOWN", "recovery publication unavailable")
	e.as("alice", args...).failsWith(t, ExitError, "recovery publication unavailable")
	var key, txn string
	for k, record := range e.state("alice").LivenessAlerts {
		if strings.Contains(k, "host:") && record.Value == "healthy" && record.Pending != nil {
			key, txn = k, record.Pending.Txn
		}
	}
	if key == "" || txn == "" {
		t.Fatal("failed recovery lost its pending transaction")
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, route)
	e.hs.mu.Unlock()
	e.as("alice", args...).ok(t)
	if e.state("alice").LivenessAlerts[key].Pending != nil {
		t.Fatal("host recovery pending transaction was not retried")
	}
	before := e.hs.CallCount(route)
	e.as("alice", args...).ok(t)
	if e.hs.CallCount(route) != before {
		t.Fatal("completed recovery was published again")
	}
}

// Proves retrying a persisted transition's already accepted Matrix transaction
// creates no duplicate event. This simulates loss after the server accepted.
func TestLivenessAlertAcknowledgementLoss(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("waiting-human")...).ok(t)
	args := []string{"who", "--stale-after", "3m", "--product", "swarm", "--alert", "--alert-room", "proj", "--keeper", "bob"}
	e.as("alice", args...).ok(t)
	st := e.state("alice")
	var key string
	for k := range st.LivenessAlerts {
		key = k
	}
	if key == "" {
		t.Fatal("no durable transition")
	}
	e.hs.mu.Lock()
	var event matrix.Event
	for _, ev := range e.hs.events {
		h, ok := decodeHeader(ev.Message().A2A)
		if ok && h.Kind == "owner-needed" {
			event = ev
			break
		}
	}
	var txn string
	for k, id := range e.hs.txns {
		if id == event.EventID {
			parts := strings.Split(k, "|")
			txn = parts[len(parts)-1]
		}
	}
	count := len(e.hs.events)
	e.hs.mu.Unlock()
	var content map[string]any
	if err := json.Unmarshal(event.Content, &content); err != nil {
		t.Fatal(err)
	}
	_, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		record := st.LivenessAlerts[key]
		record.Pending = &config.LivenessAlertMessage{Txn: txn, Room: event.RoomID, Content: content}
		st.LivenessAlerts[key] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", args...).ok(t)
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	if len(e.hs.events) != count {
		t.Fatal("acknowledgement loss duplicated alert")
	}
}

// Proves watcher cancellation is clean and missing explicit poll is refused.
func TestLivenessWatchCancellation(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", "who", "--watch", "--stale-after", "3m").failsWith(t, ExitUsage, "--poll")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e.runCtx(ctx, "--as", "alice", "who", "--watch", "--poll", "5ms", "--stale-after", "3m").ok(t)
}

// Proves periodic transport heartbeats and disarming do not overwrite a
// human state recorded while a waiter is alive.
func TestLivenessWaitPreservesLaterManualState(t *testing.T) {
	e, _ := world(t)
	e.extra[EnvSessionID] = "wait-liveness-session"
	e.as("alice", "status", "init").ok(t)
	calls := e.hs.CallCount("GET /_matrix/client/v3/sync")
	ch := e.waitAsync("alice", "--timeout", "150ms", "--status", "idle", "--heartbeat", "10ms", "--runtime", "codex", "--hooks", "none")
	e.syncCalls(t, calls+2)
	e.as("alice", statusArgs("waiting-human")...).ok(t)
	recv(t, ch).ok(t)
	st := e.state("alice")
	if st.Liveness.State != "waiting-human" || st.LivenessSource != "manual" || st.Liveness.Doorbell != "none" {
		t.Fatalf("wait overwrote human state: %+v source=%s", st.Liveness, st.LivenessSource)
	}
}
