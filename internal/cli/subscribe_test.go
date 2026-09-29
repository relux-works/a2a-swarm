package cli

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const accountDataPut = "PUT /_matrix/client/v3/user/{user}/account_data/{type}"

func kindMsg(from, kind, body string) map[string]any {
	return msg("- from: "+from+"\n\n"+body, map[string]any{ContentKey: Header{V: 1, Type: "message", From: from, Kind: kind, Slug: "alpha"}})
}

// hdrMsg is a typed message with a structured subtype and subject.
func hdrMsg(from, kind, subtype, subject, body string) map[string]any {
	return msg("- from: "+from+"\n\n"+body, map[string]any{ContentKey: Header{V: 1, Type: "message", From: from, Kind: kind,
		Subtype: subtype, Subject: subject, Slug: "alpha"}})
}

// subWorld is world() with the fake server stamping events with the wall
// clock, so that subscription creation times (App.Now) compare with event times.
func subWorld(t *testing.T) (*env, string) {
	t.Helper()
	e, room := world(t)
	e.hs.mu.Lock()
	e.hs.liveClock = true
	e.hs.mu.Unlock()
	return e, room
}

var subLineRE = regexp.MustCompile(`^(sub-[0-9a-f]{8})\t(.*)$`)

// subscribe runs `a2a subscribe` for name and returns the new id and summary.
func (e *env) subscribe(name string, args ...string) (string, string) {
	e.t.Helper()
	out := strings.TrimSuffix(e.as(name, append([]string{"subscribe"}, args...)...).ok(e.t).stdout, "\n")
	m := subLineRE.FindStringSubmatch(out)
	if m == nil {
		e.t.Fatalf("subscribe output %q is not sub-<8 hex>\\t<summary>", out)
	}
	return m[1], m[2]
}

// serverSubs decodes the subscriptions stored in name's account data.
func (e *env) serverSubs(name string) SubList {
	e.t.Helper()
	var l SubList
	if raw := e.hs.accountDataOf("@"+name+":"+testServer, SubsType); raw != nil {
		if err := json.Unmarshal(raw, &l); err != nil {
			e.t.Fatal(err)
		}
	}
	return l
}

// Proves subscribe stores each subscription in the agent's account data (not
// the local state file) with an id and a summary of every selector; subs lists
// them (line and JSON); unsubscribe removes one by id or all; subscriptions are per agent.
func TestSubscribeCommands(t *testing.T) {
	e, _ := subWorld(t)
	e.as("bob", "topic", "new", "proj", "alpha", "A").ok(t)
	id1, sum1 := e.subscribe("alice", "--kind", "system,owner-needed", "--room", "proj")
	if sum1 != "room=#proj kinds=system,owner-needed" {
		t.Fatalf("summary = %q", sum1)
	}
	id2, sum2 := e.subscribe("alice", "--kind", "decision")
	if sum2 != "room=all kinds=decision" {
		t.Fatalf("summary = %q", sum2)
	}
	until := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	id3, sum3 := e.subscribe("alice", "--room", "proj", "--topic", "alpha", "--from", "bob", "--subject", "BUG-1",
		"--subtype", "landing", "--until", until, "--until-kind", "system")
	for _, part := range []string{"room=#proj", "topic=$ev001", "from=bob", "subject=BUG-1", "subtype=landing", "until=" + until, "until-kind=system"} {
		tuitestkit.ContainsStr(t, sum3, part)
	}

	lines := tuitestkit.LinesFromStr(strings.TrimSpace(e.as("alice", "subs").ok(t).stdout))
	if len(lines) != 3 || lines[0] != id1+"\t"+sum1 || lines[1] != id2+"\t"+sum2 || lines[2] != id3+"\t"+sum3 {
		t.Fatalf("subs = %q", lines)
	}
	var js SubList
	if err := json.Unmarshal([]byte(e.as("alice", "subs", "--json").ok(t).stdout), &js); err != nil || len(js.Subs) != 3 || js.V != 1 {
		t.Fatalf("subs --json = %+v, %v", js, err)
	}
	if s := js.Subs[2]; s.ID != id3 || s.Room != "!projRoomID" || s.Topic != "$ev001" || s.From != "@bob:test.local" || s.CreatedAt == "" {
		t.Fatalf("stored sub = %+v", s)
	}
	if got := e.serverSubs("alice"); len(got.Subs) != 3 {
		t.Fatalf("account data holds %d subs", len(got.Subs))
	}
	if data, _ := os.ReadFile(e.paths().StateFile("alice")); strings.Contains(string(data), "subscriptions") {
		t.Fatalf("subscriptions leaked into the local state: %s", data)
	}

	tuitestkit.ContainsStr(t, e.as("alice", "unsubscribe", id1).ok(t).stdout, id1+"\tremoved")
	e.as("alice", "unsubscribe", id1).failsWith(t, ExitError, "no subscription")
	e.as("alice", "unsubscribe", "sub-00000000").failsWith(t, ExitError, "no subscription")
	out := e.as("alice", "unsubscribe", "--all").ok(t).stdout
	tuitestkit.ContainsStr(t, out, id2+"\tremoved")
	tuitestkit.ContainsStr(t, out, id3+"\tremoved")
	if out := e.as("alice", "subs").ok(t).stdout; out != "" {
		t.Fatalf("subs after --all = %q", out)
	}
	tuitestkit.ContainsStr(t, e.as("alice", "unsubscribe", "--all").ok(t).stdout, "no subscriptions")
	e.subscribe("alice", "--kind", "system")
	if out := e.as("bob", "subs").ok(t).stdout; out != "" {
		t.Fatalf("bob sees %q", out)
	}
}

// Proves bad subscribe and unsubscribe input is a usage error raised before
// any request reaches the server, and nothing is stored; the positive
// controls (--room R --all-events, a plain --kind) are accepted.
func TestSubscribeNegative(t *testing.T) {
	e, _ := subWorld(t)
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	before := len(e.hs.Calls())
	cases := []struct {
		args     []string
		fragment string
	}{
		{[]string{"--room", "proj"}, "name at least one selector"},
		{[]string{}, "name at least one selector"},
		{[]string{"--until-kind", "system"}, "name at least one selector"},
		{[]string{"--all-events"}, "--all-events needs a concrete --room"},
		{[]string{"--room", "all", "--all-events"}, "--all-events needs a concrete --room"},
		{[]string{"--topic", "alpha"}, "--topic needs a concrete --room"},
		{[]string{"--kind", "system,gossip"}, "invalid --kind"},
		{[]string{"--subtype", "party"}, "invalid --subtype"},
		{[]string{"--subject", "has space"}, "invalid --subject"},
		{[]string{"--subject", strings.Repeat("x", 129)}, "invalid --subject"},
		{[]string{"--kind", "system", "--until", "tomorrow"}, "invalid --until"},
		{[]string{"--kind", "system", "--until", past}, "not in the future"},
		{[]string{"--kind", "system", "--until-kind", "gossip"}, "invalid --until-kind"},
		{[]string{"--kind", "system", "--since", "ev1"}, "invalid --since"},
		{[]string{"--from", "bad user"}, "invalid --from"},
		{[]string{"--room", "proj", "--topic", "Bad_Slug"}, "invalid --topic"},
		{[]string{"--kind", "system", "extra"}, "usage"},
	}
	for _, tc := range cases {
		e.as("alice", append([]string{"subscribe"}, tc.args...)...).failsWith(t, ExitUsage, tc.fragment)
	}
	e.as("alice", "unsubscribe").failsWith(t, ExitUsage, "usage")
	e.as("alice", "unsubscribe", "sub-1", "--all").failsWith(t, ExitUsage, "usage")
	if n := len(e.hs.Calls()); n != before {
		t.Fatalf("usage errors reached the server: %d requests", n-before)
	}
	e.as("alice", "subscribe", "--kind", "system", "--room", "nosuch").failsWith(t, ExitError, "")
	if got := e.serverSubs("alice"); len(got.Subs) != 0 {
		t.Fatalf("refused input was stored: %+v", got)
	}
	e.subscribe("alice", "--room", "proj", "--all-events")
	e.subscribe("alice", "--kind", "system")
	if got := e.serverSubs("alice"); len(got.Subs) != 2 {
		t.Fatalf("positive controls stored %d subs", len(got.Subs))
	}
}

// Proves the legacy PR #7 syntax `subscribe --kind k --room R` still works as
// a kind selector: wait wakes (type sub, naming the sub) on an unaddressed
// message of that kind in that room, and not on other kinds, other rooms, or
// my own messages of that kind.
func TestWaitWakesOnSubscribedKind(t *testing.T) {
	e, room := subWorld(t)
	lobby := e.hs.aliases["#lobby:"+testServer]
	id, _ := e.subscribe("alice", "--kind", "system", "--room", "proj")
	e.as("alice", "wait", "--timeout", "10ms").ok(t) // take a position
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "notice", "plain notice"))
	e.hs.inject(lobby, "@bob:test.local", kindMsg("bob", "system", "system elsewhere"))
	e.hs.inject(room, "@alice:test.local", kindMsg("alice", "system", "my own system"))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("woke without a match: %q", r.stdout)
	}
	ev := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "landing on main"))
	r := e.as("alice", "wait", "--timeout", "2s", "--json").ok(t)
	recs := decodeRecords(t, r.stdout)
	if len(recs) != 1 || recs[0].Type != "sub" || recs[0].EventID != ev || recs[0].Kind != "system" || recs[0].Sub != id {
		t.Fatalf("records = %+v", recs)
	}
}

// Proves wait --all wakes on any other agent's message in the watched room,
// never on my own, never in rooms outside --room, and needs a --room.
func TestWaitAll(t *testing.T) {
	e, room := world(t)
	lobby := e.hs.aliases["#lobby:"+testServer]
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.hs.inject(room, "@alice:test.local", msg("mine", nil))
	e.hs.inject(lobby, "@bob:test.local", msg("lobby chatter", nil))
	if r := e.as("alice", "wait", "--room", "proj", "--all", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("woke on own or foreign-room message: %q", r.stdout)
	}
	id := e.hs.inject(room, "@bob:test.local", msg("anything at all", nil))
	r := e.as("alice", "wait", "--room", "proj", "--all", "--timeout", "2s").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "all #proj "+id)
	e.as("alice", "wait", "--all").failsWith(t, ExitUsage, "--all needs at least one --room")
}

// Proves each selector type delivers its matching event (type sub, naming the
// subscription), while a near-miss event injected in the same batch is not
// delivered (the paired negative of each selector).
func TestSubSelectorsDeliver(t *testing.T) {
	reply := func(root, body string) map[string]any {
		return msg(body, map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}})
	}
	cases := []struct {
		name  string
		args  []string
		noise func(e *env, room, lobby, alpha, beta string)
		match func(e *env, room, lobby, alpha, beta string) string
	}{
		{"kind in room", []string{"--kind", "decision", "--room", "proj"},
			func(e *env, room, lobby, _, _ string) {
				e.hs.inject(room, "@bob:test.local", kindMsg("bob", "notice", "not a decision"))
				e.hs.inject(lobby, "@bob:test.local", kindMsg("bob", "decision", "decision elsewhere"))
			},
			func(e *env, room, _, _, _ string) string {
				return e.hs.inject(room, "@bob:test.local", kindMsg("bob", "decision", "we go"))
			}},
		{"topic", []string{"--room", "proj", "--topic", "alpha"},
			func(e *env, room, _, _, beta string) { e.hs.inject(room, "@bob:test.local", reply(beta, "in beta")) },
			func(e *env, room, _, alpha, _ string) string {
				return e.hs.inject(room, "@bob:test.local", reply(alpha, "in alpha"))
			}},
		{"from", []string{"--from", "bob"},
			func(e *env, room, _, _, _ string) { e.hs.inject(room, "@carol:test.local", msg("carol talks", nil)) },
			func(e *env, room, _, _, _ string) string {
				return e.hs.inject(room, "@bob:test.local", msg("bob talks", nil))
			}},
		{"structured subject", []string{"--subject", "PR#12"},
			func(e *env, room, _, _, _ string) {
				e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "notice", "", "PR#13", "other pr"))
			},
			func(e *env, room, _, _, _ string) string {
				return e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "notice", "", "PR#12", "reviewed"))
			}},
		{"subject token in body", []string{"--subject", "BUG-1"},
			func(e *env, room, _, _, _ string) {
				e.hs.inject(room, "@bob:test.local", msg("BUG-12 and xBUG-1 again", nil))
			},
			func(e *env, room, _, _, _ string) string {
				return e.hs.inject(room, "@bob:test.local", msg("fixed BUG-1.", nil))
			}},
		{"subtype", []string{"--subtype", "landing"},
			func(e *env, room, _, _, _ string) {
				e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "system", "window", "", "window opens"))
			},
			func(e *env, room, _, _, _ string) string {
				return e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "system", "landing", "", "landed"))
			}},
		{"all events of a room", []string{"--room", "proj", "--all-events"},
			func(e *env, _, lobby, _, _ string) { e.hs.inject(lobby, "@bob:test.local", msg("lobby chatter", nil)) },
			func(e *env, room, _, _, _ string) string {
				return e.hs.inject(room, "@bob:test.local", msg("anything", nil))
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, room := subWorld(t)
			lobby := e.hs.aliases["#lobby:"+testServer]
			alpha := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
			beta := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "beta", "B").ok(t).stdout)
			e.as("alice", "wait", "--timeout", "10ms").ok(t)
			id, _ := e.subscribe("alice", tc.args...)
			tc.noise(e, room, lobby, alpha, beta)
			ev := tc.match(e, room, lobby, alpha, beta)
			recs := decodeRecords(t, e.as("alice", "wait", "--timeout", "2s", "--json").ok(t).stdout)
			if len(recs) != 1 || recs[0].Type != "sub" || recs[0].Sub != id || recs[0].EventID != ev {
				t.Fatalf("records = %+v, want exactly %s via %s", recs, ev, id)
			}
			if r := e.as("alice", "wait", "--timeout", "60ms").ok(t); r.stdout != "timeout\n" {
				t.Fatalf("second wait = %q", r.stdout)
			}
		})
	}
}

// Proves a room-scoped subscription never delivers events from another room,
// while the same kind in its own room is delivered (positive control).
func TestSubRoomScopedIgnoresForeignRooms(t *testing.T) {
	e, room := subWorld(t)
	lobby := e.hs.aliases["#lobby:"+testServer]
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.subscribe("alice", "--room", "proj", "--kind", "system")
	e.hs.inject(lobby, "@bob:test.local", kindMsg("bob", "system", "foreign"))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("foreign room delivered: %q", r.stdout)
	}
	id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "home"))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "sub #proj "+id)
}

// Proves a subject-scoped subscription delivers no unrelated traffic of the
// same room (no subject, other subjects, longer tokens), and delivers the
// related message (positive control).
func TestSubSubjectScopedIgnoresUnrelated(t *testing.T) {
	e, room := subWorld(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.subscribe("alice", "--subject", "relux-works/skill-project-management@main")
	e.hs.inject(room, "@bob:test.local", msg("unrelated chatter", nil))
	e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "system", "landing", "relux-works/a2a-swarm@main", "other repo"))
	e.hs.inject(room, "@bob:test.local", msg("see relux-works/skill-project-management@main-v2", nil))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("unrelated traffic delivered: %q", r.stdout)
	}
	id := e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "system", "landing", "relux-works/skill-project-management@main", "landed PR #9"))
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "sub #proj "+id)
	tuitestkit.ContainsStr(t, out, "kind=system subtype=landing subject=relux-works/skill-project-management@main")
}

// Proves --until-kind delivers matching events until the first one of that
// kind, delivers that closing event, drops later matches of the same batch,
// and removes the subscription when the closing event is persisted, so
// nothing further is delivered.
func TestSubUntilKindClosesAfterFirstClosingEvent(t *testing.T) {
	e, room := subWorld(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id, _ := e.subscribe("alice", "--subject", "BUG-1", "--until-kind", "system")
	first := e.hs.inject(room, "@bob:test.local", msg("working on BUG-1", nil))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "sub #proj "+first+" from=bob sub="+id)

	closing := e.hs.inject(room, "@bob:test.local", hdrMsg("bob", "system", "incident", "BUG-1", "closed"))
	late := e.hs.inject(room, "@bob:test.local", msg("BUG-1 again", nil))
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "sub #proj "+closing)
	tuitestkit.NotContainsStr(t, out, late)
	// The subscription closes once its closing event is durable in the mailbox
	// (the old code waited for the ack of the wait batch, the implicit ack of
	// defect 1 in research/waking-sessions-converged.md).
	if n := len(e.serverSubs("alice").Subs); n != 0 {
		t.Fatalf("sub kept after its closing event was persisted: %d left", n)
	}
	e.hs.inject(room, "@bob:test.local", msg("BUG-1 still?", nil))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("closed sub delivered: %q", r.stdout)
	}
	if n := len(e.serverSubs("alice").Subs); n != 0 {
		t.Fatalf("closed sub still stored: %d", n)
	}
}

// Proves an expired (--until in the past) subscription delivers nothing and is
// removed from account data at the next wait, while a live one in the same
// list keeps delivering (positive control).
func TestSubExpiredDeliversNothing(t *testing.T) {
	e, room := subWorld(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	e.hs.setAccountData("@alice:test.local", SubsType, SubList{V: 1, Subs: []Sub{
		{ID: "sub-0000dead", SubSelector: SubSelector{Kinds: []string{"decision"}, Until: past}, CreatedAt: legacyCreatedAt},
		{ID: "sub-0000beef", SubSelector: SubSelector{Kinds: []string{"answer"}}, CreatedAt: legacyCreatedAt},
	}})
	dead := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "decision", "after expiry"))
	live := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "answer", "still wanted"))
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "sub #proj "+live+" from=bob sub=sub-0000beef")
	tuitestkit.NotContainsStr(t, out, dead)
	if got := e.serverSubs("alice"); len(got.Subs) != 1 || got.Subs[0].ID != "sub-0000beef" {
		t.Fatalf("expired sub not removed: %+v", got.Subs)
	}
	tuitestkit.NotContainsStr(t, e.as("alice", "subs").ok(t).stdout, "sub-0000dead")
}

// Proves an unsubscribed subscription delivers nothing further, after it
// delivered while it existed (positive control).
func TestUnsubscribedDeliversNothing(t *testing.T) {
	e, room := subWorld(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id, _ := e.subscribe("alice", "--kind", "decision")
	ev := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "decision", "one"))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, ev)
	e.as("alice", "unsubscribe", id).ok(t)
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "decision", "two"))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("unsubscribed sub delivered: %q", r.stdout)
	}
}

// Proves the creation gate: events created before the subscription do not
// match even when they are still ahead of the cursor; with --since $ev, the
// events after that event (and not the event itself) match.
func TestSubCreationGateAndSince(t *testing.T) {
	t.Run("created_at", func(t *testing.T) {
		e, room := subWorld(t)
		e.as("alice", "wait", "--timeout", "10ms").ok(t)
		e.hs.inject(room, "@bob:test.local", msg("old BUG-7 note", nil))
		time.Sleep(5 * time.Millisecond)
		e.subscribe("alice", "--subject", "BUG-7")
		if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
			t.Fatalf("event older than the sub delivered: %q", r.stdout)
		}
		id := e.hs.inject(room, "@bob:test.local", msg("new BUG-7 note", nil))
		tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, id)
	})
	t.Run("since", func(t *testing.T) {
		e, room := subWorld(t)
		e.as("alice", "wait", "--timeout", "10ms").ok(t)
		e0 := e.hs.inject(room, "@bob:test.local", msg("BUG-7 zero", nil))
		e1 := e.hs.inject(room, "@bob:test.local", msg("BUG-7 one", nil))
		e2 := e.hs.inject(room, "@bob:test.local", msg("BUG-7 two", nil))
		time.Sleep(5 * time.Millisecond)
		_, sum := e.subscribe("alice", "--subject", "BUG-7", "--since", e1)
		tuitestkit.ContainsStr(t, sum, "since="+e1)
		out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
		tuitestkit.ContainsStr(t, out, e2)
		tuitestkit.NotContainsStr(t, out, e1+" ")
		tuitestkit.NotContainsStr(t, out, e0+" ")
		e.as("alice", "subscribe", "--subject", "BUG-7", "--since", "$nosuch").failsWith(t, ExitError, "not found")
	})
}

// Proves the legacy local `subscriptions` map is moved into account data on
// first use (one sub per room entry with its kinds), dropped from the state
// file, not duplicated on later runs, and delivers like before; a failed
// account-data write keeps the legacy map (negative control).
func TestSubsMigrateLegacyState(t *testing.T) {
	e, _ := world(t)
	lobby := e.hs.aliases["#lobby:"+testServer]
	legacy := config.State{LegacySubscriptions: map[string][]string{config.LegacyAllRooms: {"system"}, "!projRoomID": {"decision", "owner-needed"}}}
	if err := config.SaveState(e.paths(), "alice", legacy); err != nil {
		t.Fatal(err)
	}
	e.hs.fail(accountDataPut, 500, "M_UNKNOWN", "disk full")
	e.as("alice", "subs").failsWith(t, ExitError, "disk full")
	if st, _ := config.LoadState(e.paths(), "alice"); len(st.LegacySubscriptions) != 2 {
		t.Fatalf("legacy map dropped after a failed migration: %+v", st)
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, accountDataPut)
	e.hs.mu.Unlock()

	out := e.as("alice", "subs").ok(t).stdout
	lines := tuitestkit.LinesFromStr(strings.TrimSpace(out))
	if len(lines) != 2 {
		t.Fatalf("subs = %q", out)
	}
	tuitestkit.ContainsStr(t, out, "\troom=all kinds=system source=legacy")
	tuitestkit.ContainsStr(t, out, "\troom=#proj kinds=decision,owner-needed source=legacy")
	if st, _ := config.LoadState(e.paths(), "alice"); st.LegacySubscriptions != nil {
		t.Fatalf("legacy map kept: %+v", st.LegacySubscriptions)
	}
	if n := len(e.serverSubs("alice").Subs); n != 2 {
		t.Fatalf("account data holds %d subs", n)
	}
	e.as("alice", "subs").ok(t)
	if n := len(e.serverSubs("alice").Subs); n != 2 {
		t.Fatalf("second run duplicated: %d", n)
	}
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	id := e.hs.inject(lobby, "@bob:test.local", kindMsg("bob", "system", "migrated delivery"))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "sub #lobby "+id)
}

// Proves whole-token subject matching in bodies: the token matches at word
// boundaries and before trailing punctuation, never inside a longer token.
func TestContainsToken(t *testing.T) {
	cases := []struct {
		body, tok string
		want      bool
	}{
		{"fixes BUG-1", "BUG-1", true},
		{"BUG-1: done", "BUG-1", true},
		{"(BUG-1).", "BUG-1", true},
		{"see PR#12, merged", "PR#12", true},
		{"repo relux-works/x@main.", "relux-works/x@main", true},
		{"BUG-12", "BUG-1", false},
		{"xBUG-1", "BUG-1", false},
		{"BUG-1.5", "BUG-1", false},
		{"relux-works/x@main-v2", "relux-works/x@main", false},
		{"nothing here", "BUG-1", false},
	}
	for _, tc := range cases {
		if got := containsToken(tc.body, tc.tok); got != tc.want {
			t.Errorf("containsToken(%q, %q) = %v, want %v", tc.body, tc.tok, got, tc.want)
		}
	}
}
