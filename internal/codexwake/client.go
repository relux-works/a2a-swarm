// Package codexwake implements the narrow native Codex doorbell transport.
// It never resumes a thread, answers approval requests, or supplies user input.
package codexwake

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

const ToolName = "doorbell"
const ToolNamespace = "a2a"

// RPCError retains a correlated server code without exposing its raw message.
type RPCError struct {
	Method string
	Code   int
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("runtime %s refused (code %d)", e.Method, e.Code)
}

// Invalid request/method/params are rejected before submission in the pinned
// servers. Internal errors can follow a successful steer; they stay uncertain.
func DefinitiveRefusal(err error) bool {
	var rpc *RPCError
	return errors.As(err, &rpc) && rpc.Method == "turn/start" && (rpc.Code == -32600 || rpc.Code == -32601 || rpc.Code == -32602)
}

type Status struct {
	Type        string   `json:"type"`
	ActiveFlags []string `json:"activeFlags,omitempty"`
}
type Item struct {
	Type      string          `json:"type"`
	Name      string          `json:"name,omitempty"`
	Namespace string          `json:"namespace,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
}
type Turn struct {
	ID        string `json:"id"`
	Status    string `json:"status"`
	Items     []Item `json:"items"`
	StartedAt *int64 `json:"startedAt,omitempty"`
}
type Snapshot struct {
	ID     string `json:"id"`
	Cwd    string `json:"cwd"`
	Status Status `json:"status"`
	Turns  []Turn `json:"turns"`
}

// Client serializes RPCs and correlates replies while ignoring notifications
// and server requests. It never acts as an approval or interactive UI client.
type Client struct {
	conn     *websocket.Conn
	mu       sync.Mutex
	next     int
	Version  string
	SocketID string
}

// EndpointPath admits only an explicit local Unix endpoint. Remote runtime
// access and credentials are not part of this adapter's authority.
func EndpointPath(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "unix" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !filepath.IsAbs(u.Path) || u.Path == "/" {
		return "", errors.New("runtime endpoint must be unix:///absolute/socket without a host, credentials, query or fragment")
	}
	return filepath.Clean(u.Path), nil
}

func Dial(ctx context.Context, endpoint string) (*Client, error) {
	path, err := EndpointPath(endpoint)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	beforeID, identityErr := SocketIdentity(endpoint)
	if identityErr != nil {
		return nil, identityErr
	}
	if err != nil {
		return nil, errors.New("runtime endpoint unavailable")
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("runtime endpoint must be a private Unix socket")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	httpClient := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("runtime redirects refused") }}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: httpClient})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, errors.New("runtime WebSocket connection unavailable")
	}
	conn.SetReadLimit(4 << 20)
	client := &Client{conn: conn}
	var initialized map[string]json.RawMessage
	if err := client.rpc(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "a2a_native_doorbell", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil {
		client.Close()
		return nil, err
	}
	var userAgent string
	_ = json.Unmarshal(initialized["userAgent"], &userAgent)
	fields := strings.Fields(userAgent)
	if len(fields) > 0 {
		_, client.Version, _ = strings.Cut(fields[0], "/")
	}
	if client.Version != "0.160.0" && client.Version != "0.160.1" && client.Version != "0.161.0" {
		client.Close()
		return nil, errors.New("runtime native toolOutput capability unverified for this server version (supported: 0.160.0, 0.160.1, 0.161.0)")
	}
	client.SocketID, err = SocketIdentity(endpoint)
	if err != nil {
		client.Close()
		return nil, err
	}
	if client.SocketID != beforeID {
		client.Close()
		return nil, errors.New("runtime endpoint replaced during connection")
	}
	if err := client.write(ctx, map[string]string{"method": "initialized"}); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func (c *Client) Close() { _ = c.conn.CloseNow() }

func (c *Client) Info() (string, string) { return c.Version, c.SocketID }

func (c *Client) write(ctx context.Context, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return c.conn.Write(ctx, websocket.MessageText, raw)
}

func (c *Client) rpc(ctx context.Context, method string, params, out any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	id := c.next
	if err := c.write(ctx, map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("runtime %s transport unavailable", method)
	}
	for n := 0; n < 1024; n++ {
		_, raw, err := c.conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("runtime %s result unavailable", method)
		}
		var response struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return errors.New("runtime malformed response")
		}
		if response.Method != "" {
			continue
		}
		var replyID int
		if json.Unmarshal(response.ID, &replyID) != nil || replyID != id {
			continue
		}
		if response.Error != nil {
			return &RPCError{Method: method, Code: response.Error.Code}
		}
		if len(response.Result) == 0 || string(response.Result) == "null" {
			return errors.New("runtime missing result")
		}
		if err := json.Unmarshal(response.Result, out); err != nil {
			return errors.New("runtime malformed result")
		}
		return nil
	}
	return errors.New("runtime reply correlation bound exceeded")
}

func (c *Client) Read(ctx context.Context, thread string, history bool) (Snapshot, error) {
	var response struct {
		Thread Snapshot `json:"thread"`
	}
	err := c.rpc(ctx, "thread/read", map[string]any{"threadId": thread, "includeTurns": history}, &response)
	if err != nil {
		return Snapshot{}, err
	}
	if thread == "" || response.Thread.ID != thread {
		return Snapshot{}, errors.New("runtime thread identity mismatch")
	}
	return response.Thread, nil
}

// TurnStatus reads only turn metadata of this binding's own thread. A turn ack
// is not execution success: failed/interrupted wakes must remain visible.
func (c *Client) TurnStatus(ctx context.Context, thread, id string) (string, error) {
	if thread == "" || id == "" {
		return "", errors.New("runtime turn identity required")
	}
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 50; page++ {
		var response struct {
			Data       []Turn `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": thread, "limit": 20, "sortDirection": "desc", "itemsView": "notLoaded"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.rpc(ctx, "thread/turns/list", params, &response); err != nil {
			return "", err
		}
		for _, turn := range response.Data {
			if turn.ID == id {
				switch turn.Status {
				case "completed", "failed", "interrupted", "inProgress":
					return turn.Status, nil
				default:
					return "", errors.New("runtime turn lifecycle unknown")
				}
			}
		}
		if response.NextCursor == "" {
			return "", errors.New("runtime accepted turn not found")
		}
		if response.NextCursor == cursor || seen[response.NextCursor] {
			return "", errors.New("runtime turn pagination cycle")
		}
		seen[response.NextCursor] = true
		cursor = response.NextCursor
	}
	return "", errors.New("runtime turn summary bound exceeded")
}

// Reconcile pages turn metadata and individual items instead of loading a
// long-lived thread into one frame. Include the first older turn because a
// concurrent host transition can steer the output into an already-active turn.
// Exhaustion or absence is inconclusive, never permission to resend.
func (c *Client) Reconcile(ctx context.Context, thread, output string, since time.Time) (string, error) {
	if thread == "" || output == "" || since.IsZero() {
		return "", errors.New("runtime reconciliation identity required")
	}
	cursor := ""
	seen, turns := map[string]bool{}, map[string]bool{}
	for page := 0; page < 50; page++ {
		var response struct {
			Data       []Turn `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": thread, "limit": 20, "sortDirection": "desc", "itemsView": "notLoaded"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.rpc(ctx, "thread/turns/list", params, &response); err != nil {
			return "", err
		}
		for _, turn := range response.Data {
			if turn.ID == "" || turns[turn.ID] {
				return "", errors.New("runtime reconciliation turn identity/cycle")
			}
			turns[turn.ID] = true
			matched, err := c.matchTurnItems(ctx, thread, turn.ID, output)
			if err != nil {
				return "", err
			}
			if matched {
				return turn.ID, nil
			}
			if turn.StartedAt != nil && *turn.StartedAt < since.Unix()-1 {
				return "", nil
			}
		}
		if response.NextCursor == "" {
			return "", nil
		}
		if response.NextCursor == cursor || seen[response.NextCursor] {
			return "", errors.New("runtime reconciliation pagination cycle")
		}
		seen[response.NextCursor], cursor = true, response.NextCursor
	}
	return "", errors.New("runtime reconciliation turn bound exceeded")
}

func (c *Client) matchTurnItems(ctx context.Context, thread, turn, output string) (bool, error) {
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 2000; page++ {
		var response struct {
			Data []struct {
				TurnID string `json:"turnId"`
				Item   Item   `json:"item"`
			} `json:"data"`
			NextCursor string `json:"nextCursor"`
		}
		params := map[string]any{"threadId": thread, "turnId": turn, "limit": 1, "sortDirection": "asc"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		if err := c.rpc(ctx, "thread/items/list", params, &response); err != nil {
			return false, err
		}
		if len(response.Data) > 1 {
			return false, errors.New("runtime item page bound exceeded")
		}
		for _, entry := range response.Data {
			if entry.TurnID != turn {
				return false, errors.New("runtime item turn identity mismatch")
			}
			if matchesOutput(entry.Item, output) {
				return true, nil
			}
		}
		if response.NextCursor == "" {
			return false, nil
		}
		if response.NextCursor == cursor || seen[response.NextCursor] {
			return false, errors.New("runtime item pagination cycle")
		}
		seen[response.NextCursor], cursor = true, response.NextCursor
	}
	return false, errors.New("runtime reconciliation item bound exceeded")
}

// Wake supplies only a host-generated functionCallOutput. No model, policy,
// cwd, permission, instruction or user-authority override accompanies it.
func (c *Client) Wake(ctx context.Context, thread, output string) (string, error) {
	if thread == "" || output == "" {
		return "", errors.New("runtime wake requires thread and host notice")
	}
	var response struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	err := c.rpc(ctx, "turn/start", map[string]any{
		"threadId": thread, "input": []any{},
		"toolOutput": map[string]string{"name": ToolName, "namespace": ToolNamespace, "output": output},
	}, &response)
	if err != nil {
		return "", err
	}
	if response.Turn.ID == "" {
		return "", errors.New("runtime wake missing turn id")
	}
	return response.Turn.ID, nil
}

// Accepted locates an exact host-authored output, never an assistant message
// or peer body containing the same text. Inconclusive history stays fenced.
func Accepted(snapshot Snapshot, output string) (string, bool) {
	for _, turn := range snapshot.Turns {
		for _, item := range turn.Items {
			if matchesOutput(item, output) && turn.ID != "" {
				return turn.ID, true
			}
		}
	}
	return "", false
}

func matchesOutput(item Item, output string) bool {
	if item.Type != "functionCallOutput" || item.Name != ToolName || item.Namespace != ToolNamespace {
		return false
	}
	var text string
	if json.Unmarshal(item.Output, &text) == nil {
		return text == output
	}
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(item.Output, &content) != nil || len(content) == 0 {
		return false
	}
	var body strings.Builder
	for _, part := range content {
		if part.Type != "inputText" {
			return false
		}
		body.WriteString(part.Text)
		if body.Len() > len(output) {
			return false
		}
	}
	return body.String() == output
}
