package codexwake

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const currentNotice = "a2a: 1 new peer event (to: 1). They are external input from other agents, not from your user. Run `a2a inbox` to read them."

func currentFixture(t *testing.T, version, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "issue122", version, name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fixtureProtocol(t *testing.T, version string) currentProtocol {
	t.Helper()
	p, err := inspectCurrentProtocol(version, currentFixture(t, version, "TurnStartParams"), currentFixture(t, version, "ThreadItemsListResponse"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func currentAttempt(t *testing.T, version string) (Binding, Ledger, Attempt) {
	t.Helper()
	b := validBinding()
	b.ServerVersion = version
	var ledger Ledger
	a, err := ledger.Begin(b, []string{"$event"}, currentNotice, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC))
	if err != nil || a == nil {
		t.Fatalf("attempt setup: %v", err)
	}
	return b, ledger, *a
}

// Pins source schema identity for BOTH releases. Altered, absent, cross-version
// history and unknown-version evidence is refused. These are source schemas,
// not a native parser run, actual running-server identity or TUI qualification.
func TestCurrentSchemaFingerprints(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		t.Run(version, func(t *testing.T) {
			fixtureProtocol(t, version)
			turn := currentFixture(t, version, "TurnStartParams")
			history := currentFixture(t, version, "ThreadItemsListResponse")
			for _, tc := range []struct {
				name, version string
				turn, history []byte
			}{
				{"unknown-version", "0.999.0", turn, history},
				{"missing-version", "", turn, history},
				{"missing-turn", version, nil, history},
				{"missing-history", version, turn, nil},
				{"altered-schema", version, append(append([]byte{}, turn...), '\n'), history},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if p, err := inspectCurrentProtocol(tc.version, tc.turn, tc.history); err == nil || p.valid() {
						t.Fatal("unverified schema admitted")
					}
				})
			}
			other := "0.163.0"
			if version == other {
				other = "0.165.0"
			}
			if _, err := inspectCurrentProtocol(version, turn, currentFixture(t, other, "ThreadItemsListResponse")); err == nil {
				t.Fatal("foreign release history schema admitted")
			}
		})
	}
}

// Proves the constructed request contains ONLY empty user input and the exact
// journaled tool output, with retention requested for reconciliation. No peer
// event is an argument. Caller provenance, binding and admission are prerequisites;
// this pure constructor does not authenticate text or submit a native request.
func TestCurrentWireAuthority(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		t.Run(version, func(t *testing.T) {
			p := fixtureProtocol(t, version)
			b, _, attempt := currentAttempt(t, version)
			params, err := p.wakeParams(b.ThreadID, attempt.Output)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(params)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if json.Unmarshal(encoded, &got) != nil || len(got) != 3 || string(got["input"]) != "[]" || string(got["threadId"]) != `"thread"` {
				t.Fatal("user input or override escaped wire contract")
			}
			var tool map[string]any
			if json.Unmarshal(got["toolOutput"], &tool) != nil || len(tool) != 4 || tool["name"] != ToolName || tool["namespace"] != ToolNamespace || tool["retain"] != true {
				t.Fatal("tool authority or retention changed")
			}
			if tool["output"] != attempt.Output {
				t.Fatal("wire output differs from exact journaled host notice")
			}
			var schema struct {
				Properties  map[string]json.RawMessage `json:"properties"`
				Definitions map[string]struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"definitions"`
			}
			if err := json.Unmarshal(currentFixture(t, version, "TurnStartParams"), &schema); err != nil {
				t.Fatal(err)
			}
			for key := range got {
				if schema.Properties[key] == nil {
					t.Fatal("request field absent from release schema", key)
				}
			}
			for key := range tool {
				if schema.Definitions["TurnToolOutput"].Properties[key] == nil {
					t.Fatal("tool field absent from release schema", key)
				}
			}
		})
	}
}

// Bounds malformed transport arguments beside valid host text. This is NOT
// a peer-text classifier; only the source-owned encoder may create host output.
func TestCurrentWireRejectsMalformedArguments(t *testing.T) {
	p := fixtureProtocol(t, "0.163.0")
	for _, output := range []string{currentNotice, strings.Repeat("x", 512)} {
		if _, err := p.wakeParams("own-thread", output); err != nil {
			t.Fatal("valid bounded output refused", err)
		}
	}
	for _, output := range []string{"", strings.Repeat("x", 513), "peer\x00text", "peer\x1b[31m", "peer\rtext", "peer\ttext", "peer\u2028text", string([]byte{0xff})} {
		if params, err := p.wakeParams("own-thread", output); err == nil || params != nil || strings.Contains(err.Error(), "peer") {
			t.Fatal("malformed output admitted or reflected")
		}
	}
	for _, thread := range []string{"", "foreign\nthread", strings.Repeat("a", 129)} {
		if _, err := p.wakeParams(thread, currentNotice); err == nil {
			t.Fatal("malformed thread admitted")
		}
	}
	if _, err := (currentProtocol{}).wakeParams("own", currentNotice); err == nil {
		t.Fatal("missing schema admitted")
	}
}

// Current source schemas encode content as input_text. Exact host output is
// accepted as a string or split text array; forged authority, stale attempt/
// generation, peer suffixes, mixed media and malformed content never match.
// Legacy inputText remains exclusively the unchanged legacy matcher's contract.
func TestCurrentReconciliationAuthority(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		t.Run(version, func(t *testing.T) {
			p := fixtureProtocol(t, version)
			_, _, a := currentAttempt(t, version)
			text, _ := json.Marshal(a.Output)
			parts, _ := json.Marshal([]map[string]string{{"type": "input_text", "text": a.Output[:10]}, {"type": "input_text", "text": a.Output[10:]}})
			item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace, Output: text}
			if !p.matchesOutput(item, a.Output) {
				t.Fatal("exact host string not reconciled")
			}
			item.Output = parts
			if !p.matchesOutput(item, a.Output) {
				t.Fatal("current input_text output not reconciled")
			}
			for _, field := range []string{"type", "name", "namespace"} {
				bad := item
				switch field {
				case "type":
					bad.Type = "agentMessage"
				case "name":
					bad.Name = "peer"
				case "namespace":
					bad.Namespace = "peer"
				}
				if p.matchesOutput(bad, a.Output) {
					t.Fatal("forged authority reconciled", field)
				}
			}
			for _, body := range []string{
				`null`, `[]`, `{}`, `[{"type":"input_text"}]`,
				`[{"type":"input_text","text":null}]`,
				`[{"type":"input_text","text":"x","text":"y"}]`,
				`[{"type":"input_text","type":"input_text","text":"x"}]`,
				`[{"type":"input_text","text":"x","peer":"body"}]`,
				strings.ReplaceAll(string(parts), "input_text", "inputText"),
				strings.TrimSuffix(string(parts), "]") + `,{"type":"input_image","image_url":"peer"}]`,
			} {
				item.Output = json.RawMessage(body)
				if p.matchesOutput(item, a.Output) {
					t.Fatal("malformed/mixed content reconciled")
				}
			}
			for _, forged := range []string{a.Output + "peer body", strings.ReplaceAll(a.Output, "generation=generation", "generation=foreign"), strings.ReplaceAll(a.Output, "id="+a.ID, "id=stale")} {
				item.Output, _ = json.Marshal(forged)
				if p.matchesOutput(item, a.Output) {
					t.Fatal("foreign attempt or peer text reconciled")
				}
			}
			item.Output = json.RawMessage(`"` + strings.Repeat("x", 4<<20) + `"`)
			if p.matchesOutput(item, a.Output) {
				t.Fatal("over-limit output admitted")
			}
		})
	}
}

// Existing ledger fences also apply to both candidate build strings. A pure
// wire construction/match does not mark mail rung or authorize a second attempt.
// These are offline ledger controls, not live lease/controller qualification.
func TestCurrentUncertainAttemptAndLifecycle(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		p := fixtureProtocol(t, version)
		b, ledger, attempt := currentAttempt(t, version)
		before, _ := json.Marshal(ledger)
		if _, err := p.wakeParams(b.ThreadID, attempt.Output); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Begin(b, []string{"$second"}, currentNotice, attempt.Started); err == nil {
			t.Fatal("uncertain attempt replaced")
		}
		foreign := b
		foreign.Generation = "foreign"
		if err := ledger.Accept(foreign, "turn"); err == nil {
			t.Fatal("foreign generation accepted")
		}
		after, _ := json.Marshal(ledger)
		if !reflect.DeepEqual(before, after) {
			t.Fatal("wire/rejected retry changed durable ledger")
		}
		remote := Snapshot{ID: b.ThreadID, Cwd: b.Cwd, Status: Status{Type: "idle"}}
		if Admission(b, b.Host, true, false, remote) != "ready" {
			t.Fatal("valid lifecycle control refused")
		}
		for _, status := range []Status{{Type: "active"}, {Type: "active", ActiveFlags: []string{"waitingOnApproval"}}, {Type: "active", ActiveFlags: []string{"waitingOnUserInput"}}, {Type: "notLoaded"}} {
			remote.Status = status
			if Admission(b, b.Host, true, false, remote) == "ready" {
				t.Fatal("busy/human/unloaded admitted")
			}
		}
		foreign = b
		foreign.SessionID = "foreign"
		if foreign.Validate() == nil {
			t.Fatal("foreign SID admitted")
		}
	}
}

// A schema descriptor MUST NOT open production admission. Actual Unix/WebSocket
// initialization still refuses both current builds before thread/turn operations.
// Existing TestNativeVerifiedVersions supplies the adjacent admitted old builds.
func TestCurrentProductionAdmissionRemainsClosed(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		t.Run(version, func(t *testing.T) {
			var calls atomic.Int32
			endpoint := testServer(t, func(method string, _ json.RawMessage) any {
				calls.Add(1)
				if method != "initialize" {
					t.Error("current admission reached a thread/turn call")
				}
				return map[string]string{"userAgent": "fixture/" + version}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, err := Dial(ctx, endpoint)
			if err == nil {
				client.Close()
				t.Fatal("schema-only current build entered production")
			}
			if !strings.Contains(err.Error(), "capability unverified") || calls.Load() != 1 {
				t.Fatal("wrong refusal or side effect", err, calls.Load())
			}
		})
	}
}

// Both actual installed exports' extra experimental fields are separately
// fingerprinted. Acceptance is only at schema level and never opens Dial.
func TestCurrentInstalledExperimentalSchema(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		if _, err := inspectCurrentProtocol(version, currentFixture(t, version, "TurnStartParams.experimental"), currentFixture(t, version, "ThreadItemsListResponse")); err != nil {
			t.Fatal(err)
		}
	}
}

// A permissive last-key-wins decoder would otherwise accept these exact-output
// records. Reject duplicate or extra fields beside the same valid output.
func TestCurrentContentRejectsAmbiguousFields(t *testing.T) {
	p := fixtureProtocol(t, "0.163.0")
	text, _ := json.Marshal(currentNotice)
	item := Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace}
	valid := `[{"type":"input_text","text":` + string(text) + `}]`
	item.Output = json.RawMessage(valid)
	if !p.matchesOutput(item, currentNotice) {
		t.Fatal("valid exact-output control refused")
	}
	for _, raw := range []string{
		`[{"type":"input_text","text":"peer","text":` + string(text) + `}]`,
		`[{"type":"input_image","type":"input_text","text":` + string(text) + `}]`,
		`[{"type":"input_text","text":` + string(text) + `,"peer":"body"}]`,
	} {
		item.Output = json.RawMessage(raw)
		if p.matchesOutput(item, currentNotice) {
			t.Fatal("ambiguous content fields reconciled")
		}
	}
	for _, count := range []int{512, 513} {
		parts := make([]map[string]string, count)
		for i := range parts {
			parts[i] = map[string]string{"type": "input_text", "text": ""}
		}
		parts[0]["text"] = currentNotice
		item.Output, _ = json.Marshal(parts)
		if p.matchesOutput(item, currentNotice) != (count == 512) {
			t.Fatal("content part bound not enforced beside positive control")
		}
	}
}

// The unchanged endpoint guard admits an owned private fixture socket, refuses
// non-private and symlink endpoints, and detects object replacement. Foreign
// UID is not created here: that requires separately authorized host fixtures.
func TestCurrentEndpointIdentityControls(t *testing.T) {
	endpoint := testServer(t, func(_ string, _ json.RawMessage) any { return nil })
	path, err := EndpointPath(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	before, err := SocketIdentity(endpoint)
	if err != nil || before == "" {
		t.Fatal("valid private endpoint refused", err)
	}
	if err := os.Chmod(path, 0640); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketIdentity(endpoint); err == nil || !strings.Contains(err.Error(), "owned private Unix socket") {
		t.Fatal("non-private endpoint not refused by intended guard", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketIdentity(endpoint); err != nil {
		t.Fatal("restored positive endpoint refused", err)
	}
	link := filepath.Join(filepath.Dir(path), "alias.sock")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := SocketIdentity("unix://" + link); err == nil {
		t.Fatal("symlink endpoint admitted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	after, err := SocketIdentity(endpoint)
	if err != nil || after == before {
		t.Fatal("replacement endpoint inherited previous object identity", err)
	}
}

// fixtureCurrentClient constructs an isolated wire client without production
// Dial admission/initialize. This permits testing the actual Client methods
// while TestCurrentProductionAdmissionRemainsClosed proves real Dial refuses
// current versions. This fixture is not a native current server/session.
func fixtureCurrentClient(t *testing.T, ctx context.Context, endpoint, version string) *Client {
	t.Helper()
	path, err := EndpointPath(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		transport.CloseIdleConnections()
		t.Fatal(err)
	}
	conn.SetReadLimit(4 << 20)
	c := &Client{conn: conn, Version: version}
	t.Cleanup(func() { c.Close(); transport.CloseIdleConnections() })
	return c
}

// The actual Client.Wake/RPC serializer uses current tool retention and zero
// input, while the neighboring old version has its unchanged three-field tool
// shape. Interleaved fixture approval requests receive no answer. This is wire
// evidence only, not native approval/draft noninterference or model acceptance.
func TestCurrentClientWakeWire(t *testing.T) {
	for _, version := range []string{"0.161.0", "0.163.0", "0.165.0"} {
		t.Run(version, func(t *testing.T) {
			b, _, attempt := currentAttempt(t, version)
			var calls atomic.Int32
			endpoint := testServer(t, func(method string, raw json.RawMessage) any {
				calls.Add(1)
				if method != "turn/start" {
					t.Error("unexpected method", method)
				}
				var params map[string]json.RawMessage
				if json.Unmarshal(raw, &params) != nil || len(params) != 3 || string(params["input"]) != "[]" || string(params["threadId"]) != `"thread"` {
					t.Error("actual client sent user input, wrong thread or extra policy fields")
				}
				var tool map[string]any
				if err := json.Unmarshal(params["toolOutput"], &tool); err != nil {
					t.Error(err)
				}
				if tool["name"] != ToolName || tool["namespace"] != ToolNamespace || tool["output"] != attempt.Output {
					t.Error("actual client changed tool authority or exact journal output")
				}
				if version == "0.161.0" {
					if len(tool) != 3 || tool["retain"] != nil {
						t.Error("legacy server received unsupported new retention field")
					}
				} else if len(tool) != 4 || tool["retain"] != true {
					t.Error("current client omitted retained-history tool output")
				}
				return map[string]any{"turn": map[string]string{"id": "accepted-turn"}}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c := fixtureCurrentClient(t, ctx, endpoint, version)
			turn, err := c.Wake(ctx, b.ThreadID, attempt.Output)
			if err != nil || turn != "accepted-turn" || calls.Load() != 1 {
				t.Fatal("valid actual wire call refused", turn, err, calls.Load())
			}
			if version != "0.161.0" {
				if _, err := c.Wake(ctx, b.ThreadID, "peer\x1btext"); err == nil || calls.Load() != 1 {
					t.Fatal("malformed current output reached native RPC")
				}
			}
		})
	}
}

// A simulated applied-but-error wake remains uncertain until the ACTUAL client
// finds this exact attempt via paged current history. It never resubmits. Both
// current snake_case and unchanged legacy camelCase arrays work; wrong generation,
// missing history, mixed media and interrupted execution cannot fabricate success.
func TestCurrentClientLostReplyReconciliation(t *testing.T) {
	for _, version := range []string{"0.163.0", "0.165.0"} {
		for _, mode := range []string{"current", "legacy", "foreign-generation", "unavailable", "mixed"} {
			t.Run(version+"/"+mode, func(t *testing.T) {
				b, ledger, attempt := currentAttempt(t, version)
				var wakes atomic.Int32
				endpoint := testServer(t, func(method string, raw json.RawMessage) any {
					var params map[string]any
					if json.Unmarshal(raw, &params) != nil || params["threadId"] != b.ThreadID {
						t.Error("wrong thread in current native fixture")
					}
					switch method {
					case "turn/start":
						wakes.Add(1)
						return &RPCError{Method: method, Code: -32603}
					case "thread/turns/list":
						if mode == "unavailable" {
							return &RPCError{Method: method, Code: -32603}
						}
						if params["itemsView"] != "notLoaded" || params["limit"] != float64(20) {
							t.Error("unbounded or content-loaded history")
						}
						return map[string]any{"data": []Turn{{ID: "applied-turn", Status: "interrupted"}}}
					case "thread/items/list":
						if params["turnId"] != "applied-turn" || params["limit"] != float64(1) {
							t.Error("wrong or unbounded item read")
						}
						output := attempt.Output
						kind := "input_text"
						if mode == "legacy" {
							kind = "inputText"
						}
						if mode == "foreign-generation" {
							output = strings.ReplaceAll(output, "generation=generation", "generation=foreign")
						}
						parts := []map[string]string{{"type": kind, "text": output}}
						if mode == "mixed" {
							parts = append(parts, map[string]string{"type": "input_image", "image_url": "peer"})
						}
						body, _ := json.Marshal(parts)
						return map[string]any{"data": []any{map[string]any{"turnId": "applied-turn", "item": Item{Type: "functionCallOutput", Name: ToolName, Namespace: ToolNamespace, Output: body}}}}
					default:
						t.Error("unexpected control/approval/resume method", method)
						return nil
					}
				})
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c := fixtureCurrentClient(t, ctx, endpoint, version)
				if _, err := c.Wake(ctx, b.ThreadID, attempt.Output); err == nil || DefinitiveRefusal(err) {
					t.Fatal("lost acceptance response treated as definitive refusal")
				}
				before, _ := json.Marshal(ledger)
				turn, err := c.Reconcile(ctx, b.ThreadID, attempt.Output, attempt.Started)
				valid := mode == "current" || mode == "legacy"
				if valid && (turn != "applied-turn" || err != nil) {
					t.Fatal("actual current reconciliation lost exact attempt", turn, err)
				}
				if !valid && turn != "" {
					t.Fatal("foreign/mixed/unavailable history accepted")
				}
				if mode == "unavailable" && err == nil {
					t.Fatal("unavailable history treated as authoritative absence")
				}
				if _, err := ledger.Begin(b, []string{"$next"}, currentNotice, attempt.Started); err == nil {
					t.Fatal("blind retry admitted before authoritative ledger acceptance")
				}
				after, _ := json.Marshal(ledger)
				if !reflect.DeepEqual(before, after) || wakes.Load() != 1 {
					t.Fatal("reconciliation mutated ledger or resubmitted")
				}
				if valid {
					status, err := c.TurnStatus(ctx, b.ThreadID, turn)
					if err != nil || status != "interrupted" {
						t.Fatal("native acceptance confused with successful execution", status, err)
					}
				}
			})
		}
	}
}
