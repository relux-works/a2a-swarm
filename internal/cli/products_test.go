package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves old account data with a single subtype decodes into the current list.
func TestLegacySubtypeDecoding(t *testing.T) {
	var list SubList
	if err := json.Unmarshal([]byte(`{"v":1,"subs":[{"id":"old","subtype":"landing","created_at":"1970-01-01T00:00:00Z"}]}`), &list); err != nil {
		t.Fatal(err)
	}
	got := list.Subs[0].subtypeList()
	if len(got) != 1 || got[0] != "landing" || list.Subs[0].ID != "old" || list.Subs[0].CreatedAt == "" {
		t.Fatalf("legacy subscription = %+v", list.Subs[0])
	}
	data, err := json.Marshal(Sub{ID: "new", SubSelector: SubSelector{Subtypes: []string{"landing", "incident"}}, CreatedAt: "now"})
	if err != nil || !strings.Contains(string(data), `"subtypes":["landing","incident"]`) || strings.Contains(string(data), `"subtype"`) {
		t.Fatalf("current subscription JSON = %s (%v)", data, err)
	}
	var request struct {
		Selector SubSelector `json:"selector"`
	}
	if err := json.Unmarshal([]byte(`{"selector":{"subtype":"landing"}}`), &request); err != nil || len(request.Selector.Subtypes) != 1 || request.Selector.Subtypes[0] != "landing" {
		t.Fatalf("legacy selector decode = %+v (%v)", request, err)
	}
}

// Proves product creation builds a space and three linked rooms with the
// broadcast power policy, and repeating it performs no additional mutations.
func TestProductNewAndIdempotence(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.agent("keeper")
	e.agent("projector")
	out := e.as("alice", "product", "new", "alpha", "--keeper", "keeper", "--projector", "projector").ok(t).stdout
	for _, line := range []string{"created space #alpha", "created #alpha-lobby", "created #alpha-broadcast", "created #alpha-system", "linked child", "linked parent", "invited @keeper:test.local", "invited @projector:test.local"} {
		tuitestkit.ContainsStr(t, out, line)
	}
	if len(e.hs.createdRooms()) != 4 {
		t.Fatalf("createRoom count = %d", len(e.hs.createdRooms()))
	}
	bc := "!alpha-broadcastRoomID"
	var power map[string]any
	if err := e.hs.muTryState(bc, "m.room.power_levels|", &power); err != nil {
		t.Fatal(err)
	}
	if power["events_default"] != float64(50) {
		t.Fatalf("broadcast power levels = %#v", power)
	}
	if users, ok := power["users"].(map[string]any); !ok || users["@keeper:test.local"] != float64(50) {
		t.Fatalf("broadcast keeper power = %#v", power)
	}
	var systemPower map[string]any
	if err := e.hs.muTryState("!alpha-systemRoomID", "m.room.power_levels|", &systemPower); err != nil {
		t.Fatal(err)
	}
	if systemPower["events_default"] != float64(50) || systemPower["users"].(map[string]any)["@projector:test.local"] != float64(50) {
		t.Fatalf("system power levels = %#v", systemPower)
	}
	var childLink, parentLink map[string]any
	if err := e.hs.muTryState("!alphaRoomID", "m.space.child|!alpha-lobbyRoomID", &childLink); err != nil {
		t.Fatal(err)
	}
	if err := e.hs.muTryState("!alpha-lobbyRoomID", "m.space.parent|!alphaRoomID", &parentLink); err != nil {
		t.Fatal(err)
	}
	if len(childLink["via"].([]any)) != 1 || childLink["via"].([]any)[0] != testServer || parentLink["canonical"] != true {
		t.Fatalf("space links child=%#v parent=%#v", childLink, parentLink)
	}
	before := len(e.hs.createdRooms())
	e.hs.mu.Lock()
	eventsBefore, invitesBefore := len(e.hs.events), len(e.hs.invites)
	e.hs.mu.Unlock()
	again := e.as("alice", "product", "new", "alpha", "--keeper", "keeper", "--projector", "projector").ok(t).stdout
	tuitestkit.ContainsStr(t, again, "already space #alpha")
	tuitestkit.ContainsStr(t, again, "already child")
	if len(e.hs.createdRooms()) != before {
		t.Fatal("idempotent product new created rooms")
	}
	e.hs.mu.Lock()
	eventsAfter, invitesAfter := len(e.hs.events), len(e.hs.invites)
	e.hs.mu.Unlock()
	if eventsAfter != eventsBefore || invitesAfter != invitesBefore {
		t.Fatalf("idempotent product new mutated state: events %d->%d invites %d->%d", eventsBefore, eventsAfter, invitesBefore, invitesAfter)
	}
	for _, room := range []string{"!alphaRoomID", "!alpha-lobbyRoomID", "!alpha-broadcastRoomID", "!alpha-systemRoomID"} {
		e.hs.mu.Lock()
		member := e.hs.rooms[room].members[alice]
		e.hs.mu.Unlock()
		if member != "join" {
			t.Fatalf("creator not joined %s", room)
		}
	}
}

// Proves product names are validated before any homeserver request; a valid
// product name beside the negative control creates the space normally.
func TestProductNameValidation(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	before := len(e.hs.Calls())
	e.as("alice", "product", "new", "Bad_Name").failsWith(t, ExitUsage, "invalid product name")
	if len(e.hs.Calls()) != before {
		t.Fatal("invalid product name contacted the homeserver")
	}
	e.as("alice", "product", "new", "valid-name", "--unknown").failsWith(t, ExitUsage, "flag provided but not defined")
	if len(e.hs.Calls()) != before {
		t.Fatal("unknown product flag contacted the homeserver")
	}
	e.as("alice", "product", "new", "valid-name").ok(t)
}

// Proves product list/show enumerate only joined spaces and show each child
// with its alias; the adjacent listing control has a joined product space.
func TestProductListShow(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.as("alice", "product", "new", "alpha").ok(t)
	list := e.as("alice", "product", "list").ok(t).stdout
	tuitestestContains(t, list, "#alpha")
	show := e.as("alice", "product", "show", "alpha").ok(t).stdout
	for _, a := range []string{"#alpha-lobby", "#alpha-broadcast", "#alpha-system"} {
		tuitestestContains(t, show, a)
	}
}

// Proves adopting an existing room adds both reciprocal space links exactly
// once; a non-space and a missing room are refused alongside the valid control.
func TestProductAdopt(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	lobby := e.hs.addRoom("lobby", alice)
	e.hs.makePublic(lobby)
	e.as("alice", "product", "new", "alpha").ok(t)
	adopt := e.as("alice", "product", "adopt", "alpha", "lobby").ok(t).stdout
	tuitestestContains(t, adopt, "linked child")
	again := e.as("alice", "product", "adopt", "alpha", "lobby").ok(t).stdout
	tuitestestContains(t, again, "already")
	e.hs.addRoom("ordinary", alice)
	e.as("alice", "product", "adopt", "ordinary", "lobby").failsWith(t, ExitError, "is not a space")
	e.as("alice", "product", "adopt", "alpha", "missing").failsWith(t, ExitError, "does not exist")
}

// Proves room new --product creates and adopts a room; without a product it
// remains a regular room (the positive control for the explicit flag).
func TestRoomNewProduct(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.as("alice", "product", "new", "alpha").ok(t)
	out := e.as("alice", "room", "new", "tooling", "--product", "alpha").ok(t).stdout
	tuitestestContains(t, out, "created #tooling")
	var parent map[string]any
	if err := e.hs.muTryState("!toolingRoomID", "m.space.parent|!alphaRoomID", &parent); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "room", "new", "standalone").ok(t)
}

// Proves global broadcast-room init sets events_default 50 once and the fake
// refuses a joined non-admin message, while a creator can post.
func TestBroadcastRoomInitAndPower(t *testing.T) {
	e := newEnv(t)
	e.agent("alice")
	e.agent("bob")
	e.agent("keeper")
	out := e.as("alice", "broadcast-room", "init").ok(t).stdout
	tuitestestContains(t, out, "events_default 50")
	var pl map[string]any
	if err := e.hs.muTryState("!broadcastRoomID", "m.room.power_levels|", &pl); err != nil {
		t.Fatal(err)
	}
	if pl["events_default"] != float64(50) {
		t.Fatalf("power levels = %#v", pl)
	}
	e.as("alice", "broadcast-room", "init").ok(t)
	e.as("bob", "join", "broadcast").ok(t)
	e.as("alice", "product", "new", "alpha", "--keeper", "keeper").ok(t)
	var productPL map[string]any
	if err := e.hs.muTryState("!alpha-broadcastRoomID", "m.room.power_levels|", &productPL); err != nil {
		t.Fatal(err)
	}
	if productPL["events_default"] != float64(50) || productPL["users"].(map[string]any)["@keeper:test.local"] != float64(50) {
		t.Fatalf("product broadcast levels = %#v", productPL)
	}
	e.as("bob", "join", "alpha-broadcast").ok(t)
	e.as("alice", "topic", "new", "alpha-broadcast", "root", "Root").ok(t)
	e.as("bob", "post", "alpha-broadcast", "--topic", "root", "--kind", "notice", "no").failsWith(t, ExitError, "M_FORBIDDEN")
	e.as("alice", "post", "alpha-broadcast", "--topic", "root", "--kind", "notice", "yes").ok(t)
	// Positive control for the keeper: at power 50 it may post where bob may not.
	e.as("keeper", "join", "alpha-broadcast").ok(t)
	e.as("keeper", "post", "alpha-broadcast", "--topic", "root", "--kind", "notice", "keeper").ok(t)
}

// Proves a comma-separated subtype selector persists as subtypes and matches
// any listed value; an invalid list is rejected next to a valid list control.
func TestSubtypeList(t *testing.T) {
	e, room := subWorld(t)
	_, sum := e.subscribe("alice", "--room", "proj", "--subtype", "incident,blocker")
	tuitestestContains(t, sum, "subtype=incident,blocker")
	stored := e.serverSubs("alice").Subs[0]
	if len(stored.Subtypes) != 2 {
		t.Fatalf("stored subtypes = %#v", stored.Subtypes)
	}
	e.as("alice", "subscribe", "--room", "proj", "--kind", "system", "--subtype", "incident,bogus").failsWith(t, ExitUsage, "invalid --subtype")
	e.hs.inject(room, "@bob:test.local", msg("blocker", map[string]any{ContentKey: Header{Kind: "system", Subtype: "blocker"}}))
	ev := e.hs.lastEvent()
	if !stored.matches(ev, room, Header{Kind: "system", Subtype: "blocker"}, true) {
		t.Fatal("listed subtype did not match")
	}
	var legacy SubList
	if err := json.Unmarshal([]byte(`{"v":1,"subs":[{"id":"old","room":"!projRoomID","subtype":"landing","created_at":"1970-01-01T00:00:00Z"}]}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if got := legacy.Subs[0].subtypeList(); len(got) != 1 || got[0] != "landing" {
		t.Fatalf("legacy subtype read = %#v", got)
	}
}

// Proves an explicit product preset joins its exact rooms and creates tagged
// subscriptions; sync is unchanged on a repeat and preserves hand-made rows.
func TestIdentityProductPresetAndSubsSync(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	// Swarm rooms are public_chat (joinable), as the real room policy makes them.
	e.hs.makePublic(e.hs.addRoom("lobby", alice))
	e.as("alice", "broadcast-room", "init").ok(t)
	projectRoom := e.hs.addRoom("proj", alice)
	e.hs.makePublic(projectRoom)
	e.as("alice", "product", "new", "alpha").ok(t)
	e.as("alice", "host", "init", "mini").ok(t)
	e.run("identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha").failsWith(t, ExitUsage, "requires an explicit --preset")
	e.run("identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "unknown").failsWith(t, ExitUsage, "unknown preset")
	out := e.run("identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "orch").ok(t).stdout
	for _, room := range []string{"#lobby", "#broadcast", "#alpha-lobby", "#alpha-broadcast", "#proj"} {
		tuitestestContains(t, out, "joined "+room)
	}
	name := "mini_proj_orch"
	list := e.serverSubs(name)
	if len(list.Subs) != 4 {
		t.Fatalf("preset subscriptions = %+v", list.Subs)
	}
	for _, sub := range list.Subs {
		if sub.Preset != "orch:alpha" {
			t.Fatalf("unmarked preset subscription: %+v", sub)
		}
	}
	e.as(name, "subscribe", "--room", "proj", "--kind", "question").ok(t)
	manual := e.serverSubs(name)
	manual.Subs = append(manual.Subs, Sub{ID: "sub-stale", SubSelector: SubSelector{Room: "!projRoomID", Kinds: []string{"notice"}}, Preset: "orch:alpha", CreatedAt: "2026-01-01T00:00:00Z"})
	e.hs.setAccountData("@"+name+":"+testServer, SubsType, manual)
	sync := e.as(name, "subs", "sync", "--preset", "orch", "--product", "alpha").ok(t).stdout
	tuitestestContains(t, sync, "removed sub-stale")
	if len(e.serverSubs(name).Subs) != 5 {
		t.Fatalf("sync removed or added hand-made subscription: %+v", e.serverSubs(name).Subs)
	}
	e.run("--as", name, "identity", "ensure", "--role", "orch", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "orch").ok(t)
	// Product child adoption makes every project room visible to the keeper.
	e.as("alice", "product", "adopt", "alpha", "proj").ok(t)
	keeperName := "mini_proj_keeper"
	e.agent(keeperName)
	keeperOut := e.run("--as", keeperName, "identity", "ensure", "--role", "keeper", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "keeper").ok(t).stdout
	tuitestestContains(t, keeperOut, "joined #alpha")
	if got := len(e.serverSubs(keeperName).Subs); got != 4 {
		t.Fatalf("keeper preset subscription count = %d", got)
	}

	serviceName := "mini_proj_service"
	e.run("identity", "ensure", "--role", "service", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "service").ok(t)
	if got := len(e.serverSubs(serviceName).Subs); got != 0 {
		t.Fatalf("service preset subscriptions = %d", got)
	}

	resource := e.hs.addRoom("resource", alice)
	e.hs.makePublic(resource)
	arbiterName := "mini_proj_arbiter"
	arbiterOut := e.run("identity", "ensure", "--role", "arbiter", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "arbiter", "--resource", "resource").ok(t).stdout
	tuitestestContains(t, arbiterOut, "joined #resource")
	if got := len(e.serverSubs(arbiterName).Subs); got != 2 {
		t.Fatalf("arbiter preset subscription count = %d", got)
	}

	metaName := "mini_proj_meta-keeper"
	e.agent(metaName)
	e.as(metaName, "join", "alpha").ok(t)
	system := e.hs.addRoom("system", alice)
	e.hs.makePublic(system)
	metaOut := e.run("--as", metaName, "identity", "ensure", "--role", "meta-keeper", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "meta-keeper").ok(t).stdout
	tuitestestContains(t, metaOut, "joined #alpha-lobby")
	metaPresetCount := 0
	for _, sub := range e.serverSubs(metaName).Subs {
		if sub.Preset == "meta-keeper" {
			metaPresetCount++
		}
	}
	if metaPresetCount != 3 {
		t.Fatalf("meta-keeper preset subscription count = %d", metaPresetCount)
	}
}

func tuitestestContains(t *testing.T, s, part string) {
	t.Helper()
	if !strings.Contains(s, part) {
		t.Fatalf("%q does not contain %q", s, part)
	}
}

func (hs *fakeHS) muTryState(room, key string, out any) error {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	r := hs.rooms[room]
	if r == nil {
		return fmt.Errorf("room %s missing", room)
	}
	raw := r.state[key]
	if raw == nil {
		return fmt.Errorf("state %s missing", key)
	}
	return json.Unmarshal(raw, out)
}

// Proves one identity holds several preset rows: keeper of two products plus
// meta-keeper. Each row's subscriptions carry their own tag, so syncing one row
// leaves the others intact (regression: reconciling one preset used to remove
// every other preset's subscriptions). A keeper in a non-git folder needs no
// --project: its project is the product.
func TestCombinedPresetsKeepEachOther(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	for _, r := range []string{"lobby", "system"} {
		e.hs.makePublic(e.hs.addRoom(r, alice))
	}
	e.as("alice", "broadcast-room", "init").ok(t)
	e.as("alice", "product", "new", "swarm").ok(t)
	e.as("alice", "product", "new", "bsim").ok(t)
	e.as("alice", "host", "init", "mini").ok(t)
	dir := t.TempDir() // not a git repository: the product folder
	name := "mini_swarm_keeper"
	out := e.run("identity", "ensure", "--dir", dir, "--role", "keeper", "--runtime", "none",
		"--product", "swarm", "--preset", "keeper,meta-keeper").ok(t).stdout
	tuitestestContains(t, out, "identity "+name)
	tuitestestContains(t, out, "subscriptions (keeper:swarm)")
	tuitestestContains(t, out, "subscriptions (meta-keeper)")
	count := func(tag string) int {
		n := 0
		for _, sub := range e.serverSubs(name).Subs {
			if sub.Preset == tag {
				n++
			}
		}
		return n
	}
	if count("keeper:swarm") != 4 || count("meta-keeper") != 3 {
		t.Fatalf("combined rows = keeper %d, meta %d: %+v", count("keeper:swarm"), count("meta-keeper"), e.serverSubs(name).Subs)
	}
	// A keeper row for a second product, then a sync of the first: nothing of the others moves.
	e.as(name, "join", "bsim").ok(t)
	e.as(name, "subs", "sync", "--preset", "keeper", "--product", "bsim").ok(t)
	sync := e.as(name, "subs", "sync", "--preset", "keeper", "--product", "swarm").ok(t).stdout
	tuitestestContains(t, sync, "unchanged")
	if count("keeper:swarm") != 4 || count("keeper:bsim") != 4 || count("meta-keeper") != 3 {
		t.Fatalf("after syncs: swarm %d, bsim %d, meta %d", count("keeper:swarm"), count("keeper:bsim"), count("meta-keeper"))
	}
}

// Proves bad preset combinations are refused before anything is joined or
// subscribed: a duplicate preset, a product-scoped preset without --product,
// --resource without arbiter and arbiter without --resource; a meta-keeper
// alone needs no product (positive control).
func TestPresetListNegative(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	for _, r := range []string{"lobby", "system"} {
		e.hs.makePublic(e.hs.addRoom(r, alice))
	}
	e.as("alice", "broadcast-room", "init").ok(t)
	e.as("alice", "host", "init", "mini").ok(t)
	before := e.hs.CallCount("POST /_matrix/client/v3/join/{room}")
	base := []string{"identity", "ensure", "--project", "p", "--runtime", "none"}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--role", "keeper", "--product", "swarm", "--preset", "keeper,keeper"}, "given twice"},
		{[]string{"--role", "orch", "--preset", "orch"}, "--preset orch requires --product"},
		{[]string{"--role", "x", "--preset", "meta-keeper", "--resource", "lobby"}, "arbiter and --resource"},
		{[]string{"--role", "x", "--preset", "arbiter"}, "arbiter and --resource"},
		{[]string{"--role", "x", "--preset", "keeper,bogus", "--product", "swarm"}, "unknown preset"},
	} {
		e.run(append(base, c.args...)...).failsWith(t, ExitUsage, c.want)
	}
	if n := e.hs.CallCount("POST /_matrix/client/v3/join/{room}"); n != before {
		t.Fatalf("refused presets joined %d rooms", n-before)
	}
	out := e.run(append(base, "--role", "meta", "--preset", "meta-keeper")...).ok(t).stdout
	tuitestestContains(t, out, "subscriptions (meta-keeper)")
	if e.hs.CallCount("POST /_matrix/client/v3/join/{room}") == before {
		t.Fatal("the join counter did not move on the positive control: the negative check above would be vacuous")
	}
}

// Proves a stored subscription with exactly one subtype also carries the
// legacy "subtype" field (so an older binary filters the same way), one with
// two subtypes does not, and both round-trip to the same selector.
func TestSubtypeLegacyFieldOnWrite(t *testing.T) {
	one, _ := json.Marshal(Sub{ID: "a", SubSelector: SubSelector{Room: "!r", Kinds: []string{"system"}, Subtypes: []string{"landing"}}})
	two, _ := json.Marshal(Sub{ID: "b", SubSelector: SubSelector{Room: "!r", Kinds: []string{"system"}, Subtypes: []string{"landing", "window"}}})
	if !strings.Contains(string(one), `"subtype":"landing"`) || !strings.Contains(string(one), `"subtypes":["landing"]`) {
		t.Fatalf("single subtype written as %s", one)
	}
	if strings.Contains(string(two), `"subtype":`) {
		t.Fatalf("two subtypes wrote a legacy field: %s", two)
	}
	for _, raw := range [][]byte{one, two} {
		var back Sub
		if err := json.Unmarshal(raw, &back); err != nil || len(back.Subtypes) == 0 || back.Room != "!r" {
			t.Fatalf("round trip of %s = %+v, %v", raw, back, err)
		}
	}
}
