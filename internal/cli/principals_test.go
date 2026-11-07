package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const fixturePrincipal = "@principal:authority.invalid"

func principalWorld(t *testing.T) (*env, string, string) {
	t.Helper()
	e := newEnv(t)
	worker := e.agent("worker")
	room := e.hs.addRoom("principal-room", worker, fixturePrincipal)
	ordinary := e.hs.addRoom("ordinary-room", worker)
	return e, room, ordinary
}

// Exact, permanent all-events coverage is required. Restricted, temporary,
// future, wrong-room and lookalike-sender selectors leave a principal gap.
func TestPrincipalCoverageSelectors(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	const room = "!room:fixture.invalid"
	for _, tc := range []struct {
		name    string
		change  func(*Sub)
		covered bool
	}{
		{"sender-all-events", func(*Sub) {}, true},
		{"room-all-events", func(s *Sub) { s.From = "" }, false},
		{"manual-row", func(s *Sub) { s.Source = "" }, true},
		{"preset-owned", func(s *Sub) { s.Preset = "keeper:fixture" }, true},
		{"no-all-events", func(s *Sub) { s.AllEvents = false }, false},
		{"room", func(s *Sub) { s.Room = "!other:fixture.invalid" }, false},
		{"global", func(s *Sub) { s.Room = "" }, false},
		{"sender-case", func(s *Sub) { s.From = "@Principal:authority.invalid" }, false},
		{"sender-domain", func(s *Sub) { s.From = "@principal:other.invalid" }, false},
		{"sender-suffix", func(s *Sub) { s.From = "@principal-copy:authority.invalid" }, false},
		{"kinds", func(s *Sub) { s.Kinds = onboardKinds }, false},
		{"thread", func(s *Sub) { s.Topic = "$thread" }, false},
		{"product", func(s *Sub) { s.Product = "fixture" }, false},
		{"subject", func(s *Sub) { s.Subject = "TASK-1" }, false},
		{"subtype", func(s *Sub) { s.Subtypes = []string{"incident"} }, false},
		{"one-shot", func(s *Sub) { s.UntilKind = "decision" }, false},
		{"future-expiry", func(s *Sub) { s.Until = now.Add(time.Hour).Format(time.RFC3339) }, false},
		{"expired", func(s *Sub) { s.Until = now.Add(-time.Hour).Format(time.RFC3339) }, false},
		{"future-created", func(s *Sub) { s.CreatedAt = now.Add(time.Hour).Format(time.RFC3339) }, false},
		{"invalid-created", func(s *Sub) { s.CreatedAt = "invalid" }, false},
		{"past-since", func(s *Sub) { s.Since, s.SinceTS = "$past", now.Add(-time.Hour).UnixMilli() }, true},
		{"future-since", func(s *Sub) { s.Since, s.SinceTS = "$future", now.Add(time.Hour).UnixMilli() }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub := Sub{ID: "sub-fixture", SubSelector: SubSelector{Room: room, From: fixturePrincipal, AllEvents: true}, CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), Source: principalSubSource}
			tc.change(&sub)
			missing := missingRoomPrincipals(room, []string{fixturePrincipal}, SubList{V: 1, Subs: []Sub{sub}}, now)
			if (len(missing) == 0) != tc.covered || (!tc.covered && !reflect.DeepEqual(missing, []string{fixturePrincipal})) {
				t.Fatalf("missing principals = %v; covered want %v", missing, tc.covered)
			}
		})
	}
}

// Repair adds only missing sender rows, preserves neighbors, and is idempotent.
// Its selector matches plain client messages only for the exact event sender;
// a body signature naming a principal does not grant the sender that identity.
func TestPrincipalRepairAndPlainMessages(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	const room = "!room:fixture.invalid"
	neighbor := Sub{ID: "sub-neighbor", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}
	list := SubList{V: 1, Subs: []Sub{neighbor}}
	if !repairPrincipalSubs(&list, room, []string{fixturePrincipal}, now) || len(list.Subs) != 2 || !reflect.DeepEqual(list.Subs[0], neighbor) {
		t.Fatalf("repair changed neighbors or missed principal: %+v", list)
	}
	if repairPrincipalSubs(&list, room, []string{fixturePrincipal}, now) || len(list.Subs) != 2 {
		t.Fatal("repair duplicated principal coverage")
	}
	for _, sender := range []string{fixturePrincipal, "@Principal:authority.invalid", "@principal:other.invalid", "@copy:fixture.invalid"} {
		ev := matrix.Event{Type: "m.room.message", Sender: sender, OriginServerTS: now.UnixMilli(), Content: json.RawMessage(`{"msgtype":"m.text","body":"Signed: @principal:authority.invalid"}`)}
		if got := list.Subs[1].matches(ev, room, Header{}, false); got != (sender == fixturePrincipal) {
			t.Fatalf("plain signed message sender %s matched = %v", sender, got)
		}
		rel, _, matched := classify(ev, room, "@worker:fixture.invalid", "worker", config.State{}, list.Subs)
		if sender == fixturePrincipal {
			if rel != relSub || !reflect.DeepEqual(matched, []string{list.Subs[1].ID}) {
				t.Fatalf("principal plain message was not relevant: %s %v", rel, matched)
			}
		} else if rel != "" || len(matched) != 0 {
			t.Fatalf("lookalike signature became relevant: %s %v", rel, matched)
		}
	}
}

// Host CLI round-trips exact IDs offline, canonicalizes duplicates and leaves
// the owner pin unchanged. Invalid shorthand cannot overwrite the stored list;
// clear explicitly disables the policy by removing its private file.
func TestHostPrincipalsCLI(t *testing.T) {
	e := newEnv(t) // No identity credentials are provisioned.
	f := ownerFiles(e.paths(), "worker")
	if err := os.MkdirAll(filepath.Dir(f.path("pin")), 0700); err != nil {
		t.Fatal(err)
	}
	pin := []byte(`{"mode":"host","owner":"@owner:fixture.invalid"}`)
	if err := os.WriteFile(f.path("pin"), pin, 0600); err != nil {
		t.Fatal(err)
	}
	e.as("worker", "host", "principals", "show").ok(t)
	e.as("worker", "host", "principals", "set", fixturePrincipal, "@second:fixture.invalid", fixturePrincipal).ok(t)
	out := e.as("worker", "host", "principals", "show", "--json").ok(t).stdout
	var got principalConfig
	if err := json.Unmarshal([]byte(out), &got); err != nil || got.V != 1 || !reflect.DeepEqual(got.Principals, []string{fixturePrincipal, "@second:fixture.invalid"}) {
		t.Fatalf("principal readback = %s (%v)", out, err)
	}
	path := f.path("principals.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"principal", "@principal", "Principal Name", "@principal:authority.invalid\n"} {
		e.as("worker", "host", "principals", "set", invalid).failsWith(t, ExitUsage, "exact Matrix user IDs")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid set overwrote principals")
	}
	if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("principal mode = %v (%v)", st, err)
	}
	readPin, err := os.ReadFile(f.path("pin"))
	if err != nil || !bytes.Equal(readPin, pin) || f.pin().Owner != "@owner:fixture.invalid" {
		t.Fatal("principal config changed the owner pin")
	}
	e.as("worker", "host", "principals", "clear").ok(t)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("clear did not restore absent-file path: %v", err)
	}
	if out := e.as("worker", "host", "principals", "show", "--json").ok(t).stdout; strings.TrimSpace(out) != `{"v":1,"principals":[]}` {
		t.Fatalf("clear readback = %s", out)
	}
	if e.hs.CallCount("GET /_matrix/client/v3/account/whoami") != 0 {
		t.Fatal("host metadata command opened a Matrix session")
	}
}

// Missing config is disabled, while malformed/ambiguous JSON and insecure
// file modes fail closed; a valid empty list remains an explicit opt-out.
func TestPrincipalConfigValidation(t *testing.T) {
	f := ownerFiles(config.Paths{Base: t.TempDir()}, "worker")
	if ids, err := f.principals(); err != nil || len(ids) != 0 {
		t.Fatalf("missing config = %v, %v", ids, err)
	}
	if err := os.MkdirAll(filepath.Dir(f.path("principals.json")), 0700); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body string
		mode os.FileMode
		ok   bool
	}{
		{`{"v":1,"principals":[]}`, 0600, true},
		{`{"v":1,"principals":["@principal:authority.invalid"]}`, 0600, true},
		{`{"v":2,"principals":[]}`, 0600, false},
		{`{"v":1,"principals":null}`, 0600, false},
		{`{"v":1}`, 0600, false},
		{`{"v":1,"principals":["principal"]}`, 0600, false},
		{`{"v":1,"principals":[],"principals":["@other:fixture.invalid"]}`, 0600, false},
		{`{"v":1,"principals":[],"extra":true}`, 0600, false},
		{`{"v":1,"principals":[]}`, 0644, false},
	} {
		path := f.path("principals.json")
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, tc.mode); err != nil {
			t.Fatal(err)
		}
		if _, err := f.principals(); (err == nil) != tc.ok {
			t.Fatalf("config %s mode %o error = %v", tc.body, tc.mode, err)
		}
	}
}

// A principal-only gap fails the real read-only checker with a specific MXID;
// sync repairs it, repeat sync is idempotent, and the same checker then passes.
func TestPrincipalCheckAndSync(t *testing.T) {
	e, room, _ := principalWorld(t)
	e.as("worker", "subs", "sync").ok(t)
	e.as("worker", "host", "principals", "set", fixturePrincipal).ok(t)
	puts := e.hs.CallCount(accountDataPut)
	before := memberFiles(t, e.dir)
	r := e.as("worker", "subs", "check", "--json").failsWith(t, ExitError, "subscription coverage gaps")
	var report struct {
		OK    bool           `json:"ok"`
		Rooms []roomCoverage `json:"rooms"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &report); err != nil || report.OK {
		t.Fatalf("principal coverage = %s (%v)", r.stdout, err)
	}
	found := false
	for _, row := range report.Rooms {
		if row.Room == room {
			found = row.Status == "missing" && len(row.Missing) == 0 && reflect.DeepEqual(row.MissingPrincipals, []string{fixturePrincipal})
		} else if len(row.MissingPrincipals) != 0 {
			t.Fatal("non-member principal was flagged")
		}
	}
	if !found || e.hs.CallCount(accountDataPut) != puts || !reflect.DeepEqual(before, memberFiles(t, e.dir)) {
		t.Fatal("check missed principal gap or mutated state")
	}
	e.as("worker", "subs", "sync").ok(t)
	e.as("worker", "subs", "check").ok(t)
	if missing := missingRoomPrincipals(room, []string{fixturePrincipal}, e.serverSubs("worker"), time.Now()); len(missing) > 0 {
		t.Fatalf("sync did not repair principal: %v", missing)
	}
	puts = e.hs.CallCount(accountDataPut)
	e.as("worker", "subs", "sync").ok(t)
	if e.hs.CallCount(accountDataPut) != puts {
		t.Fatal("repeat sync rewrote subscriptions")
	}
}

// Kind trim records preserve separate principal rows and never write account
// data. Sync retains principal coverage and both ordinary/principal-room trims.
func TestPrincipalTrimPreservesCoverage(t *testing.T) {
	e, room, ordinary := principalWorld(t)
	e.as("worker", "subs", "sync").ok(t)
	e.as("worker", "subs", "trim", "--room", ordinary, "--drop", "decision", "--reason", "ordinary filter").ok(t)
	e.as("worker", "host", "principals", "set", fixturePrincipal).ok(t)
	e.as("worker", "subs", "sync").ok(t)
	remote := e.serverSubs("worker")
	puts := e.hs.CallCount(accountDataPut)
	e.as("worker", "subs", "trim", "--room", room, "--drop", "decision", "--reason", "kind filter").ok(t)
	if !reflect.DeepEqual(remote, e.serverSubs("worker")) || puts != e.hs.CallCount(accountDataPut) {
		t.Fatal("kind trim changed server subscriptions")
	}
	e.as("worker", "subs", "sync").ok(t)
	if missing := missingRoomPrincipals(room, []string{fixturePrincipal}, e.serverSubs("worker"), time.Now()); len(missing) != 0 {
		t.Fatalf("trim lost principal coverage: %v", missing)
	}
	policy, err := config.LoadSubscriptionPolicy(e.paths(), "worker")
	if err != nil || !reflect.DeepEqual(policy.Trims[ordinary].Dropped, []string{"decision"}) || !reflect.DeepEqual(policy.Trims[room].Dropped, []string{"decision"}) {
		t.Fatalf("trim policy = %+v (%v)", policy, err)
	}
}

// Preset sync repairs a direct room outside its resource plan even with an old
// full trim, retains an existing tagged principal row, and removes an obsolete
// non-principal selector under that same preset tag.
func TestPrincipalPresetCleanup(t *testing.T) {
	e, room, ordinary := principalWorld(t)
	e.as("worker", "subs", "trim", "--room", room, "--drop", "system,owner-needed,decision", "--reason", "pre-policy trim").ok(t)
	e.as("worker", "host", "principals", "set", fixturePrincipal).ok(t)
	args := []string{"subs", "sync", "--preset", "arbiter", "--resource", ordinary}
	e.as("worker", args...).ok(t)
	list := e.serverSubs("worker")
	principalID := ""
	for i := range list.Subs {
		if coversPrincipal(list.Subs[i], room, fixturePrincipal, time.Now()) {
			principalID = list.Subs[i].ID
			list.Subs[i].Preset = presetTag("arbiter", "", ordinary)
		}
	}
	if principalID == "" {
		t.Fatal("preset did not repair direct room outside plan")
	}
	list.Subs = append(list.Subs, Sub{ID: "sub-obsolete", SubSelector: SubSelector{Room: room, From: "@other:fixture.invalid", AllEvents: true}, Preset: presetTag("arbiter", "", ordinary)})
	e.hs.setAccountData("@worker:"+testServer, SubsType, list)
	e.as("worker", args...).ok(t)
	list = e.serverSubs("worker")
	kept := false
	for _, sub := range list.Subs {
		kept = kept || sub.ID == principalID
		if sub.ID == "sub-obsolete" {
			t.Fatal("ordinary obsolete preset selector survived cleanup")
		}
	}
	if !kept || len(missingRoomPrincipals(room, []string{fixturePrincipal}, list, time.Now())) > 0 {
		t.Fatal("preset cleanup removed principal coverage")
	}
}

// Only the configured state key's membership is queried. Nonmembers (including
// invite/leave/ban/knock) and neighboring case/domain identities do not cover it.
func TestPrincipalMembershipExactness(t *testing.T) {
	for _, membership := range []string{"absent", "invite", "leave", "ban", "knock", "join"} {
		t.Run(membership, func(t *testing.T) {
			e := newEnv(t)
			worker := e.agent("worker")
			room := e.hs.addRoom("fixture-room", worker, "@Principal:authority.invalid", "@principal:other.invalid")
			e.hs.mu.Lock()
			if membership != "absent" {
				e.hs.rooms[room].members[fixturePrincipal] = membership
			}
			e.hs.mu.Unlock()
			e.as("worker", "subs", "sync").ok(t)
			e.as("worker", "host", "principals", "set", fixturePrincipal).ok(t)
			if membership == "join" {
				e.as("worker", "subs", "check").failsWith(t, ExitError, "subscription coverage gaps")
			} else {
				e.as("worker", "subs", "check").ok(t)
			}
		})
	}
}

// Without configured principals, stdout (text and JSON), coverage and sync
// writes keep their previous shape, including empty stderr. Even an owner pin
// alone does not infer a principal or read room membership.
func TestPrincipalUnconfiguredCompatibility(t *testing.T) {
	e, _, _ := principalWorld(t)
	f := ownerFiles(e.paths(), "worker")
	if err := ownerWrite(context.Background(), f.path("pin"), ownerDMPin{Mode: "host", Owner: fixturePrincipal}); err != nil {
		t.Fatal(err)
	}
	membershipReads := 0
	e.configure = func(a *App) {
		base := a.HTTP.Transport
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/state/m.room.member/") {
				membershipReads++
			}
			return base.RoundTrip(r)
		})}
	}
	e.as("worker", "subs", "sync").ok(t)
	for _, sub := range e.serverSubs("worker").Subs {
		if sub.AllEvents || sub.From != "" {
			t.Fatal("no-config sync inferred principal coverage")
		}
	}
	text := e.as("worker", "subs", "check").ok(t)
	want := "#ordinary-room\tcovered\n#principal-room\tcovered\ncoverage: ok\n"
	if text.stdout != want || text.stderr != "" {
		t.Fatalf("no-config output = %+v", text)
	}
	out := e.as("worker", "subs", "check", "--json").ok(t).stdout
	if strings.Contains(out, `"missing_principals"`) || membershipReads != 0 {
		t.Fatalf("no-config JSON added principal fields: %s", out)
	}
	var report map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &report); err != nil || len(report) != 2 || string(report["ok"]) != "true" {
		t.Fatalf("no-config JSON shape changed: %s (%v)", out, err)
	}
}

// Membership transport/schema failures cannot authorize a trim or claim/repair
// coverage. The same command succeeds after the bounded fault is removed.
func TestPrincipalMembershipReadFailures(t *testing.T) {
	for _, command := range []string{"check", "sync", "trim", "preset"} {
		for _, fault := range []string{"unavailable", "malformed"} {
			t.Run(command+"/"+fault, func(t *testing.T) {
				e, _, ordinary := principalWorld(t)
				e.as("worker", "subs", "sync").ok(t)
				e.as("worker", "host", "principals", "set", fixturePrincipal).ok(t)
				// Pre-repair coverage so the recovered checker is a positive control.
				e.as("worker", "subs", "sync").ok(t)
				before := memberFiles(t, e.dir)
				puts := e.hs.CallCount(accountDataPut)
				reads := 0
				e.configure = func(a *App) {
					base := a.HTTP.Transport
					a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
						if strings.Contains(r.URL.Path, "/state/m.room.member/") {
							reads++
							status, body := 200, `{}`
							if fault == "unavailable" {
								status, body = 503, `{"errcode":"M_UNAVAILABLE","error":"fixture failure"}`
							}
							return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
						}
						return base.RoundTrip(r)
					})}
				}
				args := []string{"subs", command}
				switch command {
				case "trim":
					args = append(args, "--room", ordinary, "--drop", "decision", "--reason", "ordinary filter")
				case "preset":
					args = []string{"subs", "sync", "--preset", "arbiter", "--resource", ordinary}
				}
				e.as("worker", args...).failsWith(t, ExitError, "principal membership")
				if reads == 0 || e.hs.CallCount(accountDataPut) != puts || !reflect.DeepEqual(before, memberFiles(t, e.dir)) {
					t.Fatal("membership fault did not execute or allowed a write")
				}
				e.configure = nil
				e.as("worker", args...).ok(t)
			})
		}
	}
}
