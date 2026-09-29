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

	mu         sync.Mutex
	server     string
	regToken   string
	tokens     map[string]string // token -> user id
	users      map[string]bool
	aliases    map[string]string
	rooms      map[string]*fakeRoom
	events     []matrix.Event // stream position = index + 1
	invites    []fakeInvite
	presence   map[string]matrix.Presence
	forbidden  map[string]bool // state event types that are forbidden
	threadsAPI bool
	failures   map[string]fakeFailure // route -> forced failure
	rawURLs    []string
	wake       chan struct{}
	seq        int
	txns       map[string]string // user|room|txn -> event id (send idempotency)
	// media repository
	media     map[string]fakeMedia // media id -> stored upload
	serveAs   map[string][]byte    // media id -> bytes served instead (a lying server)
	uploadMax int64                // m.upload.size
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
}

type fakeInvite struct {
	pos            int
	room, user, by string
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
	route("GET "+v3+"/account/whoami", true, func(w http.ResponseWriter, r *http.Request, u string) {
		writeJSON(w, 200, map[string]string{"user_id": u, "device_id": "DEV" + localpart(u)})
	})
	route("GET "+v3+"/joined_rooms", true, hs.joinedRooms)
	route("GET "+v3+"/directory/room/{alias}", true, hs.directory)
	route("POST "+v3+"/join/{room}", true, hs.join)
	route("POST "+v3+"/rooms/{room}/leave", true, hs.leave)
	route("POST "+v3+"/rooms/{room}/invite", true, hs.invite)
	route("PUT "+v3+"/rooms/{room}/send/{type}/{txn}", true, hs.send)
	route("GET "+v3+"/rooms/{room}/state/{type}/{key...}", true, hs.getState)
	route("PUT "+v3+"/rooms/{room}/state/{type}/{key...}", true, hs.putState)
	route("GET "+v3+"/rooms/{room}/messages", true, hs.messages)
	route("GET "+v3+"/rooms/{room}/event/{event}", true, hs.event)
	route("GET "+v1+"/rooms/{room}/threads", true, hs.threads)
	route("GET "+v1+"/rooms/{room}/relations/{event}/m.thread", true, hs.relations)
	route("GET "+v3+"/sync", true, hs.sync)
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
	return uid, tok
}

// addRoom creates a room with an alias and joined members.
func (hs *fakeHS) addRoom(alias string, members ...string) string {
	hs.mu.Lock()
	defer hs.mu.Unlock()
	id := "!" + alias + "RoomID"
	full := "#" + alias + ":" + hs.server
	r := &fakeRoom{id: id, alias: full, name: alias, members: map[string]string{}, state: map[string]json.RawMessage{}}
	for _, m := range members {
		r.members[m] = "join"
	}
	r.state["m.room.canonical_alias|"], _ = json.Marshal(map[string]string{"alias": full})
	r.state["m.room.name|"], _ = json.Marshal(map[string]string{"name": alias + " room"})
	hs.rooms[id] = r
	hs.aliases[full] = id
	return id
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
	hs.events = append(hs.events, matrix.Event{
		EventID: id, Type: typ, Sender: sender, RoomID: roomID,
		OriginServerTS: time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC).UnixMilli() + int64(hs.seq)*1000,
		Content:        raw,
	})
	close(hs.wake)
	hs.wake = make(chan struct{})
	return id
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
	hs.tokens[tok] = uid
	writeJSON(w, 200, map[string]string{"user_id": uid, "access_token": tok, "device_id": "NEWDEV"})
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
	if room.members[u] != "invite" && room.members[u] != "join" {
		writeErr(w, 403, "M_FORBIDDEN", "You are not invited to this room.")
		return
	}
	room.members[u] = "join"
	writeJSON(w, 200, map[string]string{"room_id": room.id})
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
	c, ok := room.state[r.PathValue("type")+"|"+r.PathValue("key")]
	if !ok {
		writeErr(w, 404, "M_NOT_FOUND", "Event not found.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(c)
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
	if hs.forbidden[typ] {
		writeErr(w, 403, "M_FORBIDDEN", "You don't have permission to post that to the room.")
		return
	}
	room.state[typ+"|"+r.PathValue("key")] = content
	hs.seq++
	writeJSON(w, 200, map[string]string{"event_id": fmt.Sprintf("$state%03d", hs.seq)})
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
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	evs := hs.roomMessages(room.id)
	end := len(evs)
	if from := r.URL.Query().Get("from"); from != "" {
		end, _ = strconv.Atoi(strings.TrimPrefix(from, "m"))
	}
	start := end - limit
	if start < 0 {
		start = 0
	}
	chunk := []matrix.Event{}
	for i := end - 1; i >= start; i-- {
		chunk = append(chunk, evs[i])
	}
	resp := map[string]any{"chunk": chunk, "start": "m" + strconv.Itoa(end)}
	if start > 0 {
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
