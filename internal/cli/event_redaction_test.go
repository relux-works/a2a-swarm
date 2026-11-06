package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const removedFixture = "REMOVED-PAYLOAD-42"
const redactRoute = "PUT /_matrix/client/v3/rooms/{room}/redact/{event}/{txn}"

type fakeRedactionRequest struct{ User, Room, Event, Txn, Reason string }

// This server fixture applies authority itself. The client must never query
// power levels or substitute its own decision for this response.
func (hs *fakeHS) redactHTTP(w http.ResponseWriter, r *http.Request, user string) {
	var in struct {
		Reason string `json:"reason"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil {
		writeErr(w, 400, "M_BAD_JSON", removedFixture)
		return
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, user, true)
	if room == nil {
		return
	}
	hs.redactionRequests = append(hs.redactionRequests, fakeRedactionRequest{user, room.id, r.PathValue("event"), r.PathValue("txn"), in.Reason})
	key := user + "|" + room.id + "|redact|" + r.PathValue("txn")
	if id := hs.txns[key]; id != "" {
		writeJSON(w, 200, map[string]string{"event_id": id})
		return
	}
	idx := -1
	for i, ev := range hs.events {
		if ev.EventID == r.PathValue("event") && ev.RoomID == room.id {
			idx = i
			break
		}
	}
	if idx < 0 {
		writeErr(w, 404, "M_NOT_FOUND", removedFixture)
		return
	}
	target := hs.events[idx]
	var pl struct {
		Users  map[string]int `json:"users"`
		Redact *int           `json:"redact"`
	}
	_ = json.Unmarshal(room.state["m.room.power_levels|"], &pl)
	need := 50
	if pl.Redact != nil {
		need = *pl.Redact
	}
	have := pl.Users[user]
	if user == roomCreator(room) {
		have = 100
	}
	if target.Sender != user && have < need {
		writeErr(w, 403, "M_FORBIDDEN", removedFixture)
		return
	}
	id := hs.appendLocked(room.id, user, "m.room.redaction", map[string]string{"redacts": target.EventID, "reason": in.Reason})
	hs.events[idx] = target.WithoutContent()
	if hs.txns == nil {
		hs.txns = map[string]string{}
	}
	hs.txns[key] = id
	writeJSON(w, 200, map[string]string{"event_id": id})
}

func noRemoved(t *testing.T, r result) {
	t.Helper()
	if strings.Contains(r.stdout+r.stderr, removedFixture) {
		t.Fatal("removed fixture escaped command output")
	}
}

func TestRedactCommandAuthority(t *testing.T) {
	for _, tc := range []struct {
		name, caller, sender, room, code string
		status                           int
		want                             int
		puts                             int
	}{
		{name: "own", caller: "bob", sender: "bob", want: ExitOK, puts: 1},
		{name: "moderator", caller: "alice", sender: "bob", want: ExitOK, puts: 1},
		{name: "other-without-power", caller: "bob", sender: "alice", code: "M_FORBIDDEN", want: ExitError, puts: 1},
		{name: "unknown", caller: "bob", sender: "bob", code: "M_NOT_FOUND", want: ExitError},
		{name: "wrong-room", caller: "bob", sender: "bob", room: "!lobbyRoomID", code: "M_NOT_FOUND", want: ExitError},
		{name: "server-error", caller: "bob", sender: "bob", status: 500, code: "REDACT_FAILED", want: ExitError, puts: 1},
		{name: "own-server-refused", caller: "bob", sender: "bob", status: 403, code: "M_FORBIDDEN", want: ExitError, puts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, room := world(t)
			id := e.hs.inject(room, "@"+tc.sender+":"+testServer, map[string]any{"msgtype": "m.text", "body": removedFixture})
			if tc.name == "unknown" {
				id = "$unknown"
			}
			if tc.room != "" {
				room = tc.room
			}
			if tc.status != 0 {
				code := "M_UNKNOWN"
				if tc.status == 403 {
					code = "M_FORBIDDEN"
				}
				e.hs.fail(redactRoute, tc.status, code, removedFixture)
			}
			reason := `cleanup "quote" \\ unicode π <tag> $(literal)`
			r := e.as(tc.caller, "redact", room, id, "--reason", reason, "--txn-id", "replay-42")
			noRemoved(t, r)
			if r.code != tc.want || tc.code != "" && !strings.Contains(r.stderr, tc.code) {
				t.Fatalf("code=%d stderr=%s", r.code, r.stderr)
			}
			if e.hs.CallCount(redactRoute) != tc.puts {
				t.Fatalf("PUT attempts=%d want=%d", e.hs.CallCount(redactRoute), tc.puts)
			}
			if e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/state/{type}/{key...}") != 0 || e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/state/{type}") != 0 {
				t.Fatal("client consulted state for authority")
			}
			if tc.want == ExitOK {
				req := e.hs.redactionRequests[0]
				if req.User != "@"+tc.caller+":"+testServer || req.Room != room || req.Event != id || req.Txn != "replay-42" || req.Reason != reason {
					t.Fatal("request identity/path/reason not preserved")
				}
				if !strings.Contains(r.stderr, "Rotate leaked secrets") || strings.Contains(r.stdout+r.stderr, reason) {
					t.Fatal("limit missing or reason echoed")
				}
				first := strings.TrimSpace(r.stdout)
				replay := e.as(tc.caller, "redact", room, id, "--reason", reason, "--txn-id", "replay-42").ok(t)
				if strings.TrimSpace(replay.stdout) != first || e.hs.CallCount(redactRoute) != 2 {
					t.Fatal("explicit replay did not reuse transaction")
				}
				if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != 0 {
					t.Fatal("unsolicited notice sent")
				}
			}
		})
	}
}

func TestRedactValidationAndRedactionTarget(t *testing.T) {
	e, room := world(t)
	for _, args := range [][]string{
		{room, "$event"}, {room, "$event", "--reason"}, {room, "$event", "--reason="},
		{room, "$event", "--reason", strings.Repeat("π", 513)}, {room, "$event", "--reason", "\xff"},
		{room, "$event", "--reason", "line\n" + removedFixture}, {room, "$event", "--reason", "\x1b[2J" + removedFixture},
		{room, "$event", "--reason", "x", "--txn-id", "bad\n" + removedFixture},
		{room, "event", "--reason", "x"}, {room, "$event", "--json=" + removedFixture},
	} {
		r := e.as("bob", append([]string{"redact"}, args...)...)
		noRemoved(t, r)
		if r.code != ExitUsage {
			t.Fatal("invalid input accepted")
		}
	}
	if e.hs.CallCount("GET /_matrix/client/v3/rooms/{room}/event/{event}") != 0 || e.hs.CallCount(redactRoute) != 0 {
		t.Fatal("invalid input reached HTTP")
	}
	e.hs.mu.Lock()
	id := e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", map[string]string{"redacts": "$original", "reason": removedFixture})
	e.hs.mu.Unlock()
	r := e.as("bob", "redact", room, id, "--reason", "cleanup")
	noRemoved(t, r)
	r.failsWith(t, ExitError, "REDACT_TARGET_REDACTION")
	if e.hs.CallCount(redactRoute) != 0 {
		t.Fatal("redaction-of-redaction was sent")
	}
	// The inclusive byte boundary is a positive control for validation.
	id = e.hs.inject(room, "@bob:test.local", map[string]any{"body": removedFixture, "msgtype": "m.text"})
	e.as("bob", "redact", room, id, "--reason", strings.Repeat("π", 512)).ok(t)
}

func TestRedactSafeFailuresAndOneAttempt(t *testing.T) {
	for _, mode := range []string{"get-malformed", "put-malformed", "put-transport", "redirect", "auth", "oversized-target", "local-cache"} {
		t.Run(mode, func(t *testing.T) {
			e, room := world(t)
			id := e.hs.inject(room, "@bob:test.local", map[string]any{"body": removedFixture, "msgtype": "m.text"})
			put := 0
			get := 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.Contains(r.URL.Path, "/event/") {
						get++
					}
					if strings.Contains(r.URL.Path, "/redact/") {
						put++
					}
					if mode == "put-transport" && r.Method == "PUT" {
						return nil, errors.New(removedFixture)
					}
					if mode == "local-cache" && r.Method == "PUT" {
						os.MkdirAll(filepath.Dir(e.paths().MailboxFile("bob")), 0700)
						os.WriteFile(e.paths().MailboxFile("bob"), []byte("invalid "+removedFixture+"\n"), 0600)
					}
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil {
						return resp, err
					}
					change := (mode == "get-malformed" && strings.Contains(r.URL.Path, "/event/")) || (mode == "put-malformed" && r.Method == "PUT") || (mode == "oversized-target" && strings.Contains(r.URL.Path, "/event/"))
					if change {
						resp.Body.Close()
						body := `{"type":` + removedFixture
						if mode == "put-malformed" {
							body = `{"event_id":` + removedFixture
						}
						if mode == "oversized-target" {
							body = strings.Repeat(" ", 2<<20) + removedFixture
						}
						resp.Body = io.NopCloser(strings.NewReader(body))
						resp.ContentLength = int64(len(body))
					}
					if mode == "redirect" && r.Method == "PUT" {
						resp.Body.Close()
						resp.StatusCode = 307
						resp.Header.Set("Location", "/unexpected/"+removedFixture)
						resp.Body = io.NopCloser(strings.NewReader(removedFixture))
					}
					return resp, nil
				})}
			}
			if mode == "auth" {
				e.hs.fail(redactRoute, 401, "M_UNKNOWN_TOKEN", removedFixture)
			}
			r := e.as("bob", "redact", room, id, "--reason", "cleanup")
			noRemoved(t, r)
			if r.code != ExitError || put > 1 || get != 1 {
				t.Fatalf("exit=%d get=%d put=%d", r.code, get, put)
			}
			if !strings.Contains(r.stderr, "redact transaction:") {
				t.Fatal("actual transaction unavailable after ambiguity")
			}
		})
	}
}

// All state mutation uses the actual sync ingestion and durable mailbox, not
// a renderer-only simulation. Each next command constructs a fresh session.
func TestRedactionSyncDurabilityAndOutputs(t *testing.T) {
	for _, format := range []string{"legacy", "v11", "unsigned"} {
		t.Run(format, func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			e.as("alice", "inbox").ok(t)
			content := map[string]any{"msgtype": "m.file", "body": "@alice:test.local " + removedFixture, "formatted_body": removedFixture, ContentKey: map[string]any{"v": 1, "type": "message", "to": []string{"alice"}, "subject": removedFixture, "attachment": map[string]any{"name": removedFixture, "size": 5, "sha256": removedFixture, "mxc": "mxc://test.local/" + removedFixture, "encoding": map[string]string{"private": removedFixture}}}}
			id := e.hs.inject(room, "@bob:test.local", content)
			e.as("alice", "wait", "--timeout", "100ms").ok(t) // retains arrival, shown but unreceived
			neighbour := e.hs.inject(room, "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "@alice:test.local neighbour control"})
			e.hs.mu.Lock()
			if format == "unsigned" {
				// Replayed original with server metadata but even a stale body must scrub.
				ev := e.hs.events[0]
				ev.Unsigned = json.RawMessage(`{"redacted_because":{"content":{"reason":"` + removedFixture + `"}}}`)
				e.hs.events = append(e.hs.events, ev)
			} else {
				c := map[string]string{"reason": removedFixture}
				if format == "v11" {
					c["redacts"] = id
				}
				red := e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", c)
				if format == "legacy" {
					e.hs.events[len(e.hs.events)-1].Redacts = id
				}
				_ = red
			}
			e.hs.mu.Unlock()
			// Bring sync current without receiving the pending entries.
			a := &App{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(k string) string {
				if k == config.EnvConfigDir {
					return e.dir
				}
				return ""
			}, HTTP: e.hs.srv.Client(), Now: time.Now, as: "alice"}
			s, err := a.session()
			if err != nil {
				t.Fatal(err)
			}
			st, err := config.LoadState(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.syncNow(context.Background(), st, nil, time.Time{}); err != nil {
				t.Fatal(err)
			}
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if len(mb.Pending()) != 2 || mb.Pending()[0].ID != id || mb.Pending()[1].ID != neighbour || mb.Received(id) {
				t.Fatal("redaction lost arrival/order/unrelated mail")
			}
			disk, err := os.ReadFile(e.paths().MailboxFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(disk), removedFixture) {
				t.Fatal("mailbox retained removed content")
			}
			for _, args := range [][]string{{"read", room, "--event", id}, {"read", room, "--event", id, "--json"}, {"read", room}, {"files", room, "--json"}, {"fetch", room, id, "-o", filepath.Join(t.TempDir(), "file")}} {
				r := e.as("alice", args...)
				noRemoved(t, r)
			}
			for _, asJSON := range []bool{false, true} {
				args := []string{"inbox"}
				if asJSON {
					args = append(args, "--json")
				}
				e.configure = func(a *App) { a.afterPrint = func() error { return errors.New("fixture interrupted before receipt") } }
				r := e.as("alice", args...)
				noRemoved(t, r)
				if !strings.Contains(r.stdout, "[redacted]") || !strings.Contains(r.stdout, "neighbour control") {
					t.Fatal("tombstone or positive neighbour missing")
				}
			}
			e.configure = nil
			e.as("alice", "inbox").ok(t)
			mb, _ = config.OpenMailbox(e.paths(), "alice")
			if !mb.Received(id) || !mb.Received(neighbour) {
				t.Fatal("explicit inbox did not receipt")
			}
		})
	}
}

func syncRedactionFixture(t *testing.T, e *env, name string) {
	t.Helper()
	a := &App{Stdout: io.Discard, Stderr: io.Discard, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}, HTTP: e.hs.srv.Client(), Now: time.Now, as: name}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	st, err := config.LoadState(e.paths(), name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.syncNow(context.Background(), st, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestRedactionBeforeOriginalReplayRestart(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	e.hs.mu.Lock()
	e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", map[string]string{"redacts": "$future", "reason": removedFixture})
	e.hs.mu.Unlock()
	syncRedactionFixture(t, e, "alice")
	mb, err := config.OpenMailbox(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !mb.IsRedacted(room, "$future") || len(mb.Pending()) != 0 {
		t.Fatal("target not retained independently of delivery")
	}
	e.hs.mu.Lock()
	e.hs.appendLocked(room, "@bob:test.local", "m.room.message", map[string]string{"body": "@alice:test.local " + removedFixture, "msgtype": "m.text"})
	e.hs.events[len(e.hs.events)-1].EventID = "$future"
	original := e.hs.events[len(e.hs.events)-1]
	e.hs.mu.Unlock()
	syncRedactionFixture(t, e, "alice")
	mb, _ = config.OpenMailbox(e.paths(), "alice")
	if len(mb.Pending()) != 1 || mb.Pending()[0].ID != "$future" || mb.Received("$future") {
		t.Fatal("future arrival lost")
	}
	r := e.as("alice", "inbox", "--json").ok(t)
	noRemoved(t, r)
	if !strings.Contains(r.stdout, `"redacted":true`) {
		t.Fatal("JSON tombstone missing")
	}
	// Replay after receipt and restart must neither deliver nor retain old data.
	e.hs.mu.Lock()
	e.hs.events = append(e.hs.events, original)
	e.hs.mu.Unlock()
	r = e.as("alice", "inbox").ok(t)
	noRemoved(t, r)
	if r.stdout != "no new peer events\n" {
		t.Fatal("received original resurrected")
	}
	r = e.as("alice", "read", room, "--event", "$future", "--json").ok(t)
	noRemoved(t, r)
	data, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	if strings.Contains(string(data), removedFixture) {
		t.Fatal("replayed content persisted")
	}
}

func TestRedactionLimitedSyncBackfill(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "@alice:test.local " + removedFixture})
	syncRedactionFixture(t, e, "alice")
	e.hs.mu.Lock()
	redID := e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", map[string]string{"redacts": id})
	red := e.hs.events[len(e.hs.events)-1]
	// Force the redaction into the backfill rather than the live tail.
	e.hs.appendLocked(room, "@bob:test.local", "m.room.message", map[string]string{"msgtype": "m.text", "body": "unrelated tail"})
	e.hs.syncLimit = 1
	e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {"m1": {page: matrix.Page{Chunk: []matrix.Event{red}}}}}
	e.hs.mu.Unlock()
	_ = redID
	syncRedactionFixture(t, e, "alice")
	mb, _ := config.OpenMailbox(e.paths(), "alice")
	if len(mb.Pending()) != 1 || !mb.IsRedacted(room, id) {
		t.Fatal("limited sync failed to retain redaction from backfill")
	}
	noRemoved(t, e.as("alice", "inbox", "--json").ok(t))
}

func TestRedactionArchiveAndMediaObservation(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", room, "private-topic", strings.Repeat("Title ", 1024)+removedFixture, "--body", removedFixture).ok(t).stdout)
	p := writeFile(t, t.TempDir(), "payload.txt", []byte("local media fixture bytes"))
	id := strings.TrimSpace(e.as("bob", "attach", room, "--topic", root, p, removedFixture).ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	att, ok := attachmentOf(ev)
	if !ok {
		t.Fatal("attachment control missing")
	}
	dir := filepath.Join(t.TempDir(), "archive")
	e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	before := readFileStr(t, filepath.Join(dir, "proj", "private-topic.md"))
	if !strings.Contains(before, removedFixture) {
		t.Fatal("initial export positive control failed")
	}
	// Paired observation is strictly this local HTTP fixture: bytes remain
	// available by known MXC even after the event is redacted. No live claim.
	first := filepath.Join(t.TempDir(), "before")
	e.as("bob", "fetch", room, id, "-o", first).ok(t)
	e.as("bob", "redact", room, id, "--reason", "cleanup").ok(t)
	e.as("alice", "redact", room, root, "--reason", "cleanup").ok(t)
	blocked := e.as("bob", "fetch", room, id, "-o", filepath.Join(t.TempDir(), "blocked"))
	noRemoved(t, blocked)
	if blocked.code != ExitError {
		t.Fatal("redacted event attachment still downloadable through event")
	}
	after := filepath.Join(t.TempDir(), "after")
	e.as("bob", "fetch", att.MXC, "--sha256", att.SHA256, "-o", after).ok(t)
	if readFileStr(t, first) != readFileStr(t, after) {
		t.Fatal("paired local media fixture changed unexpectedly")
	}
	r := e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	noRemoved(t, r)
	if !strings.Contains(r.stderr, "earlier archive Git commits") {
		t.Fatal("earlier history warning missing")
	}
	files, err := os.ReadDir(filepath.Join(dir, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.Contains(readFileStr(t, filepath.Join(dir, "proj", f.Name())), removedFixture) {
			t.Fatal("later export retained removed body or title")
		}
	}
	// The earlier captured copy persists; the command cannot recall it.
	if !strings.Contains(before, removedFixture) {
		t.Fatal("earlier copy control missing")
	}
	t.Log("SCOPED ACTUAL: local HTTP fixture media fetch succeeded before and after event redaction; live homeserver/media UNRUN")
}

func TestRedactionCrashBeforeCursor(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "@alice:test.local " + removedFixture})
	syncRedactionFixture(t, e, "alice")
	before, err := config.LoadState(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	e.hs.mu.Lock()
	e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", map[string]string{"redacts": id})
	e.hs.mu.Unlock()
	e.configure = func(a *App) { a.afterAppend = func() error { return errors.New("fixture crash before cursor") } }
	r := e.as("alice", "inbox")
	noRemoved(t, r)
	r.failsWith(t, ExitError, "fixture crash before cursor")
	after, err := config.LoadState(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if before.NextBatch != after.NextBatch {
		t.Fatal("failed persist advanced cursor")
	}
	mb, err := config.OpenMailbox(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !mb.IsRedacted(room, id) || len(mb.Pending()) != 1 || mb.Received(id) {
		t.Fatal("crash lost redaction or pending arrival")
	}
	data, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	if strings.Contains(string(data), removedFixture) {
		t.Fatal("crash left old payload in mailbox")
	}
	e.configure = nil
	r = e.as("alice", "inbox", "--json").ok(t)
	noRemoved(t, r)
	if !strings.Contains(r.stdout, `"redacted":true`) {
		t.Fatal("restart did not replay safe tombstone")
	}
}
