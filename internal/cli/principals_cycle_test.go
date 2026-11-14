package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// The command fixtures use memory HTTP responses and private temporary stores.
// They start no server, subprocess, terminal, or external credential lookup.
func principalCycleApp(t *testing.T, since string) (*App, config.Paths, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	p := config.Paths{Base: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(p.AgentFile("worker")), 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		p.AgentFile("worker"): `{"user_id":"@worker:fixture.invalid","access_token":"synthetic-token","homeserver":"https://fixture.invalid"}`,
		p.EnvFile():           "A2A_MATRIX_URL=https://fixture.invalid\nA2A_MATRIX_SERVER_NAME=fixture.invalid\n",
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := config.SaveState(p, "worker", config.State{NextBatch: since}); err != nil {
		t.Fatal(err)
	}
	out, errout := new(bytes.Buffer), new(bytes.Buffer)
	a := NewApp()
	a.Stdout, a.Stderr = out, errout
	a.Getenv = func(k string) string {
		if k == config.EnvConfigDir {
			return p.Base
		}
		return ""
	}
	a.leaseBirth = func(context.Context, int) (int64, error) { return 1234, nil }
	a.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.Contains(r.URL.Path, "/account_data/"):
			return waitResponse(r, 200, `{"v":1,"subs":[]}`)
		case strings.Contains(r.URL.Path, "/state/m.room.canonical_alias"):
			return waitResponse(r, 200, `{"alias":"#room:fixture.invalid"}`)
		case strings.Contains(r.URL.Path, "/state/m.room.create"):
			return waitResponse(r, 200, `{"type":"m.space"}`)
		case strings.Contains(r.URL.Path, "/state/m.room.member/"):
			return waitResponse(r, 200, `{"membership":"leave"}`)
		case strings.HasSuffix(r.URL.Path, "/joined_rooms"):
			return waitResponse(r, 200, `{"joined_rooms":["!room:fixture.invalid"]}`)
		case strings.Contains(r.URL.Path, "/directory/room/"):
			return waitResponse(r, 404, `{"errcode":"M_NOT_FOUND"}`)
		default:
			return nil, fmt.Errorf("unexpected fixture request: %s %s", r.Method, r.URL.Path)
		}
	})}
	return a, p, out, errout
}

func principalCycleResponse(events ...matrix.Event) matrix.SyncResponse {
	var resp matrix.SyncResponse
	wire, _ := json.Marshal(map[string]any{"next_batch": "next", "rooms": map[string]any{"join": map[string]any{"!room:fixture.invalid": map[string]any{"timeline": map[string]any{"events": events}}}}})
	_ = json.Unmarshal(wire, &resp)
	return resp
}

func principalCycleSession(a *App, p config.Paths) *session {
	c := matrix.New("https://fixture.invalid", "synthetic-token")
	c.HTTP = a.HTTP
	return &session{app: a, paths: p, name: "worker", server: "fixture.invalid", creds: config.Credentials{UserID: "@worker:fixture.invalid"}, client: c}
}

// No-config classification equals the existing main classifier, including a
// normal subscription, mention and irrelevant neighbor. Collection/persistence
// preserves real IDs and cursor behavior, with no policy lock file or warning.
func TestPrincipalNoConfigDeliveryEquality(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(fmt.Sprint(configured), func(t *testing.T) {
			a, p, out, errout := principalCycleApp(t, "seed")
			s := principalCycleSession(a, p)
			if configured {
				if err := ownerWrite(context.Background(), ownerFiles(p, "worker").path("principals.json"), principalConfig{V: 1, Principals: []string{fixturePrincipal}}); err != nil {
					t.Fatal(err)
				}
			}
			s.principals = s.readDeliveryPrincipals()
			subs := []Sub{{ID: "sub-real", SubSelector: SubSelector{Room: "!room:fixture.invalid", From: "@peer:fixture.invalid", AllEvents: true}, CreatedAt: legacyCreatedAt}}
			mention := principalFixtureEvent("@other:fixture.invalid")
			mention.Content = json.RawMessage(`{"msgtype":"m.text","body":"@worker:fixture.invalid ordinary mail"}`)
			events := []matrix.Event{principalFixtureEvent("@peer:fixture.invalid"), principalFixtureEvent(fixturePrincipal), principalFixtureEvent("@other:fixture.invalid"), mention}
			for i := range events {
				events[i].EventID = fmt.Sprintf("$event%d", i)
				want, root, ids := classify(events[i], "!room:fixture.invalid", s.creds.UserID, s.name, config.State{}, subs)
				got, gotRoot, gotIDs := classifyWithPrincipals(events[i], "!room:fixture.invalid", s.creds.UserID, s.name, config.State{}, subs, s.principals)
				if configured && i == 1 {
					if got != relSub || len(gotIDs) != 0 {
						t.Fatal("configured neighbor not covered independently")
					}
				} else if got != want || gotRoot != root || !reflect.DeepEqual(gotIDs, ids) {
					t.Fatalf("main classification mismatch: %s / %s, %v / %v", got, want, gotIDs, ids)
				}
			}
			resp := principalCycleResponse(events...)
			if _, err := s.ingest(context.Background(), resp, "seed", subs, nil, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.showNew(context.Background(), s, deliveryFilter{}, false); err != nil {
				t.Fatal(err)
			}
			want := "sub #room $event0 from=@peer:fixture.invalid sub=sub-real: stop and call me; Signed: @principal:authority.invalid\n"
			want += "mention #room $event3 from=@other:fixture.invalid: @worker:fixture.invalid ordinary mail\n"
			if !configured && out.String() != want {
				t.Fatalf("main text mismatch: %q", out.String())
			}
			if strings.Contains(out.String(), "$event1") != configured || strings.Contains(out.String(), "$event2") || errout.Len() != 0 {
				t.Fatalf("neighbor/output: %q %q", out, errout)
			}
			st, err := config.LoadState(p, "worker")
			if err != nil || st.NextBatch != "next" {
				t.Fatalf("cursor: %+v %v", st, err)
			}
			if _, err := os.Lstat(filepath.Join(p.Base, "state", "worker.runtime-subscription-policy.lock")); !os.IsNotExist(err) {
				t.Fatalf("delivery created policy lock: %v", err)
			}
			if !configured {
				if _, err := os.Lstat(filepath.Join(p.Base, "owner-dm")); !os.IsNotExist(err) {
					t.Fatalf("absent metadata was provisioned: %v", err)
				}
				mb, err := config.OpenMailbox(p, "worker")
				if err != nil {
					t.Fatal(err)
				}
				out.Reset()
				if err := a.emit(itemsOf(mb.Pending(), s.server), true); err != nil {
					t.Fatal(err)
				}
				wantJSON := `{"type":"sub","room":"#room","room_id":"!room:fixture.invalid","event_id":"$event0","sender":"@peer:fixture.invalid","at":"1970-01-01T00:00:00Z","at_ms":1,"sub":"sub-real","excerpt":"stop and call me; Signed: @principal:authority.invalid"}` + "\n" +
					`{"type":"mention","room":"#room","room_id":"!room:fixture.invalid","event_id":"$event3","sender":"@other:fixture.invalid","at":"1970-01-01T00:00:00Z","at_ms":1,"excerpt":"@worker:fixture.invalid ordinary mail"}` + "\n"
				if out.String() != wantJSON {
					t.Fatalf("main JSON mismatch: %q", out.String())
				}
			}
		})
	}
}

// First positioning applies principal coverage only after the own join,
// retains invites and commits the cursor. Pre-join principal mail is excluded.
func TestPrincipalFirstCursorJoinBoundary(t *testing.T) {
	for _, limited := range []bool{false, true} {
		a, p, _, _ := principalCycleApp(t, "")
		s := principalCycleSession(a, p)
		if err := ownerWrite(context.Background(), ownerFiles(p, "worker").path("principals.json"), principalConfig{V: 1, Principals: []string{fixturePrincipal}}); err != nil {
			t.Fatal(err)
		}
		before := principalFixtureEvent(fixturePrincipal)
		before.EventID = "$before"
		after := principalFixtureEvent(fixturePrincipal)
		after.EventID = "$after"
		resp := principalCycleResponse(before, firstSyncJoin("@worker:fixture.invalid", "$join"), after)
		jr := resp.Rooms.Join["!room:fixture.invalid"]
		jr.Timeline.Limited, jr.Timeline.PrevBatch = limited, "must-not-page"
		resp.Rooms.Join["!room:fixture.invalid"] = jr
		inviteRaw := []byte(`{"!invite:fixture.invalid":{"invite_state":{"events":[]}}}`)
		if err := json.Unmarshal(inviteRaw, &resp.Rooms.Invite); err != nil {
			t.Fatal(err)
		}
		if _, err := s.savePosition(context.Background(), resp, time.Time{}); err != nil {
			t.Fatal(err)
		}
		mb, err := config.OpenMailbox(p, "worker")
		if err != nil {
			t.Fatal(err)
		}
		pending := mb.Pending()
		if len(pending) != 2 || pending[0].Kind != config.MailInvite || pending[1].ID != "$after" {
			t.Fatalf("join boundary: %+v", pending)
		}
		st, err := config.LoadState(p, "worker")
		if err != nil || st.NextBatch != "next" {
			t.Fatalf("position stalled: %+v %v", st, err)
		}
	}
}

// An actual subs sync holds its policy lock in a memory HTTP request while an
// actual unbounded wait ingests, persists and prints. Uncontended and configured
// neighbors exercise the same path. This is in-process concurrency, not a live
// daemon or a multiprocess/flock portability test.
func TestPrincipalWaitContinuesDuringSubsSync(t *testing.T) {
	for _, held := range []bool{false, true} {
		for _, configured := range []bool{false, true} {
			t.Run(fmt.Sprintf("held=%t/configured=%t", held, configured), func(t *testing.T) {
				a, p, out, errout := principalCycleApp(t, "seed")
				if configured {
					if err := ownerWrite(context.Background(), ownerFiles(p, "worker").path("principals.json"), principalConfig{V: 1, Principals: []string{fixturePrincipal}}); err != nil {
						t.Fatal(err)
					}
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				polling, deliver, policyHeld, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
				releasePolicy := sync.OnceFunc(func() { close(release) })
				defer releasePolicy()
				base := a.HTTP.Transport
				a.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/sync") {
						close(polling)
						select {
						case <-deliver:
						case <-ctx.Done():
							return nil, ctx.Err()
						}
						ev := principalFixtureEvent("@peer:fixture.invalid")
						ev.Content = json.RawMessage(`{"msgtype":"m.text","body":"@worker:fixture.invalid ordinary mail"}`)
						body, _ := json.Marshal(principalCycleResponse(ev))
						return waitResponse(r, 200, string(body))
					}
					return base.RoundTrip(r)
				})}
				repair := *a
				repair.Stdout, repair.Stderr = new(bytes.Buffer), new(bytes.Buffer)
				repair.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "/state/m.room.create") {
						close(policyHeld)
						if held {
							select {
							case <-release:
							case <-ctx.Done():
								return nil, ctx.Err()
							}
						}
					}
					return base.RoundTrip(r)
				})}
				waitDone, syncDone := make(chan int, 1), make(chan int, 1)
				go func() { waitDone <- a.Run(ctx, []string{"--as", "worker", "wait", "--timeout", "0"}) }()
				select {
				case <-polling:
				case <-ctx.Done():
					t.Fatal("wait never polled")
				}
				go func() { syncDone <- repair.Run(ctx, []string{"--as", "worker", "subs", "sync"}) }()
				select {
				case <-policyHeld:
				case <-ctx.Done():
					t.Fatal("sync never acquired policy lock")
				}
				if !held {
					if code := <-syncDone; code != ExitOK {
						t.Fatalf("neighbor sync: %d", code)
					}
				}
				close(deliver)
				select {
				case code := <-waitDone:
					if code != ExitOK || !strings.Contains(out.String(), "$plain") {
						t.Fatalf("wait stalled/exited: %d %q %q", code, out, errout)
					}
				case <-ctx.Done():
					t.Fatal("wait blocked behind subscription policy lock")
				}
				if held {
					// Release only after wait has completed while sync still holds its lock.
					releasePolicy()
					if code := <-syncDone; code != ExitOK {
						t.Fatalf("concurrent sync: %d", code)
					}
				}
				st, err := config.LoadState(p, "worker")
				if err != nil || st.NextBatch != "next" {
					t.Fatalf("cursor: %+v %v", st, err)
				}
			})
		}
	}
}

// Malformed JSON and an unreadable file type behave like absent metadata:
// ordinary mail persists, advances the cursor and prints; warning occurs once.
// A valid list additionally delivers plain principal mail without subs sync.
func TestPrincipalMalformedMetadataDoesNotStopMail(t *testing.T) {
	for _, kind := range []string{"malformed", "unreadable", "insecure-mode", "valid"} {
		t.Run(kind, func(t *testing.T) {
			a, p, out, errout := principalCycleApp(t, "seed")
			s := principalCycleSession(a, p)
			mb, err := config.OpenMailbox(p, "worker")
			if err != nil {
				t.Fatal(err)
			}
			pending := principalFixtureEvent(fixturePrincipal)
			pending.EventID = "$pending"
			raw, _ := json.Marshal(pending)
			if _, err := mb.Append([]config.MailEntry{{ID: pending.EventID, Kind: config.MailEvent, Room: "!room:fixture.invalid", Label: "#room", Rel: string(relSub), Sender: fixturePrincipal, Event: raw}}); err != nil {
				t.Fatal(err)
			}
			path := ownerFiles(p, "worker").path("principals.json")
			if kind == "unreadable" {
				if err := os.MkdirAll(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				if code := a.Run(context.Background(), []string{"--as", "worker", "host", "principals", "set", fixturePrincipal}); code != ExitOK {
					t.Fatalf("host set failed: %d %q", code, errout)
				}
				out.Reset()
				errout.Reset()
				if kind == "malformed" {
					if err := os.WriteFile(path, []byte(`{"v":`), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "insecure-mode" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.principals = s.readDeliveryPrincipals()
			s.principals = s.readDeliveryPrincipals()
			ordinary := principalFixtureEvent("@peer:fixture.invalid")
			ordinary.EventID = "$ordinary"
			ordinary.Content = json.RawMessage(`{"msgtype":"m.text","body":"@worker:fixture.invalid ordinary mail"}`)
			resp := principalCycleResponse(ordinary, principalFixtureEvent(fixturePrincipal))
			if _, err := s.ingest(context.Background(), resp, "seed", nil, nil, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.showNew(context.Background(), s, deliveryFilter{}, false); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out.String(), "$pending") || !strings.Contains(out.String(), "$ordinary") || strings.Contains(out.String(), "$plain") != (kind == "valid") {
				t.Fatalf("mail hidden: %q", out)
			}
			wantWarnings := 1
			if kind == "valid" {
				wantWarnings = 0
			}
			if strings.Count(errout.String(), principalConfigProblem) != wantWarnings {
				t.Fatalf("warning count: %q", errout)
			}
			st, err := config.LoadState(p, "worker")
			if err != nil || st.NextBatch != "next" {
				t.Fatalf("cursor frozen: %+v %v", st, err)
			}
		})
	}
}

// Bulk removal is partial, deterministic and preserves only dedicated rows;
// clearing the host list allows those rows too. Manual exact-sender rows are
// removable without losing their configured projection matching semantics.
func TestPrincipalUnsubscribeAllPartial(t *testing.T) {
	for _, configured := range [][]string{{fixturePrincipal}, nil} {
		list := SubList{V: 1, Subs: []Sub{
			{ID: "principal", Source: principalSubSource, SubSelector: SubSelector{Room: "!r:s", From: fixturePrincipal, AllEvents: true}},
			{ID: "manual", SubSelector: SubSelector{Room: "!r:s", From: fixturePrincipal, AllEvents: true}},
			{ID: "ordinary", SubSelector: SubSelector{Room: "!r:s", Kinds: onboardKinds}},
		}}
		removed, kept := removeAllSubscriptions(&list, configured)
		if len(configured) > 0 {
			if !reflect.DeepEqual(removed, []string{"manual", "ordinary"}) || len(kept) != 1 || kept[0].ID != "principal" || len(list.Subs) != 1 {
				t.Fatalf("bulk selection: %v %+v %+v", removed, kept, list)
			}
		} else if len(removed) != 3 || len(kept) != 0 || len(list.Subs) != 0 {
			t.Fatalf("clear did not release rows: %v %+v", removed, kept)
		}
	}
}

// Both diagnostic commands expose malformed host metadata without hiding
// their normal report. A valid empty list preserves their previous JSON shape
// and successful verdict; no terminal is bound or probed in this fixture.
func TestPrincipalMetadataDiagnostics(t *testing.T) {
	for _, valid := range []bool{false, true} {
		for _, command := range []string{"subs", "bind"} {
			for _, asJSON := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/valid=%t/json=%t", command, valid, asJSON), func(t *testing.T) {
					a, p, out, errout := principalCycleApp(t, "seed")
					path := ownerFiles(p, "worker").path("principals.json")
					if err := ownerWrite(context.Background(), path, principalConfig{V: 1, Principals: []string{}}); err != nil {
						t.Fatal(err)
					}
					if !valid {
						if err := os.WriteFile(path, []byte(`{"v":`), 0600); err != nil {
							t.Fatal(err)
						}
					}
					args := []string{"--as", "worker", command, "status"}
					if command == "subs" {
						args[3] = "check"
					}
					if asJSON {
						args = append(args, "--json")
					}
					code := a.Run(context.Background(), args)
					want := ExitOK
					if command == "subs" && !valid {
						want = ExitError
					}
					if code != want {
						t.Fatalf("diagnostic verdict: %d %q", code, errout)
					}
					if asJSON {
						var report map[string]json.RawMessage
						if err := json.Unmarshal(out.Bytes(), &report); err != nil {
							t.Fatal(err)
						}
						if _, present := report["principal_error"]; present == valid {
							t.Fatalf("diagnostic field: %s", out)
						}
						key := "publication"
						if command == "subs" {
							key = "rooms"
						}
						if len(report[key]) == 0 {
							t.Fatal("normal diagnostics hidden")
						}
					} else if strings.Contains(out.String(), principalConfigProblem) == valid {
						t.Fatalf("text diagnostic: %s", out)
					}
				})
			}
		}
	}
}

// The CLI bulk report corresponds to the actual saved list; a single-row
// refusal names its remedy and leaves server rows unchanged. Clearing the
// host configuration permits the previously protected row to be removed.
func TestPrincipalUnsubscribeReportsKeptRows(t *testing.T) {
	a, _, out, errout := principalCycleApp(t, "seed")
	list := SubList{V: 1, Subs: []Sub{
		{ID: "protected", Source: principalSubSource, CreatedAt: legacyCreatedAt, SubSelector: SubSelector{Room: "!r:s", From: fixturePrincipal, AllEvents: true}},
		{ID: "manual", CreatedAt: legacyCreatedAt, SubSelector: SubSelector{Room: "!r:s", From: fixturePrincipal, AllEvents: true}},
	}}
	puts := 0
	base := a.HTTP.Transport
	a.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/account_data/"+SubsType) {
			if r.Method == http.MethodPut {
				puts++
				if err := json.NewDecoder(r.Body).Decode(&list); err != nil {
					return nil, err
				}
			}
			wire, _ := json.Marshal(list)
			return waitResponse(r, 200, string(wire))
		}
		return base.RoundTrip(r)
	})}
	run := func(args ...string) int {
		out.Reset()
		errout.Reset()
		return a.Run(context.Background(), append([]string{"--as", "worker"}, args...))
	}
	if code := run("host", "principals", "set", fixturePrincipal); code != ExitOK {
		t.Fatalf("set: %d %q", code, errout)
	}
	if code := run("unsubscribe", "protected"); code != ExitError || puts != 0 || len(list.Subs) != 2 || !strings.Contains(errout.String(), "host principals clear") {
		t.Fatalf("single refusal: %d %q %+v", code, errout, list)
	}
	if code := run("unsubscribe", "--all"); code != ExitOK || puts != 1 || len(list.Subs) != 1 || list.Subs[0].ID != "protected" {
		t.Fatalf("bulk removal: %d %q %q %+v", code, out, errout, list)
	}
	for _, required := range []string{"manual\tremoved", "protected\tkept:", PrincipalSubscriptionRequired, "host principals clear", "host principals set"} {
		if !strings.Contains(out.String(), required) {
			t.Fatalf("report omits %q: %q", required, out)
		}
	}
	if code := run("host", "principals", "clear"); code != ExitOK {
		t.Fatal(code)
	}
	if code := run("unsubscribe", "--all"); code != ExitOK || len(list.Subs) != 0 || puts != 2 {
		t.Fatalf("host remedy: %d %+v", code, list)
	}
}
