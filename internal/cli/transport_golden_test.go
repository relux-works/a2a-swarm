package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	mt "github.com/relux-works/a2a-swarm/internal/matrixtransport"
	tr "github.com/relux-works/a2a-swarm/transport"
	"github.com/relux-works/a2a-swarm/transport/conformancetest"
)

type transportFaults struct {
	mu              sync.Mutex
	afterSubmit     bool
	partial, refuse bool
	parts           map[string]*transportParts
	lossBefore      int
	room            string
	calls           atomic.Int64
	logs            transportLog
}
type transportLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *transportLog) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(b)
}
func (l *transportLog) lines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Split(l.b.String(), "\n")
}

// transportParts records server-side fanout delivery attempts, not client reads
// or HTTP retry counts. Matrix submits one transaction; the injected server fault
// leaves one recipient pending and reconciles only that part on the duplicate txn.
type transportParts struct {
	event      string
	position   int
	recipients []tr.PrincipalID
	calls      map[tr.PrincipalID]int
}

// reuseTransportServer injects faults around the existing fakeHS. Events,
// transaction deduplication, authentication and wire decoding remain in fakeHS.
func reuseTransportServer(hs *fakeHS) *transportFaults {
	f := &transportFaults{parts: map[string]*transportParts{}}
	base := hs.srv.Config.Handler
	hs.srv.Config.ErrorLog = log.New(&f.logs, "", 0)
	hs.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		isSync := strings.HasSuffix(r.URL.Path, "/sync")
		isHistory := strings.HasSuffix(r.URL.Path, "/messages")
		if isSync || isHistory {
			token, prefix := r.URL.Query().Get("since"), "s"
			if isHistory {
				token, prefix = r.URL.Query().Get("from"), "m"
			}
			if token != "" {
				n, e := strconv.Atoi(strings.TrimPrefix(token, prefix))
				if !strings.HasPrefix(token, prefix) || e != nil || n < 0 {
					writeErr(w, 400, "M_UNKNOWN_POS", "received-SECRET-marker")
					return
				}
			}
		}
		// The existing fake server exposes membership through full /state. Reuse
		// its authentication and state snapshot to supply the native /members shape.
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/members") {
			clone := r.Clone(r.Context())
			copyURL := *r.URL
			clone.URL = &copyURL
			room := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/_matrix/client/v3/rooms/"), "/members")
			clone.URL.Path = strings.TrimSuffix(r.URL.Path, "/members") + "/state"
			capture := httptest.NewRecorder()
			base.ServeHTTP(capture, clone)
			if capture.Code < 200 || capture.Code >= 300 {
				copyTransportResponse(w, capture)
				return
			}
			var state []matrix.Event
			if json.Unmarshal(capture.Body.Bytes(), &state) != nil {
				writeErr(w, 500, "M_UNKNOWN", "invalid state fixture")
				return
			}
			members := []matrix.Event{}
			for _, event := range state {
				if event.Type == "m.room.member" {
					event.RoomID = room
					members = append(members, event)
				}
			}
			writeJSON(w, 200, map[string]any{"chunk": members})
			return
		}
		if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/send/") {
			raw, e := io.ReadAll(r.Body)
			if e != nil {
				writeErr(w, 400, "M_BAD_JSON", "invalid body")
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			var content struct {
				Mentions struct {
					Users []tr.PrincipalID `json:"user_ids"`
				} `json:"m.mentions"`
			}
			if json.Unmarshal(raw, &content) != nil {
				writeErr(w, 400, "M_BAD_JSON", "invalid body")
				return
			}
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			hs.mu.Lock()
			sender := hs.tokens[token]
			hs.mu.Unlock()
			f.mu.Lock()
			fail, partial, refuse := f.afterSubmit, f.partial, f.refuse
			f.afterSubmit, f.partial, f.refuse = false, false, false
			f.mu.Unlock()
			if refuse {
				writeErr(w, 400, "M_FORBIDDEN", "received-SECRET-marker")
				return
			}
			capture := httptest.NewRecorder()
			base.ServeHTTP(capture, r)
			if capture.Code >= 200 && capture.Code < 300 {
				var receipt struct {
					ID string `json:"event_id"`
				}
				_ = json.Unmarshal(capture.Body.Bytes(), &receipt)
				hs.mu.Lock()
				position := 0
				for i, e := range hs.events {
					if e.EventID == receipt.ID {
						position = i
					}
				}
				hs.mu.Unlock()
				txn := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
				f.mu.Lock()
				p := f.parts[txn]
				if p == nil {
					p = &transportParts{event: receipt.ID, position: position, recipients: append([]tr.PrincipalID{tr.PrincipalID(sender)}, content.Mentions.Users...), calls: map[tr.PrincipalID]int{}}
					f.parts[txn] = p
				}
				for i, id := range p.recipients {
					if partial && i >= 2 {
						break
					}
					if p.calls[id] == 0 {
						p.calls[id]++
					}
				}
				f.mu.Unlock()
				if fail || partial {
					writeErr(w, 500, "M_UNKNOWN", "received-SECRET-marker")
					return
				}
			}
			copyTransportResponse(w, capture)
			return
		}
		if isSync {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			hs.mu.Lock()
			user := tr.PrincipalID(hs.tokens[token])
			positions := map[string]int{}
			for i, e := range hs.events {
				positions[e.EventID] = i
			}
			hs.mu.Unlock()
			f.mu.Lock()
			hold := -1
			for _, p := range f.parts {
				for _, id := range p.recipients {
					if id == user && p.calls[id] == 0 && (hold < 0 || p.position < hold) {
						hold = p.position
					}
				}
			}
			floor, room := f.lossBefore, f.room
			f.mu.Unlock()
			capture := httptest.NewRecorder()
			base.ServeHTTP(capture, r)
			if capture.Code < 200 || capture.Code >= 300 {
				copyTransportResponse(w, capture)
				return
			}
			var response map[string]any
			if json.Unmarshal(capture.Body.Bytes(), &response) != nil {
				copyTransportResponse(w, capture)
				return
			}
			if hold >= 0 {
				response["next_batch"] = "s" + strconv.Itoa(hold)
			}
			rooms, _ := response["rooms"].(map[string]any)
			join, _ := rooms["join"].(map[string]any)
			for _, value := range join {
				j, _ := value.(map[string]any)
				timeline, _ := j["timeline"].(map[string]any)
				events, _ := timeline["events"].([]any)
				kept := []any{}
				for _, raw := range events {
					event, _ := raw.(map[string]any)
					id, _ := event["event_id"].(string)
					pos := positions[id]
					if (hold < 0 || pos < hold) && pos >= floor {
						kept = append(kept, raw)
					}
				}
				timeline["events"] = kept
			}
			since, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Query().Get("since"), "s"))
			if floor > since {
				if rooms == nil {
					rooms = map[string]any{}
					response["rooms"] = rooms
				}
				if join == nil {
					join = map[string]any{}
					rooms["join"] = join
				}
				value, _ := join[room].(map[string]any)
				if value == nil {
					value = map[string]any{}
					join[room] = value
				}
				timeline, _ := value["timeline"].(map[string]any)
				if timeline == nil {
					timeline = map[string]any{"events": []any{}}
					value["timeline"] = timeline
				}
				timeline["limited"] = true
			}
			writeJSON(w, 200, response)
			return
		}
		base.ServeHTTP(w, r)
	})
	return f
}
func copyTransportResponse(w http.ResponseWriter, capture *httptest.ResponseRecorder) {
	for key, values := range capture.Header() {
		w.Header()[key] = values
	}
	w.WriteHeader(capture.Code)
	_, _ = w.Write(capture.Body.Bytes())
}
func (f *transportFaults) plant()        { f.mu.Lock(); f.afterSubmit = true; f.mu.Unlock() }
func (f *transportFaults) plantPartial() { f.mu.Lock(); f.partial = true; f.mu.Unlock() }
func (f *transportFaults) plantRefusal() { f.mu.Lock(); f.refuse = true; f.mu.Unlock() }
func (f *transportFaults) partCalls(key string) map[tr.PrincipalID]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[tr.PrincipalID]int{}
	if p := f.parts[mt.TxnID(key)]; p != nil {
		for id, n := range p.calls {
			out[id] = n
		}
	}
	return out
}
func transportRoot(t *testing.T) string {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	return root
}
func adapterWorld(t *testing.T) (*fakeHS, *transportFaults, *mt.Transport, *mt.Transport, tr.ConversationID, string, string) {
	t.Helper()
	hs := newFakeHS(t)
	faults := reuseTransportServer(hs)
	alice, at := hs.addUser("transport-a")
	bob, bt := hs.addUser("transport-b")
	room := hs.addRoom("transport", alice, bob)
	// Older CLI fixtures use a colonless room ID; this fixture claims canonical T1 addressing.
	canonical := room + ":" + testServer
	hs.mu.Lock()
	r := hs.rooms[room]
	delete(hs.rooms, room)
	r.id = canonical
	hs.rooms[canonical] = r
	hs.aliases[r.alias] = canonical
	hs.mu.Unlock()
	root := transportRoot(t)
	state, sentinel := filepath.Join(root, "state"), filepath.Join(root, "sentinel")
	if e := os.Mkdir(state, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Mkdir(sentinel, 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(sentinel, "keep"), []byte("untouched"), 0644); e != nil {
		t.Fatal(e)
	}
	a, e := mt.New(matrix.New(hs.srv.URL, at), tr.Config{StateDir: filepath.Join(state, "a")})
	if e != nil {
		t.Fatal(e)
	}
	b, e := mt.New(matrix.New(hs.srv.URL, bt), tr.Config{StateDir: filepath.Join(state, "b")})
	if e != nil {
		t.Fatal(e)
	}
	faults.mu.Lock()
	faults.room = canonical
	faults.mu.Unlock()
	return hs, faults, a, b, tr.ConversationID(canonical), state, sentinel
}
func adapterHarness() conformancetest.Harness {
	return conformancetest.Harness{New: func(t *testing.T) *conformancetest.Fixture {
		hs, f, a, b, room, state, sentinel := adapterWorld(t)
		carol, ct := hs.addUser("transport-c")
		hs.mu.Lock()
		hs.rooms[string(room)].members[carol] = "join"
		hs.mu.Unlock()
		c, e := mt.New(matrix.New(hs.srv.URL, ct), tr.Config{StateDir: filepath.Join(state, "c")})
		if e != nil {
			t.Fatal(e)
		}
		return &conformancetest.Fixture{A: a, B: b, C: c, Extensions: map[string]json.RawMessage{"formatted_body": json.RawMessage(`"valid extension"`)}, Conversation: room, StateDir: state, SentinelDir: sentinel,
			RestartB: func() tr.Transport {
				hs.mu.Lock()
				token := ""
				for k, id := range hs.tokens {
					if id == "@transport-b:"+testServer {
						token = k
					}
				}
				hs.mu.Unlock()
				next, e := mt.New(matrix.New(hs.srv.URL, token), tr.Config{StateDir: filepath.Join(state, "b")})
				if e != nil {
					t.Fatal(e)
				}
				return next
			},
			FailAfterSubmit: f.plant, PartialAck: f.plantPartial, PartWireCalls: f.partCalls,
			DefiniteRefusal:  f.plantRefusal,
			PreSubmitFailure: func() context.Context { ctx, cancel := context.WithCancel(context.Background()); cancel(); return ctx },
			InduceLoss: func() {
				hs.mu.Lock()
				n := len(hs.events)
				hs.mu.Unlock()
				f.mu.Lock()
				f.lossBefore = n
				f.mu.Unlock()
			},
			UnverifiableSender: func(claim tr.PrincipalID, marker string) tr.EventID {
				// Matrix authenticates sender at the server: an untrusted payload claims B,
				// while malformed server sender provenance makes the entire event unverifiable.
				return tr.EventID(hs.inject(string(room), "forged-sender", map[string]any{"body": marker, "msgtype": "m.text", ContentKey: map[string]any{"from": claim}}))
			},
			BackdatedPublish: func(at time.Time) tr.EventID {
				id := hs.inject(string(room), "@transport-a:"+testServer, map[string]any{"body": "backdated", "msgtype": "m.text"})
				hs.mu.Lock()
				for i := range hs.events {
					if hs.events[i].EventID == id {
						hs.events[i].OriginServerTS = at.UnixMilli()
					}
				}
				hs.mu.Unlock()
				return tr.EventID(id)
			},
			ForeignCursor: func() tr.Cursor { return tr.Cursor("foreign-server-token") },
			OversizeReceived: func(marker string) error {
				id := hs.inject(string(room), "@transport-a:"+testServer, map[string]any{"body": marker + strings.Repeat("x", mt.MaxContentBytes), "msgtype": "m.text"})
				_, e := b.Event(context.Background(), room, tr.EventID(id))
				return e
			},
			WireCalls: func() int { return int(f.calls.Load()) }, MessageAtSize: func(n int) tr.Message {
				m := tr.Message{}
				b, e := mt.Content(m)
				if e != nil {
					t.Fatal(e)
				}
				m.Text = strings.Repeat("x", n-len(b))
				return m
			}, Logs: f.logs.lines,
		}
	}}
}

// Proves byte-equal content against captured golden JSON AND the actual CLI helpers,
// pins all sync filters, and runs the adapter through the existing fake homeserver.
// This is a test-only import: no production CLI behavior or imports change.
func TestTransportGolden(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			h      *Header
			to     []tr.PrincipalID
			golden string
		}{
			{"plain_text", nil, nil, `{"body":"plain","msgtype":"m.text"}`},
			{"plain_post", &Header{V: 1, Type: "message", From: "sender", At: "2026-11-09T00:00:00Z", Kind: "notice"}, nil, `{"ai.relux.a2a":{"v":1,"type":"message","from":"sender","at":"2026-11-09T00:00:00Z","kind":"notice"},"body":"plain","m.mentions":{},"msgtype":"m.text"}`},
			{"reply", &Header{V: 1, Type: "message", From: "sender", At: "2026-11-09T00:00:00Z", Re: "$target", Kind: "answer"}, nil, `{"ai.relux.a2a":{"v":1,"type":"message","from":"sender","at":"2026-11-09T00:00:00Z","re":"$target","kind":"answer"},"body":"plain","m.mentions":{},"m.relates_to":{"m.in_reply_to":{"event_id":"$target"}},"msgtype":"m.text"}`},
			{"thread_mentions", &Header{V: 1, Type: "message", From: "sender", At: "2026-11-09T00:00:00Z", Topic: "$root", Re: "$target", To: []string{"recipient"}, Kind: "answer"}, []tr.PrincipalID{"@recipient:test.local"}, `{"ai.relux.a2a":{"v":1,"type":"message","topic":"$root","from":"sender","to":["recipient"],"at":"2026-11-09T00:00:00Z","re":"$target","kind":"answer"},"body":"plain","m.mentions":{"user_ids":["@recipient:test.local"]},"m.relates_to":{"rel_type":"m.thread","event_id":"$root","m.in_reply_to":{"event_id":"$target"}},"msgtype":"m.text"}`},
			{"thread_fallback", &Header{V: 1, Type: "message", From: "sender", At: "2026-11-09T00:00:00Z", Topic: "$root", Kind: "notice"}, nil, `{"ai.relux.a2a":{"v":1,"type":"message","topic":"$root","from":"sender","at":"2026-11-09T00:00:00Z","kind":"notice"},"body":"plain","m.mentions":{},"m.relates_to":{"rel_type":"m.thread","event_id":"$root","is_falling_back":true,"m.in_reply_to":{"event_id":"$root"}},"msgtype":"m.text"}`},
			{"kind_to_topic", &Header{V: 1, Type: "message", From: "sender", At: "2026-11-09T00:00:00Z", Topic: "$root", To: []string{"recipient"}, Kind: "question", Subject: "repo@main"}, []tr.PrincipalID{"@recipient:test.local"}, `{"ai.relux.a2a":{"v":1,"type":"message","topic":"$root","from":"sender","to":["recipient"],"at":"2026-11-09T00:00:00Z","kind":"question","subject":"repo@main"},"body":"plain","m.mentions":{"user_ids":["@recipient:test.local"]},"m.relates_to":{"rel_type":"m.thread","event_id":"$root","is_falling_back":true,"m.in_reply_to":{"event_id":"$root"}},"msgtype":"m.text"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				m := tr.Message{Text: "plain", To: tc.to}
				var fields map[string]any
				if tc.h != nil {
					h := *tc.h
					raw, e := json.Marshal(h)
					if e != nil {
						t.Fatal(e)
					}
					m.Extensions = map[string]json.RawMessage{ContentKey: raw}
					m.Kind = h.Kind
					m.Re = tr.EventID(h.Re)
					m.Thread = tr.EventID(h.Topic)
					var mentions []string
					for _, p := range tc.to {
						mentions = append(mentions, string(p))
					}
					if h.Re != "" || h.Topic != "" {
						fields = threadReply(h, mentions)
					} else {
						fields = map[string]any{ContentKey: h, "m.mentions": matrix.Mentions{UserIDs: mentions}}
					}
				}
				content, e := textContent("plain", fields)
				if e != nil {
					t.Fatal(e)
				}
				cliBytes, e := json.Marshal(content)
				if e != nil {
					t.Fatal(e)
				}
				got, e := mt.Content(m)
				if e != nil || string(cliBytes) != tc.golden || !bytes.Equal(got, cliBytes) {
					t.Fatalf("golden mismatch\nCLI: %s\nadapter: %s\nexpected: %s\nerror: %v", cliBytes, got, tc.golden, e)
				}
			})
		}
	})
	t.Run("filters", func(t *testing.T) {
		if mt.SyncFilter != syncFilter || mt.InitialFilter != initialFilter || mt.FirstDeliveryFilter != firstDeliveryFilter {
			t.Fatal("Matrix sync filter bytes drifted")
		}
	})
	t.Run("conformance", func(t *testing.T) { conformancetest.Run(t, adapterHarness()) })
	t.Run("oversize_control", func(t *testing.T) {
		_, f, a, _, room, _, _ := adapterWorld(t)
		m := tr.Message{}
		b, e := mt.Content(m)
		if e != nil {
			t.Fatal(e)
		}
		m.Text = strings.Repeat("x", mt.MaxContentBytes-len(b)+1)
		before := f.calls.Load()
		r, e := a.Send(context.Background(), tr.SendRequest{Conversation: room, IdempotencyKey: "oversize", Message: m})
		if tr.Code(e) != tr.ContentTooLarge || f.calls.Load() != before || r.Outcome == tr.ACK {
			t.Fatalf("adapter oversize: expected content_too_large and zero HTTP requests; error=%v calls=%d", e, f.calls.Load()-before)
		}
	})
	t.Run("reads_extensions", func(t *testing.T) {
		hs, _, a, b, room, _, _ := adapterWorld(t)
		c := context.Background()
		r, e := a.Send(c, tr.SendRequest{Conversation: room, IdempotencyKey: "read", Message: tr.Message{Text: "valid"}})
		if e != nil || r.Outcome != tr.ACK {
			t.Fatal("send failed")
		}
		ev, e := b.Event(c, room, r.Event)
		if e != nil || ev.Message.Text != "valid" {
			t.Fatal("event wrapper failed")
		}
		if _, e = b.Addressing().Conversation(c, "#transport:"+testServer); e != nil {
			t.Fatal(e)
		}
		if _, e = b.ConversationCaps(c, room); e != nil {
			t.Fatal(e)
		}
		if _, e = a.PutState(c, room, "test.state", "", json.RawMessage(`{"value":"kept"}`)); e != nil {
			t.Fatal(e)
		}
		raw, e := b.GetState(c, room, "test.state", "")
		if e != nil || !bytes.Contains(raw, []byte("kept")) {
			t.Fatal("state wrapper failed")
		}
		result, e := a.Redact(c, room, r.Event, "redact")
		if e != nil || result.Outcome != tr.ACK {
			t.Fatal("redact wrapper failed")
		}
		hs.mu.Lock()
		hs.syncLimit = 1
		hs.mu.Unlock()
		hs.inject(string(room), "@transport-a:"+testServer, map[string]any{"body": "one"})
		hs.inject(string(room), "@transport-a:"+testServer, map[string]any{"body": "two"})
		batch, e := b.Sync(c, tr.Cursor("s0"), tr.SyncOptions{})
		if e != nil || len(batch.Gaps) != 1 || batch.Gaps[0].Reason != tr.TimelineLimited {
			t.Fatal("limited timeline not reported")
		}
		var output bytes.Buffer
		_, e = b.Download(c, tr.NewMediaRef("received-SECRET-marker"), io.Writer(&output), 1024)
		if tr.Code(e) != tr.InvalidReference {
			t.Fatal(fmt.Sprint(e))
		}
	})
}
