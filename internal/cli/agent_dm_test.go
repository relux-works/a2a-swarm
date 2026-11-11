package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type agentDMTestTransport struct {
	base      http.RoundTripper
	e         *env
	mu        sync.Mutex
	mutations map[string]int
	fault     string
}

func (tr *agentDMTestTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	path := r.URL.Path
	tr.mu.Lock()
	fault := tr.fault
	if r.Method != "GET" {
		tr.mutations[path]++
	}
	tr.mu.Unlock()
	resp, err := tr.base.RoundTrip(r)
	if err != nil {
		return resp, err
	}
	// Supply the fake's missing server state metadata. Invitation sender and
	// v11/v12 creation sender are actual event senders, never marker inference.
	if r.Method == "POST" && (strings.HasSuffix(path, "/createRoom") || strings.HasSuffix(path, "/invite") || strings.Contains(path, "/join/")) {
		tr.e.hs.mu.Lock()
		for _, room := range tr.e.hs.rooms {
			if !strings.HasPrefix(room.alias, "#a2a-dm-v1-") {
				continue
			}
			var create struct {
				Creator string `json:"creator"`
			}
			_ = json.Unmarshal(room.state["m.room.create|"], &create)
			if room.stateSenders == nil {
				room.stateSenders = map[string]string{}
			}
			for key := range room.state {
				if room.stateSenders[key] == "" {
					room.stateSenders[key] = create.Creator
				}
			}
			for user, membership := range room.members {
				key := "m.room.member|" + user
				room.state[key], _ = json.Marshal(map[string]string{"membership": membership})
				room.stateSenders[key] = user
				if membership == "invite" {
					room.stateSenders[key] = create.Creator
				}
			}
		}
		tr.e.hs.mu.Unlock()
	}
	if fault == "create-ack" && strings.HasSuffix(path, "/createRoom") || fault == "invite-ack" && strings.HasSuffix(path, "/invite") || fault == "send-ack" && strings.Contains(path, "/send/") || fault == "leave-ack" && strings.HasSuffix(path, "/leave") {
		resp.Body.Close()
		return nil, errors.New("synthetic lost acknowledgement")
	}
	if fault == "state" && strings.HasSuffix(path, "/state") {
		resp.Body.Close()
		return nil, errors.New("synthetic state read failure")
	}
	return resp, nil
}
func (tr *agentDMTestTransport) setFault(f string) { tr.mu.Lock(); defer tr.mu.Unlock(); tr.fault = f }
func (tr *agentDMTestTransport) count(fragment string) int {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	n := 0
	for k, v := range tr.mutations {
		if strings.Contains(k, fragment) {
			n += v
		}
	}
	return n
}

func agentDMFixture(t *testing.T) (*env, *agentDMTestTransport, string, string) {
	t.Helper()
	e := newEnv(t)
	e.dir = principalDMTestResolve(t, e.dir)
	a, b := e.agent("alice"), e.agent("bob")
	e.agent("parent")
	tr := &agentDMTestTransport{base: e.hs.srv.Client().Transport, e: e, mutations: map[string]int{}}
	e.configure = func(a *App) { a.HTTP = &http.Client{Transport: tr} }
	e.sequentialSessions()
	for _, pair := range [][3]string{{"alice", a, b}, {"bob", b, a}} {
		stageAgentAdmission(t, e, pair[0], pair[1], pair[2])
	}
	return e, tr, a, b
}
func principalDMTestResolve(t *testing.T, path string) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func stageAgentAdmission(t *testing.T, e *env, name, me, peer string) {
	t.Helper()
	dir := agentDMDir(e.paths(), name)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	a := &App{Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}}
	s := &session{app: a, paths: e.paths(), name: name, creds: config.Credentials{UserID: me}, client: matrix.New(e.hs.srv.URL, ""), server: testServer}
	connection, err := s.agentConnection()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"contract": agentDMVersion, "caller_mxid": me, "connection_id": connection, "caller_role": "keeper", "caller_admission_ref": "fixture-host-decision-1", "peers": []any{map[string]string{"mxid": peer, "role": "orchestrator", "admission_ref": "fixture-host-decision-2"}}})
	if err = os.WriteFile(filepath.Join(dir, "admission.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func agentDMEntry(t *testing.T, e *env, name, peer string) agentDMRecord {
	t.Helper()
	var st agentDMRecords
	raw, err := agentRead(agentDMDir(e.paths(), name), "records.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = agentDecode(raw, &st); err != nil {
		t.Fatal(err)
	}
	d, ok := st.Pairs[peer]
	if !ok {
		t.Fatal("missing pair record")
	}
	return d
}

func agentMessageEvents(e *env, room string) []matrix.Event {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	var out []matrix.Event
	for _, ev := range e.hs.events {
		if ev.RoomID == room && ev.Type == "m.room.message" {
			out = append(out, ev)
		}
	}
	return out
}
func activeAgentFixture(t *testing.T) (*env, *agentDMTestTransport, string, string, string) {
	t.Helper()
	e, tr, a, b := agentDMFixture(t)
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	d := agentDMEntry(t, e, "alice", b)
	if d.Room == nil {
		t.Fatal("missing room")
	}
	e.as("bob", "agent-dm", "ensure", a).failsWith(t, ExitError, "needs_explicit_join")
	e.as("bob", "join", *d.Room).ok(t)
	e.as("bob", "agent-dm", "ensure", a).ok(t)
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	return e, tr, a, b, *d.Room
}

func TestAgentDMLifecycle(t *testing.T) {
	e, tr, a, b := agentDMFixture(t)
	if r := e.as("bob", "agent-dm", "ensure", a).ok(t); !strings.Contains(r.stdout, "needs_creator") {
		t.Fatal(r.stdout)
	}
	if tr.count("createRoom") != 0 {
		t.Fatal("noncreator created")
	}
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	d := agentDMEntry(t, e, "alice", b)
	if d.Room == nil || d.Qualified || !d.Enabled {
		t.Fatalf("not pending %+v", d)
	}
	room := *d.Room
	if tr.count("createRoom") != 1 || tr.count("/invite") != 1 {
		t.Fatal("wrong create/invite count")
	}
	before := readFile(t, filepath.Join(agentDMDir(e.paths(), "alice"), "records.json"))
	e.as("alice", "agent-dm", "status", b, "--json").ok(t)
	if before != readFile(t, filepath.Join(agentDMDir(e.paths(), "alice"), "records.json")) {
		t.Fatal("status wrote records")
	}
	e.as("alice", "agent-dm", "say", b, "--kind", "question", "pending").failsWith(t, ExitError, "no active")
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	e.as("bob", "agent-dm", "ensure", a).failsWith(t, ExitError, "needs_explicit_join")
	if got := agentDMEntry(t, e, "bob", a); got.Room == nil || *got.Room != room || got.Qualified {
		t.Fatal("unreadable pending audience claimed active")
	}
	if tr.count("/join/") != 0 {
		t.Fatal("ensure autojoined")
	}
	e.as("bob", "join", room).ok(t)
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	e.as("bob", "agent-dm", "ensure", a).ok(t)
	if tr.count("createRoom") != 1 || tr.count("/invite") != 1 {
		t.Fatal("repeated remote effects")
	}
	e.as("alice", "agent-dm", "say", b, "--kind", "question", "hello peer").ok(t)
	if bodies := agentMessageEvents(e, room); len(bodies) != 1 || !strings.Contains(bodies[0].Message().Body, "hello peer") {
		t.Fatal("missing single message", bodies)
	}
	e.as("bob", "inbox").ok(t)
	e.as("alice", "agent-dm", "unset", b).ok(t)
	e.as("alice", "agent-dm", "unset", b).ok(t)
	if got := agentDMEntry(t, e, "alice", b); got.Enabled || got.Room == nil || *got.Room != room {
		t.Fatal("unset lost mapping")
	}
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	if tr.count("createRoom") != 1 || tr.count("/invite") != 1 {
		t.Fatal("unset allowed replacement")
	}
	e.as("bob", "agent-dm", "leave", a).ok(t)
	e.as("bob", "agent-dm", "leave", a).ok(t)
	if tr.count("/leave") != 1 {
		t.Fatal("repeated leave")
	}
	e.hs.mu.Lock()
	joined := e.hs.rooms[room].members[a]
	e.hs.mu.Unlock()
	if joined != "join" {
		t.Fatal("leave touched peer")
	}
}

func TestAgentDMPreflightZeroEffects(t *testing.T) {
	for _, bad := range []string{"missing", "permissions", "dir-permissions", "symlink", "duplicate", "wrong-caller", "wrong-connection", "unadmitted", "human-role", "self", "short", "principal", "caller-principal", "whoami"} {
		t.Run(bad, func(t *testing.T) {
			e, tr, a, b := agentDMFixture(t)
			dir := agentDMDir(e.paths(), "alice")
			path := filepath.Join(dir, "admission.json")
			raw := readFile(t, path)
			peer := b
			switch bad {
			case "missing":
				os.Remove(path)
			case "permissions":
				os.Chmod(path, 0644)
			case "dir-permissions":
				os.Chmod(dir, 0755)
			case "symlink":
				os.Rename(path, path+".saved")
				os.Symlink(path+".saved", path)
			case "duplicate":
				os.WriteFile(path, []byte(strings.Replace(raw, `"caller_role":"keeper"`, `"caller_role":"keeper","caller_role":"keeper"`, 1)), 0600)
			case "wrong-caller":
				os.WriteFile(path, []byte(strings.Replace(raw, a, "@other:"+testServer, 1)), 0600)
			case "wrong-connection":
				os.WriteFile(path, []byte(strings.Replace(raw, `"connection_id":"legacy-`, `"connection_id":"wrong-`, 1)), 0600)
			case "unadmitted":
				peer = "@parent:" + testServer
			case "human-role":
				os.WriteFile(path, []byte(strings.Replace(raw, `"role":"orchestrator"`, `"role":"human"`, 1)), 0600)
			case "self":
				peer = a
			case "short":
				peer = "bob"
			case "principal":
				ownerPut(t, ownerFiles(e.paths(), "alice").path("principals.json"), principalConfig{V: 1, Principals: []string{b}})
			case "caller-principal":
				ownerPut(t, ownerFiles(e.paths(), "alice").path("principals.json"), principalConfig{V: 1, Principals: []string{a}})
			case "whoami":
				e.writeCred("alice", a, e.tokens["bob"], 0600)
			}
			r := e.as("alice", "agent-dm", "ensure", peer)
			if r.code == 0 {
				t.Fatalf("admitted %s", bad)
			}
			if tr.count("createRoom") != 0 || tr.count("/invite") != 0 || tr.count("/join/") != 0 || tr.count("/send/") != 0 {
				t.Fatal("preflight mutation")
			}
			if _, err := os.Stat(filepath.Join(dir, "records.json")); !os.IsNotExist(err) {
				t.Fatal("preflight wrote pair state")
			}
		})
	}
}

func TestAgentDMUncertainEffects(t *testing.T) {
	for _, fault := range []string{"create-ack", "invite-ack", "send-ack", "leave-ack"} {
		t.Run(fault, func(t *testing.T) {
			var e *env
			var tr *agentDMTestTransport
			var a, b, room string
			if fault == "create-ack" || fault == "invite-ack" {
				e, tr, a, b = agentDMFixture(t)
			} else {
				e, tr, a, b, room = activeAgentFixture(t)
			}
			tr.setFault(fault)
			if fault == "send-ack" {
				e.as("alice", "agent-dm", "say", b, "--kind", "notice", "one attempt").failsWith(t, ExitError, "uncertain")
			} else if fault == "leave-ack" {
				e.as("alice", "agent-dm", "leave", b).failsWith(t, ExitError, "cleanup_pending")
			} else {
				e.as("alice", "agent-dm", "ensure", b).failsWith(t, ExitError, "attempted")
			}
			tr.setFault("")
			switch fault {
			case "create-ack":
				e.as("alice", "agent-dm", "ensure", b).failsWith(t, ExitError, "audience")
				e.as("alice", "agent-dm", "unset", b).ok(t)
				e.as("alice", "agent-dm", "ensure", b)
				if tr.count("createRoom") != 1 {
					t.Fatal("retried create")
				}
			case "invite-ack":
				e.as("alice", "agent-dm", "ensure", b).ok(t)
				if tr.count("/invite") != 1 {
					t.Fatal("retried invite")
				}
			case "send-ack":
				e.as("alice", "agent-dm", "say", b, "--kind", "notice", "one attempt").failsWith(t, ExitError, "uncertain send")
				if tr.count("/send/") != 1 {
					t.Fatal("retried send")
				}
				if len(agentMessageEvents(e, room)) != 1 {
					t.Fatal("message lost in fake")
				}
			case "leave-ack":
				e.as("alice", "agent-dm", "leave", b).failsWith(t, ExitError, "uncertain ACK")
				if tr.count("/leave") != 1 {
					t.Fatal("retried leave")
				}
			}
			_ = a
		})
	}
}

func setAgentCursor(t *testing.T, e *env, name string) {
	t.Helper()
	if _, err := config.UpdateState(e.paths(), name, func(st *config.State) error { st.NextBatch = "s0"; return nil }); err != nil {
		t.Fatal(err)
	}
}
func appendAgentEvent(e *env, room string, ev matrix.Event) string {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	return e.hs.appendLocked(room, ev.Sender, ev.Type, ev.Content)
}
func assertAgentMailbox(t *testing.T, e *env, name string, want []string) {
	t.Helper()
	mb, err := config.OpenMailbox(e.paths(), name)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, entry := range mb.Pending() {
		ids = append(ids, entry.ID)
	}
	if !reflect.DeepEqual(ids, want) {
		t.Fatalf("recipient mailbox assertion: got %v want %v", ids, want)
	}
}

// This is the regression witness used by the bounded planted bypass control.
// It must fail here on mailbox/wake admission if the pair gate is removed.
func TestAgentDMRecipientMailboxWake(t *testing.T) {
	e, _, a, b, room := activeAgentFixture(t)
	setAgentCursor(t, e, "bob")
	e.as("bob", "subscribe", "--room", room, "--from", a, "--all-events").ok(t)
	e.as("bob", "subscribe", "--room", room, "--from", "@parent:"+testServer, "--all-events").ok(t)
	before := e.as("bob", "subs", "--json").ok(t).stdout
	wrong := agentDirectedEvent(t, "$wrong", a, "@parent:"+testServer)
	var content map[string]any
	_ = json.Unmarshal(wrong.Content, &content)
	content["body"] = "wrong recipient " + b
	content["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": "$followed"}
	wrong.Content, _ = json.Marshal(content)
	if _, err := config.UpdateState(e.paths(), "bob", func(st *config.State) error { st.AddThread("$followed"); return nil }); err != nil {
		t.Fatal(err)
	}
	appendAgentEvent(e, room, wrong)
	third := agentDirectedEvent(t, "$third", "@parent:"+testServer, b)
	appendAgentEvent(e, room, third)
	if out := e.hook("bob", "stop", `{"session_id":"pair-gate"}`).ok(t); out.stdout != "" {
		t.Fatalf("recipient wake assertion: wrong-recipient notice %s", out.stdout)
	}
	assertAgentMailbox(t, e, "bob", []string{})
	if after := e.as("bob", "subs", "--json").ok(t).stdout; after != before {
		t.Fatal("invalid mail changed subscriptions")
	}
	e.as("alice", "agent-dm", "say", b, "--kind", "question", "valid pair payload").ok(t)
	valid := agentMessageEvents(e, room)
	id := valid[len(valid)-1].EventID
	if out := e.hook("bob", "stop", `{"session_id":"pair-gate"}`).ok(t); out.stdout == "" {
		t.Fatal("positive recipient wake assertion: no notice")
	}
	assertAgentMailbox(t, e, "bob", []string{id})
	inbox := e.as("bob", "inbox").ok(t).stdout
	if !strings.Contains(inbox, inboxBegin) || !strings.Contains(inbox, "valid pair payload") || strings.Contains(inbox, "wrong recipient") {
		t.Fatal("pair mail lost untrusted frame or recipient boundary", inbox)
	}
	assertAgentMailbox(t, e, "bob", []string{})
}

func TestAgentDMFreshRuntimeDispatch(t *testing.T) {
	for _, mode := range []string{"valid", "principal", "wrong-recipient", "inactive", "invalid-policy"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, a, b, room := activeAgentFixture(t)
			dir, err := os.MkdirTemp("", "a2a-pair-native-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(dir) })
			path := filepath.Join(dir, "fixture.sock")
			listener, err := net.Listen("unix", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { listener.Close() })
			if err = os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			endpoint := "unix://" + path
			socket, err := codexwake.SocketIdentity(endpoint)
			if err != nil {
				t.Fatal(err)
			}
			cwd, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			n := &nativeFixture{thread: codexwake.Snapshot{ID: "pair-fixture-thread", Cwd: cwd, Status: codexwake.Status{Type: "idle"}}, endpoint: endpoint, socket: socket, cwd: cwd}
			e.configure = func(app *App) {
				app.HTTP = &http.Client{Transport: tr}
				app.nativeDial = func(context.Context, string) (nativeRuntime, error) { return n, nil }
			}
			e.as("bob", "status", "init").ok(t)
			e.as("bob", "runtime", "bind", "--codex-thread", n.thread.ID, "--endpoint", endpoint, "--owner-pid", strconv.Itoa(os.Getpid()), "--heartbeat", "1m", "--max-wakes-per-hour", "2", "--gate", "ready", "--hooks", "none").ok(t)
			e.extra[EnvSessionID] = n.thread.ID
			generation := e.state("bob").Runtime.Generation
			ev := agentDirectedEvent(t, "$retained-pair", a, b)
			if mode == "wrong-recipient" {
				ev = agentDirectedEvent(t, ev.EventID, a, "@other:"+testServer)
			}
			if mode == "principal" {
				ownerPut(t, ownerFiles(e.paths(), "bob").path("principals.json"), principalConfig{V: 1, Principals: []string{"@parent:" + testServer}})
				ev = agentDirectedEvent(t, ev.EventID, "@parent:"+testServer, "@other:"+testServer)
			}
			raw, _ := json.Marshal(ev)
			mb, err := config.OpenMailbox(e.paths(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			if _, err = mb.Append([]config.MailEntry{{ID: ev.EventID, Kind: config.MailEvent, Room: room, Event: raw, Rel: string(relSub)}}); err != nil {
				t.Fatal(err)
			}
			if mode == "inactive" || mode == "principal" {
				e.as("bob", "agent-dm", "unset", a).ok(t)
			}
			if mode == "invalid-policy" {
				e.hs.mu.Lock()
				e.hs.rooms[room].members["@third:"+testServer] = "join"
				e.hs.mu.Unlock()
			}
			e.as("bob", "runtime", "dispatch", "--generation", generation).ok(t)
			if (len(n.wakes) == 1) != (mode == "valid" || mode == "principal") {
				t.Fatalf("fresh runtime pair wake assertion: mode=%s wakes=%d", mode, len(n.wakes))
			}
			mb, err = config.OpenMailbox(e.paths(), "bob")
			if err != nil || !mb.Has(ev.EventID) || mb.Received(ev.EventID) {
				t.Fatal("dispatch lost or receipted retained pair mail", err)
			}
		})
	}
}

func TestAgentDMPendingRetainsMailAndPrincipal(t *testing.T) {
	e, _, a, b := agentDMFixture(t)
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	room := *agentDMEntry(t, e, "alice", b).Room
	e.as("bob", "agent-dm", "ensure", a).failsWith(t, ExitError, "needs_explicit_join")
	e.as("bob", "join", room).ok(t)
	setAgentCursor(t, e, "bob")
	ownerPut(t, ownerFiles(e.paths(), "bob").path("principals.json"), principalConfig{V: 1, Principals: []string{"@parent:" + testServer}})
	valid := agentDirectedEvent(t, "$valid", a, b)
	validID := appendAgentEvent(e, room, valid)
	principal := agentDirectedEvent(t, "$principal", "@parent:"+testServer, "@wrong:"+testServer)
	principalID := appendAgentEvent(e, room, principal)
	// Local pair is pending despite both users now joined. Its joined-room
	// subscription cannot leak peer mail; true principal coverage still works.
	e.hook("bob", "stop", `{"session_id":"pending-pair"}`).ok(t)
	assertAgentMailbox(t, e, "bob", []string{principalID})
	e.as("bob", "inbox").ok(t)
	if got := agentDMEntry(t, e, "bob", a); got.Qualified {
		t.Fatal("delivery activated pair")
	}
	e.as("bob", "agent-dm", "ensure", a).ok(t)
	e.hook("bob", "stop", `{"session_id":"qualified-pair"}`).ok(t)
	assertAgentMailbox(t, e, "bob", []string{validID})
	mb, err := config.OpenMailbox(e.paths(), "bob")
	if err != nil {
		t.Fatal(err)
	}
	if mb.Received(validID) {
		t.Fatal("qualification fabricated receipt")
	}
}

func TestAgentDMHeldCapacityPreservesPrincipalWake(t *testing.T) {
	for _, bound := range []string{"count", "bytes"} {
		t.Run(bound, func(t *testing.T) {
			e, _, a, b := agentDMFixture(t)
			e.as("alice", "agent-dm", "ensure", b).ok(t)
			room := *agentDMEntry(t, e, "alice", b).Room
			e.as("bob", "join", room).ok(t) // locally unqualified
			setAgentCursor(t, e, "bob")
			ownerPut(t, ownerFiles(e.paths(), "bob").path("principals.json"), principalConfig{V: 1, Principals: []string{"@parent:" + testServer}})
			n, body := 512, "small held payload"
			if bound == "bytes" {
				n, body = 17, strings.Repeat("x", 60000)
			}
			makeEvent := func(id string) matrix.Event {
				ev := agentDirectedEvent(t, id, a, b)
				var content map[string]any
				if err := json.Unmarshal(ev.Content, &content); err != nil {
					t.Fatal(err)
				}
				content["body"] = body
				ev.Content, _ = json.Marshal(content)
				return ev
			}
			held := agentDMHeldStore{Contract: agentDMVersion, Entries: []agentDMHeld{}}
			for i := 0; i < n; i++ {
				ev := makeEvent(fmt.Sprintf("$held-%d", i))
				raw, _ := json.Marshal(ev)
				held.Entries = append(held.Entries, agentDMHeld{Room: room, ID: ev.EventID, Event: raw})
			}
			if err := agentWrite(context.Background(), agentDMDir(e.paths(), "bob"), "held.json", held); err != nil {
				t.Fatal(err)
			}
			peerID := appendAgentEvent(e, room, makeEvent("$overflow"))
			principalID := appendAgentEvent(e, room, agentDirectedEvent(t, "$principal", "@parent:"+testServer, "@wrong:"+testServer))
			r := e.hook("bob", "stop", `{"session_id":"capacity-principal"}`)
			if r.code != 0 || r.stdout == "" {
				t.Fatalf("principal capacity wake assertion: code=%d output=%s error=%s", r.code, r.stdout, r.stderr)
			}
			assertAgentMailbox(t, e, "bob", []string{principalID})
			st, err := config.LoadState(e.paths(), "bob")
			if err != nil || st.NextBatch != "s0" {
				t.Fatal("capacity advanced cursor past unretained peer mail", st.NextBatch, err)
			}
			e.as("bob", "agent-dm", "ensure", a).ok(t)
			e.hook("bob", "stop", `{"session_id":"capacity-qualified"}`).ok(t)
			mb, err := config.OpenMailbox(e.paths(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			if !mb.Has(peerID) || !mb.Has("$held-0") || len(mb.Pending()) != n+2 || mb.Received(peerID) || mb.Received("$held-0") {
				t.Fatal("qualification lost or receipted deferred peer mail")
			}
			st, err = config.LoadState(e.paths(), "bob")
			if err != nil || st.NextBatch == "s0" {
				t.Fatal("qualified backlog did not resume cursor", st.NextBatch, err)
			}
		})
	}
}

func TestAgentDMCollectorAllAndOrdinaryControls(t *testing.T) {
	e, _, a, b, room := activeAgentFixture(t)
	var out strings.Builder
	app := &App{as: "bob", Stdout: &out, Stderr: &out, HTTP: e.hs.srv.Client(), Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}}
	s, err := app.session()
	if err != nil {
		t.Fatal(err)
	}
	defer app.closeAdmission()
	// State metadata already exists in the fake; its ordinary HTTP client is
	// sufficient for the read-only collector.
	bad := agentDirectedEvent(t, "$wrong", a, "@parent:"+testServer)
	good := agentDirectedEvent(t, "$valid", a, b)
	ordinary := agentDirectedEvent(t, "$ordinary", a, "@parent:"+testServer)
	resp := matrix.SyncResponse{}
	// Marshal through the existing public sync wire to avoid a second room type.
	raw, _ := json.Marshal(map[string]any{"next_batch": "s100", "rooms": map[string]any{"join": map[string]any{room: map[string]any{"timeline": map[string]any{"events": []matrix.Event{bad, good}}}, "!ordinary": map[string]any{"timeline": map[string]any{"events": []matrix.Event{ordinary}}}}}})
	if err = json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	batch, err := s.collectBatch(context.Background(), resp, "s0", config.State{}, []Sub{{ID: "broad", SubSelector: SubSelector{From: a, AllEvents: true}}}, map[string]bool{room: true, "!ordinary": true}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, entry := range batch.entries {
		ids = append(ids, entry.ID)
	}
	if !reflect.DeepEqual(ids, []string{"$valid", "$ordinary"}) {
		t.Fatalf("recipient mailbox assertion: --all bypass or ordinary suppression: %v", ids)
	}
}

func TestAgentDMExplicitJoinBeforeEnsure(t *testing.T) {
	e, _, a, b := agentDMFixture(t)
	e.as("alice", "agent-dm", "ensure", b).ok(t)
	room := *agentDMEntry(t, e, "alice", b).Room
	// No preceding noncreator ensure, and no stripped-state autojoin.
	e.as("bob", "join", room).ok(t)
	if d := agentDMEntry(t, e, "bob", a); d.Room == nil || *d.Room != room || d.Qualified {
		t.Fatal("explicit join did not retain unverified pair context")
	}
	setAgentCursor(t, e, "bob")
	validID := appendAgentEvent(e, room, agentDirectedEvent(t, "$held", a, b))
	e.hook("bob", "stop", `{"session_id":"explicit-pending"}`).ok(t)
	assertAgentMailbox(t, e, "bob", []string{})
	e.as("bob", "agent-dm", "ensure", a).ok(t)
	e.hook("bob", "stop", `{"session_id":"explicit-active"}`).ok(t)
	assertAgentMailbox(t, e, "bob", []string{validID})
}

func TestAgentDMConcurrentEnsureAndCancellation(t *testing.T) {
	e, tr, _, b := agentDMFixture(t)
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func() { results <- e.as("alice", "agent-dm", "ensure", b) }()
	}
	for i := 0; i < 2; i++ {
		if r := <-results; r.code != 0 {
			t.Fatal(r.stderr)
		}
	}
	if tr.count("createRoom") != 1 || tr.count("/invite") != 1 {
		t.Fatal("concurrent ensures repeated effects")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := tr.count("createRoom") + tr.count("/invite") + tr.count("/send/")
	r := e.runCtx(ctx, "--as", "alice", "agent-dm", "say", b, "--kind", "notice", "cancelled")
	if r.code == 0 {
		t.Fatal("canceled operation succeeded")
	}
	if after := tr.count("createRoom") + tr.count("/invite") + tr.count("/send/"); after != before {
		t.Fatal("canceled preflight mutated transport")
	}
}

func TestAgentDMFreshPolicyRefusalAndUnreadPreservation(t *testing.T) {
	for _, bad := range []string{"join", "invite", "knock", "encryption", "wrong-marker", "wrong-creator", "state"} {
		t.Run(bad, func(t *testing.T) {
			e, tr, a, b, room := activeAgentFixture(t)
			setAgentCursor(t, e, "bob")
			id := appendAgentEvent(e, room, agentDirectedEvent(t, "$unread", a, b))
			e.hook("bob", "stop", `{"session_id":"valid-unread"}`).ok(t)
			assertAgentMailbox(t, e, "bob", []string{id})
			e.hs.mu.Lock()
			r := e.hs.rooms[room]
			switch bad {
			case "join", "invite", "knock":
				r.members["@parent:"+testServer] = bad
			case "encryption":
				r.state["m.room.encryption|"] = json.RawMessage(`{"algorithm":"m.megolm.v1.aes-sha2"}`)
			case "wrong-marker":
				r.state[agentDMMarker+"|"] = json.RawMessage(`{"contract":"wrong"}`)
			case "wrong-creator":
				r.stateSenders["m.room.create|"] = b
			}
			e.hs.mu.Unlock()
			if bad == "state" {
				tr.setFault("state")
			}
			before := tr.count("/send/")
			if out := e.as("alice", "agent-dm", "say", b, "--kind", "notice", "refuse"); out.code == 0 {
				t.Fatal("fresh audience refusal missed")
			}
			if tr.count("/send/") != before {
				t.Fatal("policy refusal sent")
			}
			// Invalid/unknown audience hides existing unread mail but does not
			// fabricate a receipt or delete durable history.
			if out := e.as("bob", "inbox").ok(t); strings.Contains(out.stdout, "peer payload") {
				t.Fatal("invalid pair leaked retained mail")
			}
			mb, err := config.OpenMailbox(e.paths(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			if !mb.Has(id) || mb.Received(id) {
				t.Fatal("invalid pair lost unread mail")
			}
		})
	}
}

func TestAgentDMAdmissionRereadAndLockTimeout(t *testing.T) {
	e, tr, _, b := agentDMFixture(t)
	unlock, err := agentLock(context.Background(), agentDMDir(e.paths(), "alice"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	r := e.runCtx(ctx, "--as", "alice", "agent-dm", "ensure", b)
	cancel()
	unlock()
	if r.code == 0 || tr.count("createRoom") != 0 {
		t.Fatal("lock timeout caused effect")
	}
	// Change the host file after initial admission but during read-only whoami.
	path := filepath.Join(agentDMDir(e.paths(), "alice"), "admission.json")
	raw := readFile(t, path)
	e.configure = func(a *App) {
		a.HTTP = &http.Client{Transport: waitRoundTrip(func(req *http.Request) (*http.Response, error) {
			resp, err := tr.RoundTrip(req)
			if strings.HasSuffix(req.URL.Path, "/account/whoami") {
				if err := os.WriteFile(path, []byte(strings.Replace(raw, `"caller_role":"keeper"`, `"caller_role":"human"`, 1)), 0600); err != nil {
					t.Error(err)
				}
			}
			return resp, err
		})}
	}
	if r := e.as("alice", "agent-dm", "ensure", b); r.code == 0 {
		t.Fatal("post-lock admission was not re-read")
	}
	if tr.count("createRoom") != 0 || tr.count("/invite") != 0 {
		t.Fatal("changed admission caused effect")
	}
}

func TestAgentDMNamedConnectionBinding(t *testing.T) {
	f := newNamedFileFixture(t)
	f.root = principalDMTestResolve(t, f.root)
	peer := "@aaa:other.invalid" // caller is the noncreator
	dir := agentDMDir(f.paths("alpha", false), "alice")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	stage := func(connection string) {
		raw, _ := json.Marshal(map[string]any{"contract": agentDMVersion, "caller_mxid": "@alice:alpha.invalid", "connection_id": connection, "caller_role": "keeper", "caller_admission_ref": "fixture-caller", "peers": []any{map[string]string{"mxid": peer, "role": "orchestrator", "admission_ref": "fixture-peer"}}})
		if err := os.WriteFile(filepath.Join(dir, "admission.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	reads := 0
	run := func(args ...string) int {
		f.out.Reset()
		f.err.Reset()
		a := f.app()
		forbidFileKeychain(a)
		a.HTTP = &http.Client{Transport: waitRoundTrip(func(r *http.Request) (*http.Response, error) {
			reads++
			if r.Method != http.MethodGet {
				t.Error("named binding performed remote mutation")
			}
			status, body := http.StatusOK, `{"user_id":"@alice:alpha.invalid"}`
			if strings.Contains(r.URL.Path, "/directory/room/") {
				status, body = http.StatusNotFound, `{"errcode":"M_NOT_FOUND"}`
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), ContentLength: int64(len(body)), Request: r}, nil
		})}
		return a.Run(context.Background(), append([]string{"--profile", "alpha", "--as", "alice", "agent-dm"}, args...))
	}
	stage("alpha-id")
	if run("ensure", peer) != 0 || !strings.Contains(f.out.String(), "needs_creator") {
		t.Fatal("selected profile failed", f.err.String(), f.out.String())
	}
	if run("status", peer, "--json") != 0 || !strings.Contains(f.out.String(), `"connection_id":"alpha-id"`) || !strings.Contains(f.out.String(), `"declared_admission":{`) {
		t.Fatal("selected profile lost admission provenance", f.err.String(), f.out.String())
	}
	before := reads
	stage("beta-id")
	if run("ensure", peer) == 0 || reads != before {
		t.Fatal("other connection admission reached HTTP")
	}
	if dir == agentDMDir(f.paths("beta", false), "alice") {
		t.Fatal("selected profiles share pair namespace")
	}
}

func TestAgentDMAliasConflictAndReadbackFence(t *testing.T) {
	t.Run("retained-mapping", func(t *testing.T) {
		e, tr, a, b, room := activeAgentFixture(t)
		route, _ := agentPair(a, b)
		e.hs.mu.Lock()
		delete(e.hs.aliases, route.Alias)
		e.hs.mu.Unlock()
		e.as("alice", "agent-dm", "ensure", b).ok(t)
		e.as("alice", "agent-dm", "say", b, "--kind", "question", "same bound room").ok(t)
		if len(agentMessageEvents(e, room)) != 1 || tr.count("createRoom") != 1 || tr.count("/invite") != 1 {
			t.Fatal("missing alias replaced or disabled valid known room")
		}
		e.hs.mu.Lock()
		e.hs.aliases[route.Alias] = "!conflict"
		e.hs.mu.Unlock()
		e.as("alice", "agent-dm", "ensure", b).failsWith(t, ExitError, "conflicting alias")
		e.as("alice", "agent-dm", "say", b, "--kind", "question", "must not send").failsWith(t, ExitError, "conflicting alias")
		if len(agentMessageEvents(e, room)) != 1 || tr.count("createRoom") != 1 {
			t.Fatal("alias conflict caused effect")
		}
	})
	t.Run("wrong-creator", func(t *testing.T) {
		e, tr, a, b := agentDMFixture(t)
		route, _ := agentPair(a, b)
		local, _, _ := strings.Cut(strings.TrimPrefix(route.Alias, "#"), ":")
		room := e.hs.addRoom(local, "@parent:"+testServer)
		e.hs.mu.Lock()
		e.hs.rooms[room].members[a] = "join"
		e.hs.rooms[room].stateSenders = map[string]string{"m.room.create|": "@parent:" + testServer}
		e.hs.mu.Unlock()
		e.as("alice", "agent-dm", "ensure", b).failsWith(t, ExitError, "wrong_creator")
		status := e.as("alice", "agent-dm", "status", b, "--json").ok(t).stdout
		if !strings.Contains(status, "wrong_creator") || !strings.Contains(status, `"state":"invalid"`) {
			t.Fatal("collision provenance missing", status)
		}
		if tr.count("createRoom") != 0 || tr.count("/invite") != 0 {
			t.Fatal("collision repaired/replaced")
		}
	})
	t.Run("readback", func(t *testing.T) {
		e, tr, _, b := agentDMFixture(t)
		tr.setFault("state")
		e.as("alice", "agent-dm", "ensure", b).failsWith(t, ExitError, "state read failure")
		d := agentDMEntry(t, e, "alice", b)
		if d.Room == nil || d.CreateAttempt != "confirmed" {
			t.Fatal("lost create recovery evidence")
		}
		e.as("alice", "agent-dm", "unset", b).ok(t)
		e.as("alice", "agent-dm", "ensure", b)
		if tr.count("createRoom") != 1 || tr.count("/invite") != 0 {
			t.Fatal("readback failure retried effects")
		}
		tr.setFault("")
		e.as("alice", "agent-dm", "ensure", b).ok(t)
		if tr.count("createRoom") != 1 || tr.count("/invite") != 1 {
			t.Fatal("explicit recovery duplicated effects")
		}
	})
}

func agentDirectedEvent(t *testing.T, id, sender, recipient string) matrix.Event {
	t.Helper()
	h := Header{V: 1, Type: "message", From: sender, To: []string{recipient}, At: time.Now().UTC().Format(time.RFC3339), Kind: "question"}
	raw, err := json.Marshal(map[string]any{"msgtype": "m.text", "body": "peer payload", ContentKey: h, "m.mentions": matrix.Mentions{UserIDs: []string{recipient}}})
	if err != nil {
		t.Fatal(err)
	}
	return matrix.Event{EventID: id, Sender: sender, Type: "m.room.message", Content: raw}
}

func TestAgentDMRecipientGate(t *testing.T) {
	me, peer := "@alice:one.invalid", "@bob:two.invalid"
	gate := agentDMDelivery{Rooms: map[string]agentDMGate{"!pair": {Peer: peer, Active: true}}}
	for _, bad := range []string{"valid", "valid-optional", "wrong-to", "missing-to", "multi-to", "duplicate-to", "short-to", "sender", "from", "projection", "missing-header", "mention-only", "duplicate-key", "case-key", "room-mention", "bad-optional", "null-optional"} {
		t.Run(bad, func(t *testing.T) {
			ev := agentDirectedEvent(t, "$event", peer, me)
			raw := string(ev.Content)
			switch bad {
			case "valid-optional":
				raw = strings.Replace(raw, `"type":"message"`, `"type":"message","title":"context","subject":"example","topic":"$root","reply_by":"tomorrow"`, 1)
			case "bad-optional":
				raw = strings.Replace(raw, `"type":"message"`, `"type":"message","subject":7`, 1)
			case "null-optional":
				raw = strings.Replace(raw, `"type":"message"`, `"type":"message","subject":null`, 1)
			case "wrong-to":
				raw = strings.Replace(raw, `"to":["`+me+`"]`, `"to":["@other:one.invalid"]`, 1)
			case "missing-to":
				raw = strings.Replace(raw, `"to":["`+me+`"]`, `"to":[]`, 1)
			case "multi-to", "duplicate-to":
				raw = strings.Replace(raw, `"to":["`+me+`"]`, `"to":["`+me+`","`+me+`"]`, 1)
			case "short-to":
				raw = strings.Replace(raw, `"to":["`+me+`"]`, `"to":["alice"]`, 1)
			case "sender":
				ev.Sender = "@parent:one.invalid"
			case "from":
				raw = strings.Replace(raw, `"from":"`+peer+`"`, `"from":"bob"`, 1)
			case "projection":
				raw = strings.Replace(raw, `"type":"message"`, `"type":"projection"`, 1)
			case "missing-header":
				raw = strings.Replace(raw, ContentKey, "irrelevant", 1)
			case "mention-only":
				raw = `{"msgtype":"m.text","body":"` + me + `","m.mentions":{"user_ids":["` + me + `"]}}`
			case "duplicate-key":
				raw = strings.Replace(raw, `"from":`, `"from":"@other:one.invalid","from":`, 1)
			case "case-key":
				raw = strings.Replace(raw, `"to":`, `"To":`, 1)
			case "room-mention":
				raw = strings.Replace(raw, `"user_ids":`, `"room":true,"user_ids":`, 1)
			}
			ev.Content = json.RawMessage(raw)
			if got := gate.decision(ev, "!pair", me, nil); (got == "allow") != strings.HasPrefix(bad, "valid") {
				t.Fatalf("recipient gate admitted=%s decision=%s", bad, got)
			}
			// Same event in an ordinary room is never made pair context by a header.
			if got := gate.decision(ev, "!ordinary", me, nil); got != "allow" {
				t.Fatal("forged header suppressed ordinary room")
			}
		})
	}
	principal := agentDirectedEvent(t, "$principal", "@owner:one.invalid", "@wrong:one.invalid")
	gate.Rooms["!pair"] = agentDMGate{Peer: peer}
	if gate.decision(principal, "!pair", me, []string{principal.Sender}) != "allow" {
		t.Fatal("principal delivery suppressed")
	}
}
