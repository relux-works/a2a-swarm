package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

const prejoinRoom = "!selected:fixture"
const prejoinRoot = "$old-root"
const prejoinContent = `{"msgtype":"m.text","body":"[topic] old: Before local history"}`

// This models server-returned context outside the local discovery window, not
// a homeserver's membership/history-visibility implementation. No live account
// or join is involved. Every allowed request is explicit; other routes refuse.
type prejoinFixture struct {
	t        *testing.T
	paths    config.Paths
	server   *httptest.Server
	mu       sync.Mutex
	body     string
	status   int
	redirect string
	visible  bool
	requests []string
	sent     []matrix.MessageContent
}

func prejoinEvent(id, content string) string {
	return `{"event_id":` + fmt.Sprintf("%q", id) + `,"room_id":"!selected:fixture","sender":"@historical:fixture","type":"m.room.message","origin_server_ts":1,"content":` + content + `}`
}

func newPrejoinFixture(t *testing.T) *prejoinFixture {
	t.Helper()
	f := &prejoinFixture{t: t, paths: config.Paths{Base: t.TempDir()}, body: prejoinEvent(prejoinRoot, prejoinContent), status: 200}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer synthetic-selected-token" {
			writeErr(w, 403, "M_FORBIDDEN", "selected credential required")
			return
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/_matrix/client/v3/rooms/"+prejoinRoom+"/event/"+prejoinRoot:
			w.Header().Set("Content-Type", "application/json")
			if f.redirect != "" {
				w.Header().Set("Location", f.redirect)
			}
			w.WriteHeader(f.status)
			io.WriteString(w, f.body)
		case r.Method == "GET" && (r.URL.Path == "/_matrix/client/v1/rooms/"+prejoinRoom+"/threads" || r.URL.Path == "/_matrix/client/v3/rooms/"+prejoinRoom+"/messages"):
			chunk := "[]"
			if f.visible {
				chunk = "[" + prejoinEvent(prejoinRoot, prejoinContent) + "]"
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"chunk":`+chunk+`}`)
		case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"+prejoinRoom+"/send/m.room.message/"):
			var m matrix.MessageContent
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				t.Errorf("invalid send: %v", err)
			}
			f.sent = append(f.sent, m)
			writeJSON(w, 200, map[string]string{"event_id": "$sent"})
		default:
			writeErr(w, 403, "M_FORBIDDEN", "route outside root-resolution fixture")
		}
	}))
	t.Cleanup(f.server.Close)
	if err := os.WriteFile(f.paths.EnvFile(), []byte("A2A_MATRIX_URL="+f.server.URL+"\nA2A_MATRIX_SERVER_NAME=fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.paths.AgentFile("alice")), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(config.Credentials{UserID: "@alice:fixture", AccessToken: "synthetic-selected-token", DeviceID: "fixture", Homeserver: f.server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.paths.AgentFile("alice"), b, 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *prejoinFixture) run(args ...string) result {
	f.t.Helper()
	var out, errb bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), Now: time.Now, HTTP: f.server.Client(), Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return f.paths.Base
		}
		return ""
	}}
	code := a.Run(context.Background(), append([]string{"--as", "alice"}, args...))
	r := result{code: code, stdout: out.String(), stderr: errb.String()}
	if strings.Contains(r.stdout+r.stderr, "synthetic-selected-token") {
		f.t.Fatal("selected token leaked")
	}
	return r
}

func (f *prejoinFixture) post(topic string) result {
	return f.run("post", prejoinRoom, "--topic", topic, "reply")
}

func (f *prejoinFixture) traffic() ([]string, []matrix.MessageContent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...), append([]matrix.MessageContent(nil), f.sent...)
}

// Seed an existing muted root so the legacy post recorder is a no-op. Compare
// all state bytes, including cursors/pending, and independently forbid account
// data/subscription writes. The unmuted legacy boundary is tested separately.
func (f *prejoinFixture) seedPreservedState() map[string]string {
	f.t.Helper()
	if err := config.SaveState(f.paths, "alice", config.State{NextBatch: "unchanged-cursor", Threads: []string{"$already-followed"}, Muted: []string{prejoinRoot}}); err != nil {
		f.t.Fatal(err)
	}
	// The mailbox holds one well-formed pending entry in another room: the
	// local redaction cache must be able to open it, and any write by the
	// command would still change these exact bytes.
	for name, content := range map[string]string{"alice.lock": "", "alice.mailbox.jsonl": `{"op":"add","entry":{"id":"synthetic-pending-preserved","kind":"gap","room":"!elsewhere:fixture"}}` + "\n"} {
		if err := os.WriteFile(filepath.Join(f.paths.Base, "state", name), []byte(content), 0600); err != nil {
			f.t.Fatal(err)
		}
	}
	return memberFiles(f.t, f.paths.Base)
}

// The positive existed at the frozen base: exact event fetch already bypassed
// local discovery. The new strict transport must retain it, both with and
// without a returned room_id, and retain an ordinary visible root.
func TestPrejoinRootServerAddressable(t *testing.T) {
	for _, kind := range []string{"absent-local", "absent-room-id", "ordinary-visible"} {
		t.Run(kind, func(t *testing.T) {
			f := newPrejoinFixture(t)
			if kind == "ordinary-visible" {
				f.visible = true
			}
			if kind == "absent-room-id" {
				f.body = strings.Replace(f.body, `,"room_id":"!selected:fixture"`, "", 1)
			}
			before := f.seedPreservedState()
			f.post(prejoinRoot).ok(t)
			calls, sent := f.traffic()
			if len(calls) != 2 || calls[0] != "GET /_matrix/client/v3/rooms/"+prejoinRoom+"/event/"+prejoinRoot || !strings.HasPrefix(calls[1], "PUT /_matrix/client/v3/rooms/"+prejoinRoom+"/send/") || len(sent) != 1 {
				t.Fatalf("unexpected root-resolution traffic: %v sends=%d", calls, len(sent))
			}
			h, ok := decodeHeader(sent[0].A2A)
			if !ok || h.Topic != prejoinRoot || h.Slug != "old" || h.Kind != "notice" || h.Re != "" || sent[0].RelatesTo == nil || sent[0].RelatesTo.EventID != prejoinRoot || !sent[0].RelatesTo.IsFallingBack {
				t.Fatal("ordinary post/root contract changed")
			}
			if !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
				t.Fatal("post changed preserved follow/mute/cursor/mailbox state")
			}
		})
	}
}

func TestPrejoinRootServerRefusal(t *testing.T) {
	for _, status := range []int{403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			f := newPrejoinFixture(t)
			f.status = status
			code := "M_FORBIDDEN"
			if status == 404 {
				code = "M_NOT_FOUND"
			}
			f.body = `{"errcode":"` + code + `","error":"server refuses historical root"}`
			before := f.seedPreservedState()
			r := f.post(prejoinRoot).failsWith(t, ExitError, "server")
			if !strings.Contains(r.stderr, "topic event "+prejoinRoot) || !strings.Contains(r.stderr, prejoinRoom) {
				t.Fatal("refusal did not identify selected root/room")
			}
			calls, sent := f.traffic()
			if len(calls) != 1 || len(sent) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
				t.Fatal("refusal fetched more history, sent, or mutated state")
			}
		})
	}
}

// Every invalid response is paired with the valid response in the positive
// test. A cycle is never walked, nor is the root hinted by an m.thread reply.
func TestPrejoinRootStrictRefusals(t *testing.T) {
	good := prejoinEvent(prejoinRoot, prejoinContent)
	cases := map[string]string{
		"wrong-room":        strings.Replace(good, prejoinRoom, "!foreign:fixture", 1),
		"wrong-event":       strings.Replace(good, prejoinRoot, "$different", 1),
		"missing-event":     strings.Replace(good, `"event_id":"$old-root",`, "", 1),
		"wrong-type":        strings.Replace(good, `"m.room.message"`, `"m.room.member"`, 1),
		"redacted":          strings.TrimSuffix(good, "}") + `,"unsigned":{"redacted_because":{}}}`,
		"duplicate":         strings.TrimSuffix(good, "}") + `,"event_id":"$different"}`,
		"trailing":          good + `{}`,
		"bad-content":       prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":123}`),
		"bad-relation":      prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"x","m.relates_to":"not-an-object"}`),
		"self-cycle":        prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"x","m.relates_to":{"rel_type":"m.thread","event_id":"$old-root"}}`),
		"cycle-or-reply":    prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"x","m.relates_to":{"rel_type":"m.thread","event_id":"$another-reply"}}`),
		"control-root":      prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"x","m.relates_to":{"rel_type":"m.thread","event_id":"$bad\nroot"}}`),
		"empty-thread-root": prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"x","m.relates_to":{"rel_type":"m.thread"}}`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPrejoinFixture(t)
			f.body = body
			before := f.seedPreservedState()
			r := f.post(prejoinRoot)
			if r.code != ExitError || r.stdout != "" {
				t.Fatalf("invalid root accepted: exit=%d", r.code)
			}
			calls, sent := f.traffic()
			if len(calls) != 1 || len(sent) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
				t.Fatal("invalid root caused extra lookup, send or state change")
			}
		})
	}
	t.Run("control-selection", func(t *testing.T) {
		f := newPrejoinFixture(t)
		before := f.seedPreservedState()
		r := f.post("$bad\nselection")
		calls, sent := f.traffic()
		if r.code == ExitOK || len(calls) != 0 || len(sent) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
			t.Fatal("invalid selection gained access")
		}
	})
}

// Preserve the existing reply-ID diagnostic instead of introducing automatic
// traversal or altering --re. Its single m.thread root ID is discoverable; its
// availability still requires a separate exact-root server read by the caller.
func TestPrejoinRootReplyDiscoveryIsBounded(t *testing.T) {
	f := newPrejoinFixture(t)
	f.body = prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"reply","m.relates_to":{"rel_type":"m.thread","event_id":"$discoverable-root","m.in_reply_to":{"event_id":"$native-parent"}}}`)
	f.post(prejoinRoot).failsWith(t, ExitError, "is a thread reply, not a topic root (use the root: $discoverable-root)")
	calls, sent := f.traffic()
	if len(calls) != 1 || len(sent) != 0 {
		t.Fatal("reply discovery traversed a root or native-reply chain")
	}
}

func TestPrejoinRootSlugBoundary(t *testing.T) {
	for _, visible := range []bool{false, true} {
		t.Run(fmt.Sprint(visible), func(t *testing.T) {
			f := newPrejoinFixture(t)
			f.visible = visible
			before := f.seedPreservedState()
			r := f.post("old")
			if visible {
				r.ok(t)
			} else {
				r.failsWith(t, ExitError, "root event ID")
			}
			calls, sent := f.traffic()
			want := 2
			if visible {
				want = 3
			}
			if len(calls) != want || len(sent) != map[bool]int{false: 0, true: 1}[visible] {
				t.Fatalf("slug widened discovery: calls=%v sends=%d", calls, len(sent))
			}
			for _, call := range calls {
				if strings.Contains(call, "/event/") {
					t.Fatal("slug invented an unknown event ID")
				}
			}
			if !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
				t.Fatal("slug resolution changed preserved state")
			}
		})
	}
}

// This is an explicit acceptance limit, not proof of #118: the frozen base's
// existing post recorder still adds an unmuted root to legacy Threads. Root
// resolution must not add a separate subscription or change that owned code.
func TestPrejoinRootLegacyFollowBoundary(t *testing.T) {
	f := newPrejoinFixture(t)
	before, err := config.LoadState(f.paths, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if before.HasThread(prejoinRoot) {
		t.Fatal("fixture already follows root")
	}
	f.post(prejoinRoot).ok(t)
	after, err := config.LoadState(f.paths, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !after.HasThread(prejoinRoot) {
		t.Fatal("legacy post behavior changed outside root-resolution scope")
	}
	calls, _ := f.traffic()
	if len(calls) != 2 {
		t.Fatal("post added subscription/account-data traffic")
	}
}

// The selected named connection guard also applies to root resolution. A
// binding changed after the observation must prevent the subsequent send.
func TestPrejoinRootSelectedConnection(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(fmt.Sprint(changed), func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			if err := config.SaveState(f.paths("beta", false), "alice", config.State{Muted: []string{prejoinRoot}}); err != nil {
				t.Fatal(err)
			}
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.URL.Scheme+"://"+r.URL.Host != f.r.Profiles["beta"].URL || r.Header.Get("Authorization") != "Bearer isolated-beta" {
					t.Fatal("wrong selected connection")
				}
				body := `{"event_id":"$sent"}`
				if calls == 1 {
					if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/"+prejoinRoom+"/event/"+prejoinRoot {
						t.Fatal("wrong root selection")
					}
					body = prejoinEvent(prejoinRoot, prejoinContent)
					if changed {
						c := f.r.Profiles["beta"]
						c.ID = "changed"
						f.r.Profiles["beta"] = c
						f.save()
					}
				} else if r.Method != "PUT" || !strings.HasPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"+prejoinRoom+"/send/") {
					t.Fatal("unexpected additional request")
				}
				return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			code := a.Run(context.Background(), []string{"--profile", "beta", "--as", "alice", "post", prejoinRoom, "--topic", prejoinRoot, "reply"})
			if changed {
				if code == ExitOK || calls != 1 || f.out.Len() != 0 {
					t.Fatal("changed binding sent a reply")
				}
			} else if code != ExitOK || calls != 2 {
				t.Fatalf("selected post failed: %s", f.err.String())
			}
		})
	}
}

// A same-origin redirect must not broaden an exact event observation to a
// second endpoint. This is a real HTTP redirect, not a mocked response URL.
func TestPrejoinRootRedirectRefusal(t *testing.T) {
	f := newPrejoinFixture(t)
	f.status = http.StatusFound
	f.body = `{"errcode":"M_UNKNOWN","error":"redirect"}`
	f.redirect = f.server.URL + "/_matrix/client/v3/rooms/!foreign:fixture/event/$foreign"
	before := f.seedPreservedState()
	f.post(prejoinRoot).failsWith(t, ExitError, "server did not return a usable root")
	calls, sent := f.traffic()
	if len(calls) != 1 || len(sent) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
		t.Fatal("redirect broadened read access")
	}
}

// Native replies remain ordinary candidate roots in their own right. The
// native parent is never fetched or substituted; no recursive lookup occurs.
func TestPrejoinRootNativeReplyDoesNotTraverse(t *testing.T) {
	f := newPrejoinFixture(t)
	f.body = prejoinEvent(prejoinRoot, `{"msgtype":"m.text","body":"native reply","m.relates_to":{"m.in_reply_to":{"event_id":"$not-discovered"}}}`)
	before := f.seedPreservedState()
	f.post(prejoinRoot).ok(t)
	calls, sent := f.traffic()
	if len(calls) != 2 || len(sent) != 1 || sent[0].RelatesTo.EventID != prejoinRoot || !reflect.DeepEqual(before, memberFiles(t, f.paths.Base)) {
		t.Fatal("native reply changed root or state")
	}
}
