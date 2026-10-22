package cli

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

const projectJoinRoute = "POST /_matrix/client/v3/join/{room}"

// These observations include rejected requests as well as accepted events.
func projectPaths(e *env, contains string) []string {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var paths []string
	for _, raw := range e.hs.rawURLs {
		u, err := url.Parse(raw)
		if err != nil {
			e.t.Fatal(err)
		}
		if strings.Contains(u.Path, contains) {
			paths = append(paths, u.Path)
		}
	}
	return paths
}

func projectCursor(t *testing.T, e *env) string {
	t.Helper()
	st, err := config.LoadState(e.paths(), "proj")
	if err != nil {
		t.Fatal(err)
	}
	return st.NextBatch
}

func TestProjectConfigFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		args   []string
		code   int
		reason string
	}{
		{"missing target", []string{"--source", "proj"}, ExitUsage, "--system"},
		{"empty target", []string{"--system", "", "--source", "proj"}, ExitUsage, "--system"},
		{"missing sources", []string{"--system", "system"}, ExitUsage, "--source"},
		{"empty source", []string{"--system", "system", "--source", ""}, ExitUsage, "invalid projector room"},
		{"empty among valid sources", []string{"--system", "system", "--source", "proj", "--source", ""}, ExitUsage, "invalid projector room"},
		{"whitespace target", []string{"--system", " \t", "--source", "proj"}, ExitUsage, "invalid projector room"},
		{"whitespace source", []string{"--system", "system", "--source", " "}, ExitUsage, "invalid projector room"},
		{"malformed target alias", []string{"--system", "bad/target", "--source", "proj"}, ExitUsage, "resolve --system"},
		{"malformed source alias", []string{"--system", "system", "--source", "bad/source"}, ExitUsage, "resolve --source"},
		{"empty target ID", []string{"--system", "!", "--source", "proj"}, ExitUsage, "invalid projector room"},
		{"empty source ID", []string{"--system", "system", "--source", "!"}, ExitUsage, "invalid projector room"},
		{"control in ID", []string{"--system", "system", "--source", "!bad\n:test.local"}, ExitUsage, "invalid projector room"},
		{"unknown target", []string{"--system", "absent", "--source", "proj"}, ExitError, "resolve --system: unknown room alias"},
		{"unknown source after valid", []string{"--system", "system", "--source", "proj", "--source", "absent"}, ExitError, "resolve --source: unknown room alias"},
	}
	for _, tc := range cases {
		for _, existing := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/existing=%t", tc.name, existing), func(t *testing.T) {
				e, room, system := projWorld(t)
				if existing {
					e.project().ok(t)
				}
				side := e.hs.addRoom("side", "@bob:test.local")
				e.as("bob", "invite", "side", "proj").ok(t)
				e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "pending"))
				before, readErr := os.ReadFile(e.paths().StateFile("proj"))
				syncs := e.hs.CallCount("GET /_matrix/client/v3/sync")
				e.as("proj", append([]string{"project", "--once", "--poll", "1ms"}, tc.args...)...).failsWith(t, tc.code, tc.reason)
				after, afterErr := os.ReadFile(e.paths().StateFile("proj"))
				if !reflect.DeepEqual(before, after) || (readErr == nil) != (afterErr == nil) {
					t.Fatal("invalid config changed durable state")
				}
				if e.hs.CallCount("GET /_matrix/client/v3/sync") != syncs || len(projectPaths(e, "/join/")) != 0 || len(projectPaths(e, "/send/")) != 0 {
					t.Fatal("invalid config caused sync/join/send")
				}
				e.hs.mu.Lock()
				membership := e.hs.rooms[side].members["@proj:test.local"]
				e.hs.mu.Unlock()
				if membership != "invite" || len(e.projections(system)) != 0 {
					t.Fatal("invalid config changed membership/events")
				}
			})
		}
	}
}

func TestProjectRejectsInvalidResolution(t *testing.T) {
	for _, selector := range []string{"system", "proj"} {
		for _, bad := range []string{"", "!", "not-a-room-id", "!bad\nID"} {
			t.Run(selector+"/"+bad, func(t *testing.T) {
				e, _, _ := projWorld(t)
				e.hs.mu.Lock()
				e.hs.aliases["#"+selector+":test.local"] = bad
				e.hs.mu.Unlock()
				e.project().failsWith(t, ExitError, "invalid")
				if e.hs.CallCount("GET /_matrix/client/v3/sync") != 0 || len(projectPaths(e, "/join/")) != 0 || len(projectPaths(e, "/send/")) != 0 || projectCursor(t, e) != "" {
					t.Fatal("bad directory response caused effects")
				}
			})
		}
	}
}

// Give public fixtures server-qualified IDs while preserving the normal fake
// homeserver behavior (the shared fake also supports opaque !roomid forms).
func projectFixtureID(e *env, old, id string) string {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	room := e.hs.rooms[old]
	delete(e.hs.rooms, old)
	room.id = id
	e.hs.rooms[id] = room
	e.hs.aliases[room.alias] = id
	return id
}

func TestProjectSourceAdmissionByActualID(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(fmt.Sprintf("initial=%t", initial), func(t *testing.T) {
			e, room, system := projWorld(t)
			side := projectFixtureID(e, e.hs.addRoom("side", "@bob:test.local"), "!allowedSide:test.local")
			foreign := projectFixtureID(e, e.hs.addRoom("foreign", "@bob:test.local"), "!foreignInvite:elsewhere.test")
			// Canonical alias/name in invite_state is untrusted, even if identical to
			// the allowlisted alias. The directory still resolves side to its own ID.
			e.hs.mu.Lock()
			e.hs.rooms[foreign].alias = "#side:test.local"
			e.hs.rooms[foreign].name = "side"
			e.hs.mu.Unlock()
			if !initial {
				e.project().ok(t)
			}
			e.as("bob", "invite", side, "proj").ok(t)
			e.as("bob", "invite", foreign, "proj").ok(t)
			e.project("--source", "#side:test.local", "--source", side, "--source", "side").ok(t)
			joins := projectPaths(e, "/join/")
			if !reflect.DeepEqual(joins, []string{"/_matrix/client/v3/join/" + side}) {
				t.Fatalf("join requests = %v; want only allowlisted side", joins)
			}
			e.hs.mu.Lock()
			allowedMembership, foreignMembership := e.hs.rooms[side].members["@proj:test.local"], e.hs.rooms[foreign].members["@proj:test.local"]
			e.hs.mu.Unlock()
			if allowedMembership != "join" || foreignMembership != "invite" {
				t.Fatalf("memberships allowed=%s foreign=%s", allowedMembership, foreignMembership)
			}
			a := e.hs.inject(side, "@bob:test.local", kindMsg("bob", "owner-needed", "allowed invited source"))
			b := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "allowed already joined"))
			e.hs.inject(foreign, "@bob:test.local", kindMsg("bob", "system", "uninvited foreign"))
			e.project("--source", side).ok(t)
			got := e.projections(system)
			origins := map[string]bool{}
			for _, ev := range got {
				origins[projectionOf(t, ev).OriginEvent] = true
			}
			if len(got) != 2 || !origins[a] || !origins[b] || len(projectPaths(e, "/send/")) != 2 {
				t.Fatalf("source admission projections = %+v", got)
			}
		})
	}
}

func TestProjectIgnoresUnlistedJoinedBeforeBackfill(t *testing.T) {
	e, room, system := projWorld(t)
	other := e.hs.addRoom("outside", "@proj:test.local", "@bob:test.local")
	e.hs.syncLimit = 1
	e.project().ok(t)
	e.hs.inject(other, "@bob:test.local", kindMsg("bob", "system", "must not backfill"))
	e.hs.inject(other, "@bob:test.local", kindMsg("bob", "owner-needed", "must not project"))
	id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "positive"))
	e.project().ok(t)
	got := e.projections(system)
	if len(got) != 1 || projectionOf(t, got[0]).OriginEvent != id || len(projectPaths(e, "/send/")) != 1 {
		t.Fatalf("unlisted joined source projected: %+v", got)
	}
	if n := e.hs.CallCount(messagesRoute); n != 0 {
		t.Fatalf("unlisted source caused %d backfills", n)
	}
}

func TestProjectOnlyExplicitTargetWithForeignProduct(t *testing.T) {
	e, room, defaultTarget := projWorld(t)
	_, foreign := addProductSpace(e, "alpha", room)
	target := projectFixtureID(e, e.hs.addRoom("chosen", "@proj:test.local"), "!chosenTarget:test.local")
	for _, targetArg := range []string{"chosen", "#chosen", "#chosen:test.local", target} {
		e.project("--system", targetArg, "--source", "#proj:test.local", "--source", room).ok(t)
		id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "positive; foreign target would accept"))
		e.project("--system", targetArg).ok(t)
		got := e.projections(target)
		p := projectionOf(t, got[len(got)-1])
		if p.OriginEvent != id || p.ProductAlias != "#alpha" || p.Product == "" {
			t.Fatalf("missing positive metadata control: %+v", p)
		}
	}
	if len(e.projections(target)) != 4 || len(e.projections(defaultTarget)) != 0 || len(e.projections(foreign)) != 0 {
		t.Fatal("projection destinations differ from explicit target")
	}
	for _, path := range projectPaths(e, "/send/") {
		if !strings.HasPrefix(path, "/_matrix/client/v3/rooms/"+target+"/send/") {
			t.Fatalf("forbidden send request: %s", path)
		}
	}
	if len(projectPaths(e, "/send/")) != 4 {
		t.Fatal("unexpected send count")
	}
	// Even a resolution attempt to the foreign product destination is forbidden.
	for _, path := range projectPaths(e, "/directory/room/") {
		if strings.Contains(path, "#alpha-system:") {
			t.Fatalf("product target resolution attempted: %s", path)
		}
	}
	// Nearby valid control: the same identity can write to the foreign room
	// when explicitly configured to do so. Permission denial cannot mask fanout.
	e.project("--system", "alpha-system").ok(t)
	id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "explicit foreign control"))
	e.project("--system", "alpha-system").ok(t)
	if got := e.projections(foreign); len(got) != 1 || projectionOf(t, got[0]).OriginEvent != id {
		t.Fatalf("explicit foreign target positive control = %v", got)
	}
}

func TestProjectRetryPinsTargetAndTransaction(t *testing.T) {
	e, room, target := projWorld(t)
	_, foreign := addProductSpace(e, "alpha", room)
	e.project().ok(t)
	before := projectCursor(t, e)
	id := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "retry"))
	e.hs.fail(sendRoute, 500, "M_UNKNOWN", "send down")
	e.project().failsWith(t, ExitError, "HTTP 500")
	if projectCursor(t, e) != before {
		t.Fatal("failed send advanced cursor")
	}
	e.hs.mu.Lock()
	delete(e.hs.failures, sendRoute)
	e.hs.mu.Unlock()
	e.project().ok(t)
	if projectCursor(t, e) == before {
		t.Fatal("successful retry did not advance cursor")
	}
	if _, err := config.UpdateState(e.paths(), "proj", func(st *config.State) error { st.NextBatch = before; return nil }); err != nil {
		t.Fatal(err)
	}
	e.project().ok(t)
	paths := projectPaths(e, "/send/")
	expected := "/_matrix/client/v3/rooms/" + target + "/send/m.room.message/" + projectionTxn(target, room, id)
	if len(paths) != 3 {
		t.Fatalf("send attempts = %v", paths)
	}
	for _, path := range paths {
		if path != expected {
			t.Fatalf("retry changed target/txn: %s want %s", path, expected)
		}
	}
	if len(e.projections(target)) != 1 || len(e.projections(foreign)) != 0 {
		t.Fatal("retry duplicated or fanned out")
	}
}

func TestProjectFirstRunSkipsHistoryAndJoinFailureKeepsPosition(t *testing.T) {
	for _, initial := range []bool{true, false} {
		t.Run(fmt.Sprintf("initial=%t", initial), func(t *testing.T) {
			e, room, target := projWorld(t)
			e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "old history"))
			if !initial {
				e.project().ok(t)
			}
			side := e.hs.addRoom("side", "@bob:test.local")
			e.as("bob", "invite", side, "proj").ok(t)
			before := projectCursor(t, e)
			e.hs.fail(projectJoinRoute, 500, "M_UNKNOWN", "join down")
			e.project("--source", "side").failsWith(t, ExitError, "HTTP 500")
			if projectCursor(t, e) != before {
				t.Fatal("failed join advanced cursor")
			}
			e.hs.mu.Lock()
			delete(e.hs.failures, projectJoinRoute)
			e.hs.mu.Unlock()
			e.project("--source", "side").ok(t)
			if len(e.projections(target)) != 0 {
				t.Fatal("first run replayed history")
			}
			id := e.hs.inject(side, "@bob:test.local", kindMsg("bob", "owner-needed", "new event"))
			e.project("--source", "side").ok(t)
			got := e.projections(target)
			if len(got) != 1 || projectionOf(t, got[0]).OriginEvent != id {
				t.Fatal("recovered invite did not project new event")
			}
		})
	}
}

// Exercise one long-running public invocation with alias reassignment after
// its first sync. Resolution must remain pinned across later sync batches.
func TestProjectPinsAliasesForRun(t *testing.T) {
	e, room, target := projWorld(t)
	foreign := e.hs.addRoom("foreign", "@proj:test.local", "@bob:test.local")
	foreignTarget := e.hs.addRoom("foreign-target", "@proj:test.local")
	e.project().ok(t)
	e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "first"))
	e.hs.inject(foreign, "@bob:test.local", kindMsg("bob", "system", "foreign first"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var sends int
	e.configure = func(a *App) {
		a.HTTP = &http.Client{Transport: projectRoundTrip(func(r *http.Request) (*http.Response, error) {
			resp, err := e.hs.srv.Client().Transport.RoundTrip(r)
			if err == nil && strings.Contains(r.URL.Path, "/send/") {
				sends++
				if sends == 1 {
					e.hs.mu.Lock()
					e.hs.aliases["#proj:test.local"] = foreign
					e.hs.aliases["#system:test.local"] = foreignTarget
					e.hs.mu.Unlock()
					e.hs.inject(room, "@bob:test.local", kindMsg("bob", "owner-needed", "second"))
					e.hs.inject(foreign, "@bob:test.local", kindMsg("bob", "system", "foreign second"))
				} else {
					cancel()
				}
			}
			return resp, err
		})}
	}
	// Cancellation also bounds a broken candidate which stops making progress.
	done := make(chan result, 1)
	go func() {
		done <- e.runCtx(ctx, "--as", "proj", "project", "--system", "system", "--source", "proj", "--poll", "1ms")
	}()
	r := recv(t, done)
	// A canceled final state write/send can report either success or cancellation;
	// destination/event observations below, not this incidental error, are the gate.
	if r.code != ExitOK && !strings.Contains(r.stderr, "context canceled") {
		t.Fatalf("unexpected error: %+v", r)
	}
	got := e.projections(target)
	if len(got) != 2 || len(e.projections(foreignTarget)) != 0 || sends != 2 {
		t.Fatalf("pinned sends=%d configured=%d foreign=%d", sends, len(got), len(e.projections(foreignTarget)))
	}
	for _, ev := range got {
		if projectionOf(t, ev).OriginRoom != room {
			t.Fatal("alias reassignment admitted foreign source")
		}
	}
}

type projectRoundTrip func(*http.Request) (*http.Response, error)

func (f projectRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProjectBackfillFailureAndGap(t *testing.T) {
	for _, gap := range []bool{false, true} {
		t.Run(fmt.Sprintf("gap=%t", gap), func(t *testing.T) {
			e, room, target := projWorld(t)
			e.hs.syncLimit = 1
			e.project().ok(t)
			before := projectCursor(t, e)
			early := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "early"))
			e.hs.inject(room, "@bob:test.local", msg("noise", nil))
			late := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "owner-needed", "late"))
			if gap {
				e.hs.historyFloor = 2
			} else {
				e.hs.fail(messagesRoute, 500, "M_UNKNOWN", "backfill down")
			}
			if !gap {
				e.project().failsWith(t, ExitError, "HTTP 500")
				if projectCursor(t, e) != before || len(e.projections(target)) != 0 {
					t.Fatal("backfill failure lost cursor/events")
				}
				e.hs.mu.Lock()
				delete(e.hs.failures, messagesRoute)
				e.hs.mu.Unlock()
			}
			r := e.project().ok(t)
			got := e.projections(target)
			if gap {
				if len(got) != 1 || projectionOf(t, got[0]).OriginEvent != late || !strings.Contains(r.stderr, "gap: events between") {
					t.Fatalf("gap evidence events=%v stderr=%q", got, r.stderr)
				}
			} else if len(got) != 2 || projectionOf(t, got[0]).OriginEvent != early || projectionOf(t, got[1]).OriginEvent != late {
				t.Fatalf("backfill recovery order=%v", got)
			}
		})
	}
}
