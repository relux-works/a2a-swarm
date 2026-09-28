// Package matrix is a thin Matrix client-server API client built on net/http.
// It implements only what the a2a CLI needs and never includes the access
// token in errors, URLs, or logs (the token travels only in the Authorization header).
package matrix

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultRequestTimeout bounds every non-sync request.
const DefaultRequestTimeout = 30 * time.Second

// maxErrorBody caps how much of an error response is read.
const maxErrorBody = 64 << 10

// Client talks to one homeserver as one user (or anonymously for registration).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

// New returns a client for baseURL with the given access token (may be empty).
func New(baseURL, token string) *Client {
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{}}
}

// Error is a Matrix API error (the standard errcode/error body plus the HTTP status).
type Error struct {
	Status  int
	ErrCode string `json:"errcode"`
	Message string `json:"error"`
	// Raw is the decoded body, used for UIA responses (session, flows, completed).
	Raw json.RawMessage `json:"-"`
}

func (e *Error) Error() string {
	code := e.ErrCode
	if code == "" {
		code = "HTTP " + strconv.Itoa(e.Status)
	}
	if e.Message == "" {
		return fmt.Sprintf("matrix: %s (status %d)", code, e.Status)
	}
	return fmt.Sprintf("matrix: %s: %s (status %d)", code, e.Message, e.Status)
}

// IsCode reports whether err is a Matrix error with the given errcode.
func IsCode(err error, code string) bool {
	var me *Error
	return errors.As(err, &me) && me.ErrCode == code
}

// IsStatus reports whether err is a Matrix error with the given HTTP status.
func IsStatus(err error, status int) bool {
	var me *Error
	return errors.As(err, &me) && me.Status == status
}

// ErrAuth wraps token rejections so the CLI can print one clear message.
var ErrAuth = errors.New("access token rejected by the homeserver")

// seg escapes one path segment (room ids, aliases, event ids, user ids).
func seg(s string) string { return url.PathEscape(s) }

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries the token.
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		me := &Error{Status: resp.StatusCode, Raw: data}
		_ = json.Unmarshal(data, me)
		if resp.StatusCode == http.StatusUnauthorized && (me.ErrCode == "M_UNKNOWN_TOKEN" || me.ErrCode == "M_MISSING_TOKEN") {
			return fmt.Errorf("%w (%s)", ErrAuth, me.ErrCode)
		}
		return me
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultRequestTimeout)
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, in, out any) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return c.do(ctx, method, path, query, in, out)
}

// NewTxnID returns a random transaction id for idempotent sends.
func NewTxnID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "a2a" + hex.EncodeToString(b[:])
}

const (
	v3 = "/_matrix/client/v3"
	v1 = "/_matrix/client/v1"
)

// --- registration -----------------------------------------------------------

// RegisterRequest is the body of POST /register.
type RegisterRequest struct {
	Username                 string         `json:"username"`
	Password                 string         `json:"password"`
	InitialDeviceDisplayName string         `json:"initial_device_display_name,omitempty"`
	Auth                     map[string]any `json:"auth,omitempty"`
}

// RegisterResponse is the successful /register response.
type RegisterResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
}

type uiaResponse struct {
	Session   string   `json:"session"`
	Completed []string `json:"completed"`
	Flows     []struct {
		Stages []string `json:"stages"`
	} `json:"flows"`
}

// Register performs user-interactive registration with the
// m.login.registration_token stage (plus m.login.dummy if the server asks for it).
func (c *Client) Register(ctx context.Context, username, password, regToken, deviceName string) (RegisterResponse, error) {
	req := RegisterRequest{Username: username, Password: password, InitialDeviceDisplayName: deviceName}
	var out RegisterResponse
	lastStage := ""
	for step := 0; step < 5; step++ {
		err := c.call(ctx, http.MethodPost, v3+"/register", nil, req, &out)
		if err == nil {
			return out, nil
		}
		var me *Error
		if !errors.As(err, &me) || me.Status != http.StatusUnauthorized {
			return out, err
		}
		var uia uiaResponse
		if json.Unmarshal(me.Raw, &uia) != nil || uia.Session == "" {
			return out, err
		}
		stage, ok := nextStage(uia)
		if lastStage != "" && (me.ErrCode != "" || stage == lastStage) {
			// The stage we just submitted was not accepted.
			if lastStage == "m.login.registration_token" {
				return out, fmt.Errorf("register: registration token rejected by the homeserver (%s)", me.ErrCode)
			}
			return out, fmt.Errorf("register: stage %s rejected: %w", lastStage, err)
		}
		if !ok {
			return out, fmt.Errorf("register: no supported registration flow (supported stages: m.login.registration_token, m.login.dummy)")
		}
		auth := map[string]any{"type": stage, "session": uia.Session}
		if stage == "m.login.registration_token" {
			if regToken == "" {
				return out, fmt.Errorf("register: the homeserver requires a registration token; set %s", "A2A_MATRIX_REGISTRATION_TOKEN")
			}
			auth["token"] = regToken
		}
		req.Auth = auth
		lastStage = stage
	}
	return out, fmt.Errorf("register: too many authentication stages")
}

var supportedStages = map[string]bool{"m.login.registration_token": true, "m.login.dummy": true}

func nextStage(u uiaResponse) (string, bool) {
	done := map[string]bool{}
	for _, s := range u.Completed {
		done[s] = true
	}
	for _, f := range u.Flows {
		ok := true
		next := ""
		for _, s := range f.Stages {
			if !supportedStages[s] {
				ok = false
				break
			}
			if next == "" && !done[s] {
				next = s
			}
		}
		if ok && next != "" {
			return next, true
		}
	}
	return "", false
}

// --- account, rooms ------------------------------------------------------------

// WhoamiResponse is GET /account/whoami.
type WhoamiResponse struct {
	UserID   string `json:"user_id"`
	DeviceID string `json:"device_id"`
}

// Whoami returns the authenticated user.
func (c *Client) Whoami(ctx context.Context) (WhoamiResponse, error) {
	var out WhoamiResponse
	err := c.call(ctx, http.MethodGet, v3+"/account/whoami", nil, nil, &out)
	return out, err
}

// JoinedRooms lists joined room ids.
func (c *Client) JoinedRooms(ctx context.Context) ([]string, error) {
	var out struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	err := c.call(ctx, http.MethodGet, v3+"/joined_rooms", nil, nil, &out)
	return out.JoinedRooms, err
}

// ErrUnknownAlias is returned when an alias does not resolve.
var ErrUnknownAlias = errors.New("unknown room alias")

// ResolveAlias maps #alias:server to a room id.
func (c *Client) ResolveAlias(ctx context.Context, alias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.call(ctx, http.MethodGet, v3+"/directory/room/"+seg(alias), nil, nil, &out)
	if IsStatus(err, http.StatusNotFound) || IsCode(err, "M_NOT_FOUND") {
		return "", fmt.Errorf("%w %s", ErrUnknownAlias, alias)
	}
	return out.RoomID, err
}

// Join joins a room by id or alias and returns the room id.
func (c *Client) Join(ctx context.Context, roomIDOrAlias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.call(ctx, http.MethodPost, v3+"/join/"+seg(roomIDOrAlias), nil, map[string]any{}, &out)
	return out.RoomID, err
}

// Leave leaves a room.
func (c *Client) Leave(ctx context.Context, roomID string) error {
	return c.call(ctx, http.MethodPost, v3+"/rooms/"+seg(roomID)+"/leave", nil, map[string]any{}, nil)
}

// Invite invites userID to roomID.
func (c *Client) Invite(ctx context.Context, roomID, userID string) error {
	return c.call(ctx, http.MethodPost, v3+"/rooms/"+seg(roomID)+"/invite", nil, map[string]any{"user_id": userID}, nil)
}

// GetState reads one state event's content into out.
func (c *Client) GetState(ctx context.Context, roomID, evType, stateKey string, out any) error {
	return c.call(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/state/"+seg(evType)+"/"+seg(stateKey), nil, nil, out)
}

// PutState sends a state event and returns its event id.
func (c *Client) PutState(ctx context.Context, roomID, evType, stateKey string, content any) (string, error) {
	var out struct {
		EventID string `json:"event_id"`
	}
	err := c.call(ctx, http.MethodPut, v3+"/rooms/"+seg(roomID)+"/state/"+seg(evType)+"/"+seg(stateKey), nil, content, &out)
	return out.EventID, err
}

// SendMessage sends an m.room.message event and returns its event id.
func (c *Client) SendMessage(ctx context.Context, roomID string, content any) (string, error) {
	var out struct {
		EventID string `json:"event_id"`
	}
	err := c.call(ctx, http.MethodPut, v3+"/rooms/"+seg(roomID)+"/send/m.room.message/"+seg(NewTxnID()), nil, content, &out)
	return out.EventID, err
}

// --- events ---------------------------------------------------------------------

// Event is a client-format room event.
type Event struct {
	EventID        string          `json:"event_id"`
	Type           string          `json:"type"`
	Sender         string          `json:"sender"`
	RoomID         string          `json:"room_id,omitempty"`
	OriginServerTS int64           `json:"origin_server_ts"`
	StateKey       *string         `json:"state_key,omitempty"`
	Content        json.RawMessage `json:"content"`
	Unsigned       json.RawMessage `json:"unsigned,omitempty"`
}

// MessageContent is the subset of m.room.message content the CLI reads.
type MessageContent struct {
	MsgType   string          `json:"msgtype"`
	Body      string          `json:"body"`
	RelatesTo *RelatesTo      `json:"m.relates_to,omitempty"`
	Mentions  *Mentions       `json:"m.mentions,omitempty"`
	A2A       json.RawMessage `json:"ai.relux.a2a,omitempty"`
	// Membership is set for m.room.member events.
	Membership string `json:"membership,omitempty"`
}

// RelatesTo is m.relates_to.
type RelatesTo struct {
	RelType       string     `json:"rel_type,omitempty"`
	EventID       string     `json:"event_id,omitempty"`
	IsFallingBack bool       `json:"is_falling_back,omitempty"`
	InReplyTo     *InReplyTo `json:"m.in_reply_to,omitempty"`
}

// InReplyTo is m.in_reply_to.
type InReplyTo struct {
	EventID string `json:"event_id"`
}

// Mentions is m.mentions.
type Mentions struct {
	UserIDs []string `json:"user_ids,omitempty"`
	Room    bool     `json:"room,omitempty"`
}

// Message decodes the event content as a message.
func (e Event) Message() MessageContent {
	var m MessageContent
	_ = json.Unmarshal(e.Content, &m)
	return m
}

// ThreadRoot returns the thread root id if the event is a thread reply.
func (e Event) ThreadRoot() string {
	m := e.Message()
	if m.RelatesTo != nil && m.RelatesTo.RelType == "m.thread" {
		return m.RelatesTo.EventID
	}
	return ""
}

// ThreadCount returns unsigned.m.relations.m.thread.count, or -1 if absent.
func (e Event) ThreadCount() int {
	var u struct {
		Relations struct {
			Thread *struct {
				Count int `json:"count"`
			} `json:"m.thread"`
		} `json:"m.relations"`
	}
	if json.Unmarshal(e.Unsigned, &u) != nil || u.Relations.Thread == nil {
		return -1
	}
	return u.Relations.Thread.Count
}

// Page is a paginated list of events.
type Page struct {
	Chunk     []Event `json:"chunk"`
	End       string  `json:"end,omitempty"`
	NextBatch string  `json:"next_batch,omitempty"`
}

// Messages pages through the room timeline. dir is "b" (newest first) or "f".
func (c *Client) Messages(ctx context.Context, roomID, from, dir string, limit int) (Page, error) {
	q := url.Values{"dir": {dir}, "limit": {strconv.Itoa(limit)}}
	if from != "" {
		q.Set("from", from)
	}
	q.Set("filter", `{"types":["m.room.message"]}`)
	var out Page
	err := c.call(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/messages", q, nil, &out)
	return out, err
}

// Threads lists thread roots (v1 /threads), newest first.
func (c *Client) Threads(ctx context.Context, roomID, from string, limit int) (Page, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if from != "" {
		q.Set("from", from)
	}
	var out Page
	err := c.call(ctx, http.MethodGet, v1+"/rooms/"+seg(roomID)+"/threads", q, nil, &out)
	return out, err
}

// ThreadReplies lists replies to root (v1 /relations/{root}/m.thread). dir "b" is newest first.
func (c *Client) ThreadReplies(ctx context.Context, roomID, root, from, dir string, limit int) (Page, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "dir": {dir}}
	if from != "" {
		q.Set("from", from)
	}
	var out Page
	err := c.call(ctx, http.MethodGet, v1+"/rooms/"+seg(roomID)+"/relations/"+seg(root)+"/m.thread", q, nil, &out)
	return out, err
}

// GetEvent fetches one event.
func (c *Client) GetEvent(ctx context.Context, roomID, eventID string) (Event, error) {
	var out Event
	err := c.call(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, &out)
	return out, err
}

// --- sync -------------------------------------------------------------------------

// SyncResponse is the subset of /sync the CLI reads.
type SyncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events  []Event `json:"events"`
				Limited bool    `json:"limited"`
			} `json:"timeline"`
		} `json:"join"`
		Invite map[string]struct {
			InviteState struct {
				Events []Event `json:"events"`
			} `json:"invite_state"`
		} `json:"invite"`
	} `json:"rooms"`
}

// Sync long-polls /sync. timeout is the server-side wait.
func (c *Client) Sync(ctx context.Context, since string, timeout time.Duration, filter string) (SyncResponse, error) {
	q := url.Values{"timeout": {strconv.FormatInt(timeout.Milliseconds(), 10)}}
	if since != "" {
		q.Set("since", since)
	}
	if filter != "" {
		q.Set("filter", filter)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout+DefaultRequestTimeout)
	defer cancel()
	var out SyncResponse
	err := c.do(ctx, http.MethodGet, v3+"/sync", q, nil, &out)
	return out, err
}

// --- presence -------------------------------------------------------------------

// Presence is the presence status body.
type Presence struct {
	Presence  string `json:"presence"`
	StatusMsg string `json:"status_msg,omitempty"`
}

// SetPresence sets the user's presence.
func (c *Client) SetPresence(ctx context.Context, userID string, p Presence) error {
	return c.call(ctx, http.MethodPut, v3+"/presence/"+seg(userID)+"/status", nil, p, nil)
}

// GetPresence reads a user's presence.
func (c *Client) GetPresence(ctx context.Context, userID string) (Presence, error) {
	var out Presence
	err := c.call(ctx, http.MethodGet, v3+"/presence/"+seg(userID)+"/status", nil, nil, &out)
	return out, err
}
