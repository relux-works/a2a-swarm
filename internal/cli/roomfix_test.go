package cli

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves `room new` subscribes its creator to the new room (the standard join
// kinds), so a parent that creates a child's room hears it; an alias that
// already exists creates no subscription (the caller may not even be a member).
func TestRoomNewSubscribesCreator(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	out := e.as("alice", "room", "new", "child").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "created #child")
	tuitestkit.ContainsStr(t, out, "subscribed ")
	id := e.hs.aliases["#child:"+testServer]
	subs := e.serverSubs("alice").Subs
	if len(subs) != 1 || subs[0].Room != id {
		t.Fatalf("creator subscriptions = %+v, want one on %s", subs, id)
	}
	e.agent("bob")
	again := e.as("bob", "room", "new", "child").ok(t).stdout
	tuitestkit.ContainsStr(t, again, "exists #child")
	if got := e.serverSubs("bob").Subs; len(got) != 0 {
		t.Fatalf("an existing alias subscribed its non-creator: %+v", got)
	}
}

// Proves a refused space link is an error that says the room is NOT linked
// and names who can link it, while the room itself was created; the named
// space admin then adopts it (positive control).
func TestRoomNewProductLinkRefusedNamesLinkers(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.as("alice", "product", "new", "alpha").ok(t)
	e.agent("bob")
	e.as("bob", "join", "alpha").ok(t)
	r := e.as("bob", "room", "new", "alpha-tools", "--product", "alpha")
	r.failsWith(t, ExitError, "is NOT linked into product alpha")
	tuitestkit.ContainsStr(t, r.stderr, "alice (creator)")
	tuitestkit.ContainsStr(t, r.stderr, "a2a product adopt alpha #alpha-tools")
	if _, ok := e.hs.aliases["#alpha-tools:"+testServer]; !ok {
		t.Fatal("the room was not created")
	}
	e.as("alice", "product", "adopt", "alpha", "alpha-tools").ok(t)
	// alice is not a member of bob's room, so show lists it by id.
	tuitestkit.ContainsStr(t, e.as("alice", "product", "show", "alpha").ok(t).stdout, e.hs.aliases["#alpha-tools:"+testServer])
}

// Proves adopt links an existing room the caller is not a member of (its
// alias resolves, its state is unreadable), and refuses a bare room id it
// cannot verify instead of linking an unknown id.
func TestAdoptRoomNotMember(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.as("alice", "product", "new", "alpha").ok(t)
	carol := e.agent("carol")
	e.hs.addRoom("secret", carol)
	out := e.as("alice", "product", "adopt", "alpha", "secret").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "cannot read #secret (not a member)")
	tuitestkit.ContainsStr(t, out, "linked child")
	e.as("alice", "product", "adopt", "alpha", "!nosuchRoomID").failsWith(t, ExitError, "cannot be verified")
}

// Proves product new gives the named keeper power 50 in the product space
// (never 100), and that a re-run on an existing space raises a keeper that
// is missing there; the space creator stays implicit (room v12).
func TestProductNewKeeperPower(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.agent("bob")
	e.agent("dave")
	e.as("alice", "product", "new", "alpha", "--keeper", "bob").ok(t)
	if lvl := spacePower(t, e, "#alpha", "@bob:"+testServer); lvl != 50 {
		t.Fatalf("keeper power = %d, want 50", lvl)
	}
	out := e.as("alice", "product", "new", "alpha", "--keeper", "dave").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "keeper power @dave:"+testServer+" 50 in #alpha")
	if lvl := spacePower(t, e, "#alpha", "@dave:"+testServer); lvl != 50 {
		t.Fatalf("raised keeper power = %d, want 50", lvl)
	}
	tuitestkit.ContainsStr(t, e.as("alice", "product", "new", "alpha", "--keeper", "dave").ok(t).stdout, "already keeper power")
}

func spacePower(t *testing.T, e *env, alias, user string) int {
	t.Helper()
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var pl struct {
		Users map[string]int `json:"users"`
	}
	_ = json.Unmarshal(e.hs.rooms[e.hs.aliases[alias+":"+testServer]].state["m.room.power_levels|"], &pl)
	return pl.Users[user]
}

// Proves room retire removes an EMPTY room: it unlinks the room from its
// product space, deletes the alias, leaves and removes only that room's
// subscriptions; a room with a message is
// refused and keeps its alias (history is archived, never retired this way).
func TestRoomRetire(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.as("alice", "product", "new", "alpha").ok(t)
	e.as("alice", "room", "new", "keep").ok(t)
	e.as("alice", "room", "new", "alpha-oops", "--product", "alpha").ok(t)
	retiredID := e.hs.aliases["#alpha-oops:"+testServer]
	keptID := e.hs.aliases["#keep:"+testServer]
	out := e.as("alice", "room", "retire", "alpha-oops").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "unlinked from ")
	tuitestkit.ContainsStr(t, out, "deleted alias #alpha-oops")
	tuitestkit.ContainsStr(t, out, "left ")
	if _, ok := e.hs.aliases["#alpha-oops:"+testServer]; ok {
		t.Fatal("alias still exists after retire")
	}
	if strings.Contains(e.as("alice", "product", "show", "alpha").ok(t).stdout, "alpha-oops") {
		t.Fatal("retired room is still a live child of the space")
	}
	kept := false
	for _, sub := range e.serverSubs("alice").Subs {
		if sub.Room == retiredID {
			t.Fatal("retired room still has a subscription")
		}
		kept = kept || sub.Room == keptID
	}
	if !kept {
		t.Fatal("retirement removed an unrelated room's subscription")
	}
	e.as("alice", "room", "new", "busy").ok(t)
	e.as("alice", "topic", "new", "busy", "t1", "T").ok(t)
	e.as("alice", "room", "retire", "busy").failsWith(t, ExitError, "has messages")
	if _, ok := e.hs.aliases["#busy:"+testServer]; !ok {
		t.Fatal("a room with messages lost its alias")
	}
	e.as("alice", "room", "retire", "ghost").failsWith(t, ExitError, "does not exist")
}

// Proves --product without --preset names the valid presets in its usage error.
func TestPresetErrorListsPresets(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.as("alice", "host", "init", "mini").ok(t)
	e.run("identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha").
		failsWith(t, ExitUsage, "one of: orch, keeper, meta-keeper, arbiter, service")
}

// Proves filtered empty pages are followed to a terminal page: older messages
// prevent retirement; a truly empty terminal history permits it. Cycles,
// excessive pagination and later read failures refuse without changing the
// alias, space links, membership or subscriptions. This checks the read phase,
// not messages posted concurrently after that phase.
func TestRoomRetireFilteredPages(t *testing.T) {
	type retirementCase struct {
		name      string
		pages     map[string]fakeMessagePage
		wantError string
		wantReads int
	}
	tests := []retirementCase{
		{name: "older message", pages: map[string]fakeMessagePage{
			"":      {page: matrix.Page{End: "older"}},
			"older": {page: matrix.Page{Chunk: []matrix.Event{{Type: "m.room.message", EventID: "$history"}}}},
		}, wantError: "has messages", wantReads: 2},
		{name: "terminal empty", pages: map[string]fakeMessagePage{
			"": {page: matrix.Page{End: "older"}}, "older": {page: matrix.Page{}},
		}, wantReads: 2},
		{name: "stalled token", pages: map[string]fakeMessagePage{
			"": {page: matrix.Page{End: "older"}}, "older": {page: matrix.Page{End: "older"}},
		}, wantError: "pagination repeated", wantReads: 2},
		{name: "token cycle", pages: map[string]fakeMessagePage{
			"": {page: matrix.Page{End: "a"}}, "a": {page: matrix.Page{End: "b"}}, "b": {page: matrix.Page{End: "a"}},
		}, wantError: "pagination repeated", wantReads: 3},
		{name: "later read failure", pages: map[string]fakeMessagePage{
			"": {page: matrix.Page{End: "older"}}, "older": {failure: &fakeFailure{status: 503, errcode: "M_UNKNOWN", message: "history unavailable"}},
		}, wantError: "HTTP 503", wantReads: 2},
	}
	// Fixed contract: 100 pages without a terminal result must refuse, even
	// when a terminal empty page exists just beyond the cap.
	many := map[string]fakeMessagePage{}
	from := ""
	for i := 0; i < 100; i++ {
		next := fmt.Sprintf("page-%d", i)
		many[from] = fakeMessagePage{page: matrix.Page{End: next}}
		from = next
	}
	many[from] = fakeMessagePage{page: matrix.Page{}}
	tests = append(tests, retirementCase{
		name: "page limit", pages: many, wantError: "100 pages", wantReads: 100,
	})
	for _, message := range []bool{false, true} {
		pages := make(map[string]fakeMessagePage, len(many))
		for token, page := range many {
			pages[token] = page
		}
		last := matrix.Page{}
		name, wantError := "terminal at page limit", ""
		if message {
			last.Chunk = []matrix.Event{{Type: "m.room.message", EventID: "$history"}}
			name, wantError = "message at page limit", "has messages"
		}
		pages["page-98"] = fakeMessagePage{page: last}
		tests = append(tests, retirementCase{name: name, pages: pages, wantError: wantError, wantReads: 100})
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			alice := e.agent("alice")
			e.hs.makePublic(e.hs.addRoom("lobby", alice))
			e.as("alice", "product", "new", "alpha").ok(t)
			e.as("alice", "room", "new", "alpha-room", "--product", "alpha").ok(t)
			room := e.hs.aliases["#alpha-room:"+testServer]
			space := e.hs.aliases["#alpha:"+testServer]
			beforeSubs := e.serverSubs("alice")
			e.hs.mu.Lock()
			beforeLink := append(json.RawMessage(nil), e.hs.rooms[space].state["m.space.child|"+room]...)
			e.hs.messagePages = map[string]map[string]fakeMessagePage{room: tc.pages}
			e.hs.mu.Unlock()
			e.hs.Reset()
			result := e.as("alice", "room", "retire", "alpha-room")
			if tc.wantError == "" {
				result.ok(t)
				tuitestkit.ContainsStr(t, result.stdout, "deleted alias #alpha-room")
			} else {
				result.failsWith(t, ExitError, tc.wantError)
				e.hs.mu.Lock()
				if e.hs.aliases["#alpha-room:"+testServer] != room || e.hs.rooms[room].members[alice] != "join" || !reflect.DeepEqual(beforeLink, e.hs.rooms[space].state["m.space.child|"+room]) {
					t.Error("refused retirement changed alias, membership or space link")
				}
				e.hs.mu.Unlock()
				if !reflect.DeepEqual(beforeSubs, e.serverSubs("alice")) {
					t.Error("refused retirement changed subscriptions")
				}
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "DELETE /_matrix/client/v3/directory/room/{alias}")
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "POST /_matrix/client/v3/rooms/{room}/leave")
				tuitestkit.AssertNotCalled(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/state/{type}/{key}")
			}
			tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "GET /_matrix/client/v3/rooms/{room}/messages", tc.wantReads)
		})
	}
}
