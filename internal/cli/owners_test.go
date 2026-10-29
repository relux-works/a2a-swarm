package cli

import (
	"context"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const (
	ownerHome = "@alexis:relux.works"
	ownerFed  = "@alexis:gr.relux.works"
)

func ownerRows(e *env, name, owner string) int {
	n := 0
	for _, sub := range e.serverSubs(name).Subs {
		if sub.From == owner && sub.Room == "" && len(sub.Kinds) == 0 && sub.Topic == "" && sub.Subject == "" && sub.Product == "" {
			n++
		}
	}
	return n
}

// Owner reachability: with no owners configured, subs check reports
// "unconfigured" without failing. Configured owners are missing until subs
// sync adds exactly one unrestricted all-rooms sender row per owner, and a
// second sync adds nothing. Kind coverage, a room-scoped or kind-restricted
// owner row and a same-localpart sender on another server never cover an
// owner. The registry refuses a localpart and grants nothing else.
func TestOwnerReachability(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "subs", "sync").ok(t)
	check := e.as("alice", "subs", "check").ok(t)
	tuitestestContains(t, check.stdout, "owners\tunconfigured")

	e.as("alice", "subs", "owners", "add", "alexis").failsWith(t, ExitUsage, "full Matrix user ID")
	tuitestestContains(t, e.as("alice", "subs", "owners", "add", ownerHome).ok(t).stdout, "added "+ownerHome)
	tuitestestContains(t, e.as("alice", "subs", "owners", "add", ownerHome).ok(t).stdout, "unchanged "+ownerHome)
	e.as("alice", "subs", "owners", "add", ownerFed).ok(t)

	missing := e.as("alice", "subs", "check").failsWith(t, ExitError, "missing kinds or owners")
	tuitestestContains(t, missing.stdout, "owner "+ownerHome+"\tmissing")
	tuitestestContains(t, missing.stdout, "owner "+ownerFed+"\tmissing")

	e.as("alice", "subscribe", "--room", "proj", "--from", ownerHome).ok(t)
	e.as("alice", "subscribe", "--from", ownerHome, "--kind", "decision").ok(t)
	e.as("alice", "subscribe", "--from", "@alexis:elsewhere.example").ok(t)
	still := e.as("alice", "subs", "check").failsWith(t, ExitError, "missing kinds or owners")
	tuitestestContains(t, still.stdout, "owner "+ownerHome+"\tmissing")

	sync := e.as("alice", "subs", "sync").ok(t).stdout
	tuitestestContains(t, sync, "owner subscriptions:")
	if strings.Count(sync, "\nsubscribed sub-") != 2 {
		t.Fatalf("sync did not add one row per owner:\n%s", sync)
	}
	covered := e.as("alice", "subs", "check").ok(t).stdout
	tuitestestContains(t, covered, "owner "+ownerHome+"\tcovered")
	tuitestestContains(t, covered, "owner "+ownerFed+"\tcovered")
	again := e.as("alice", "subs", "sync").ok(t).stdout
	if strings.Count(again, "already subscribed") < 2 || ownerRows(e, "alice", ownerHome) != 1 || ownerRows(e, "alice", ownerFed) != 1 {
		t.Fatalf("second sync changed owner rows:\n%s", again)
	}

	tuitestestContains(t, e.as("alice", "subs", "owners", "remove", ownerFed).ok(t).stdout, "removed "+ownerFed)
	list := e.as("alice", "subs", "owners", "--json").ok(t).stdout
	if !strings.Contains(list, `"configured":true`) || strings.Contains(list, ownerFed) {
		t.Fatalf("owner list after removal: %s", list)
	}
	if ownerRows(e, "alice", ownerFed) != 1 {
		t.Fatal("removing an owner unsubscribed it; removal only stops future sync and checks")
	}
}

// Preset onboarding of a session adds the owner row exactly once, preset
// sync neither duplicates it nor fails to restore it, and preset onboarding
// with --runtime none (a service account) adds none even for a session preset.
func TestOwnerRowsFromPresets(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.swarm()
	e.as("alice", "broadcast-room", "init").ok(t)
	e.hs.makePublic(e.hs.addRoom("alpha-proj", alice))
	e.hs.makePublic(e.hs.addRoom("alpha-svc", alice))
	e.as("alice", "product", "new", "alpha").ok(t)
	e.run("subs", "owners", "add", ownerHome).ok(t)
	out := e.ensure("--role", "orch", "--project", "proj", "--product", "alpha", "--preset", "orch").ok(t).stdout
	tuitestestContains(t, out, "owner subscriptions:")
	name := "mini_proj_orch"
	if ownerRows(e, name, ownerHome) != 1 {
		t.Fatalf("preset onboarding owner rows: %+v", e.serverSubs(name).Subs)
	}
	e.as(name, "subs", "check").ok(t)
	e.as(name, "subs", "sync", "--preset", "orch", "--product", "alpha").ok(t)
	if ownerRows(e, name, ownerHome) != 1 {
		t.Fatal("preset sync duplicated the owner row")
	}
	list := e.serverSubs(name)
	list.Subs = slices.DeleteFunc(list.Subs, func(sub Sub) bool { return sub.From == ownerHome })
	e.hs.setAccountData("@"+name+":"+testServer, SubsType, list)
	e.as(name, "subs", "sync", "--preset", "orch", "--product", "alpha").ok(t)
	if ownerRows(e, name, ownerHome) != 1 {
		t.Fatal("preset sync did not restore the owner row")
	}
	e.run("identity", "ensure", "--role", "orch", "--project", "svc", "--runtime", "none", "--product", "alpha", "--preset", "orch").ok(t)
	if n := ownerRows(e, "mini_svc_orch", ownerHome); n != 0 {
		t.Fatalf("--runtime none preset onboarding added %d owner rows", n)
	}
}

// A pre-existing owner row that cannot cover the owner (product-filtered,
// time-limited or not yet active) is reported missing and is not reused:
// sync keeps it and adds a covering row beside it, and a second sync adds
// nothing more.
func TestOwnerSyncDoesNotReuseNonCoveringRows(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "subs", "sync").ok(t)
	e.as("alice", "subs", "owners", "add", ownerHome).ok(t)
	list := e.serverSubs("alice")
	list.Subs = append(list.Subs,
		Sub{ID: "sub-product", SubSelector: SubSelector{From: ownerHome, Product: "alpha"}, CreatedAt: "2026-01-01T00:00:00Z"},
		Sub{ID: "sub-limited", SubSelector: SubSelector{From: ownerHome, Until: "2099-01-01T00:00:00Z"}, CreatedAt: "2026-01-01T00:00:00Z"},
		Sub{ID: "sub-future", SubSelector: SubSelector{From: ownerHome}, CreatedAt: "2099-01-01T00:00:00Z"})
	e.hs.setAccountData("@alice:"+testServer, SubsType, list)
	missing := e.as("alice", "subs", "check").failsWith(t, ExitError, "missing kinds or owners")
	tuitestestContains(t, missing.stdout, "owner "+ownerHome+"\tmissing")
	tuitestestContains(t, e.as("alice", "subs", "sync").ok(t).stdout, "\nsubscribed sub-")
	ids := map[string]bool{}
	for _, sub := range e.serverSubs("alice").Subs {
		ids[sub.ID] = true
	}
	if !ids["sub-product"] || !ids["sub-limited"] || !ids["sub-future"] {
		t.Fatalf("sync removed a pre-existing row: %v", ids)
	}
	e.as("alice", "subs", "check").ok(t)
	before := len(e.serverSubs("alice").Subs)
	e.as("alice", "subs", "sync").ok(t)
	if after := len(e.serverSubs("alice").Subs); after != before {
		t.Fatalf("second sync changed the row count %d -> %d", before, after)
	}
}

// Owner coverage uses the consumer's own activity gate (Sub.after): the
// --since event when set, otherwise the creation time; an unreadable creation
// time does not gate either. Each gate case is checked against Sub.after for
// an event at "now", and permanent unrestricted exact-owner rows are the only
// covering ones.
func TestOwnerRowGateMatchesConsumer(t *testing.T) {
	now := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	past, future := now.Add(-time.Hour), now.Add(time.Hour)
	ev := matrix.Event{Type: "m.room.message", EventID: "$now", Sender: ownerHome, OriginServerTS: now.UnixMilli()}
	sel := SubSelector{From: ownerHome}
	cases := []struct {
		name string
		sub  Sub
		want bool
	}{
		{"created in the past", Sub{SubSelector: sel, CreatedAt: past.Format(createdAtLayout)}, true},
		{"created in the future", Sub{SubSelector: sel, CreatedAt: future.Format(createdAtLayout)}, false},
		{"since past, created future", Sub{SubSelector: sel, CreatedAt: future.Format(createdAtLayout), Since: "$old", SinceTS: past.UnixMilli()}, true},
		{"since in the future", Sub{SubSelector: sel, CreatedAt: past.Format(createdAtLayout), Since: "$new", SinceTS: future.UnixMilli()}, false},
		{"unreadable creation time", Sub{SubSelector: sel, CreatedAt: "not-a-time"}, true},
	}
	for _, c := range cases {
		if got := ownerRowLive(c.sub, ownerHome, now); got != c.want || c.sub.after(ev) != c.want {
			t.Errorf("%s: ownerRowLive %v, consumer after %v, want %v", c.name, got, c.sub.after(ev), c.want)
		}
	}
	for name, s := range map[string]SubSelector{
		"time-limited": {From: ownerHome, Until: future.Format(time.RFC3339)},
		"other server": {From: "@alexis:elsewhere.example"},
		"room-scoped":  {From: ownerHome, Room: "!room"},
		"product":      {From: ownerHome, Product: "alpha"},
		"kind":         {From: ownerHome, Kinds: []string{"decision"}},
		"until-kind":   {From: ownerHome, UntilKind: "system"},
	} {
		if ownerRowLive(Sub{SubSelector: s, CreatedAt: past.Format(createdAtLayout)}, ownerHome, now) {
			t.Errorf("%s row covers the owner", name)
		}
	}
}

// Plain onboarding of a session adds the owner row; a service account
// (--runtime none, no session to notify) gets none.
func TestOwnerRowsFromPlainOnboarding(t *testing.T) {
	e := newEnv(t)
	e.swarm()
	e.run("subs", "owners", "add", ownerHome).ok(t)
	out := e.ensure("--role", "orch", "--project", "proj").ok(t).stdout
	tuitestestContains(t, out, "owner subscriptions:")
	if ownerRows(e, "mini_proj_orch", ownerHome) != 1 {
		t.Fatalf("plain onboarding owner rows: %+v", e.serverSubs("mini_proj_orch").Subs)
	}
	e.ensure("--role", "keeper", "--project", "svc", "--runtime", "none").ok(t)
	if n := ownerRows(e, "mini_svc_keeper", ownerHome); n != 0 {
		t.Fatalf("service account got %d owner rows", n)
	}
}

// A repair decides reuse with a clock read after it has read the current
// subscriptions under the locks. Here another repair has just written a
// covering row at t1; the clock reads t0 (earlier) until this repair's own
// account-data read and t2 afterwards. The repair must reuse that row, leaving
// exactly one covering row, instead of rejecting it as future-created.
func TestOwnerRepairUsesTimeAfterRead(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "subs", "owners", "add", ownerHome).ok(t)
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	t1, t2 := t0.Add(time.Second), t0.Add(2*time.Second)
	list := e.serverSubs("alice")
	list.Subs = append(list.Subs, Sub{ID: "sub-other-repair", SubSelector: ownerSelector(ownerHome), CreatedAt: t1.Format(createdAtLayout)})
	e.hs.setAccountData("@alice:"+testServer, SubsType, list)

	read := false
	a := &App{as: "alice", Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}, Stdout: io.Discard, Stderr: io.Discard}
	base := e.hs.srv.Client().Transport
	a.HTTP = &http.Client{Transport: predicateTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/account_data/") {
			read = true
		}
		return base.RoundTrip(r)
	})}
	a.Now = func() time.Time {
		if read {
			return t2
		}
		return t0
	}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	read = false
	if err := s.ensureOwnerSubs(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if n := ownerRows(e, "alice", ownerHome); n != 1 {
		t.Fatalf("repair added a duplicate covering row: %d rows %+v", n, e.serverSubs("alice").Subs)
	}
}

// The service preset adds no owner rows: neither preset onboarding of a
// service identity nor `subs sync --preset service`.
func TestServicePresetGetsNoOwnerRows(t *testing.T) {
	e := newEnv(t)
	alice := e.agent("alice")
	e.swarm()
	e.as("alice", "broadcast-room", "init").ok(t)
	e.hs.makePublic(e.hs.addRoom("alpha-proj", alice))
	e.as("alice", "product", "new", "alpha").ok(t)
	e.run("subs", "owners", "add", ownerHome).ok(t)
	e.run("identity", "ensure", "--role", "service", "--project", "proj", "--runtime", "none", "--product", "alpha", "--preset", "service").ok(t)
	name := "mini_proj_service"
	e.as(name, "subs", "sync", "--preset", "service", "--product", "alpha").ok(t)
	if n := ownerRows(e, name, ownerHome); n != 0 {
		t.Fatalf("service preset got %d owner rows", n)
	}
}
