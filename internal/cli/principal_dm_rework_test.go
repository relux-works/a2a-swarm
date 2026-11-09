package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func principalSubs(t *testing.T, e *env) SubList {
	t.Helper()
	raw := e.hs.accountDataOf("@bob:"+testServer, SubsType)
	if raw == nil {
		return SubList{V: 1}
	}
	var list SubList
	if err := json.Unmarshal(raw, &list); err != nil {
		t.Fatal(err)
	}
	return list
}

func principalSnapshot(t *testing.T, e *env, f ownerDMFiles) string {
	t.Helper()
	files := map[string]string{}
	for _, path := range []string{f.path("json"), f.path("pin"), e.paths().StateFile("bob"), ownerFiles(e.paths(), "bob").path("archive-exclude")} {
		b, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		files[path] = string(b)
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	b, err := json.Marshal([]any{files, e.hs.rooms, e.hs.accountData})
	if err != nil {
		t.Fatal(err)
	}
	// fakeRoom has unexported fields: include state and membership explicitly.
	rooms := map[string]any{}
	for id, r := range e.hs.rooms {
		rooms[id] = []any{r.members, r.state, r.stateSenders}
	}
	rb, err := json.Marshal(rooms)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + string(rb)
}

// H1: the App entry point rejects global selectors in every position/form,
// before terminal/identity/network effects. Canonical neighbours use A2A_AGENT.
func TestDMCanonicalTerminalArgv(t *testing.T) {
	flags := []struct{ key, value string }{
		{"--as", "bob"}, {"-as", "bob"}, {"--profile", "test"},
		{"--connection-fingerprint", strings.Repeat("a", 64)}, {"--human", "true"}, {"--future-global", "x"},
	}
	for _, verb := range []string{"principal-dm", "owner-dm"} {
		for _, sub := range []string{"ensure", "unset"} {
			for _, flag := range flags {
				for _, equals := range []bool{false, true} {
					for _, position := range []int{0, 1, 2, 3} {
						t.Run(verb+"/"+sub+"/"+flag.key+"/"+map[bool]string{false: "split", true: "equals"}[equals]+"/"+string(rune('0'+position)), func(t *testing.T) {
							e, f, who := principalFixture(t)
							e.extra[config.EnvAgent] = "bob"
							confirms := 0
							e.configure = func(a *App) { a.ownerConfirm = func(string) error { confirms++; return nil } }
							if verb == "owner-dm" {
								f = ownerFiles(e.paths(), "bob")
							}
							args := []string{verb, sub}
							if verb == "principal-dm" {
								args = append(args, who)
							}
							tokens := []string{flag.key, flag.value}
							if flag.key == "--human" {
								tokens = []string{flag.key}
							}
							if equals {
								tokens = []string{flag.key + "=" + flag.value}
							}
							at := min(position, len(args))
							bad := append(append(append([]string{}, args[:at]...), tokens...), args[at:]...)
							before := principalSnapshot(t, e, principalFiles(e.paths(), "bob"))
							e.run(bad...).failsWith(t, ExitUsage, "canonical form: a2a "+verb+" "+sub)
							if confirms != 0 || principalSnapshot(t, e, principalFiles(e.paths(), "bob")) != before || len(e.hs.createdRooms()) != 0 {
								t.Fatal("noncanonical invocation had effects")
							}
							e.run(args...).ok(t)
							if confirms != 1 {
								t.Fatal("canonical neighbour missed confirmation")
							}
							if sub == "ensure" {
								if _, st, err := ownerRead(f.path("json")); err != nil || st == nil {
									t.Fatal("canonical ensure did not persist", err)
								}
							}
						})
					}
				}
			}
		}
	}
	// A selector value that looks like another subcommand must not hide ensure.
	e, _, who := principalFixture(t)
	e.run("principal-dm", "--as", "status", "ensure", who).failsWith(t, ExitUsage, "canonical form")
}

// H2/M1/M2: actual post-create boundary faults leave no new subscription,
// preserve the old row, keep archive closed, and do not record/adopt the orphan.
// Every injected step has the same fixture with the fault disabled as a control.
func TestPrincipalDMPostCreationFailures(t *testing.T) {
	for _, step := range []string{"exclusion", "audience", "subscription-rejected", "subscription-lost-response", "config-write"} {
		for _, inject := range []bool{true, false} {
			t.Run(step+"/"+map[bool]string{true: "fault", false: "control"}[inject], func(t *testing.T) {
				e, f, who := principalFixture(t)
				now := time.Now().Add(-72 * time.Hour)
				e.now = func() time.Time { return now }
				e.as("bob", "principal-dm", "ensure", who).ok(t)
				old := principalEntry(t, f, who)
				before := readFile(t, f.path("json"))
				now = now.Add(principalDMPendingTTL)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				hit := false
				orphan := ""
				base := e.hs.srv.Client().Transport
				prior := e.configure
				e.configure = func(a *App) {
					prior(a)
					a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/createRoom") {
							reserved, err := ownerFiles(e.paths(), "bob").exclusions()
							if err != nil || !reserved[principalArchiveReservation] {
								t.Error("CreateRoom preceded durable exclusion reservation", reserved, err)
							}
						}
						isSub := r.Method == "PUT" && strings.Contains(r.URL.Path, "/account_data/"+SubsType)
						if inject && !hit && step == "subscription-rejected" && isSub {
							hit = true
							return nil, errors.New("injected subscription rejection")
						}
						resp, err := base.RoundTrip(r)
						if err != nil {
							return resp, err
						}
						if strings.HasSuffix(r.URL.Path, "/createRoom") {
							b, _ := io.ReadAll(resp.Body)
							resp.Body.Close()
							resp.Body = io.NopCloser(bytes.NewReader(b))
							var created struct {
								Room string `json:"room_id"`
							}
							if err := json.Unmarshal(b, &created); err != nil {
								t.Error(err)
							}
							orphan = created.Room
							if inject && step == "exclusion" {
								hit = true
								if err := os.Chmod(ownerFiles(e.paths(), "bob").path("archive-exclude"), 0400); err != nil {
									t.Error(err)
								}
							}
							if inject && step == "audience" {
								hit = true
								e.hs.mu.Lock()
								e.hs.rooms[orphan].state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"shared"}`)
								e.hs.mu.Unlock()
							}
						}
						if inject && !hit && isSub {
							if step == "subscription-lost-response" {
								hit = true
								resp.Body.Close()
								return nil, errors.New("injected lost subscription response")
							}
							if step == "config-write" {
								hit = true
								if err := os.Remove(f.path("json")); err != nil {
									t.Error(err)
								}
								if err := os.Mkdir(f.path("json"), 0700); err != nil {
									t.Error(err)
								}
							}
						}
						return resp, nil
					})}
				}
				e.extra[config.EnvAgent] = "bob"
				out := e.runCtx(ctx, "principal-dm", "ensure", who)
				if !inject {
					out.ok(t)
					next := principalEntry(t, f, who)
					excluded, err := ownerFiles(e.paths(), "bob").exclusions()
					if err != nil || !excluded[next.Room] || excluded[principalArchiveReservation] {
						t.Fatal("successful creation did not narrow reservation to exact room", excluded, err)
					}
					rows := principalSubs(t, e)
					if next.Room == old.Room || len(rows.Subs) != 1 || rows.Subs[0].ID != next.SubID || !rows.Subs[0].AllEvents || rows.Subs[0].Room != next.Room {
						t.Fatal("replacement control", next, rows)
					}
					return
				}
				out.failsWith(t, ExitError, "was created but not recorded")
				if !hit || orphan == "" {
					t.Fatal("fault not reached")
				}
				if step == "exclusion" {
					e.as("bob", "archive", orphan, "--dir", filepath.Join(t.TempDir(), "blocked-export")).failsWith(t, ExitError, "expected regular mode 0600 file")
					if err := os.Chmod(ownerFiles(e.paths(), "bob").path("archive-exclude"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if step == "config-write" {
					if err := os.Remove(f.path("json")); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.path("json"), []byte(before), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if readFile(t, f.path("json")) != before {
					t.Fatal("failed creation recorded config")
				}
				exclusions, err := ownerFiles(e.paths(), "bob").exclusions()
				if err != nil || (!exclusions[orphan] && !exclusions[principalArchiveReservation]) {
					t.Fatal("orphan not archive-excluded", exclusions, err)
				}
				rows := principalSubs(t, e)
				if len(rows.Subs) != 1 || rows.Subs[0].ID != old.SubID || !rows.Subs[0].AllEvents {
					t.Fatal("rollback lost old row or kept new row", rows)
				}
				// Exercise archive, so a reservation stored but ignored by the exporter fails.
				dir := filepath.Join(t.TempDir(), "export")
				archive := e.as("bob", "archive", orphan, "--dir", dir)
				if exclusions[principalArchiveReservation] {
					archive.failsWith(t, ExitError, "creation reservation excludes all rooms")
				} else {
					archive.ok(t)
				}
				if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("archive exported orphan", err)
				}
				e.configure = prior
				e.as("bob", "principal-dm", "ensure", who).ok(t)
				next := principalEntry(t, f, who)
				if next.Room == orphan || next.Room == old.Room || len(e.hs.createdRooms()) != 3 {
					t.Fatal("retry adopted room", next)
				}
				rows = principalSubs(t, e)
				if len(rows.Subs) != 1 || rows.Subs[0].ID != next.SubID || !rows.Subs[0].AllEvents {
					t.Fatal("retry rows", rows)
				}
			})
		}
	}
}

// M2: a held owner/archive lock expires before CreateRoom or reservation.
// The same ensure succeeds as soon as that real flock is released.
func TestPrincipalDMOwnerLockTimeout(t *testing.T) {
	e, f, who := principalFixture(t)
	unlock, err := ownerFiles(e.paths(), "bob").lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	before := principalSnapshot(t, e, f)
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	e.extra[config.EnvAgent] = "bob"
	e.runCtx(ctx, "principal-dm", "ensure", who).failsWith(t, ExitError, "owner-dm lock: context deadline exceeded")
	if len(e.hs.createdRooms()) != 0 || principalSnapshot(t, e, f) != before {
		t.Fatal("lock timeout created room or wrote exclusion/config")
	}
	unlock()
	unlock = nil
	e.as("bob", "principal-dm", "ensure", who).ok(t)
}

// M3: failed remote removal leaves durable inactive state plus its cleanup ID;
// retries remove the row without another generation transition, for both DMs.
func TestDMUnsetLocalFirstAndRetry(t *testing.T) {
	for _, verb := range []string{"principal-dm", "owner-dm"} {
		t.Run(verb, func(t *testing.T) {
			e, f, who := principalFixture(t)
			args := []string{verb, "ensure"}
			unset := []string{verb, "unset"}
			if verb == "principal-dm" {
				args = append(args, who)
				unset = append(unset, who)
			} else {
				f = ownerFiles(e.paths(), "bob")
			}
			e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
			e.as("bob", args...).ok(t)
			var sub string
			if verb == "principal-dm" {
				sub = principalEntry(t, f, who).SubID
			} else {
				sub = ownerLoad(t, f).SubID
			}
			before := readFile(t, f.path("json"))
			// An unreadable local config must never issue remote removal; the
			// transport below checks durable deactivation at the real Put boundary.
			if err := os.Remove(f.path("json")); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(f.path("json"), 0700); err != nil {
				t.Fatal(err)
			}
			e.as("bob", unset...).failsWith(t, ExitError, "invalid "+verb+" config")
			if principalSubs(t, e).find(sub) < 0 {
				t.Fatal("local failure removed subscription")
			}
			if err := os.Remove(f.path("json")); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(f.path("json"), []byte(before), 0600); err != nil {
				t.Fatal(err)
			}
			prior := e.configure
			e.configure = func(a *App) {
				prior(a)
				a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
					if r.Method == "PUT" && strings.Contains(r.URL.Path, "/account_data/"+SubsType) {
						var local struct {
							Active  bool   `json:"active"`
							Pending bool   `json:"pending_join"`
							SubID   string `json:"sub_id"`
						}
						if verb == "principal-dm" {
							d := principalEntry(t, f, who)
							local.Active, local.Pending, local.SubID = d.Active, d.Pending, d.SubID
						} else {
							d := ownerLoad(t, f)
							local.Active, local.Pending, local.SubID = d.Active, d.Pending, d.SubID
						}
						if local.Active || local.Pending || local.SubID != sub {
							t.Error("remote removal preceded durable deactivation", local)
						}
						return nil, errors.New("injected remove failure")
					}
					return e.hs.srv.Client().Transport.RoundTrip(r)
				})}
			}
			e.as("bob", unset...).failsWith(t, ExitError, "inactive; subscription cleanup pending; retry unset")
			var generation uint64
			if verb == "principal-dm" {
				d := principalEntry(t, f, who)
				generation = d.Generation
				if d.Active || d.Pending || d.SubID != sub {
					t.Fatal(d)
				}
				e.as("bob", "principal-dm", "say", who, "must refuse").failsWith(t, ExitError, "no active room")
			} else {
				d := ownerLoad(t, f)
				generation = d.Generation
				if d.Active || d.Pending || d.SubID != sub {
					t.Fatal(d)
				}
			}
			if principalSubs(t, e).find(sub) < 0 {
				t.Fatal("failure fixture removed remote row")
			}
			e.configure = prior
			out := e.as("bob", unset...).ok(t)
			if !strings.Contains(out.stdout, "subscription cleanup complete") {
				t.Fatal(out.stdout)
			}
			if principalSubs(t, e).find(sub) >= 0 {
				t.Fatal("retry kept remote row")
			}
			after := readFile(t, f.path("json"))
			e.as("bob", unset...).ok(t)
			if readFile(t, f.path("json")) != after {
				t.Fatal("idempotent retry changed state")
			}
			if verb == "principal-dm" {
				if d := principalEntry(t, f, who); d.Generation != generation || d.SubID != "" {
					t.Fatal(d)
				}
			} else {
				if d := ownerLoad(t, f); d.Generation != generation || d.SubID != "" {
					t.Fatal(d)
				}
			}
		})
	}
}

type principalTerminal struct {
	io.Reader
	bytes.Buffer
}

func (p *principalTerminal) Read(b []byte) (int, error) { return p.Reader.Read(b) }
func (*principalTerminal) Close() error                 { return nil }

// M4: inject only the terminal stream. Production prompt/comparison rejects a
// wrong or unterminated response and accepts the exact MXID with a newline.
func TestPrincipalDMProductionTypeBack(t *testing.T) {
	e, f, who := principalFixture(t)
	for _, typed := range []string{"wrong\n", who} {
		terminal := &principalTerminal{Reader: strings.NewReader(typed)}
		e.configure = func(a *App) { a.ownerTerminal = func() (io.ReadWriteCloser, error) { return terminal, nil } }
		fragment := "type-back mismatch"
		if typed == who {
			fragment = "EOF"
		}
		e.as("bob", "principal-dm", "ensure", who).failsWith(t, ExitError, fragment)
		if !strings.Contains(terminal.String(), "Type "+who+" to confirm:") {
			t.Fatal("did not reach production prompt")
		}
		if _, st, _ := ownerRead(f.path("json")); st != nil || len(e.hs.createdRooms()) != 0 {
			t.Fatal("wrong type-back had effects")
		}
	}
	terminal := &principalTerminal{Reader: strings.NewReader(who + "\n")}
	e.configure = func(a *App) { a.ownerTerminal = func() (io.ReadWriteCloser, error) { return terminal, nil } }
	e.as("bob", "principal-dm", "ensure", who).ok(t)
}

// M4: inspect the exact CreateRoom payload rather than trusting fixture state.
func TestPrincipalDMCreatedShape(t *testing.T) {
	e, _, who := principalFixture(t)
	e.as("bob", "principal-dm", "ensure", who).ok(t)
	requests := e.hs.createdRooms()
	if len(requests) != 1 {
		t.Fatal(requests)
	}
	r := requests[0]
	if r.Preset != "private_chat" || !r.IsDirect || !reflect.DeepEqual(r.Invite, []string{who}) || len(r.InitialState) != 2 {
		t.Fatalf("bad creation shape %+v", r)
	}
	for i, want := range []struct{ kind, body string }{{"m.room.join_rules", `{"join_rule":"invite"}`}, {"m.room.history_visibility", `{"history_visibility":"joined"}`}} {
		ev := r.InitialState[i]
		if ev.Type != want.kind || ev.StateKey == nil || *ev.StateKey != "" || string(ev.Content) != want.body {
			t.Fatalf("initial state %+v", ev)
		}
	}
}

// LOW: strict pin parsing rejects duplicate keys, malformed routing, and owner
// or self anywhere in the list (case variants too), before any room creation.
func TestPrincipalDMStrictPinAndOwner(t *testing.T) {
	e, f, who := principalFixture(t)
	for _, raw := range []string{
		`{"principals":[],"principals":["@carol:test.local"]}`,
		`{"principals":["@carol:test.local","@alice:test.local"]}`,
		`{"principals":["@carol:test.local","@ALICE:TEST.LOCAL"]}`,
		`{"principals":["@carol:test.local","@bob:test.local"]}`,
		`{"principals":["@carol:test.local","@BOB:TEST.LOCAL"]}`,
		`{"principals":["@carol:bad/server"]}`,
	} {
		ownerPut(t, f.path("pin"), json.RawMessage(raw))
		for _, sub := range []string{"ensure", "unset"} {
			e.as("bob", "principal-dm", sub, who).failsWith(t, ExitError, "invalid principal-dm pin")
		}
	}
	ownerPut(t, f.path("pin"), principalDMPin{Principals: []string{who}})
	for _, target := range []string{"@ALICE:TEST.LOCAL", "@BOB:TEST.LOCAL"} {
		fragment := "is this identity"
		if strings.Contains(target, "ALICE") {
			fragment = "use owner-dm"
		}
		e.as("bob", "principal-dm", "ensure", target).failsWith(t, ExitError, fragment)
	}
	owner := ownerFiles(e.paths(), "bob").path("pin")
	ownerPut(t, owner, ownerDMPin{Mode: "host", Owner: "@alice:bad/server"})
	for _, sub := range []string{"ensure", "unset"} {
		e.as("bob", "principal-dm", sub, who).failsWith(t, ExitError, "owner-dm pin is present but invalid")
	}
	if len(e.hs.createdRooms()) != 0 {
		t.Fatal("bad pin created room")
	}
	// Absent-owner-pin neighbour is supported, with the same valid principal pin.
	if err := os.Remove(owner); err != nil {
		t.Fatal(err)
	}
	e.as("bob", "principal-dm", "ensure", who).ok(t)
	e.as("bob", "principal-dm", "unset", who).ok(t)
}

// LOW: every history spelling except joined refuses without state repair;
// a failed first send stays pending, and only a successful send activates.
func TestPrincipalDMHistoryAndSendFailure(t *testing.T) {
	e, f, who := principalFixture(t)
	e.as("bob", "principal-dm", "ensure", who).ok(t)
	d := principalEntry(t, f, who)
	principalJoin(e, d.Room, who)
	for _, history := range []string{"shared", "invited", "world_readable", "garbage", ""} {
		e.hs.mu.Lock()
		e.hs.rooms[d.Room].state["m.room.history_visibility|"], _ = json.Marshal(map[string]string{"history_visibility": history})
		e.hs.mu.Unlock()
		before := principalSnapshot(t, e, f)
		e.as("bob", "principal-dm", "say", who, "refuse").failsWith(t, ExitError, "history_visibility")
		if principalSnapshot(t, e, f) != before || len(ownerBodies(e, d.Room)) != 0 {
			t.Fatal("history refusal repaired or sent")
		}
	}
	e.hs.mu.Lock()
	e.hs.rooms[d.Room].state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"joined"}`)
	e.hs.mu.Unlock()
	prior := e.configure
	e.configure = func(a *App) {
		prior(a)
		a.HTTP = &http.Client{Transport: noticeWriterTransport(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/send/") {
				return nil, errors.New("injected send failure")
			}
			return e.hs.srv.Client().Transport.RoundTrip(r)
		})}
	}
	before := principalSnapshot(t, e, f)
	e.as("bob", "principal-dm", "say", who, "refuse").failsWith(t, ExitError, "injected send failure")
	if principalSnapshot(t, e, f) != before || !principalEntry(t, f, who).Pending {
		t.Fatal("failed say activated or mutated")
	}
	e.configure = prior
	e.as("bob", "principal-dm", "say", who, "success").ok(t)
	if !principalEntry(t, f, who).Active || len(ownerBodies(e, d.Room)) != 1 {
		t.Fatal("success did not activate/send")
	}
}

// LOW: future timestamps refuse all mutations and inbound admission; expiry
// at exactly 24h removes DM-only inbound coverage, as inactive/unset does.
func TestPrincipalDMFutureAndInboundExpiry(t *testing.T) {
	e, f, who := principalFixture(t)
	now := time.Now().Add(-48 * time.Hour)
	e.now = func() time.Time { return now }
	e.as("bob", "principal-dm", "ensure", who).ok(t)
	d := principalEntry(t, f, who)
	principalJoin(e, d.Room, who)
	creds, err := config.LoadCredentials(e.paths(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	a := &App{Now: e.now}
	s := &session{app: a, paths: e.paths(), name: "bob", creds: creds, client: &matrix.Client{}}
	ev := principalFixtureEvent(who)
	ev.EventID = "$inbound"
	ev.OriginServerTS = now.UnixMilli()
	response := matrix.SyncResponse{}
	// Use JSON to avoid coupling this fixture to nested anonymous sync structs.
	raw, _ := json.Marshal(map[string]any{"next_batch": "n", "rooms": map[string]any{"join": map[string]any{d.Room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{ev}}}}}})
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	collect := func() (batch, error) {
		return s.collectBatchWithPrincipals(context.Background(), response, "prior", config.State{}, principalSubs(t, e).Subs, nil, map[string]string{d.Room: "dm"}, nil)
	}
	b, err := collect()
	if err != nil || len(b.entries) != 1 || b.entries[0].Rel != string(relSub) {
		t.Fatal("live pending inbound control", b, err)
	}
	now = now.Add(principalDMPendingTTL)
	b, err = collect()
	if err != nil || len(b.entries) != 0 {
		t.Fatal("expired pending delivered DM-only mail", b, err)
	}
	// A future timestamp is rejected rather than extending pending lifetime.
	state := principalDMState{V: 1, DMs: map[string]principalDM{who: d}}
	d.CreatedAt = now.Add(time.Millisecond).UnixMilli()
	state.DMs[who] = d
	ownerPut(t, f.path("json"), state)
	before := readFile(t, f.path("json"))
	for _, sub := range []string{"ensure", "unset", "say"} {
		args := []string{"principal-dm", sub, who}
		if sub == "say" {
			args = append(args, "x")
		}
		e.as("bob", args...).failsWith(t, ExitError, "invalid principal-dm config")
	}
	if _, err = collect(); err == nil || !strings.Contains(err.Error(), "invalid principal-dm config") {
		t.Fatal("future config admitted inbound", err)
	}
	if readFile(t, f.path("json")) != before {
		t.Fatal("future refusal mutated config")
	}
	d.CreatedAt = now.UnixMilli()
	state.DMs[who] = d
	ownerPut(t, f.path("json"), state)
	e.as("bob", "principal-dm", "say", who, "valid").ok(t)
}

// Guard controls: command body words are data, and Owner DM retains its
// historical non-world-readable history policy; principal's stricter history
// policy must not leak into owner ensure.
func TestDMGuardAndOwnerHistoryNeighbours(t *testing.T) {
	e, _, _ := principalFixture(t)
	e.as("bob", "post", "proj", "principal-dm", "ensure").ok(t)
	e2, _, f := ownerFixture(t)
	e2.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	e2.as("bob", "owner-dm", "ensure").ok(t)
	c := ownerLoad(t, f)
	ownerJoin(e2, c, c.Owner)
	e2.hs.mu.Lock()
	e2.hs.rooms[c.Room].state["m.room.history_visibility|"] = json.RawMessage(`{"history_visibility":"shared"}`)
	e2.hs.mu.Unlock()
	e2.as("bob", "owner-dm", "ensure").ok(t)
	if !ownerLoad(t, f).Active {
		t.Fatal("owner history policy changed")
	}
}
