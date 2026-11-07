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
	BaseURL       string
	Token         string
	HTTP          *http.Client
	BeforeRequest func() error                           // Optional immutable connection binding guard.
	FilterEvents  func(string, []Event) ([]Event, error) // Optional local redaction cache, before consumers decode content.
	directOnly    bool
}

// WithoutRedirects returns a scoped copy for operations whose response must
// come from the original endpoint. It shares the transport, not mutable policy.
// Other users of the client retain their configured redirect behavior.
func (c *Client) WithoutRedirects() *Client {
	copy := *c
	copy.directOnly = true
	return &copy
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
	// OriginalRequest is true only for a response to the sent URL with no
	// redirect followed. A redirected refusal says nothing about the first send.
	OriginalRequest bool `json:"-"`
	// Raw is bounded diagnostic entity data, possibly invalid or incomplete.
	// UIA consumes the private validated value, never this mutable compatibility field.
	Raw           json.RawMessage `json:"-"`
	control       *controlValue
	BodyComplete  bool  `json:"-"`
	ContentLength int64 `json:"-"`
}

func (e *Error) Error() string {
	// Server text and unknown error codes are arbitrary data, potentially secrets.
	// Keep them out of diagnostics; classification uses the validated fields.
	switch e.ErrCode {
	case "M_FORBIDDEN", "M_NOT_FOUND", "M_UNKNOWN_TOKEN", "M_MISSING_TOKEN", "M_USER_IN_USE", "M_ROOM_IN_USE", "M_LIMIT_EXCEEDED":
		return fmt.Sprintf("matrix: %s (status %d)", e.ErrCode, e.Status)
	}
	return "matrix: HTTP " + strconv.Itoa(e.Status)
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

func (c *Client) do(ctx context.Context, method, path string, query url.Values, in, out any, required ...string) error {
	_, err := c.doResponse(ctx, method, path, query, in, out, false, required...)
	return err
}

func (c *Client) doBulk(ctx context.Context, method, path string, query url.Values, in, out any) error {
	_, err := c.doResponse(ctx, method, path, query, in, out, true)
	return err
}

func (c *Client) doResponse(ctx context.Context, method, path string, query url.Values, in, out any, bulk bool, required ...string) (*controlValue, error) {
	var body io.Reader
	contentType := ""
	size := int64(-1)
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body, contentType, size = bytes.NewReader(data), "application/json", int64(len(data))
	}
	resp, err := c.send(ctx, method, path, query, body, contentType, size)
	if err != nil {
		return nil, err
	}
	if !bulk {
		v, err := readControl(ctx, resp, 0)
		if err != nil {
			return nil, err
		}
		if err = v.consume(ctx, out, required...); err != nil {
			return nil, err
		}
		return v, nil
	}
	defer resp.Body.Close()
	// Peer-content success responses intentionally retain their historical
	// decoder: duplicates and scalar repair cannot poison a whole sync.
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return nil, fmt.Errorf("%s %s: decode response: %w", method, path, err)
	}
	return nil, nil
}

// send performs one request and returns the response for statuses below 300.
// Other statuses are turned into *Error (or ErrAuth) and the body is closed.
// size is the body length, or -1 when there is no body.
func (c *Client) send(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string, size int64) (*http.Response, error) {
	if c.BeforeRequest != nil {
		if err := c.BeforeRequest(); err != nil {
			return nil, err
		}
	}
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if body != nil && size >= 0 {
		req.ContentLength = size
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	// Track provenance per call without mutating the shared client's policy.
	// Final URL equality alone misses a redirect chain returning to the first URL.
	originalURL := req.URL.String()
	redirected := false
	requestClient := *hc
	requestClient.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		redirected = true
		// Credentials in headers or registration bodies never cross origins,
		// including a redirect to a subdomain that net/http otherwise trusts.
		if next.URL.Scheme != req.URL.Scheme || !strings.EqualFold(next.URL.Host, req.URL.Host) {
			return errors.New("cross-origin Matrix redirect refused")
		}
		if c.directOnly {
			return http.ErrUseLastResponse
		}
		if hc.CheckRedirect != nil {
			return hc.CheckRedirect(next, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	resp, err := requestClient.Do(req)
	if err != nil {
		// url.Error includes the URL, which never carries the token.
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	if c.directOnly && resp.StatusCode < 300 && (resp.StatusCode < 200 || redirected || resp.Request == nil || resp.Request.URL == nil || resp.Request.URL.String() != originalURL || resp.Request.Method != method) {
		resp.Body.Close()
		return nil, fmt.Errorf("%s %s: response is not from original request", method, path)
	}
	if resp.StatusCode >= 300 {
		me := &Error{Status: resp.StatusCode,
			OriginalRequest: !redirected && resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.String() == originalURL && resp.Request.Method == method}
		v, decodeErr := readControl(ctx, resp, maxErrorBody)
		me.ContentLength = resp.ContentLength
		if v != nil {
			me.Raw = append([]byte(nil), v.raw...)
			me.BodyComplete = v.complete
		}
		if decodeErr == nil {
			var fields struct {
				ErrCode string `json:"errcode"`
				Message string `json:"error"`
			}
			if v.consume(ctx, &fields) == nil {
				me.ErrCode, me.Message = fields.ErrCode, fields.Message
				me.control = v
			}
		}
		if resp.StatusCode == http.StatusUnauthorized && (me.ErrCode == "M_UNKNOWN_TOKEN" || me.ErrCode == "M_MISSING_TOKEN") {
			return nil, fmt.Errorf("%w (%s)", ErrAuth, me.ErrCode)
		}
		return nil, me
	}
	return resp, nil
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultRequestTimeout)
}

func (c *Client) call(ctx context.Context, method, path string, query url.Values, in, out any, required ...string) error {
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	return c.do(ctx, method, path, query, in, out, required...)
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
		err := c.call(ctx, http.MethodPost, v3+"/register", nil, req, &out, "user_id", "access_token", "device_id")
		if interrupted := ctx.Err(); interrupted != nil {
			return out, interrupted
		}
		if err == nil {
			return out, nil
		}
		var me *Error
		if !errors.As(err, &me) || me.Status != http.StatusUnauthorized {
			return out, err
		}
		var uia uiaResponse
		if !me.uia(&uia) {
			return out, err
		}
		stage, ok := nextStage(uia)
		if lastStage != "" && (me.ErrCode != "" || stage == lastStage) {
			// The stage we just submitted was not accepted.
			if lastStage == "m.login.registration_token" {
				return out, fmt.Errorf("register: registration token rejected by the homeserver: %w", err)
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

// UserExists asks whether username is taken, with the first unauthenticated
// step of registration: the homeserver refuses a taken name with
// M_USER_IN_USE before any authentication, and answers a free one with the
// 401 UIA challenge, which creates nothing.
func (c *Client) UserExists(ctx context.Context, username string) (bool, error) {
	err := c.call(ctx, http.MethodPost, v3+"/register", nil, RegisterRequest{Username: username}, nil)
	if interrupted := ctx.Err(); interrupted != nil {
		return false, interrupted
	}
	switch {
	case err == nil:
		return false, fmt.Errorf("register probe: the homeserver registered %s without authentication", username)
	case IsCode(err, "M_USER_IN_USE"):
		return true, nil
	}
	var me *Error
	var uia uiaResponse
	if errors.As(err, &me) && me.Status == http.StatusUnauthorized && me.uia(&uia) {
		return false, nil
	}
	return false, fmt.Errorf("register probe: %w", err)
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

// --- login, deactivation ----------------------------------------------------------

// LoginResponse is the successful /login response.
type LoginResponse struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
	DeviceID    string `json:"device_id"`
}

// userIdentifier is the m.id.user identifier used by password login and UIA.
func userIdentifier(userID string) map[string]string {
	return map[string]string{"type": "m.id.user", "user": userID}
}

// Login performs m.login.password for userID. A non-empty deviceID asks the
// server to reuse that device, so the device (and its display name) is kept.
func (c *Client) Login(ctx context.Context, userID, password, deviceID string) (LoginResponse, error) {
	req := map[string]any{
		"type":       "m.login.password",
		"identifier": userIdentifier(userID),
		"password":   password,
	}
	if deviceID != "" {
		req["device_id"] = deviceID
	}
	var out LoginResponse
	err := c.call(ctx, http.MethodPost, v3+"/login", nil, req, &out, "user_id", "access_token", "device_id")
	return out, err
}

// Deactivate permanently deactivates the authenticated account with
// user-interactive auth: the first request learns the UIA session, the second
// completes the m.login.password stage with userID and password.
func (c *Client) Deactivate(ctx context.Context, userID, password string) error {
	path := v3 + "/account/deactivate"
	err := c.call(ctx, http.MethodPost, path, nil, map[string]any{}, nil)
	if interrupted := ctx.Err(); interrupted != nil {
		return interrupted
	}
	if err == nil {
		return nil // the server asked for no authentication
	}
	var me *Error
	if !errors.As(err, &me) || me.Status != http.StatusUnauthorized {
		return fmt.Errorf("deactivate: %w", err)
	}
	var uia uiaResponse
	if !me.uia(&uia) {
		return fmt.Errorf("deactivate: %w", err)
	}
	if !hasFlow(uia, "m.login.password") {
		return fmt.Errorf("deactivate: the homeserver offers no m.login.password flow")
	}
	req := map[string]any{"auth": map[string]any{
		"type":       "m.login.password",
		"identifier": userIdentifier(userID),
		"password":   password,
		"session":    uia.Session,
	}}
	err = c.call(ctx, http.MethodPost, path, nil, req, nil)
	if IsStatus(err, http.StatusUnauthorized) {
		return fmt.Errorf("deactivate: password rejected by the homeserver: %w", err)
	}
	if err != nil {
		return fmt.Errorf("deactivate: %w", err)
	}
	return nil
}

// hasFlow reports whether u offers a single-stage flow of stage.
func hasFlow(u uiaResponse, stage string) bool {
	for _, f := range u.Flows {
		if len(f.Stages) == 1 && f.Stages[0] == stage {
			return true
		}
	}
	return false
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
	err := c.call(ctx, http.MethodGet, v3+"/account/whoami", nil, nil, &out, "user_id")
	return out, err
}

// JoinedRooms lists joined room ids.
func (c *Client) JoinedRooms(ctx context.Context) ([]string, error) {
	var out struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	err := c.call(ctx, http.MethodGet, v3+"/joined_rooms", nil, nil, &out, "joined_rooms")
	return out.JoinedRooms, err
}

// ErrUnknownAlias is returned when an alias does not resolve.
var ErrUnknownAlias = errors.New("unknown room alias")

// ResolveAlias maps #alias:server to a room id.
func (c *Client) ResolveAlias(ctx context.Context, alias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.call(ctx, http.MethodGet, v3+"/directory/room/"+seg(alias), nil, nil, &out, "room_id")
	if IsCode(err, "M_NOT_FOUND") {
		return "", fmt.Errorf("%w %s", ErrUnknownAlias, alias)
	}
	return out.RoomID, err
}

// DeleteAlias removes a room alias from the directory (the room stays).
func (c *Client) DeleteAlias(ctx context.Context, alias string) error {
	return c.call(ctx, http.MethodDelete, v3+"/directory/room/"+seg(alias), nil, nil, nil)
}

// Join joins a room by id or alias and returns the room id.
func (c *Client) Join(ctx context.Context, roomIDOrAlias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.call(ctx, http.MethodPost, v3+"/join/"+seg(roomIDOrAlias), nil, map[string]any{}, &out, "room_id")
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

// InitialStateEvent is the client-supplied state, without server event metadata.
type InitialStateEvent struct {
	Type     string          `json:"type"`
	StateKey string          `json:"state_key"`
	Content  json.RawMessage `json:"content"`
}

// CreateRoomRequest is the body of POST /createRoom (the subset a2a sets).
type CreateRoomRequest struct {
	IsDirect        bool                `json:"is_direct,omitempty"`
	InitialState    []InitialStateEvent `json:"initial_state,omitempty"`
	Preset          string              `json:"preset,omitempty"`
	Visibility      string              `json:"visibility,omitempty"`
	CreationContent map[string]any      `json:"creation_content,omitempty"`
	RoomAliasName   string              `json:"room_alias_name,omitempty"`
	Name            string              `json:"name,omitempty"`
	Topic           string              `json:"topic,omitempty"`
	Invite          []string            `json:"invite,omitempty"`
	// PowerLevelContentOverride is merged over the preset's m.room.power_levels.
	PowerLevelContentOverride map[string]any `json:"power_level_content_override,omitempty"`
}

// CreateRoom creates a room and returns its id. A taken alias is the
// homeserver's M_ROOM_IN_USE error.
func (c *Client) CreateRoom(ctx context.Context, req CreateRoomRequest) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.call(ctx, http.MethodPost, v3+"/createRoom", nil, req, &out, "room_id")
	if err == nil && out.RoomID == "" {
		return "", fmt.Errorf("createRoom: the homeserver returned no room_id")
	}
	return out.RoomID, err
}

// GetState reads one state event's content into out.
func (c *Client) GetState(ctx context.Context, roomID, evType, stateKey string, out any) error {
	return c.callBulk(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/state/"+seg(evType)+"/"+seg(stateKey), nil, nil, out)
}

// GetStateEvents lists a room's state events of one type. The client-server API
// has no per-type listing (GET /state/{type} is the event with state key ""), so
// it reads the full room state and filters.
func (c *Client) GetStateEvents(ctx context.Context, roomID, evType string) ([]Event, error) {
	all, err := c.GetRoomState(ctx, roomID)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, ev := range all {
		if ev.Type == evType {
			out = append(out, ev)
		}
	}
	return out, nil
}

// GetRoomState reads one consistent response containing status and membership
// events, so a liveness snapshot does not need a separate full-state fetch per type.
func (c *Client) GetRoomState(ctx context.Context, roomID string) ([]Event, error) {
	var all []Event
	err := c.callBulk(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/state", nil, nil, &all)
	return all, err
}

// PutState sends a state event and returns its event id.
func (c *Client) PutState(ctx context.Context, roomID, evType, stateKey string, content any) (string, error) {
	ack, err := c.PutStateReceipt(ctx, roomID, evType, stateKey, content)
	return ack.EventID(), err
}

// PutStateReceipt returns the strict ACK together with its complete raw entity.
// Use ForOwnStatus or WithoutRedirects when original-request provenance matters.
func (c *Client) PutStateReceipt(ctx context.Context, roomID, evType, stateKey string, content any) (ACKReceipt, error) {
	return c.ack(ctx, v3+"/rooms/"+seg(roomID)+"/state/"+seg(evType)+"/"+seg(stateKey), content)
}

// SendMessage sends an m.room.message event and returns its event id.
func (c *Client) SendMessage(ctx context.Context, roomID string, content any) (string, error) {
	ack, err := c.SendMessageReceipt(ctx, roomID, content)
	return ack.EventID(), err
}

// SendMessageReceipt is SendMessage with owned, validated ACK evidence.
func (c *Client) SendMessageReceipt(ctx context.Context, roomID string, content any) (ACKReceipt, error) {
	return c.SendMessageTxnReceipt(ctx, roomID, NewTxnID(), content)
}

// --- events ---------------------------------------------------------------------

// Event is a client-format room event.
type Event struct {
	// RedactionVerified is local evidence, never accepted from wire JSON.
	RedactionVerified bool            `json:"-"`
	EventID           string          `json:"event_id"`
	Type              string          `json:"type"`
	Sender            string          `json:"sender"`
	RoomID            string          `json:"room_id,omitempty"`
	OriginServerTS    int64           `json:"origin_server_ts"`
	StateKey          *string         `json:"state_key,omitempty"`
	Content           json.RawMessage `json:"content"`
	Redacts           string          `json:"redacts,omitempty"`
	Unsigned          json.RawMessage `json:"unsigned,omitempty"`
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
	if e.IsRedacted() {
		return MessageContent{Body: "[redacted]"}
	}
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
	return c.messages(ctx, roomID, from, dir, limit, `{"types":["m.room.message"]}`)
}

// MessagesWithRedactions lets incremental readers invalidate cached messages
// after a server-authorized redaction, including room versions before v11.
func (c *Client) MessagesWithRedactions(ctx context.Context, roomID, from, dir string, limit int) (Page, error) {
	return c.messages(ctx, roomID, from, dir, limit, `{"types":["m.room.message","m.room.redaction"]}`)
}

func (c *Client) messages(ctx context.Context, roomID, from, dir string, limit int, filter string) (Page, error) {
	q := url.Values{"dir": {dir}, "limit": {strconv.Itoa(limit)}}
	if from != "" {
		q.Set("from", from)
	}
	q.Set("filter", filter)
	var out Page
	err := c.callBulk(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/messages", q, nil, &out)
	if err == nil && c.FilterEvents != nil {
		out.Chunk, err = c.FilterEvents(roomID, out.Chunk)
	}
	return out, err
}

// Threads lists thread roots (v1 /threads), newest first.
func (c *Client) Threads(ctx context.Context, roomID, from string, limit int) (Page, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}}
	if from != "" {
		q.Set("from", from)
	}
	var out Page
	err := c.callBulk(ctx, http.MethodGet, v1+"/rooms/"+seg(roomID)+"/threads", q, nil, &out)
	if err == nil && c.FilterEvents != nil {
		out.Chunk, err = c.FilterEvents(roomID, out.Chunk)
	}
	return out, err
}

// GetEvent fetches one event.
func (c *Client) GetEvent(ctx context.Context, roomID, eventID string) (Event, error) {
	var out Event
	err := c.callBulk(ctx, http.MethodGet, v3+"/rooms/"+seg(roomID)+"/event/"+seg(eventID), nil, nil, &out)
	if err == nil && c.FilterEvents != nil {
		var events []Event
		events, err = c.FilterEvents(roomID, []Event{out})
		if err == nil {
			out = events[0]
		}
	}
	return out, err
}

// --- sync -------------------------------------------------------------------------

// SyncResponse is the subset of /sync the CLI reads.
type SyncResponse struct {
	NextBatch string `json:"next_batch"`
	Rooms     struct {
		Join map[string]struct {
			Timeline struct {
				Events    []Event `json:"events"`
				Limited   bool    `json:"limited"`
				PrevBatch string  `json:"prev_batch,omitempty"`
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
	err := c.doBulk(ctx, http.MethodGet, v3+"/sync", q, nil, &out)
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
	err := c.callBulk(ctx, http.MethodGet, v3+"/presence/"+seg(userID)+"/status", nil, nil, &out)
	return out, err
}
