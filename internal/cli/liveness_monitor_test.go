package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
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
	e.as("alice", args...).failsWith(t, ExitError, "HTTP 500")
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

// Proves a heartbeat after a manual write and the subsequent disarm preserve
// that state/source, including when initial sync takes longer than the former
// 150ms fixture lifetime. A relevant live event ends the waiter; elapsed time
// alone cannot satisfy the heartbeat assertion.
func TestLivenessWaitPreservesLaterManualState(t *testing.T) {
	for _, tc := range []struct {
		name            string
		slowInitialSync bool
	}{{"normal", false}, {"slow-initial-sync", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			e.extra[EnvSessionID] = "wait-liveness-session"
			var offset atomic.Int64
			e.now = func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if tc.slowInitialSync && strings.HasSuffix(r.URL.Path, "/sync") && r.URL.Query().Get("since") == "" {
						// Model setup consuming time without a scheduler-sensitive sleep.
						offset.Add(int64(time.Second))
					}
					return resp, err
				})}
			}
			e.as("alice", "status", "init").ok(t)
			calls := e.hs.SyncCount("@alice:test.local")
			ctx, cancel := context.WithCancel(context.Background())
			ch, finished := make(chan result, 1), make(chan struct{})
			go func() {
				defer close(finished)
				ch <- e.runCtx(ctx, "--as", "alice", "wait", "--timeout", "0", "--status", "idle", "--heartbeat", "10ms", "--runtime", "codex", "--hooks", "none")
			}()
			t.Cleanup(func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(testDeadline):
					t.Error("wait did not stop after cleanup cancellation")
				}
			})
			e.syncedParked(t, "@alice:test.local", calls+2, ch, "wait ready for manual state")
			e.as("alice", append(statusArgs("waiting-human"), "owner decision pending")...).ok(t)
			manual := *e.state("alice").Liveness
			offset.Add(int64(time.Second))
			deadline := time.Now().Add(testDeadline)
			for {
				st := e.state("alice")
				if st.Liveness.HeartbeatAt.After(manual.HeartbeatAt) {
					if st.Liveness.State != manual.State || st.Liveness.Detail != manual.Detail || st.LivenessSource != "manual" || st.Liveness.Doorbell != "armed" {
						t.Fatalf("heartbeat overwrote human state: %+v source=%s", st.Liveness, st.LivenessSource)
					}
					break
				}
				select {
				case r := <-ch:
					t.Fatalf("wait returned before post-manual heartbeat: %+v", r)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("no heartbeat after manual state")
				}
				time.Sleep(2 * time.Millisecond)
			}
			id := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local wake up", nil))
			r := recv(t, ch).ok(t)
			if !strings.Contains(r.stdout, id) {
				t.Fatalf("wait did not deliver the controlled live event: %q", r.stdout)
			}
			st := e.state("alice")
			if st.Liveness.State != manual.State || st.Liveness.Detail != manual.Detail || st.LivenessSource != "manual" || st.Liveness.Doorbell != "none" {
				t.Fatalf("disarm overwrote human state: %+v source=%s", st.Liveness, st.LivenessSource)
			}
		})
	}
}

// Proves fresh transport heartbeats do not label native wake failure or an
// unarmed idle session healthy; an armed Claude harness is a valid neighbor.
func TestLivenessFreshTransportDoesNotHideWakeFailure(t *testing.T) {
	for _, tc := range []struct{ state, runtime, doorbell, wake, want string }{
		{"idle", "codex", "none", "", "wake unavailable"},
		{"idle", "codex", "armed", "", "wake unavailable"},
		{"idle", "codex", "armed", "unavailable: native thread", "wake unavailable"},
		{"idle", "claude", "none", "", "wake unavailable"},
		{"idle", "claude", "armed", "", "healthy"},
		{"idle", "codex", "armed", "runtime ready: no new mail", "healthy"},
		{"blocked", "codex", "armed", "wake failed: owner gate required", "blocked"},
	} {
		row := whoRow{Name: "agent", State: tc.state, Health: "fresh", Record: &config.AgentStatus{Runtime: tc.runtime, Doorbell: tc.doorbell, Wake: tc.wake}}
		got := incidents([]whoRow{row}, time.Now(), 0)
		if len(got) != 1 || got[0].value != tc.want || (tc.want == "wake unavailable" && !got[0].ownerNeeded) {
			t.Fatalf("fresh transport concealed wake: %+v => %+v", tc, got)
		}
	}
}

// Proves re-arming preserves pre-existing manual delivery holds; ordinary
// working records remain eligible for the caller's explicit idle transition.
func TestLivenessWaitRearmPreservesManualHolds(t *testing.T) {
	for _, state := range []string{"blocked", "stopping", "waiting-human", "working"} {
		t.Run(state, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "status", "init").ok(t)
			e.as("alice", statusArgs(state)...).ok(t)
			e.as("alice", "wait", "--status", "idle", "--heartbeat", "10ms", "--runtime", "codex", "--hooks", "none", "--timeout", "15ms").ok(t)
			st := e.state("alice")
			want, source := state, "manual"
			if state == "working" {
				want, source = "idle", "wait"
			}
			if st.Liveness.State != want || st.LivenessSource != source || st.Liveness.Doorbell != "none" {
				t.Fatalf("rearm cleared hold: %+v source=%s", st.Liveness, st.LivenessSource)
			}
		})
	}
}

// Proves explicitly recorded aliases work as request destinations as well as
// reply senders; unrecorded aliases never transfer requests to another row.
func TestLivenessAliasDestination(t *testing.T) {
	at := time.Now()
	p, err := newReplyPolicy(nil, []string{"prefixed-a=a"})
	if err != nil {
		t.Fatal(err)
	}
	q := observedEvent{event: matrix.Event{EventID: "$q", OriginServerTS: at.UnixMilli()}, header: Header{Kind: "question", To: []string{"a"}}}
	got := unanswered([]observedEvent{q}, "test.local", p)
	if len(got) != 1 || got["prefixed-a"].IsZero() {
		t.Fatalf("alias request lost: %v", got)
	}
	r := observedEvent{event: matrix.Event{Sender: "@prefixed-a:test.local", OriginServerTS: at.Add(time.Second).UnixMilli()}, header: Header{Re: "$q"}}
	if len(unanswered([]observedEvent{q, r}, "test.local", p)) != 0 {
		t.Fatal("canonical reply did not clear alias request")
	}
	p, _ = newReplyPolicy(nil, nil)
	if got := unanswered([]observedEvent{q}, "test.local", p); got["a"].IsZero() || !got["prefixed-a"].IsZero() {
		t.Fatal("unrecorded alias gained authority")
	}
}

// Proves a peer's incompatible heartbeat is isolated in watch mode while an
// independent waiting-human transition reaches its keeper; one-shot stays strict.
func TestLivenessWatchIsolatesPeerThreshold(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("bob", "join", "status").ok(t)
	e.as("alice", statusArgs("waiting-human")...).ok(t)
	args := statusArgs("working")
	args[4] = "10m"
	e.as("bob", args...).ok(t)
	e.as("alice", "who", "--stale-after", "3m").failsWith(t, ExitConfig, "bob")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.configure = func(a *App) {
		a.Stdout = &livenessCancelWriter{next: a.Stdout, cancel: cancel, match: "bob\tworking\tinvalid"}
	}
	r := e.runCtx(ctx, "--as", "alice", "who", "--watch", "--poll", "5ms", "--stale-after", "3m", "--alert", "--alert-room", "proj", "--keeper", "bob").ok(t)
	if !strings.Contains(r.stdout, "bob\tworking\tinvalid") || !strings.Contains(r.stdout, "alice\twaiting-human\tfresh") {
		t.Fatalf("peer silenced monitor: %s", r.stdout)
	}
	if len(e.state("alice").LivenessAlerts) != 2 {
		t.Fatal("independent health transition was lost")
	}
}

// Proves unknown reply history neither clears nor duplicates an existing
// unanswered incident; a complete healthy observation supplies the recovery.
func TestLivenessUnknownHistoryPreservesUnansweredIncident(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	a := eApp(e)
	a.Now = time.Now
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	room, _, err := s.resolveRoom(context.Background(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	incident := livenessIncident{key: "agent:alice", value: "unanswered", names: []string{"alice"}}
	if err := a.publishLivenessTransition(context.Background(), s, "scope:agent:alice", incident, room, []string{"bob"}); err != nil {
		t.Fatal(err)
	}
	before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
	incident.value = "unknown"
	for n := 0; n < 3; n++ {
		if err := a.publishLivenessTransition(context.Background(), s, "scope:agent:alice", incident, room, []string{"bob"}); err != nil {
			t.Fatal(err)
		}
	}
	if e.state("alice").LivenessAlerts["scope:agent:alice"].Value != "unanswered" || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != before {
		t.Fatal("unknown history cleared or duplicated prior incident")
	}
	incident.value = "healthy"
	if err := a.publishLivenessTransition(context.Background(), s, "scope:agent:alice", incident, room, []string{"bob"}); err != nil {
		t.Fatal(err)
	}
	if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != before+1 {
		t.Fatal("valid recovery missing")
	}
}

// Proves transport failure is retried at the explicit poll interval and
// cannot silently exit a watcher; the restored server yields real rows.
func TestLivenessWatchRetriesStateFailure(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("alice", statusArgs("working")...).ok(t)
	route := "GET /_matrix/client/v3/rooms/{room}/state"
	e.hs.fail(route, 500, "M_UNKNOWN", "transient liveness read")
	baseline := e.hs.CallCount(route)
	done := make(chan struct{})
	go func() {
		defer close(done)
		deadline := time.Now().Add(time.Second)
		for e.hs.CallCount(route) < baseline+2 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		e.hs.mu.Lock()
		delete(e.hs.failures, route)
		e.hs.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.configure = func(a *App) {
		a.Stdout = &livenessCancelWriter{next: a.Stdout, cancel: cancel, match: "alice\tworking\tfresh"}
	}
	r := e.runCtx(ctx, "--as", "alice", "who", "--watch", "--poll", "5ms", "--stale-after", "3m").ok(t)
	<-done
	if !strings.Contains(r.stderr, "HTTP 500") || !strings.Contains(r.stdout, "alice\tworking\tfresh") || e.hs.CallCount(route) < baseline+3 {
		t.Fatalf("watch did not recover: %+v calls=%d", r, e.hs.CallCount(route)-baseline)
	}
}

// Proves a self-marked monitoring observation does not become a keeper's
// unanswered request; identical text and a peer's marker grant no exemption.
func TestLivenessMonitorObservationExclusion(t *testing.T) {
	now := time.Now()
	body := livenessHistoryContent("Liveness transition waiting-human", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "owner-needed", To: []string{"bob"}}, "ai.relux.a2a.liveness_observation": 1})
	for _, sender := range []string{"@alice:test.local", "@peer:test.local"} {
		ev := matrix.Event{EventID: "$a", Sender: sender, Type: "m.room.message", OriginServerTS: now.UnixMilli(), Content: body}
		_, keep, err := observeLivenessEvent(ev, "room", now.Add(-time.Hour), now, "@alice:test.local")
		if err != nil || keep != (sender != "@alice:test.local") {
			t.Fatalf("marker exemption crossed authority: %s keep=%v err=%v", sender, keep, err)
		}
	}
	ev := matrix.Event{EventID: "$a", Sender: "@alice:test.local", Type: "m.room.message", OriginServerTS: now.UnixMilli(), Content: livenessHistoryContent("Liveness transition waiting-human", map[string]any{"ai.relux.a2a": Header{V: 1, Type: "message", Kind: "owner-needed", To: []string{"bob"}}})}
	if _, keep, err := observeLivenessEvent(ev, "room", now.Add(-time.Hour), now, "@alice:test.local"); err != nil || !keep {
		t.Fatal("body text suppressed real owner request")
	}
}

// Proves host correlation supersedes a prior individual alert with a notice
// explicitly denying recovery, and unchanged polls never repeat that notice.
func TestLivenessCorrelationSupersedesIndividual(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "status", "init").ok(t)
	e.as("bob", "join", "status").ok(t)
	now := time.Now()
	e.now = func() time.Time { return now }
	e.as("alice", statusArgs("working")...).ok(t)
	now = now.Add(4 * time.Minute)
	e.as("bob", statusArgs("working")...).ok(t)
	args := []string{"who", "--stale-after", "3m", "--product", "swarm", "--alert", "--alert-room", "proj", "--keeper", "bob"}
	e.as("alice", args...).ok(t)
	now = now.Add(4 * time.Minute)
	e.as("alice", args...).ok(t)
	before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
	e.as("alice", args...).ok(t)
	if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != before {
		t.Fatal("supersession repeated")
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	found := false
	for _, ev := range e.hs.events {
		h, ok := decodeHeader(ev.Message().A2A)
		if ok && h.Kind == "notice" && strings.Contains(ev.Message().Body, "superseded") {
			found = true
			if !strings.Contains(ev.Message().Body, "not a recovery") {
				t.Fatal("host grouping claimed individual recovery")
			}
		}
	}
	if !found {
		t.Fatal("supersession notice missing")
	}
}

// Proves missing/unbounded watch history budgets and ambiguous alias authority
// fail before reading Matrix; a single recorded mapping remains valid.
func TestLivenessWatchHistoryConfiguration(t *testing.T) {
	e, _ := world(t)
	since := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	base := []string{"who", "--watch", "--poll", "1s", "--stale-after", "3m", "--unanswered-since", since}
	for _, budget := range []string{"0", "101", "-1"} {
		e.as("alice", append(base, "--history-pages-per-poll", budget)...).failsWith(t, ExitUsage, "history-pages-per-poll")
	}
	for _, pairs := range [][]string{{"a=x", "b=x"}, {"a=b", "b=c"}} {
		if _, err := newReplyPolicy(nil, pairs); err == nil {
			t.Fatalf("ambiguous alias granted authority: %v", pairs)
		}
	}
	if _, err := newReplyPolicy(nil, []string{"a=x", "a=y"}); err != nil {
		t.Fatalf("valid recorded aliases refused: %v", err)
	}
	if e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/state") != 0 {
		t.Fatal("invalid flags read peer state")
	}
}

// Cancellation is triggered by observed output, never used as a success oracle.
type livenessCancelWriter struct {
	next   io.Writer
	cancel context.CancelFunc
	match  string
}

func (w *livenessCancelWriter) Write(p []byte) (int, error) {
	n, err := w.next.Write(p)
	if strings.Contains(string(p), w.match) {
		w.cancel()
	}
	return n, err
}

// Proves health transitions cannot erase a previously observed unanswered
// request while reply history is unknown; complete history supplies recovery.
func TestLivenessUnknownAfterHealthChangeRetainsUnanswered(t *testing.T) {
	e, _ := world(t)
	a := eApp(e)
	s, err := a.sessionAs("alice")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	a.Now = func() time.Time { return now }
	since := now.Add(-time.Hour)
	rows := []whoRow{{Name: "alice", State: "working", Health: "fresh", Unanswered: "2m", UnansweredSince: now.Add(-2 * time.Minute)}}
	publish := func() {
		t.Helper()
		if err := a.alertLiveness(context.Background(), s, rows, "proj", []string{"bob"}, time.Minute, "", since, 3*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	publish()
	rows[0].Health = "stale"
	publish()
	rows[0].Health = "fresh"
	rows[0].Unanswered = "unknown (history incomplete)"
	rows[0].UnansweredSince = time.Time{}
	publish()
	found := false
	for _, r := range e.state("alice").LivenessAlerts {
		found = found || r.Value == "unanswered"
	}
	if !found {
		t.Fatal("health transition erased unanswered under unknown history")
	}
	rows[0].Unanswered = "-"
	publish()
	for _, r := range e.state("alice").LivenessAlerts {
		if r.Value == "unanswered" {
			t.Fatal("complete reply history did not supply recovery")
		}
	}
}
