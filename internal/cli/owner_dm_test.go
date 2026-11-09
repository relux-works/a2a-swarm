package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

func ownerFixture(t *testing.T) (*env, string, ownerDMFiles) {
	t.Helper()
	e, room := world(t)
	e.hs.mu.Lock()
	e.hs.liveClock = true
	e.hs.mu.Unlock()
	f := ownerFiles(e.paths(), "bob")
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@alice:" + testServer})
	return e, room, f
}
func ownerPut(t *testing.T, path string, v any) {
	t.Helper()
	if err := ownerWrite(context.Background(), path, v); err != nil {
		t.Fatal(err)
	}
}
func ownerLoad(t *testing.T, f ownerDMFiles) *ownerDMConfig {
	t.Helper()
	c, _, err := f.load()
	if err != nil || c == nil {
		t.Fatalf("config: %v, %v", c, err)
	}
	return c
}
func ownerEventAt(e *env, room, by, body string, ts time.Time) string {
	id := e.hs.inject(room, by, map[string]any{"msgtype": "m.text", "body": body})
	e.hs.mu.Lock()
	for i := range e.hs.events {
		if e.hs.events[i].EventID == id {
			e.hs.events[i].OriginServerTS = ts.UnixMilli()
		}
	}
	e.hs.mu.Unlock()
	return id
}
func ownerRequest(e *env, room, action, flags string, ts time.Time) string {
	text := "a2a owner-dm " + action + " bob"
	if flags != "" {
		text += " " + flags
	}
	return ownerEventAt(e, room, "@alice:"+testServer, text, ts)
}
func ownerBind(t *testing.T, e *env, room string, f ownerDMFiles) *ownerDMConfig {
	t.Helper()
	id := ownerRequest(e, room, "bind", "mirror", time.Now().Add(-time.Second))
	e.as("bob", "owner-dm", "bind", "--event", room, id).ok(t)
	return ownerLoad(t, f)
}
func ownerJoin(e *env, c *ownerDMConfig, sender string) {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	r := e.hs.rooms[c.Room]
	r.members[c.Owner] = "join"
	if r.stateSenders == nil {
		r.stateSenders = map[string]string{}
	}
	r.stateSenders["m.room.member|"+c.Owner] = sender
	r.state["m.room.member|"+c.Owner] = json.RawMessage(`{"membership":"join"}`)
}
func ownerSnapshot(t *testing.T, f ownerDMFiles) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, ext := range []string{"json", "pin", "bindings", "journal", "archive-exclude"} {
		b, st, err := ownerRead(f.path(ext))
		if err != nil {
			t.Fatal(err)
		}
		out[ext] = string(b)
		if st != nil {
			out[ext] = fmt.Sprintf("%o %s\n%s", st.Mode().Perm(), st.ModTime().UTC().Format(time.RFC3339Nano), b)
		}
	}
	return out
}
func ownerUnchanged(t *testing.T, f ownerDMFiles, want map[string]string) {
	t.Helper()
	got := ownerSnapshot(t, f)
	for key, b := range want {
		if got[key] != b {
			t.Fatalf("%s changed", key)
		}
	}
}
func ownerActivate(t *testing.T, e *env, f ownerDMFiles, c *ownerDMConfig) {
	t.Helper()
	ownerJoin(e, c, c.Owner)
	ownerMirror(t, e, "claude", ownerPayload(t, "claude", "activation", "ok"))
	if !ownerLoad(t, f).Active {
		t.Fatal("not active")
	}
}
func ownerMirror(t *testing.T, e *env, runtime string, payload json.RawMessage) {
	t.Helper()
	a := &App{Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		if k == config.EnvAgent {
			return "bob"
		}
		return e.extra[k]
	}, HTTP: e.hs.srv.Client(), Now: time.Now, ownerMirrorBudget: time.Second}
	if e.now != nil {
		a.Now = e.now
	}
	if e.configure != nil {
		e.configure(a)
	}
	a.ownerMirrorStop(context.Background(), runtime, hookInput{Payload: payload})
}
func ownerPayload(t *testing.T, runtime, turn, text string) json.RawMessage {
	t.Helper()
	file := "claude-code-2.1.309-stop.json"
	field := "prompt_id"
	if runtime == "codex" {
		file = "codex-cli-0.163.0-stop.json"
		field = "turn_id"
	}
	b, err := os.ReadFile(filepath.Join("testdata", "stop-payloads", file))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	v[field] = turn
	v["last_assistant_message"] = text
	v["transcript_path"] = "/must-never-be-opened/owner-dm-transcript"
	return jsonBytes(t, v)
}
func ownerBodies(e *env, room string) []string {
	var out []string
	for _, ev := range e.hs.roomMessages(room) {
		if ev.Sender == "@bob:"+testServer {
			out = append(out, ev.Message().Body)
		}
	}
	return out
}

// Tests 7/21: only a verified, fresh owner command creates a private room;
// host/no-pin and each invalid request have no persistent or room side effects.
func TestOwnerDMVerifiedBind(t *testing.T) {
	for _, mode := range []string{"owner-channel", "host", "no-pin"} {
		t.Run(mode, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			if mode == "no-pin" {
				os.Remove(f.path("pin"))
			} else {
				ownerPut(t, f.path("pin"), ownerDMPin{Mode: mode, Owner: "@alice:" + testServer})
			}
			before := ownerSnapshot(t, f)
			cID := ownerRequest(e, room, "bind", "tag mirror render=outside-frame", time.Now().Add(-time.Second))
			e.as("bob", "owner-dm", "bind", "--event", room, cID).ok(t)
			if mode != "owner-channel" {
				ownerUnchanged(t, f, before)
				if len(e.hs.createdRooms()) != 0 {
					t.Fatal("created room in host mode")
				}
				return
			}
			c := ownerLoad(t, f)
			if !c.Pending || c.Active || !c.Mirror || c.Owner != "@alice:"+testServer || c.BoundBy != cID || c.SubID == "" {
				t.Fatalf("wrong pending config: %+v", c)
			}
			req := e.hs.createdRooms()[0]
			if req.Preset != "private_chat" || !req.IsDirect || len(req.Invite) != 1 || req.Invite[0] != c.Owner {
				t.Fatalf("wrong room request: %+v", req)
			}
			excluded, err := f.exclusions()
			if err != nil || !excluded[c.Room] {
				t.Fatal("room not excluded")
			}
			statusBefore := ownerSnapshot(t, f)
			e.as("bob", "owner-dm", "status").ok(t)
			ownerUnchanged(t, f, statusBefore)
			ownerJoin(e, c, "@carol:"+testServer)
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "forged-join", "x"))
			if ownerLoad(t, f).Active {
				t.Fatal("foreign-authored join activated")
			}
			ownerJoin(e, c, c.Owner)
			statusBefore = ownerSnapshot(t, f)
			r := e.as("bob", "owner-dm", "status").ok(t)
			if !strings.Contains(r.stdout, "would activate") {
				t.Fatal(r.stdout)
			}
			ownerUnchanged(t, f, statusBefore)
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "first", "hello"))
			c = ownerLoad(t, f)
			if !c.Active || c.Pending {
				t.Fatal("owner join not activated")
			}
			bodies := ownerBodies(e, c.Room)
			if len(bodies) != 2 || !strings.Contains(bodies[0], ownerFingerprint(c)) || bodies[1] != "hello" {
				t.Fatalf("confirmation/mirror: %q", bodies)
			}
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "second", "again"))
			if len(ownerBodies(e, c.Room)) != 3 {
				t.Fatal("confirmation repeated")
			}
		})
	}
	for _, bad := range []string{"sender", "display-spoof", "body", "identity", "old", "future", "edited", "redacted", "not-joined", "cli-flags"} {
		t.Run(bad, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			sender := "@alice:" + testServer
			body := "a2a owner-dm bind bob mirror"
			ts := time.Now().Add(-time.Second)
			switch bad {
			case "sender", "display-spoof":
				sender = "@carol:" + testServer
			case "body":
				body += "\n"
			case "identity":
				body = "a2a owner-dm bind carol mirror"
			case "old":
				ts = time.Now().Add(-16 * time.Minute)
			case "future":
				ts = time.Now().Add(3 * time.Minute)
			}
			id := ownerEventAt(e, room, sender, body, ts)
			e.hs.mu.Lock()
			if bad == "not-joined" {
				e.hs.rooms[room].members["@bob:"+testServer] = "invite"
			}
			if bad == "edited" {
				for i := range e.hs.events {
					if e.hs.events[i].EventID == id {
						e.hs.events[i].Content = jsonBytes(t, map[string]any{"body": body, "m.relates_to": map[string]string{"rel_type": "m.replace", "event_id": "$prior"}})
					}
				}
			}
			e.hs.mu.Unlock()
			if bad == "redacted" {
				e.hs.redact(id, sender)
			}
			before := ownerSnapshot(t, f)
			args := []string{"owner-dm", "bind", "--event", room, id}
			if bad == "cli-flags" {
				args = append(args, "--mirror")
			}
			r := e.as("bob", args...)
			if r.code == 0 {
				t.Fatalf("accepted %s", bad)
			}
			ownerUnchanged(t, f, before)
			if len(e.hs.createdRooms()) != 0 {
				t.Fatal("created unverified room")
			}
		})
	}
}

// Tests 7/8/19/22–24: accepted event IDs and owner-keyed order watermark
// survive unbind, terminal unset and mirror-journal churn; stale mutations do
// not change any identity file. Fresh requests are the positive controls.
func TestOwnerDMWatermark(t *testing.T) {
	for _, last := range []string{"flags", "unbind", "terminal-unset"} {
		t.Run(last, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			c := ownerBind(t, e, room, f)
			ownerActivate(t, e, f, c)
			base := time.Now()
			e.now = func() time.Time { return base.Add(30 * time.Second) }
			oldBind := ownerRequest(e, c.Room, "bind", "tag", base)
			oldUnbind := ownerRequest(e, room, "unbind", "", base)
			newer := ownerRequest(e, c.Room, "bind", "mirror render=outside-frame", base.Add(time.Second))
			e.as("bob", "owner-dm", "bind", "--event", c.Room, newer).ok(t)
			if last == "unbind" {
				id := ownerRequest(e, room, "unbind", "", base.Add(2*time.Second))
				e.as("bob", "owner-dm", "unbind", "--event", room, id).ok(t)
			}
			if last == "terminal-unset" {
				e.configure = func(a *App) {
					a.ownerConfirm = func(s string) error {
						if s != "unset" {
							t.Fatal(s)
						}
						return nil
					}
				}
				e.as("bob", "owner-dm", "unset").ok(t)
			}
			before := ownerSnapshot(t, f)
			for _, req := range []struct{ action, room, id string }{{"bind", c.Room, oldBind}, {"unbind", room, oldUnbind}, {"bind", c.Room, newer}} {
				r := e.as("bob", "owner-dm", req.action, "--event", req.room, req.id)
				if r.code == 0 {
					t.Fatalf("accepted stale/replayed %s", req.id)
				}
				ownerUnchanged(t, f, before)
			}
			freshAction, freshRoom := "bind", c.Room
			if last == "flags" {
				freshFlags := ownerRequest(e, c.Room, "bind", "tag", base.Add(2500*time.Millisecond))
				e.as("bob", "owner-dm", "bind", "--event", c.Room, freshFlags).ok(t)
				if !ownerLoad(t, f).Tag {
					t.Fatal("fresh flag request not applied")
				}
				freshAction = "unbind"
				freshRoom = room
			}
			fresh := ownerRequest(e, freshRoom, freshAction, "", base.Add(3*time.Second))
			e.as("bob", "owner-dm", freshAction, "--event", freshRoom, fresh).ok(t)
		})
	}
}

// Tests 3/4/18: malformed/wrong-mode config remains byte-identical and only
// owner-dm mutations refuse; archive fails closed on invalid exclusions, and
// legacy state writes never change owner files or read another identity.
func TestOwnerDMInvalidIsolation(t *testing.T) {
	for _, bad := range []string{`{`, `{"v":2}`, `{"v":1,"active":null}`, "mode"} {
		t.Run(bad, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			c := ownerBind(t, e, room, f)
			_ = c
			original := []byte(bad)
			if bad == "mode" {
				original = []byte(readFile(t, f.path("json")))
			}
			os.WriteFile(f.path("json"), original, 0600)
			if bad == "mode" {
				os.Chmod(f.path("json"), 0644)
			}
			r := e.as("bob", "owner-dm", "status").ok(t)
			if !strings.Contains(r.stdout, "invalid") {
				t.Fatal(r.stdout)
			}
			for _, action := range []string{"ensure", "set", "unset", "bind", "unbind"} {
				r = e.as("bob", "owner-dm", action)
				if r.code == 0 {
					t.Fatal("invalid config mutation accepted")
				}
			}
			if string(mustOwnerBytes(t, f.path("json"))) != string(original) {
				t.Fatal("invalid overwritten")
			}
			out := t.TempDir()
			e.as("bob", "archive", c.Room, "--dir", out).ok(t)
			if len(dirEntries(t, out)) != 0 {
				t.Fatal("excluded room exported")
			}
			if got := e.as("alice", "owner-dm", "status").ok(t).stdout; got != "never-configured\n" {
				t.Fatal("foreign identity config read", got)
			}
			if _, err := os.Stat(ownerFiles(e.paths(), "alice").path("lock")); !os.IsNotExist(err) {
				t.Fatal("other identity provisioned")
			}
		})
	}
	e, room, f := ownerFixture(t)
	ownerBind(t, e, room, f)
	before := ownerSnapshot(t, f)
	_, err := config.UpdateState(e.paths(), "bob", func(st *config.State) error { *st = config.State{}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	ownerUnchanged(t, f, before)
	os.WriteFile(f.path("archive-exclude"), []byte("not a room\n"), 0600)
	e.as("bob", "archive", "all", "--dir", t.TempDir()).failsWith(t, ExitError, f.path("archive-exclude"))
}
func mustOwnerBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	return b
}

// Tests 7/9: status is read-only, pending expires at 24 h, and terminal
// lifecycle refuses noninteractive/wrong confirmation. Ensure refuses pending,
// and creates only from never-configured/inactive, following verified bind.
func TestOwnerDMTerminalAndExpiry(t *testing.T) {
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	for _, action := range []string{"ensure", "set", "unset"} {
		args := []string{"owner-dm", action}
		if action == "set" {
			args = append(args, "--room", c.Room)
		}
		before := ownerSnapshot(t, f)
		e.as("bob", args...).failsWith(t, ExitError, "interactive")
		ownerUnchanged(t, f, before)
	}
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return fmt.Errorf("type-back mismatch") } }
	e.as("bob", "owner-dm", "ensure").failsWith(t, ExitError, "type-back mismatch")
	e.configure = func(a *App) {
		a.ownerConfirm = func(s string) error {
			if s != "@alice:"+testServer && s != "unset" {
				t.Fatal(s)
			}
			return nil
		}
	}
	e.as("bob", "owner-dm", "ensure", "--mirror").failsWith(t, ExitError, "pending claim")
	e.as("bob", "owner-dm", "unset").ok(t)
	e.as("bob", "owner-dm", "ensure", "--mirror").ok(t)
	first := ownerLoad(t, f)
	before := ownerSnapshot(t, f)
	e.as("bob", "owner-dm", "ensure", "--mirror").failsWith(t, ExitError, "pending claim")
	ownerUnchanged(t, f, before)
	st, err := os.Stat(f.path("json"))
	if err != nil {
		t.Fatal(err)
	}
	e.now = func() time.Time { return st.ModTime().Add(24 * time.Hour) }
	before = ownerSnapshot(t, f)
	e.as("bob", "owner-dm", "status").ok(t)
	ownerUnchanged(t, f, before)
	ownerMirror(t, e, "claude", ownerPayload(t, "claude", "expired", "x"))
	if x := ownerLoad(t, f); x.Pending || x.Active {
		t.Fatal("pending did not expire")
	}
	e.as("bob", "owner-dm", "ensure", "--mirror").ok(t)
	second := ownerLoad(t, f)
	if second.Room == first.Room || second.Generation <= first.Generation {
		t.Fatal("inactive ensure did not create")
	}
	e.as("bob", "owner-dm", "unset").ok(t)
	for _, old := range []string{c.Room, first.Room, second.Room} {
		excluded, err := f.exclusions()
		if err != nil || !excluded[old] {
			t.Fatal("exclusion lost")
		}
	}
}

// Tests 7/19: the inclusive freshness boundaries have millisecond controls;
// a future-skewed accepted request remains single-use at receipt +16 minutes.
func TestOwnerDMFreshnessBoundaries(t *testing.T) {
	now := time.Now().Truncate(time.Millisecond)
	for _, tc := range []struct {
		name   string
		offset time.Duration
		valid  bool
	}{{"past-edge", -15 * time.Minute, true}, {"past-refusal", -15*time.Minute - time.Millisecond, false}, {"future-edge", 2 * time.Minute, true}, {"future-refusal", 2*time.Minute + time.Millisecond, false}} {
		t.Run(tc.name, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			e.now = func() time.Time { return now }
			id := ownerRequest(e, room, "bind", "mirror", now.Add(tc.offset))
			before := ownerSnapshot(t, f)
			r := e.as("bob", "owner-dm", "bind", "--event", room, id)
			if tc.valid {
				r.ok(t)
			} else {
				r.failsWith(t, ExitError, "freshness")
				ownerUnchanged(t, f, before)
				if len(e.hs.createdRooms()) != 0 {
					t.Fatal("stale request created room")
				}
			}
			if tc.name == "future-edge" {
				e.now = func() time.Time { return now.Add(16 * time.Minute) }
				before = ownerSnapshot(t, f)
				e.as("bob", "owner-dm", "bind", "--event", room, id).failsWith(t, ExitError, "already used")
				ownerUnchanged(t, f, before)
			}
		})
	}
}

// Tests 2/8: unbind/unset retain archive exclusions and stop all mirroring;
// an unverified unbind cannot change files, and unrelated subscriptions survive.
func TestOwnerDMInactiveRetention(t *testing.T) {
	for _, action := range []string{"unset", "unbind"} {
		t.Run(action, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			c := ownerBind(t, e, room, f)
			ownerActivate(t, e, f, c)
			before := ownerSnapshot(t, f)
			wrong := ownerEventAt(e, room, "@carol:"+testServer, "a2a owner-dm unbind bob", time.Now())
			e.as("bob", "owner-dm", "unbind", "--event", room, wrong).failsWith(t, ExitError, "unverified")
			ownerUnchanged(t, f, before)
			other := e.as("bob", "subscribe", "--room", room, "--kind", "notice").ok(t).stdout
			if action == "unset" {
				e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
				e.as("bob", "owner-dm", "unset").ok(t)
			} else {
				id := ownerRequest(e, room, "unbind", "", time.Now())
				e.as("bob", "owner-dm", "unbind", "--event", room, id).ok(t)
			}
			c = ownerLoad(t, f)
			if c.Active || c.Pending || c.SubID != "" {
				t.Fatal("still configured active")
			}
			before = ownerSnapshot(t, f)
			n := len(ownerBodies(e, c.Room))
			ownerMirror(t, e, "claude", ownerPayload(t, "claude", "inactive", "never"))
			ownerUnchanged(t, f, before)
			if len(ownerBodies(e, c.Room)) != n {
				t.Fatal("inactive mirror")
			}
			out := t.TempDir()
			e.as("bob", "archive", c.Room, "--dir", out).ok(t)
			if len(dirEntries(t, out)) != 0 {
				t.Fatal("former room archived")
			}
			subs := e.as("bob", "subs").ok(t).stdout
			if !strings.Contains(subs, strings.Fields(other)[0]) {
				t.Fatal("removed unrelated sub")
			}
		})
	}
}

// Tests 8/22–24: a pin owner change disables the old binding and starts a new
// owner-keyed watermark; same-room equal timestamps require actual timeline
// order, while equal timestamps in different rooms refuse without writes.
func TestOwnerDMPinAndEqualOrder(t *testing.T) {
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	ownerActivate(t, e, f, c)
	now := time.Now().Truncate(time.Millisecond)
	e.now = func() time.Time { return now.Add(time.Second) }
	flags := ownerRequest(e, c.Room, "bind", "mirror", now)
	e.as("bob", "owner-dm", "bind", "--event", c.Room, flags).ok(t)
	next := ownerRequest(e, c.Room, "bind", "tag mirror render=outside-frame", now)
	e.as("bob", "owner-dm", "bind", "--event", c.Room, next).ok(t)
	before := ownerSnapshot(t, f)
	equalOther := ownerRequest(e, room, "unbind", "", now)
	e.as("bob", "owner-dm", "unbind", "--event", room, equalOther).failsWith(t, ExitError, "watermark")
	ownerUnchanged(t, f, before)
	oldN := len(ownerBodies(e, c.Room))
	ownerPut(t, f.path("pin"), ownerDMPin{Mode: "owner-channel", Owner: "@carol:" + testServer})
	status := e.as("bob", "owner-dm", "status").ok(t)
	if !strings.HasPrefix(status.stdout, "inactive (pin owner differs)") {
		t.Fatal(status.stdout)
	}
	ownerMirror(t, e, "claude", ownerPayload(t, "claude", "new-owner", "old owner must not receive"))
	if len(ownerBodies(e, c.Room)) != oldN {
		t.Fatal("old owner still active")
	}
	newRequest := ownerEventAt(e, room, "@carol:"+testServer, "a2a owner-dm bind bob mirror", now.Add(-time.Second))
	e.as("bob", "owner-dm", "bind", "--event", room, newRequest).ok(t)
	fresh := ownerLoad(t, f)
	if fresh.Owner != "@carol:"+testServer || fresh.Room == c.Room || !fresh.Pending {
		t.Fatal("new pin not respected")
	}
}

// Tests 19/20: independent accepted-ID storage survives journal churn and
// preserves recent records plus per-generation audit records. Two simultaneous
// initial requests serialize; the loser cannot consume its event or create a room.
func TestOwnerDMBindingStoreConcurrency(t *testing.T) {
	e, room, f := ownerFixture(t)
	now := time.Now()
	ids := []string{ownerRequest(e, room, "bind", "mirror", now.Add(-time.Second)), ownerRequest(e, room, "bind", "mirror", now)}
	results := make(chan result, 2)
	for _, id := range ids {
		go func(id string) { results <- e.as("bob", "owner-dm", "bind", "--event", room, id) }(id)
	}
	a, b := <-results, <-results
	if (a.code == 0) == (b.code == 0) || len(e.hs.createdRooms()) != 1 {
		t.Fatalf("concurrent results %+v %+v", a, b)
	}
	store, err := f.bindings()
	if err != nil || len(store.Records) != 1 {
		t.Fatal(store, err)
	}
	if !strings.Contains(a.stderr+b.stderr, "pending claim") && !strings.Contains(a.stderr+b.stderr, "watermark") {
		t.Fatal("loser not pending refusal")
	}
	entries := []ownerDMJournalEntry{}
	for i := 0; i < 300; i++ {
		entries = append(entries, ownerDMJournalEntry{Key: fmt.Sprint(i), State: "sent 1/1", At: now})
	}
	if err = f.saveJournal(context.Background(), entries); err != nil {
		t.Fatal(err)
	}
	before := ownerSnapshot(t, f)
	e.as("bob", "owner-dm", "bind", "--event", room, store.Records[0].ID).failsWith(t, ExitError, "already used")
	ownerUnchanged(t, f, before)
	audit := ownerDMBindings{V: 1, Watermarks: map[string]ownerDMMutation{}}
	for i := 0; i < 1200; i++ {
		audit.accept(ownerDMMutation{ID: fmt.Sprint(i), Owner: "@alice:" + testServer, Generation: 1, Accepted: now.UnixMilli()}, now)
	}
	if len(audit.Records) != 1200 {
		t.Fatal("young record evicted")
	}
	later := now.Add(time.Hour)
	audit.accept(ownerDMMutation{ID: "generation-2", Owner: "@alice:" + testServer, Generation: 2, Accepted: later.UnixMilli()}, later)
	audit.accept(ownerDMMutation{ID: "later", Owner: "@alice:" + testServer, Generation: 2, Accepted: later.UnixMilli()}, later)
	found := false
	for _, r := range audit.Records {
		found = found || r.ID == "1199"
	}
	if !found || len(audit.Records) < 1024 {
		t.Fatal("audit generation record or 1024 minimum lost")
	}
}

// Tests 6/17: stored tag/render values have no inbound or doorbell effect in
// S1. Both settings retain the untrusted frame and omit owner_tag/owner counts.
func TestOwnerDMTagForcedOff(t *testing.T) {
	e, _, f := ownerFixture(t)
	e.sequentialSessions()
	c := ownerBind(t, e, "!projRoomID", f)
	ownerActivate(t, e, f, c)
	e.as("bob", "wait", "--timeout", "1ms").ok(t)
	e.hs.inject(c.Room, c.Owner, map[string]any{"msgtype": "m.text", "body": "owner control"})
	c = ownerLoad(t, f)
	c.Tag = true
	c.Render = "outside-frame"
	ownerPut(t, f.path("json"), c)
	outside := e.as("bob", "read", c.Room, "--json").ok(t).stdout
	c.Tag = false
	c.Render = "in-frame"
	ownerPut(t, f.path("json"), c)
	inside := e.as("bob", "read", c.Room, "--json").ok(t).stdout
	if outside != inside || strings.Contains(outside, "owner_tag") {
		t.Fatal("tag affected JSON")
	}
	bell := e.hookRT("claude", "bob", "prompt", `{"session_id":"tag-off"}`).ok(t)
	if strings.Contains(bell.stdout, "owner:") {
		t.Fatal("owner doorbell count in S1")
	}
	inbox := e.as("bob", "inbox").ok(t).stdout
	if !strings.Contains(inbox, "BEGIN UNTRUSTED PEER EVENTS") || strings.Contains(inbox, "OWNER MESSAGES") || strings.Contains(inbox, "owner-tagged") {
		t.Fatal("tag changed frame", inbox)
	}
}

// Test 24 / terminal-ordering interpretation: unset does not invalidate an
// unused, newer-than-watermark owner request issued before the terminal action.
// It still applies within its original freshness window; already-used and
// older-than-watermark controls are covered separately above.
func TestOwnerDMTerminalWatermarkInterpretation(t *testing.T) {
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	ownerActivate(t, e, f, c)
	id := ownerRequest(e, c.Room, "bind", "mirror", time.Now())
	before, err := f.bindings()
	if err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	e.as("bob", "owner-dm", "unset").ok(t)
	after, err := f.bindings()
	if err != nil {
		t.Fatal(err)
	}
	if string(jsonBytes(t, before)) != string(jsonBytes(t, after)) {
		t.Fatal("terminal changed owner watermark")
	}
	e.as("bob", "owner-dm", "bind", "--event", c.Room, id).ok(t)
	fresh := ownerLoad(t, f)
	if fresh.Room == c.Room || !fresh.Pending {
		t.Fatal("fresh unused pre-terminal bind did not apply")
	}
}

// Tests 3/7/9: invalid pins behave as host without provisioning; invalid
// config warns exactly once on non-owner commands without changing its bytes.
// A lock held through a bounded writer context refuses without mutating state.
func TestOwnerDMInvalidFilesAndWriterDeadline(t *testing.T) {
	for _, bad := range []string{`{`, `null`, `{"mode":"owner-channel","owner":null}`, `{"mode":"owner-channel","owner":"@alice:test.local","owner":null}`, `{"mode":"unknown","owner":"@alice:test.local"}`} {
		t.Run(bad, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			os.WriteFile(f.path("pin"), []byte(bad), 0600)
			before := ownerSnapshot(t, f)
			for _, action := range []string{"bind", "unbind"} {
				id := ownerRequest(e, room, action, "", time.Now())
				r := e.as("bob", "owner-dm", action, "--event", room, id).ok(t)
				if r.stdout != "" || r.stderr != "" {
					t.Fatal("host mode output")
				}
			}
			ownerUnchanged(t, f, before)
			if len(e.hs.createdRooms()) != 0 {
				t.Fatal("invalid pin created room")
			}
		})
	}
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	good := mustOwnerBytes(t, f.path("json"))
	bad := []byte("{")
	os.WriteFile(f.path("json"), bad, 0600)
	for _, args := range [][]string{{"inbox"}, {"wait", "--timeout", "1ms"}, {"read", room}, {"files", room}, {"archive", c.Room, "--dir", t.TempDir()}, {"hook", "stop", "--runtime", "claude"}} {
		r := e.as("bob", args...).ok(t)
		if strings.Count(r.stderr, "invalid owner-dm config") != 1 {
			t.Fatal("warning count", args, r.stderr)
		}
		if string(mustOwnerBytes(t, f.path("json"))) != "{" {
			t.Fatal("invalid overwritten")
		}
	}
	os.WriteFile(f.path("json"), good, 0600)
	unlock, err := f.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
	before := ownerSnapshot(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	e.extra[config.EnvAgent] = "bob"
	r := e.runCtx(ctx, "owner-dm", "unset")
	unlock()
	r.failsWith(t, ExitError, "owner-dm lock")
	ownerUnchanged(t, f, before)
	e.as("bob", "owner-dm", "unset").ok(t)
}

// Keeper disposition: refused pending requests have no lifecycle effects;
// removal skips confirmation, while a successful bound-room flag update activates.
func TestOwnerDMPendingMutationOrdering(t *testing.T) {
	for _, action := range []string{"bind", "unbind", "unset"} {
		t.Run(action, func(t *testing.T) {
			e, room, f := ownerFixture(t)
			c := ownerBind(t, e, room, f)
			ownerJoin(e, c, c.Owner)
			before := ownerSnapshot(t, f)
			stale := ownerRequest(e, room, "bind", "", time.Now().Add(-16*time.Minute))
			e.as("bob", "owner-dm", "bind", "--event", room, stale).failsWith(t, ExitError, "fresh")
			ownerUnchanged(t, f, before)
			if len(ownerBodies(e, c.Room)) != 0 {
				t.Fatal("refusal confirmed")
			}
			if action == "unset" {
				e.configure = func(a *App) { a.ownerConfirm = func(string) error { return nil } }
				e.as("bob", "owner-dm", "unset").ok(t)
			} else {
				target := room
				flags := ""
				if action == "bind" {
					target = c.Room
					flags = "mirror tag"
				}
				id := ownerRequest(e, target, action, flags, time.Now())
				e.as("bob", "owner-dm", action, "--event", target, id).ok(t)
			}
			got := ownerLoad(t, f)
			if action == "bind" {
				if !got.Active || got.Pending || !got.Tag {
					t.Fatal("successful flag update did not activate", got)
				}
				entries, err := f.journal(time.Now())
				if err != nil || len(entries) != 1 || entries[0].Key != fmt.Sprintf("%d|confirm", got.Generation) || entries[0].State != "sent:confirm" {
					t.Fatal(entries, err)
				}
				id := ownerRequest(e, c.Room, "bind", "mirror", time.Now().Add(time.Millisecond))
				e.as("bob", "owner-dm", "bind", "--event", c.Room, id).ok(t)
				if len(ownerBodies(e, c.Room)) != 1 {
					t.Fatal("confirmation repeated")
				}
			} else if got.Active || got.Pending || len(ownerBodies(e, c.Room)) != 0 {
				t.Fatal("removal activated or confirmed", got)
			}
		})
	}
}
