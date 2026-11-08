package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// A hidden root can be muted from either retained server thread membership or
// a delivered child, without fetching the root. A cached child ID is not itself
// a root; each refusal has the valid root control in the same selected room.
func TestMuteHiddenRootEvidence(t *testing.T) {
	for _, evidence := range []string{"learned-child", "learned-root", "delivered-child"} {
		t.Run(evidence, func(t *testing.T) {
			f := newPrejoinFixture(t)
			f.status, f.body = 403, `{"errcode":"M_FORBIDDEN"}`
			st := config.State{NextBatch: "preserved", Threads: []string{prejoinRoot}}
			if evidence != "delivered-child" {
				id := "$child"
				if evidence == "learned-root" {
					id = prejoinRoot
				}
				st.RememberThreadEvent(config.ThreadEvent{Room: prejoinRoom, EventID: id, Root: prejoinRoot})
			}
			if err := config.SaveState(f.paths, "alice", st); err != nil {
				t.Fatal(err)
			}
			mb, err := config.OpenMailbox(f.paths, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if evidence == "delivered-child" {
				raw := json.RawMessage(prejoinEvent("$child", `{"msgtype":"m.text","body":"delivered","m.relates_to":{"rel_type":"m.thread","event_id":"$old-root"}}`))
				if _, err := mb.Append([]config.MailEntry{{Kind: config.MailEvent, ID: "$child", Room: prejoinRoom, Event: raw}}); err != nil {
					t.Fatal(err)
				}
			}
			before := memberFiles(t, f.paths.Base)
			f.run("mute", prejoinRoom, "--topic", "$child").failsWith(t, ExitError, "server did not return a usable root")
			if !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
				t.Fatal("refused child ID changed local files")
			}
			beforeCalls, _ := f.traffic()
			tuitestkit.ContainsStr(t, f.run("mute", prejoinRoom, "--topic", prejoinRoot).ok(t).stdout, prejoinRoot+"\t"+prejoinRoot+"\tmuted")
			calls, sent := f.traffic()
			if len(calls) != len(beforeCalls) || len(sent) != 0 {
				t.Fatalf("known hidden root caused network traffic: %v", calls)
			}
			got, err := config.LoadState(f.paths, "alice")
			st.Mute(prejoinRoot)
			gotJSON, gotErr := json.Marshal(got)
			wantJSON, wantErr := json.Marshal(st)
			if err != nil || gotErr != nil || wantErr != nil || string(gotJSON) != string(wantJSON) {
				t.Fatalf("mute changed more than existing root lists: %+v err=%v", got, err)
			}
			// Follow retains its existing resolution/refusal contract.
			f.run("follow", prejoinRoom, "--topic", prejoinRoot).failsWith(t, ExitError, "server did not return a usable root")
		})
	}
}

// No evidence, foreign-room membership/delivery, header claims and unscoped
// legacy lists cannot authorize a hidden root. Each case retains the exact
// existing refusal and state bytes, beside an ordinary visible-root control.
func TestMuteHiddenRootRefusals(t *testing.T) {
	for _, evidence := range []string{"none", "foreign-membership", "foreign-delivery", "header-only", "legacy-lists"} {
		for _, status := range []int{403, 404} {
			t.Run(fmt.Sprintf("%s/%d", evidence, status), func(t *testing.T) {
				f := newPrejoinFixture(t)
				f.status, f.body = status, fmt.Sprintf(`{"errcode":%q}`, map[int]string{403: "M_FORBIDDEN", 404: "M_NOT_FOUND"}[status])
				st := config.State{NextBatch: "preserved"}
				if evidence == "foreign-membership" {
					st.RememberThreadEvent(config.ThreadEvent{Room: "!other:fixture", EventID: prejoinRoot, Root: prejoinRoot})
				} else if evidence == "legacy-lists" {
					st.Threads, st.Muted = []string{prejoinRoot}, []string{prejoinRoot}
				}
				if err := config.SaveState(f.paths, "alice", st); err != nil {
					t.Fatal(err)
				}
				if evidence == "foreign-delivery" || evidence == "header-only" {
					room := prejoinRoom
					content := `{"msgtype":"m.text","body":"claim","ai.relux.a2a":{"v":1,"type":"message","topic":"$old-root"}}`
					if evidence == "foreign-delivery" {
						room = "!other:fixture"
						content = `{"msgtype":"m.text","body":"child","m.relates_to":{"rel_type":"m.thread","event_id":"$old-root"}}`
					}
					mb, err := config.OpenMailbox(f.paths, "alice")
					if err != nil {
						t.Fatal(err)
					}
					raw := strings.ReplaceAll(prejoinEvent("$child", content), prejoinRoom, room)
					if _, err := mb.Append([]config.MailEntry{{Kind: config.MailEvent, ID: "$child", Room: room, Event: json.RawMessage(raw)}}); err != nil {
						t.Fatal(err)
					}
				}
				before := memberFiles(t, f.paths.Base)
				want := fmt.Sprintf("a2a: cannot resolve topic event %s in %s: server did not return a usable root (history access unchanged): matrix: M_FORBIDDEN (status 403)\n", prejoinRoot, prejoinRoom)
				if status == 404 {
					want = fmt.Sprintf("a2a: topic event %s not found in %s (server did not return it; history access unchanged)\n", prejoinRoot, prejoinRoom)
				}
				r := f.run("mute", prejoinRoom, "--topic", prejoinRoot)
				if r.code != ExitError || r.stdout != "" || r.stderr != want || !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
					t.Fatalf("refusal changed: %+v want=%q", r, want)
				}
				f.mu.Lock()
				f.status, f.body = 200, prejoinEvent(prejoinRoot, prejoinContent)
				f.mu.Unlock()
				f.run("mute", prejoinRoom, "--topic", prejoinRoot).ok(t)
			})
		}
	}
}

// A delivered same-room m.thread child identifies a hidden root; the next
// ordinary reply is suppressed, while a neighboring thread, explicit address,
// and both Matrix/text mentions keep their existing delivery semantics.
func TestMuteHiddenRootNextDelivery(t *testing.T) {
	e, room := subWorld(t)
	_, s := notificationApp(t, e)
	ctx := context.Background()
	st, err := s.takePosition(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	rows := []Sub{{ID: "room", SubSelector: SubSelector{Room: room, Kinds: []string{"decision"}}}}
	child := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$hidden", "$hidden")))
	st, err = s.syncNow(ctx, st, rows, time.Time{})
	if err != nil || len(e.mailbox("alice").Pending()) != 1 {
		t.Fatalf("child was not delivered: %v", err)
	}
	e.as("alice", "inbox").ok(t) // Learned evidence survives receipting the child.
	gets := 0
	e.configure = func(a *App) {
		base := a.HTTP.Transport
		a.HTTP = &http.Client{Transport: muteRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if strings.Contains(r.URL.Path, "/event/$hidden") {
				gets++
				return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"errcode":"M_FORBIDDEN"}`)), Request: r}, nil
			}
			return base.RoundTrip(r)
		})}
	}
	e.as("alice", "mute", room, "--topic", "$hidden").ok(t)
	if gets != 0 || !e.state("alice").IsMuted("$hidden") || !e.received("alice", child) {
		t.Fatal("mute fetched hidden root or changed the child's receipt")
	}
	suppressed := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$hidden", "$hidden")))
	neighbor := e.hs.inject(room, "@bob:test.local", eventContent(t, headerTopicDecision(t, "$neighbor", "$neighbor")))
	wants := map[string]string{neighbor: string(relSub)}
	for _, mode := range []string{"address", "matrix-mention", "text-mention"} {
		c := eventContent(t, headerTopicDecision(t, "$hidden", "$hidden"))
		rel := relMention
		switch mode {
		case "address":
			h := c[ContentKey].(map[string]any)
			h["to"] = []string{"alice"}
			rel = relTo
		case "matrix-mention":
			c["m.mentions"] = map[string]any{"user_ids": []string{"@alice:test.local"}}
		case "text-mention":
			c["body"] = "@alice:test.local still rings"
		}
		wants[e.hs.inject(room, "@bob:test.local", c)] = string(rel)
	}
	if _, err := s.syncNow(ctx, st, rows, time.Time{}); err != nil {
		t.Fatal(err)
	}
	entries := e.mailbox("alice").Pending()
	if len(entries) != len(wants) || e.received("alice", suppressed) {
		t.Fatalf("wrong future delivery: %+v", entries)
	}
	for _, entry := range entries {
		if entry.ID == suppressed || wants[entry.ID] != entry.Rel {
			t.Fatalf("wrong relevance: %+v", entry)
		}
	}
}

// Slugs continue using ordinary discovery; a visible resolved root still mutes.
func TestMuteHiddenRootSlugControl(t *testing.T) {
	f := newPrejoinFixture(t)
	f.visible = true
	f.run("mute", prejoinRoom, "--topic", "old").ok(t)
	st, err := config.LoadState(f.paths, "alice")
	if err != nil || !st.IsMuted(prejoinRoot) {
		t.Fatalf("resolved slug did not mute its root: %+v %v", st, err)
	}
}
