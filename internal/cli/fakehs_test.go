package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// fakeHS is an in-memory Matrix homeserver that implements the subset of the
// client-server API the a2a CLI uses. It records every request (method and
// route) through tuitestkit.MockCallRecorder so tests can assert on traffic.
//
// It deliberately echoes rejected access tokens in its error messages, as a
// careless server might, so tests can prove the CLI redacts them.
type fakeHS struct {
	tuitestkit.MockCallRecorder

	t   *testing.T
	srv *httptest.Server

	mu                sync.Mutex
	server            string
	regToken          string
	tokens            map[string]string // token -> user id
	users             map[string]bool
	passwords         map[string]string // user id -> password (for login and deactivation UIA)
	logins            []string          // device ids requested by successful logins
	aliases           map[string]string
	rooms             map[string]*fakeRoom
	events            []matrix.Event // stream position = index + 1
	invites           []fakeInvite
	presence          map[string]matrix.Presence
	forbidden         map[string]bool // state event types that are forbidden
	threadsAPI        bool
	failures          map[string]fakeFailure // route -> forced failure
	rawURLs           []string
	wake              chan struct{}
	seq               int
	redactionRequests []fakeRedactionRequest
	txns              map[string]string // user|room|txn -> event id (send idempotency)
	syncLimit         int               // per-room timeline limit in /sync (0 = 100, like the real filter)
	// historyFloor, when > 0, is the index (in a room's messages) below which
	// /messages cannot page (a purged history): it keeps returning an empty
	// chunk with the same end token.
	historyFloor int
	// messagePages supplies filtered pagination fixtures keyed by room and from token.
	messagePages map[string]map[string]fakeMessagePage
	// accountData is global account data: user id -> type -> content.
	accountData map[string]map[string]json.RawMessage
	// liveClock stamps events with the wall clock (strictly increasing ms)
	// instead of the fixed 2026-09-29T10:00:00Z + seq seconds, so that
	// subscription creation times (taken from App.Now) compare with them.
	liveClock bool
	lastTS    int64
	// media repository
	media     map[string]fakeMedia // media id -> stored upload
	serveAs   map[string][]byte    // media id -> bytes served instead (a lying server)
	uploadMax int64                // m.upload.size
	// uploadRewrite optionally models a homeserver mutating media bytes.
	uploadRewrite func([]byte) []byte
	// created records every accepted createRoom request body, in order.
	created []createRoomReq
	// syncsBy counts /sync requests per user, on arrival, so a test can wait
	// for one agent's next sync while other agents (a projector) sync too.
	syncsBy map[string]int
}

// SyncCount is the number of /sync requests user has made so far.
func (hs *fakeHS) SyncCount(user string) int {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.syncsBy[user]
}

// createRoomReq is the part of a createRoom body the fake checks and records.
type createRoomReq struct {
	IsDirect                  bool           `json:"is_direct"`
	InitialState              []matrix.Event `json:"initial_state"`
	Creator                   string         `json:"-"`
	Preset                    string         `json:"preset"`
	Visibility                string         `json:"visibility"`
	CreationContent           map[string]any `json:"creation_content"`
	RoomAliasName             string         `json:"room_alias_name"`
	Name                      string         `json:"name"`
	Topic                     string         `json:"topic"`
	Invite                    []string       `json:"invite"`
	PowerLevelContentOverride map[string]any `json:"power_level_content_override"`
}

type fakeMedia struct {
	data        []byte
	contentType string
	filename    string
}

type fakeRoom struct {
	id, alias, name string
	members         map[string]string // user id -> membership
	state           map[string]json.RawMessage
	stateSenders    map[string]string
}

type fakeInvite struct {
	pos            int
	room, user, by string
}

type fakeMessagePage struct {
	page    matrix.Page
	failure *fakeFailure
}

type fakeFailure struct {
	status  int
	errcode string
	message string
}

const testServer = "test.local"

func newFakeHS(t *testing.T) *fakeHS {
	t.Helper()
	hs := &fakeHS{
		t:          t,
		server:     testServer,
		regToken:   "reg-token-SECRET-1234",
		tokens:     map[string]string{},
		users:      map[string]bool{},
		passwords:  map[string]string{},
		aliases:    map[string]string{},
		rooms:      map[string]*fakeRoom{},
		presence:   map[string]matrix.Presence{},
		forbidden:  map[string]bool{},
		threadsAPI: true,
		failures:   map[string]fakeFailure{},
		wake:       make(chan struct{}),
		media:      map[string]fakeMedia{},
		serveAs:    map[string][]byte{},
		uploadMax:  24 << 20,
	}
	mux := http.NewServeMux()
	route := func(pattern string, auth bool, h func(w http.ResponseWriter, r *http.Request, user string)) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			hs.Record(pattern)
			hs.mu.Lock()
			hs.rawURLs = append(hs.rawURLs, r.URL.String())
			f, fail := hs.failures[pattern]
			hs.mu.Unlock()
			user := ""
			if auth {
				tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
				hs.mu.Lock()
				user = hs.tokens[tok]
				hs.mu.Unlock()
				if user == "" {
					writeErr(w, 401, "M_UNKNOWN_TOKEN", "Unknown access token "+tok)
					return
				}
			}
			if fail {
				writeErr(w, f.status, f.errcode, f.message)
				return
			}
			h(w, r, user)
		})
	}
	const v3, v1 = "/_matrix/client/v3", "/_matrix/client/v1"
	route("POST "+v3+"/register", false, hs.register)
	route("POST "+v3+"/login", false, hs.login)
	route("POST "+v3+"/account/deactivate", true, hs.deactivate)
	route("GET "+v3+"/account/whoami", true, func(w http.ResponseWriter, r *http.Request, u string) {
		writeJSON(w, 200, map[string]string{"user_id": u, "device_id": "DEV" + localpart(u)})
	})
	route("GET "+v3+"/joined_rooms", true, hs.joinedRooms)
	route("GET "+v3+"/directory/room/{alias}", true, hs.directory)
	route("DELETE "+v3+"/directory/room/{alias}", true, hs.deleteAlias)
	route("POST "+v3+"/createRoom", true, hs.createRoom)
	route("POST "+v3+"/join/{room}", true, hs.join)
	route("POST "+v3+"/rooms/{room}/leave", true, hs.leave)
	route("POST "+v3+"/rooms/{room}/invite", true, hs.invite)
	route("PUT "+v3+"/rooms/{room}/send/{type}/{txn}", true, hs.send)
	route("PUT "+v3+"/rooms/{room}/redact/{event}/{txn}", true, hs.redactHTTP)
	route("GET "+v3+"/rooms/{room}/state/{type}/{key...}", true, hs.getState)
	route("GET "+v3+"/rooms/{room}/state/{type}", true, hs.getState) // state key "", as on a real server
	route("GET "+v3+"/rooms/{room}/state", true, hs.getStateEvents)
	route("PUT "+v3+"/rooms/{room}/state/{type}/{key...}", true, hs.putState)
	route("GET "+v3+"/rooms/{room}/messages", true, hs.messages)
	route("GET "+v3+"/rooms/{room}/event/{event}", true, hs.event)
	route("GET "+v1+"/rooms/{room}/threads", true, hs.threads)
	route("GET "+v1+"/rooms/{room}/relations/{event}/m.thread", true, hs.relations)
	route("GET "+v3+"/sync", true, hs.sync)
	route("GET "+v3+"/user/{user}/account_data/{type}", true, hs.getAccountData)
	route("PUT "+v3+"/user/{user}/account_data/{type}", true, hs.putAccountData)
	route("PUT "+v3+"/presence/{user}/status", true, hs.putPresence)
	route("GET "+v3+"/presence/{user}/status", true, hs.getPresence)
	route("POST /_matrix/media/v3/upload", true, hs.upload)
	route("GET "+v1+"/media/config", true, func(w http.ResponseWriter, r *http.Request, _ string) {
		hs.mu.Lock()
		defer hs.mu.Unlock()
		writeJSON(w, 200, map[string]int64{"m.upload.size": hs.uploadMax})
	})
	route("GET "+v1+"/media/download/{server}/{media}", true, hs.download)
	hs.srv = httptest.NewServer(mux)
	t.Cleanup(hs.srv.Close)
	return hs
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"errcode": code, "error": msg})
}

// --- setup helpers (called by tests, not over HTTP) --------------------------

// addUser creates a user and returns its access token.
func (hs *fakeHS) addUser(name string) (string, string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	uid := "@" + name + ":" + hs.server
	tok := "syt_" + name + "_tok_" + strconv.Itoa(len(hs.tokens)+1) + "xyz"
	hs.users[uid] = true
	hs.tokens[tok] = uid
	hs.passwords[uid] = "pw-" + name + "-secret" // what env.writeCred stores
	return uid, tok
}

// revoke invalidates every access token of userID (as a logout or admin reset does).
func (hs *fakeHS) revoke(userID string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for tok, u := range hs.tokens {
		if u == userID {
			delete(hs.tokens, tok)
		}
	}
}

func (hs *fakeHS) loginDevices() []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]string(nil), hs.logins...)
}

// addRoom creates a room with an alias and joined members.
func (hs *fakeHS) addRoom(alias string, members ...string) string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	id := "!" + alias + "RoomID"
	full := "#" + alias + ":" + hs.server
	r := &fakeRoom{id: id, alias: full, name: alias, members: map[string]string{}, state: map[string]json.RawMessage{}}
	creator := ""
	for _, m := range members {
		r.members[m] = "join"
		if creator == "" {
			creator = m
		}
	}
	r.state["m.room.create|"], _ = json.Marshal(map[string]string{"creator": creator})
	r.state["m.room.canonical_alias|"], _ = json.Marshal(map[string]string{"alias": full})
	r.state["m.room.name|"], _ = json.Marshal(map[string]string{"name": alias + " room"})
	hs.rooms[id] = r
	hs.aliases[full] = id
	return id
}

// makePublic sets join_rule public, so anyone may join without an invite.
func (hs *fakeHS) makePublic(roomID string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.rooms[roomID].state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"public"}`)
}

func (r *fakeRoom) public() bool {
	var jr struct {
		JoinRule string `json:"join_rule"`
	}
	_ = json.Unmarshal(r.state["m.room.join_rules|"], &jr)
	return jr.JoinRule == "public"
}

// createdRooms returns the accepted createRoom requests.
func (hs *fakeHS) createdRooms() []createRoomReq {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]createRoomReq(nil), hs.created...)
}

// inject appends a message event as if user had sent it.
func (hs *fakeHS) inject(roomID, sender string, content map[string]any) string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.appendLocked(roomID, sender, "m.room.message", content)
}

func (hs *fakeHS) appendLocked(roomID, sender, typ string, content any) string {
	hs.seq++
	id := fmt.Sprintf("$ev%03d", hs.seq)
	raw, _ := json.Marshal(content)
	ts := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC).UnixMilli() + int64(hs.seq)*1000
	if hs.liveClock {
		ts = time.Now().UnixMilli()
		if ts <= hs.lastTS {
			ts = hs.lastTS + 1
		}
		hs.lastTS = ts
	}
	hs.events = append(hs.events, matrix.Event{
		EventID: id, Type: typ, Sender: sender, RoomID: roomID,
		OriginServerTS: ts,
		Content:        raw,
	})
	close(hs.wake)
	hs.wake = make(chan struct{})
	return id
}

// redact strips an event's content and sets unsigned.redacted_because, as a
// server-side redaction does.
func (hs *fakeHS) redact(id, by string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for i, ev := range hs.events {
		if ev.EventID == id {
			hs.events[i].Content = json.RawMessage(`{}`)
			hs.events[i].Unsigned, _ = json.Marshal(map[string]any{"redacted_because": map[string]any{
				"type": "m.room.redaction", "sender": by, "redacts": id}})
		}
	}
}

func (hs *fakeHS) fail(route string, status int, code, msg string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.failures[route] = fakeFailure{status, code, msg}
}

func (hs *fakeHS) eventByID(id string) (matrix.Event, bool) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	for _, ev := range hs.events {
		if ev.EventID == id {
			return ev, true
		}
	}
	return matrix.Event{}, false
}

func (hs *fakeHS) lastEvent() matrix.Event {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.events[len(hs.events)-1]
}

func (hs *fakeHS) urls() []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return append([]string(nil), hs.rawURLs...)
}

// pinned returns the room's m.room.pinned_events list (nil if unset).
func (hs *fakeHS) pinned(roomID string) []string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	raw, ok := hs.rooms[roomID].state[matrixPinned+"|"]
	if !ok {
		return nil
	}
	var c struct {
		Pinned []string `json:"pinned"`
	}
	_ = json.Unmarshal(raw, &c)
	return c.Pinned
}

const matrixPinned = "m.room.pinned_events"

// --- handlers ------------------------------------------------------------------

func (hs *fakeHS) register(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		Username string         `json:"username"`
		Password string         `json:"password"`
		Auth     map[string]any `json:"auth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uia := map[string]any{"session": "sess-1", "flows": []any{map[string]any{"stages": []string{"m.login.registration_token"}}}, "params": map[string]any{}}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	uid := "@" + req.Username + ":" + hs.server
	if hs.users[uid] {
		writeErr(w, 400, "M_USER_IN_USE", "User ID already taken.")
		return
	}
	if req.Auth == nil {
		writeJSON(w, 401, uia)
		return
	}
	if req.Auth["type"] != "m.login.registration_token" || req.Auth["token"] != hs.regToken || req.Auth["session"] != "sess-1" {
		uia["errcode"], uia["error"] = "M_FORBIDDEN", "Invalid registration token"
		uia["completed"] = []string{}
		writeJSON(w, 401, uia)
		return
	}
	tok := "syt_" + req.Username + "_newtoken_abcdef"
	hs.users[uid] = true
	hs.passwords[uid] = req.Password
	hs.tokens[tok] = uid
	writeJSON(w, 200, map[string]string{"user_id": uid, "access_token": tok, "device_id": "NEWDEV"})
}

type fakeIdentifier struct {
	Type string `json:"type"`
	User string `json:"user"`
}

// passwordOK checks an m.id.user identifier and password (callers hold hs.mu).
func (hs *fakeHS) passwordOK(id fakeIdentifier, password string) (string, bool) {
	uid := id.User
	if !strings.HasPrefix(uid, "@") {
		uid = "@" + uid + ":" + hs.server
	}
	return uid, id.Type == "m.id.user" && hs.users[uid] && hs.passwords[uid] != "" && hs.passwords[uid] == password
}

func (hs *fakeHS) login(w http.ResponseWriter, r *http.Request, _ string) {
	var req struct {
		Type       string         `json:"type"`
		Identifier fakeIdentifier `json:"identifier"`
		Password   string         `json:"password"`
		DeviceID   string         `json:"device_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hs.mu.Lock()
	defer hs.mu.Unlock()
	uid, ok := hs.passwordOK(req.Identifier, req.Password)
	if req.Type != "m.login.password" || !ok {
		writeErr(w, 403, "M_FORBIDDEN", "Invalid username or password")
		return
	}
	device := req.DeviceID
	if device == "" {
		device = "LOGINDEV"
	}
	hs.logins = append(hs.logins, device)
	tok := "syt_" + localpart(uid) + "_relogin_" + strconv.Itoa(len(hs.logins)) + "xyz"
	hs.tokens[tok] = uid
	writeJSON(w, 200, map[string]string{"user_id": uid, "access_token": tok, "device_id": device})
}

// deactivate implements UIA with a single m.login.password stage.
func (hs *fakeHS) deactivate(w http.ResponseWriter, r *http.Request, u string) {
	var req struct {
		Auth *struct {
			Type       string         `json:"type"`
			Identifier fakeIdentifier `json:"identifier"`
			Password   string         `json:"password"`
			Session    string         `json:"session"`
		} `json:"auth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	uia := map[string]any{"session": "deact-1", "flows": []any{map[string]any{"stages": []string{"m.login.password"}}}, "params": map[string]any{}}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if req.Auth == nil {
		writeJSON(w, 401, uia)
		return
	}
	uid, ok := hs.passwordOK(req.Auth.Identifier, req.Auth.Password)
	if req.Auth.Type != "m.login.password" || req.Auth.Session != "deact-1" || !ok || uid != u {
		uia["errcode"], uia["error"] = "M_FORBIDDEN", "Invalid password"
		uia["completed"] = []string{}
		writeJSON(w, 401, uia)
		return
	}
	for tok, owner := range hs.tokens {
		if owner == u {
			delete(hs.tokens, tok)
		}
	}
	delete(hs.users, u)
	writeJSON(w, 200, map[string]string{"id_server_unbind_result": "no-support"})
}

func (hs *fakeHS) joinedRooms(w http.ResponseWriter, _ *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	ids := []string{}
	for id, r := range hs.rooms {
		if r.members[u] == "join" {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	writeJSON(w, 200, map[string]any{"joined_rooms": ids})
}

func (hs *fakeHS) directory(w http.ResponseWriter, r *http.Request, _ string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	id, ok := hs.aliases[r.PathValue("alias")]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Room alias not found.")
		return
	}
	writeJSON(w, 200, map[string]any{"room_id": id, "servers": []string{hs.server}})
}

// deleteAlias removes an alias; like a real server it needs the room's
// creator or a member with state power.
func (hs *fakeHS) deleteAlias(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	alias := r.PathValue("alias")
	id, ok := hs.aliases[alias]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Room alias not found.")
		return
	}
	if room := hs.rooms[id]; room == nil || !mayPost(room, u, true) {
		writeErr(w, 403, "M_FORBIDDEN", "You may not delete this alias.")
		return
	}
	delete(hs.aliases, alias)
	writeJSON(w, 200, map[string]any{})
}

// roomFor resolves the {room} path value and checks membership when needed.
func (hs *fakeHS) roomFor(w http.ResponseWriter, r *http.Request, u string, needJoin bool) *fakeRoom {
	room, ok := hs.rooms[r.PathValue("room")]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Room not found.")
		return nil
	}
	if needJoin && room.members[u] != "join" {
		writeErr(w, 403, "M_FORBIDDEN", "You are not joined to this room.")
		return nil
	}
	return room
}

func (hs *fakeHS) join(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	id := r.PathValue("room")
	if a, ok := hs.aliases[id]; ok {
		id = a
	}
	room, ok := hs.rooms[id]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Room not found.")
		return
	}
	if room.members[u] != "invite" && room.members[u] != "join" && !room.public() {
		writeErr(w, 403, "M_FORBIDDEN", "You are not invited to this room.")
		return
	}
	room.members[u] = "join"
	writeJSON(w, 200, map[string]string{"room_id": room.id})
}

// createRoom creates a room with the creator joined. Like tuwunel (room
// version 12), it refuses a creator listed in power_levels.users with
// M_INVALID_PARAM, and a taken alias with M_ROOM_IN_USE.
func (hs *fakeHS) createRoom(w http.ResponseWriter, r *http.Request, u string) {
	var req createRoomReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "M_NOT_JSON", "bad json")
		return
	}
	req.Creator = u
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if users, ok := req.PowerLevelContentOverride["users"].(map[string]any); ok {
		if _, listed := users[u]; listed {
			writeErr(w, 400, "M_INVALID_PARAM", "Room creators cannot be given power levels in room version 12.")
			return
		}
	}
	full := "#" + req.RoomAliasName + ":" + hs.server
	if _, taken := hs.aliases[full]; taken && req.RoomAliasName != "" {
		writeErr(w, 400, "M_ROOM_IN_USE", "Room alias already taken")
		return
	}
	for _, inv := range req.Invite {
		if !hs.users[inv] {
			writeErr(w, 404, "M_NOT_FOUND", "Unknown user.")
			return
		}
	}
	id := "!" + req.RoomAliasName + "RoomID"
	if req.RoomAliasName == "" {
		id = fmt.Sprintf("!dm%dRoomID", len(hs.created))
		full = ""
	}
	room := &fakeRoom{id: id, alias: full, name: req.Name, members: map[string]string{u: "join"}, state: map[string]json.RawMessage{}}
	room.state["m.room.canonical_alias|"], _ = json.Marshal(map[string]string{"alias": full})
	create := map[string]any{"creator": u}
	for k, v := range req.CreationContent {
		create[k] = v
	}
	room.state["m.room.create|"], _ = json.Marshal(create)
	if req.Name != "" {
		room.state["m.room.name|"], _ = json.Marshal(map[string]string{"name": req.Name})
	}
	if req.Topic != "" {
		room.state["m.room.topic|"], _ = json.Marshal(map[string]string{"topic": req.Topic})
	}
	if req.Preset == "public_chat" {
		room.state["m.room.join_rules|"] = json.RawMessage(`{"join_rule":"public"}`)
	}
	// Like a real server, power_levels start from the spec defaults (state
	// events need 50, messages 0) and the override is merged on top.
	pl := map[string]any{"state_default": 50, "events_default": 0, "invite": 0}
	for k, v := range req.PowerLevelContentOverride {
		pl[k] = v
	}
	room.state["m.room.power_levels|"], _ = json.Marshal(pl)
	hs.rooms[id] = room
	if full != "" {
		hs.aliases[full] = id
	}
	for _, ev := range req.InitialState {
		if ev.StateKey != nil {
			room.state[ev.Type+"|"+*ev.StateKey] = append(json.RawMessage(nil), ev.Content...)
		}
	}
	for _, inv := range req.Invite {
		room.members[inv] = "invite"
		hs.invites = append(hs.invites, fakeInvite{pos: len(hs.events) + 1, room: id, user: inv, by: u})
		hs.appendLocked(id, u, "m.room.member", map[string]string{"membership": "invite"})
	}
	hs.created = append(hs.created, req)
	writeJSON(w, 200, map[string]string{"room_id": id})
}

func (hs *fakeHS) leave(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if room := hs.roomFor(w, r, u, false); room != nil {
		delete(room.members, u)
		writeJSON(w, 200, map[string]any{})
	}
}

func (hs *fakeHS) invite(w http.ResponseWriter, r *http.Request, u string) {
	var req struct {
		UserID string `json:"user_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	if !hs.users[req.UserID] {
		writeErr(w, 404, "M_NOT_FOUND", "Unknown user.")
		return
	}
	room.members[req.UserID] = "invite"
	hs.invites = append(hs.invites, fakeInvite{pos: len(hs.events) + 1, room: room.id, user: req.UserID, by: u})
	// Invites advance the stream like any event.
	hs.appendLocked(room.id, u, "m.room.member", map[string]string{"membership": "invite"})
	writeJSON(w, 200, map[string]any{})
}

// mayPost applies the room's power levels like a homeserver: a message needs
// events_default, a state event state_default, and the creator counts as 100.
func mayPost(room *fakeRoom, u string, state bool) bool {
	var pl struct {
		EventsDefault int            `json:"events_default"`
		StateDefault  int            `json:"state_default"`
		Users         map[string]int `json:"users"`
	}
	_ = json.Unmarshal(room.state["m.room.power_levels|"], &pl)
	need := pl.EventsDefault
	if state {
		need = pl.StateDefault
	}
	have := pl.Users[u]
	if u == roomCreator(room) {
		have = 100
	}
	return have >= need
}

func (hs *fakeHS) send(w http.ResponseWriter, r *http.Request, u string) {
	var content map[string]any
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		writeErr(w, 400, "M_NOT_JSON", "bad json")
		return
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	if !mayPost(room, u, false) {
		writeErr(w, 403, "M_FORBIDDEN", "You don't have permission to post that to the room.")
		return
	}
	key := u + "|" + room.id + "|" + r.PathValue("txn")
	if id, ok := hs.txns[key]; ok {
		writeJSON(w, 200, map[string]string{"event_id": id})
		return
	}
	id := hs.appendLocked(room.id, u, r.PathValue("type"), content)
	if hs.txns == nil {
		hs.txns = map[string]string{}
	}
	hs.txns[key] = id
	writeJSON(w, 200, map[string]string{"event_id": id})
}

func (hs *fakeHS) getState(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	if r.PathValue("type") == "m.room.member" {
		// Membership is served from the room's member map, like a real server's m.room.member state.
		if m, ok := room.members[r.PathValue("key")]; ok {
			writeJSON(w, 200, map[string]string{"membership": m})
			return
		}
	}
	c, ok := room.state[r.PathValue("type")+"|"+r.PathValue("key")]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Event not found.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(c)
}

func (hs *fakeHS) getStateEvents(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	// The full room state (GET /rooms/{room}/state): every type, every state key.
	out := []matrix.Event{}
	for key, raw := range room.state {
		typ, stateKey, _ := strings.Cut(key, "|")
		k := stateKey
		out = append(out, matrix.Event{Type: typ, StateKey: &k, Sender: room.stateSenders[key], Content: append(json.RawMessage(nil), raw...)})
	}
	for uid, membership := range room.members {
		if _, exists := room.state["m.room.member|"+uid]; exists {
			continue
		}
		k := uid
		raw, _ := json.Marshal(map[string]string{"membership": membership})
		out = append(out, matrix.Event{Type: "m.room.member", StateKey: &k, Sender: uid, Content: raw})
	}
	writeJSON(w, 200, out)
}

func (hs *fakeHS) putState(w http.ResponseWriter, r *http.Request, u string) {
	var content json.RawMessage
	_ = json.NewDecoder(r.Body).Decode(&content)
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	typ := r.PathValue("type")
	// Matrix authorization forbids another user's @-prefixed state key.
	if strings.HasPrefix(r.PathValue("key"), "@") && r.PathValue("key") != u {
		writeErr(w, 403, "M_FORBIDDEN", "sender cannot write another user's state key")
		return
	}
	if hs.forbidden[typ] {
		writeErr(w, 403, "M_FORBIDDEN", "You don't have permission to post that to the room.")
		return
	}
	allowed := mayPost(room, u, typ != "m.room.message")
	var levels struct {
		Events map[string]int `json:"events"`
		Users  map[string]int `json:"users"`
	}
	_ = json.Unmarshal(room.state["m.room.power_levels|"], &levels)
	if need, ok := levels.Events[typ]; ok {
		have := levels.Users[u]
		if u == roomCreator(room) {
			have = 100
		}
		allowed = have >= need
	}
	if !allowed {
		writeErr(w, 403, "M_FORBIDDEN", "You don't have permission to post that to the room.")
		return
	}
	room.state[typ+"|"+r.PathValue("key")] = content
	if room.stateSenders == nil {
		room.stateSenders = map[string]string{}
	}
	room.stateSenders[typ+"|"+r.PathValue("key")] = u
	hs.seq++
	writeJSON(w, 200, map[string]string{"event_id": fmt.Sprintf("$state%03d", hs.seq)})
}

func roomCreator(room *fakeRoom) string {
	var create struct {
		Creator string `json:"creator"`
	}
	_ = json.Unmarshal(room.state["m.room.create|"], &create)
	return create.Creator
}

// roomMessages returns message events of a room in stream order.
func (hs *fakeHS) roomMessages(roomID string) []matrix.Event {
	var out []matrix.Event
	for _, ev := range hs.events {
		if ev.RoomID == roomID && ev.Type == "m.room.message" {
			out = append(out, ev)
		}
	}
	return out
}

func (hs *fakeHS) messages(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	if fixture, ok := hs.messagePages[room.id][r.URL.Query().Get("from")]; ok {
		if fixture.failure != nil {
			f := fixture.failure
			writeErr(w, f.status, f.errcode, f.message)
		} else {
			chunk := fixture.page.Chunk
			if chunk == nil {
				chunk = []matrix.Event{}
			}
			response := map[string]any{"chunk": chunk}
			if fixture.page.End != "" {
				response["end"] = fixture.page.End
			}
			writeJSON(w, 200, response)
		}
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs := hs.roomMessages(room.id)
	if strings.Contains(r.URL.Query().Get("filter"), "m.room.redaction") {
		evs = nil
		for _, ev := range hs.events {
			if ev.RoomID == room.id && (ev.Type == "m.room.message" || ev.Type == "m.room.redaction") {
				evs = append(evs, ev)
			}
		}
	}
	end := len(evs)
	if from := r.URL.Query().Get("from"); from != "" {
		end, _ = strconv.Atoi(strings.TrimPrefix(from, "m"))
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	if hs.historyFloor > 0 && start < hs.historyFloor {
		start = min(hs.historyFloor, end)
	}
	// "to" is a sync token sN: stop at events at or before stream position N.
	to := -1
	if t := r.URL.Query().Get("to"); strings.HasPrefix(t, "s") {
		to, _ = strconv.Atoi(strings.TrimPrefix(t, "s"))
	}
	pos := map[string]int{}
	for i, ev := range hs.events {
		pos[ev.EventID] = i + 1
	}
	chunk := []matrix.Event{}
	reachedTo := false
	for i := end - 1; i >= start; i-- {
		if to >= 0 && pos[evs[i].EventID] <= to {
			reachedTo = true
			break
		}
		chunk = append(chunk, evs[i])
	}
	resp := map[string]any{"chunk": chunk, "start": "m" + strconv.Itoa(end)}
	if start > 0 && !reachedTo {
		resp["end"] = "m" + strconv.Itoa(start)
	}
	writeJSON(w, 200, resp)
}

func (hs *fakeHS) event(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	for _, ev := range hs.events {
		if ev.EventID == r.PathValue("event") && ev.RoomID == room.id {
			writeJSON(w, 200, ev)
			return
		}
	}
	writeErr(w, 404, "M_NOT_FOUND", "Event not found.")
}

func (hs *fakeHS) repliesLocked(roomID, root string) []matrix.Event {
	var out []matrix.Event
	for _, ev := range hs.roomMessages(roomID) {
		if ev.ThreadRoot() == root {
			out = append(out, ev)
		}
	}
	return out
}

func (hs *fakeHS) threads(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if !hs.threadsAPI {
		writeErr(w, 404, "M_UNRECOGNIZED", "Unrecognized request")
		return
	}
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	chunk := []matrix.Event{}
	for _, ev := range hs.roomMessages(room.id) {
		n := len(hs.repliesLocked(room.id, ev.EventID))
		if n == 0 || ev.ThreadRoot() != "" {
			continue
		}
		ev.Unsigned, _ = json.Marshal(map[string]any{"m.relations": map[string]any{"m.thread": map[string]any{"count": n}}})
		chunk = append([]matrix.Event{ev}, chunk...)
	}
	writeJSON(w, 200, map[string]any{"chunk": chunk})
}

func (hs *fakeHS) relations(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	room := hs.roomFor(w, r, u, true)
	if room == nil {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	reps := hs.repliesLocked(room.id, r.PathValue("event"))
	end := len(reps)
	if from := r.URL.Query().Get("from"); from != "" {
		end, _ = strconv.Atoi(strings.TrimPrefix(from, "r"))
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	chunk := []matrix.Event{}
	for i := end - 1; i >= start; i-- {
		chunk = append(chunk, reps[i])
	}
	resp := map[string]any{"chunk": chunk}
	if start > 0 {
		resp["next_batch"] = "r" + strconv.Itoa(start)
	}
	writeJSON(w, 200, resp)
}

func (hs *fakeHS) sync(w http.ResponseWriter, r *http.Request, u string) {
	hs.mu.Lock()
	if hs.syncsBy == nil {
		hs.syncsBy = map[string]int{}
	}
	hs.syncsBy[u]++
	hs.mu.Unlock()
	q := r.URL.Query()
	timeout, _ := strconv.Atoi(q.Get("timeout"))
	since := -1
	if s := q.Get("since"); s != "" {
		since, _ = strconv.Atoi(strings.TrimPrefix(s, "s"))
	}
	deadline := time.After(time.Duration(timeout) * time.Millisecond)
	for {
		hs.mu.Lock()
		join := map[string]any{}
		invite := map[string]any{}
		for _, inv := range hs.invites {
			if inv.user != u || (since >= 0 && inv.pos <= since) || hs.rooms[inv.room].members[u] != "invite" {
				continue
			}
			sk := u
			alias := hs.rooms[inv.room].alias
			invite[inv.room] = map[string]any{"invite_state": map[string]any{"events": []any{
				map[string]any{"type": "m.room.canonical_alias", "state_key": "", "sender": inv.by, "content": map[string]string{"alias": alias}},
				map[string]any{"type": "m.room.member", "state_key": sk, "sender": inv.by, "content": map[string]string{"membership": "invite"}},
			}}}
		}
		if since >= 0 {
			for i := since; i < len(hs.events); i++ {
				ev := hs.events[i]
				room := hs.rooms[ev.RoomID]
				if room == nil || room.members[u] != "join" {
					continue
				}
				jr, _ := join[ev.RoomID].(map[string]any)
				if jr == nil {
					jr = map[string]any{"timeline": map[string]any{"events": []matrix.Event{}}}
					join[ev.RoomID] = jr
				}
				tl := jr["timeline"].(map[string]any)
				tl["events"] = append(tl["events"].([]matrix.Event), ev)
			}
		}
		limit := hs.syncLimit
		if limit == 0 {
			limit = 100
		}
		for roomID, v := range join {
			tl := v.(map[string]any)["timeline"].(map[string]any)
			evs := tl["events"].([]matrix.Event)
			if len(evs) <= limit {
				continue
			}
			kept := evs[len(evs)-limit:]
			idx := 0
			for i, ev := range hs.roomMessages(roomID) {
				if ev.EventID == kept[0].EventID {
					idx = i
				}
			}
			tl["events"], tl["limited"], tl["prev_batch"] = kept, true, "m"+strconv.Itoa(idx)
		}
		next := "s" + strconv.Itoa(len(hs.events))
		wake := hs.wake
		hs.mu.Unlock()
		if len(join) > 0 || len(invite) > 0 || since < 0 {
			writeJSON(w, 200, map[string]any{"next_batch": next, "rooms": map[string]any{"join": join, "invite": invite}})
			return
		}
		select {
		case <-wake:
			continue
		case <-deadline:
			writeJSON(w, 200, map[string]any{"next_batch": next, "rooms": map[string]any{}})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// setAccountData stores account data directly (test setup, not over HTTP).
func (hs *fakeHS) setAccountData(user, typ string, content any) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	if hs.accountData == nil {
		hs.accountData = map[string]map[string]json.RawMessage{}
	}
	if hs.accountData[user] == nil {
		hs.accountData[user] = map[string]json.RawMessage{}
	}
	hs.accountData[user][typ], _ = json.Marshal(content)
}

// accountDataOf returns the stored content (nil when never set).
func (hs *fakeHS) accountDataOf(user, typ string) json.RawMessage {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	return hs.accountData[user][typ]
}

func (hs *fakeHS) getAccountData(w http.ResponseWriter, r *http.Request, u string) {
	if r.PathValue("user") != u {
		writeErr(w, 403, "M_FORBIDDEN", "Cannot get account data for other users.")
		return
	}
	raw := hs.accountDataOf(u, r.PathValue("type"))
	if raw == nil {
		writeErr(w, 404, "M_NOT_FOUND", "Account data not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(raw)
}

func (hs *fakeHS) putAccountData(w http.ResponseWriter, r *http.Request, u string) {
	var content json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&content); err != nil {
		writeErr(w, 400, "M_NOT_JSON", "bad json")
		return
	}
	if r.PathValue("user") != u {
		writeErr(w, 403, "M_FORBIDDEN", "Cannot add account data for other users.")
		return
	}
	hs.setAccountData(u, r.PathValue("type"), content)
	writeJSON(w, 200, map[string]any{})
}

func (hs *fakeHS) putPresence(w http.ResponseWriter, r *http.Request, u string) {
	var p matrix.Presence
	_ = json.NewDecoder(r.Body).Decode(&p)
	if r.PathValue("user") != u {
		writeErr(w, 403, "M_FORBIDDEN", "Cannot set presence for other users")
		return
	}
	hs.mu.Lock()
	hs.presence[u] = p
	hs.mu.Unlock()
	writeJSON(w, 200, map[string]any{})
}

func (hs *fakeHS) getPresence(w http.ResponseWriter, r *http.Request, _ string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	p, ok := hs.presence[r.PathValue("user")]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Presence state for this user was not found")
		return
	}
	writeJSON(w, 200, p)
}

func (hs *fakeHS) upload(w http.ResponseWriter, r *http.Request, _ string) {
	hs.mu.Lock()
	max := hs.uploadMax
	hs.mu.Unlock()
	data, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		writeErr(w, 400, "M_UNKNOWN", "read error")
		return
	}
	if int64(len(data)) > max {
		writeErr(w, 413, "M_TOO_LARGE", "Upload too large")
		return
	}
	hs.mu.Lock()
	defer hs.mu.Unlock()
	hs.seq++
	id := fmt.Sprintf("media%03dAbC_-", hs.seq)
	if hs.uploadRewrite != nil {
		data = hs.uploadRewrite(data)
	}
	hs.media[id] = fakeMedia{data: data, contentType: r.Header.Get("Content-Type"), filename: r.URL.Query().Get("filename")}
	writeJSON(w, 200, map[string]string{"content_uri": "mxc://" + hs.server + "/" + id})
}

func (hs *fakeHS) download(w http.ResponseWriter, r *http.Request, _ string) {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	m, ok := hs.media[r.PathValue("media")]
	if !ok || r.PathValue("server") != hs.server {
		writeErr(w, 404, "M_NOT_FOUND", "Media not found")
		return
	}
	data := m.data
	if alt, ok := hs.serveAs[r.PathValue("media")]; ok {
		data = alt
	}
	w.Header().Set("Content-Type", m.contentType)
	w.Write(data)
}
