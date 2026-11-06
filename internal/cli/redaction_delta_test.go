package cli

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Exercise real command dispatch, HTTP and the durable mailbox. The server
// deliberately returns stale originals/edits without unsigned redaction flags.
func TestDeltaVerificationTombstones(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	target := e.hs.inject(room, "@bob:test.local", msg(removedFixture, nil))
	edit := e.hs.inject(room, "@bob:test.local", editContent(target, removedFixture))
	raw := `{ "msgtype":"m.text", "body":"valid-neighbour\u000a", "formatted_body":"<b>valid</b>" }`
	neighbour := e.hs.inject(room, "@bob:test.local", msg("valid-neighbour", nil))
	e.hs.mu.Lock()
	for i := range e.hs.events {
		if e.hs.events[i].EventID == neighbour {
			e.hs.events[i].Content = json.RawMessage(raw)
		}
	}
	e.hs.mu.Unlock()
	e.hs.mu.Lock()
	e.hs.appendLocked(room, "@bob:test.local", "m.room.redaction", map[string]string{"redacts": target})
	e.hs.mu.Unlock()
	syncRedactionFixture(t, e, "alice")
	for _, id := range []string{target, edit} {
		r := e.as("alice", "read", room, "--event", id, "--verification-input", "--json")
		if r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "VERIFICATION_REDACTED") {
			t.Fatalf("known-redacted verification escaped: exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
		}
		noRemoved(t, r)
	}
	e.configure = func(a *App) {
		a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
			if strings.HasSuffix(r.URL.Path, "/event/"+neighbour) {
				body := `{"event_id":"` + neighbour + `","type":"m.room.message","sender":"@bob:test.local","content":` + raw + `}`
				return &http.Response{StatusCode: 200, Request: r, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body))}, nil
			}
			return http.DefaultTransport.RoundTrip(r)
		})}
	}
	r := e.as("alice", "read", room, "--event", neighbour, "--verification-input", "--json").ok(t)
	var v VerificationInput
	if err := json.Unmarshal([]byte(r.stdout), &v); err != nil {
		t.Fatal(err)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(v.ContentBase64)
	if err != nil || string(decoded) != raw || v.ReturnedEventID != neighbour {
		t.Fatal("valid verification raw contract changed")
	}
}

// Powerless peers can publish a redaction event without the homeserver
// applying it. Neither that claim nor a failed refetch may destroy pending mail.
func TestDeltaSyncAuthority(t *testing.T) {
	for _, mode := range []string{"powerless", "refetch-failed", "server-applied", "same-sender", "same-localpart-other-server", "missing-redactor", "forged-marker"} {
		t.Run(mode, func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			e.as("alice", "inbox").ok(t)
			target := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local "+removedFixture, nil))
			syncRedactionFixture(t, e, "alice")
			sender := "@carol:test.local" // ordinary powerless room member, not Bob
			if mode == "same-sender" {
				sender = "@bob:test.local"
			}
			if mode == "same-localpart-other-server" {
				sender = "@bob:other.invalid"
			}
			if mode == "missing-redactor" {
				sender = ""
			}
			if mode == "server-applied" {
				e.hs.redact(target, sender)
			}
			e.hs.mu.Lock()
			e.hs.appendLocked(room, sender, "m.room.redaction", map[string]string{"redacts": target})
			if mode == "forged-marker" {
				// The marker must not deserialize from a peer's event entity.
				raw, _ := json.Marshal(e.hs.events[len(e.hs.events)-1])
				raw = []byte(strings.Replace(string(raw), `"content":`, `"RedactionVerified":true,"content":`, 1))
				var wire matrix.Event
				if err := json.Unmarshal(raw, &wire); err != nil {
					t.Fatal(err)
				}
				e.hs.events[len(e.hs.events)-1] = wire
			}
			e.hs.mu.Unlock()
			if mode == "refetch-failed" {
				e.hs.fail("GET /_matrix/client/v3/rooms/{room}/event/{event}", 500, "M_UNKNOWN", removedFixture)
			}
			syncRedactionFixture(t, e, "alice")
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			applied := mode == "server-applied" || mode == "same-sender"
			if mb.IsRedacted(room, target) != applied || len(mb.Pending()) != 1 || mb.Pending()[0].ID != target || mb.Received(target) {
				t.Fatal("authority decision lost or altered arrival")
			}
			// A fresh session and a replay must make the same decision.
			e.hs.mu.Lock()
			original, _ := json.Marshal(msg("@alice:test.local "+removedFixture, nil))
			e.hs.events = append(e.hs.events, matrix.Event{RoomID: room, EventID: target, Sender: "@bob:test.local", Type: "m.room.message", Content: original})
			e.hs.mu.Unlock()
			syncRedactionFixture(t, e, "alice")
			r := e.as("alice", "inbox", "--json").ok(t)
			if applied {
				noRemoved(t, r)
				if !strings.Contains(r.stdout, `"redacted":true`) {
					t.Fatal("applied redaction not shown")
				}
			} else if !strings.Contains(r.stdout, removedFixture) {
				t.Fatal("uncorroborated claim destroyed pending body")
			}
		})
	}
}

// No sender identity can be inferred from a redaction that arrives before an
// unavailable target. A later original after restart is retained. By contrast,
// a stripped refetch is sufficient even before the original reaches sync.
func TestDeltaUnknownTargetAuthority(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "unavailable", true: "stripped-refetch"}[applied], func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			e.as("alice", "inbox").ok(t)
			if applied {
				e.configure = func(a *App) {
					a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
						if strings.HasSuffix(r.URL.Path, "/event/$future") {
							return syntheticEventResponse(r, matrix.Event{RoomID: room, EventID: "$future", Sender: "@bob:test.local", Type: "m.room.message", Content: json.RawMessage(`{}`), Unsigned: json.RawMessage(`{"redacted_because":{}}`)}), nil
						}
						return http.DefaultTransport.RoundTrip(r)
					})}
				}
			}
			e.hs.mu.Lock()
			redactor := "@bob:test.local"
			if applied {
				redactor = "@carol:test.local"
			}
			e.hs.appendLocked(room, redactor, "m.room.redaction", map[string]string{"redacts": "$future"})
			e.hs.mu.Unlock()
			// One deterministic sync with the configured HTTP observation, so the
			// redaction is judged before the original exists (a short wait timeout
			// can expire first on slower hosts and merge both into one batch).
			syncRedactionFixture(t, e, "alice")
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if mb.IsRedacted(room, "$future") != applied {
				t.Fatal("unknown target authority fabricated or ignored")
			}
			e.configure = nil
			e.hs.mu.Lock()
			e.hs.appendLocked(room, "@bob:test.local", "m.room.message", msg("@alice:test.local "+removedFixture, nil))
			e.hs.events[len(e.hs.events)-1].EventID = "$future"
			e.hs.appendLocked(room, "@bob:test.local", "m.room.message", editContent("$future", removedFixture))
			e.hs.mu.Unlock()
			syncRedactionFixture(t, e, "alice")
			mb, err = config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if len(mb.Pending()) != 2 || mb.Pending()[0].ID != "$future" || mb.Received("$future") {
				t.Fatal("future arrival lost")
			}
			r := e.as("alice", "inbox", "--json").ok(t)
			if applied {
				noRemoved(t, r)
			} else if !strings.Contains(r.stdout, removedFixture) {
				t.Fatal("unavailable target fabricated sender trust")
			}
		})
	}
}

func syntheticEventResponse(r *http.Request, ev matrix.Event) *http.Response {
	raw, _ := json.Marshal(ev)
	return &http.Response{StatusCode: 200, Request: r, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(string(raw))), ContentLength: int64(len(raw))}
}

func TestDeltaEmptyEventDoesNotRing(t *testing.T) {
	e, room := world(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	empty := e.hs.inject(room, "@bob:test.local", map[string]any{})
	// Empty content has no addressing and does not establish redaction authority.
	syncRedactionFixture(t, e, "alice")
	mb, err := config.OpenMailbox(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(mb.Pending()) != 0 || mb.IsRedacted(room, empty) {
		t.Fatal("raw empty event bypassed relevance")
	}
	valid := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local valid-addressed-neighbour", nil))
	syncRedactionFixture(t, e, "alice")
	mb, err = config.OpenMailbox(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(mb.Pending()) != 1 || mb.Pending()[0].ID != valid {
		t.Fatal("addressed neighbour stopped ringing")
	}
	// Server metadata, unlike empty content, preserves genuine redacted arrivals.
	e.hs.mu.Lock()
	id := e.hs.appendLocked(room, "@bob:test.local", "m.room.message", map[string]string{})
	e.hs.events[len(e.hs.events)-1].Unsigned = json.RawMessage(`{"redacted_because":{}}`)
	e.hs.mu.Unlock()
	syncRedactionFixture(t, e, "alice")
	mb, err = config.OpenMailbox(e.paths(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(mb.Pending()) != 2 || mb.Pending()[1].ID != id || !mb.IsRedacted(room, id) {
		t.Fatal("genuine redacted arrival lost")
	}
}

func TestDeltaArchiveExistingIndex(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", room, "removed-slug", "removed-title", "--body", removedFixture).ok(t).stdout)
	good := strings.TrimSpace(e.as("alice", "topic", "new", room, "valid-slug", "valid-title").ok(t).stdout)
	dir := filepath.Join(t.TempDir(), "archive")
	e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	index := filepath.Join(dir, "proj", IndexFileName)
	before, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(before), "removed-slug") || !strings.Contains(string(before), "removed-title") {
		t.Fatal("initial index control absent")
	}
	e.as("alice", "redact", room, root, "--reason", "cleanup").ok(t)
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	after, err := os.ReadFile(index)
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"removed-slug", "removed-title", removedFixture} {
		if strings.Contains(string(after), removed) {
			t.Fatal("index without --json-index retained redacted metadata")
		}
	}
	if !strings.Contains(string(after), "valid-slug") || !strings.Contains(string(after), "valid-title") || !strings.Contains(string(after), good) {
		t.Fatal("valid index neighbour lost")
	}
	var idx archiveIndex
	if json.Unmarshal(after, &idx) != nil || idx.RoomID != room {
		t.Fatal("index invalid")
	}
}

func TestDeltaRedactInvalidAlias(t *testing.T) {
	e, room := world(t)
	id := e.hs.inject(room, "@bob:test.local", msg(removedFixture, nil))
	r := e.as("bob", "redact", "bad alias "+removedFixture, id, "--reason", "cleanup")
	noRemoved(t, r)
	if r.code != ExitUsage || !strings.Contains(r.stderr, "REDACT_INVALID_ROOM") {
		t.Fatalf("invalid alias exit=%d stderr=%q", r.code, r.stderr)
	}
	if e.hs.CallCount(redactRoute) != 0 || e.hs.CallCount("GET /_matrix/client/v3/directory/room/{alias}") != 0 {
		t.Fatal("invalid alias reached HTTP")
	}
	e.as("bob", "redact", room, id, "--reason", "cleanup").ok(t)
}

func TestDeltaTransactionFalseSuccess(t *testing.T) {
	e, room := world(t)
	first := e.hs.inject(room, "@bob:test.local", msg("first", nil))
	second := e.hs.inject(room, "@bob:test.local", msg(removedFixture, nil))
	e.as("bob", "redact", room, first, "--reason", "cleanup", "--txn-id", "same-transaction").ok(t)
	r := e.as("bob", "redact", room, second, "--reason", "cleanup", "--txn-id", "same-transaction")
	noRemoved(t, r)
	if r.code != ExitError || !strings.Contains(r.stderr, "REDACT_UNCONFIRMED") || r.stdout != "" {
		t.Fatal("server's old transaction id accepted for new target")
	}
	mb, err := config.OpenMailbox(e.paths(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if !mb.IsRedacted(room, first) || mb.IsRedacted(room, second) {
		t.Fatal("reused transaction tombstoned another target")
	}
	r = e.as("bob", "read", room, "--event", second, "--json").ok(t)
	if !strings.Contains(r.stdout, removedFixture) {
		t.Fatal("second target body lost")
	}
	e.as("bob", "redact", room, first, "--reason", "cleanup", "--txn-id", "same-transaction").ok(t)
	if e.hs.CallCount(redactRoute) != 3 {
		t.Fatal("one-attempt/replay contract changed")
	}
	e.as("bob", "redact", room, second, "--reason", "cleanup", "--txn-id", "new-transaction").ok(t)
	mb, err = config.OpenMailbox(e.paths(), "bob")
	if err != nil || !mb.IsRedacted(room, second) {
		t.Fatal("fresh transaction valid control failed")
	}
}

func TestDeltaInitialSyncAuthority(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "powerless", true: "applied"}[applied], func(t *testing.T) {
			e, room := world(t)
			target := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local "+removedFixture, nil))
			if applied {
				e.hs.redact(target, "@carol:test.local")
			}
			e.hs.mu.Lock()
			e.hs.appendLocked(room, "@carol:test.local", "m.room.redaction", map[string]string{"redacts": target})
			e.hs.mu.Unlock()
			red := e.hs.lastEvent()
			// The general fake server omits initial history. Model the real
			// initial timeline's final redaction explicitly for this control.
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					resp, err := http.DefaultTransport.RoundTrip(r)
					if err != nil || !strings.HasSuffix(r.URL.Path, "/sync") || r.URL.Query().Get("since") != "" {
						return resp, err
					}
					var entity map[string]any
					if err := json.NewDecoder(resp.Body).Decode(&entity); err != nil {
						resp.Body.Close()
						return nil, err
					}
					resp.Body.Close()
					entity["rooms"] = map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{red}}}}}
					raw, _ := json.Marshal(entity)
					resp.Body = io.NopCloser(strings.NewReader(string(raw)))
					resp.ContentLength = int64(len(raw))
					return resp, nil
				})}
			}
			e.as("alice", "inbox").ok(t)
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if mb.IsRedacted(room, target) != applied || len(mb.Pending()) != 0 {
				t.Fatal("first position fabricated authority or replayed history")
			}
		})
	}
}

// A valid HTTP exchange is still not corroboration if it names another room
// or event. Each refusal retains the pending body and its delivery identity.
func TestDeltaRefetchBinding(t *testing.T) {
	for _, mode := range []string{"wrong-room", "wrong-event", "malformed", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			e.as("alice", "inbox").ok(t)
			target := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local "+removedFixture, nil))
			syncRedactionFixture(t, e, "alice")
			e.hs.mu.Lock()
			e.hs.appendLocked(room, "@carol:test.local", "m.room.redaction", map[string]string{"redacts": target})
			e.hs.mu.Unlock()
			calls := 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: ownerRoundTrip(func(r *http.Request) (*http.Response, error) {
					if strings.HasSuffix(r.URL.Path, "/event/"+target) {
						calls++
						ev := matrix.Event{RoomID: room, EventID: target, Type: "m.room.message", Sender: "@bob:test.local", Content: json.RawMessage(`{}`), Unsigned: json.RawMessage(`{"redacted_because":{}}`)}
						if mode == "wrong-room" {
							ev.RoomID = "!other"
						}
						if mode == "wrong-event" {
							ev.EventID = "$other"
						}
						resp := syntheticEventResponse(r, ev)
						if mode == "malformed" {
							resp.Body = io.NopCloser(strings.NewReader(removedFixture))
							resp.ContentLength = int64(len(removedFixture))
						}
						if mode == "redirect" {
							resp.StatusCode = 307
							resp.Header.Set("Location", "/unexpected")
						}
						return resp, nil
					}
					return http.DefaultTransport.RoundTrip(r)
				})}
			}
			syncRedactionFixture(t, e, "alice")
			mb, err := config.OpenMailbox(e.paths(), "alice")
			if err != nil {
				t.Fatal(err)
			}
			if calls == 0 || mb.IsRedacted(room, target) || len(mb.Pending()) != 1 || mb.Received(target) || !strings.Contains(string(mb.Pending()[0].Event), removedFixture) {
				t.Fatal("uncorroborated refetch destroyed pending mail")
			}
			e.configure = nil
			e.hs.redact(target, "@carol:test.local")
			e.hs.mu.Lock()
			e.hs.appendLocked(room, "@carol:test.local", "m.room.redaction", map[string]string{"redacts": target})
			e.hs.mu.Unlock()
			syncRedactionFixture(t, e, "alice")
			mb, err = config.OpenMailbox(e.paths(), "alice")
			if err != nil || !mb.IsRedacted(room, target) {
				t.Fatal("later applied positive control ignored")
			}
		})
	}
}
