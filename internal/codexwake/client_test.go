package codexwake

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Proves the real Unix/WebSocket client sends an empty user input and a fixed
// tool namespace, handles notifications, and never changes runtime policies.
func TestNativeToolAuthorityWire(t *testing.T) {
	endpoint := testServer(t, func(method string, params json.RawMessage) any {
		switch method {
		case "initialize":
			return map[string]any{"userAgent": "fixture/0.160.1"}
		case "thread/read":
			return map[string]any{"thread": Snapshot{ID: "thread", Cwd: "/project", Status: Status{Type: "idle"}}}
		case "turn/start":
			var p map[string]json.RawMessage
			if err := json.Unmarshal(params, &p); err != nil {
				t.Fatal(err)
			}
			if len(p) != 3 || string(p["input"]) != "[]" || string(p["threadId"]) != `"thread"` {
				t.Errorf("unsafe turn params: %s", params)
			}
			var output struct{ Name, Namespace, Output string }
			if err := json.Unmarshal(p["toolOutput"], &output); err != nil {
				t.Fatal(err)
			}
			if output.Name != "doorbell" || output.Namespace != "a2a" || output.Output != "host metadata" {
				t.Errorf("tool output: %+v", output)
			}
			return map[string]any{"turn": map[string]string{"id": "turn-control"}}
		default:
			t.Errorf("unexpected runtime method %s", method)
			return nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got, err := c.Read(ctx, "thread", false)
	if err != nil || got.Status.Type != "idle" {
		t.Fatalf("read: %+v %v", got, err)
	}
	turn, err := c.Wake(ctx, "thread", "host metadata")
	if err != nil || turn != "turn-control" {
		t.Fatalf("wake: %s %v", turn, err)
	}
}

// Proves a missing or foreign thread response cannot pass the identity check;
// a valid own thread above is the nearby control. Endpoint URLs cannot carry
// remote addresses, credentials, queries or fragments into runtime admission.
func TestReadRejectsWrongThreadAndEndpoint(t *testing.T) {
	for _, endpoint := range []string{"ws://localhost/", "unix://host/tmp/socket", "unix:///tmp/socket?x=1", "unix:///tmp/socket#x", "unix://user@host/tmp/socket", "unix://relative"} {
		if _, err := Dial(context.Background(), endpoint); err == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	endpoint := testServer(t, func(method string, _ json.RawMessage) any {
		if method == "initialize" {
			return map[string]string{"userAgent": "fixture/0.160.1"}
		}
		return map[string]any{"thread": Snapshot{ID: "foreign", Status: Status{Type: "idle"}}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Read(ctx, "own", false); err == nil {
		t.Fatal("accepted foreign thread")
	}
}

// Proves explicitly verified server versions connect, while unsupported or
// absent capability/version evidence is refused before any thread/turn call.
func TestNativeVerifiedVersions(t *testing.T) {
	for _, version := range []string{"0.160.0", "0.160.1", "0.161.0", "0.999.0", ""} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			endpoint := testServer(t, func(method string, _ json.RawMessage) any {
				calls.Add(1)
				if method != "initialize" {
					t.Errorf("unsupported method %s", method)
				}
				return map[string]string{"userAgent": "fixture/" + version}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, endpoint)
			valid := version == "0.160.0" || version == "0.160.1" || version == "0.161.0"
			if valid {
				if err != nil {
					t.Fatal(err)
				}
				client.Close()
			} else if err == nil {
				client.Close()
				t.Fatal("unverified version admitted")
			}
			if calls.Load() != 1 {
				t.Fatal("capability refusal reached other methods")
			}
		})
	}
}

// Proves scoped, metadata-only turn reads use the verified pagination API,
// find the exact accepted turn, and reject missing or cyclic summaries.
func TestNativeOwnTurnStatusSummaries(t *testing.T) {
	for _, mode := range []string{"found", "missing", "cycle", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			endpoint := testServer(t, func(method string, params json.RawMessage) any {
				if method == "initialize" {
					return map[string]string{"userAgent": "fixture/0.161.0"}
				}
				if method != "thread/turns/list" {
					t.Errorf("unexpected method %s", method)
				}
				var p map[string]any
				if err := json.Unmarshal(params, &p); err != nil {
					t.Error(err)
				}
				if p["threadId"] != "own" || p["itemsView"] != "notLoaded" || p["sortDirection"] != "desc" || p["limit"] != float64(20) {
					t.Errorf("unsafe summary params %+v", p)
				}
				if mode == "cycle" {
					return map[string]any{"data": []Turn{}, "nextCursor": "loop"}
				}
				if mode == "missing" {
					return map[string]any{"data": []Turn{{ID: "foreign-turn", Status: "completed"}}}
				}
				if p["cursor"] == nil {
					return map[string]any{"data": []Turn{{ID: "other", Status: "completed"}}, "nextCursor": "next"}
				}
				status := "completed"
				if mode == "unknown" {
					status = "unknown"
				}
				return map[string]any{"data": []Turn{{ID: "accepted", Status: status}}}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, endpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			status, err := client.TurnStatus(ctx, "own", "accepted")
			if mode == "found" {
				if err != nil || status != "completed" {
					t.Fatal(status, err)
				}
			} else if err == nil {
				t.Fatal("inconclusive summary admitted", mode)
			}
		})
	}
}

// A bounded fixture exercises the same transport as the managed daemon. It
// emits an unrelated notification and a string-ID approval request before
// each reply, proving neither interrupts correlation nor gets an answer.
func testServer(t *testing.T, reply func(string, json.RawMessage) any) string {
	t.Helper()
	// Keep the Unix path below macOS's small sockaddr_un limit.
	dir, err := os.MkdirTemp("", "a2a-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "rpc.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		for {
			_, raw, err := conn.Read(r.Context())
			if err != nil {
				return
			}
			var req struct {
				ID     *int            `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(raw, &req); err != nil {
				t.Error(err)
				return
			}
			if req.ID == nil {
				continue
			}
			out := reply(req.Method, req.Params)
			request, _ := json.Marshal(map[string]any{"id": "foreign-approval", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": "other"}})
			if err := conn.Write(r.Context(), websocket.MessageText, request); err != nil {
				return
			}
			note, _ := json.Marshal(map[string]any{"method": "thread/status/changed", "params": map[string]string{"threadId": "other"}})
			if err := conn.Write(r.Context(), websocket.MessageText, note); err != nil {
				return
			}
			response, _ := json.Marshal(map[string]any{"id": *req.ID, "result": out})
			if err := conn.Write(r.Context(), websocket.MessageText, response); err != nil {
				return
			}
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); listener.Close() })
	return "unix://" + path
}
