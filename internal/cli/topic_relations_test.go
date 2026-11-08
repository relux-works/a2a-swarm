package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const relationsRoom = "!archive:fixture"
const relationsRoot = "$archive-root"

type relationsResponse struct {
	body   string
	status int
}
type topicRelationsFixture struct {
	t        *testing.T
	paths    config.Paths
	server   *httptest.Server
	mu       sync.Mutex
	pages    map[string]relationsResponse
	requests []string
	root     matrix.Event
	timeline []matrix.Event // Optional room-read control; nil retains the original root-only fixture.
	repeat   bool
}

func relationEvent(id, root, parent string, ts int64) matrix.Event {
	content, _ := json.Marshal(matrix.MessageContent{MsgType: "m.text", Body: id,
		RelatesTo: &matrix.RelatesTo{RelType: "m.thread", EventID: root, InReplyTo: &matrix.InReplyTo{EventID: parent}},
		A2A:       json.RawMessage(fmt.Sprintf(`{"v":1,"re":%q}`, parent))})
	return matrix.Event{EventID: id, Type: "m.room.message", Sender: "@reader:fixture", RoomID: relationsRoom, OriginServerTS: ts, Content: content}
}

func relationsJSON(events []matrix.Event, next string) string {
	if events == nil {
		events = []matrix.Event{}
	}
	b, _ := json.Marshal(matrix.Page{Chunk: events, NextBatch: next})
	return string(b)
}

// Only selected read routes exist. Empty pages with live cursors model a
// filtered relations response, not a claim about any production homeserver.
func newTopicRelationsFixture(t *testing.T, old bool) *topicRelationsFixture {
	t.Helper()
	f := &topicRelationsFixture{t: t, paths: config.Paths{Base: t.TempDir()}, pages: map[string]relationsResponse{}}
	ts := int64(200000)
	if old {
		ts = 1
	}
	f.root = matrix.Event{EventID: relationsRoot, Type: "m.room.message", Sender: "@reader:fixture", RoomID: relationsRoom, OriginServerTS: ts, Content: json.RawMessage(`{"msgtype":"m.text","body":"[topic] archive: Archive","ai.relux.a2a":{"v":1,"type":"topic","slug":"archive","title":"Archive"}}`)}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		if r.Method != "GET" {
			t.Error("topic read attempted a mutation")
			writeErr(w, 403, "M_FORBIDDEN", "read only")
			return
		}
		switch r.URL.Path {
		case "/_matrix/client/v3/rooms/" + relationsRoom + "/event/" + relationsRoot:
			writeJSON(w, 200, f.root)
		case "/_matrix/client/v1/rooms/" + relationsRoom + "/threads":
			writeJSON(w, 200, matrix.Page{Chunk: []matrix.Event{f.root}})
		case "/_matrix/client/v3/rooms/" + relationsRoom + "/messages":
			if f.timeline != nil {
				writeJSON(w, 200, matrix.Page{Chunk: f.timeline})
			} else {
				writeJSON(w, 200, matrix.Page{Chunk: []matrix.Event{f.root}})
			}
		case "/_matrix/client/v1/rooms/" + relationsRoom + "/relations/" + relationsRoot + "/m.thread":
			if r.URL.Query().Get("dir") != "b" || r.URL.Query().Get("limit") != "100" {
				t.Error("relations direction/limit changed")
			}
			from := r.URL.Query().Get("from")
			response, ok := f.pages[from]
			if f.repeat {
				response = relationsResponse{relationsJSON(nil, "page-"+strconv.Itoa(len(f.requests))), 200}
				ok = true
			}
			if !ok {
				t.Errorf("unexpected relations cursor %q", from)
				writeErr(w, 400, "M_UNKNOWN", "unexpected cursor")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(response.status)
			io.WriteString(w, response.body)
		default:
			t.Errorf("unexpected read route %s", r.URL.Path)
			writeErr(w, 403, "M_FORBIDDEN", "route outside topic read")
		}
	}))
	t.Cleanup(f.server.Close)
	if err := os.WriteFile(f.paths.EnvFile(), []byte("A2A_MATRIX_URL="+f.server.URL+"\nA2A_MATRIX_SERVER_NAME=fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.paths.AgentFile("reader")), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(config.Credentials{UserID: "@reader:fixture", AccessToken: "fixture-only", Homeserver: f.server.URL})
	if err := os.WriteFile(f.paths.AgentFile("reader"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := config.SaveState(f.paths, "reader", config.State{NextBatch: "preserved-cursor", Threads: []string{"$followed"}, Muted: []string{"$muted"}}); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *topicRelationsFixture) read(topic string, last int, asJSON bool) result {
	f.t.Helper()
	args := []string{"read", relationsRoom, "--topic", topic, "--last", strconv.Itoa(last)}
	if asJSON {
		args = append(args, "--json")
	}
	return f.readArgs(args...)
}

func (f *topicRelationsFixture) readArgs(args ...string) result {
	f.t.Helper()
	before := memberFiles(f.t, f.paths.Base)
	var out, errb bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errb, Stdin: strings.NewReader(""), HTTP: f.server.Client(), Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return f.paths.Base
		}
		return ""
	}}
	args = append([]string{"--as", "reader"}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code := a.Run(ctx, args)
	if !reflect.DeepEqual(before, memberFiles(f.t, f.paths.Base)) {
		f.t.Fatal("topic read changed local state")
	}
	return result{code: code, stdout: out.String(), stderr: errb.String()}
}

func relationsRecords(t *testing.T, raw string) []EventRecord {
	t.Helper()
	var out []EventRecord
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		var r EventRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func sameRelationIDs(got []EventRecord, want []string) bool {
	ids := make([]string, len(got))
	for i, r := range got {
		ids[i] = r.EventID
	}
	return reflect.DeepEqual(ids, want)
}

func TestTopicRelationsPagination(t *testing.T) {
	for _, old := range []bool{false, true} {
		for _, topic := range []string{"archive", relationsRoot} {
			for _, gap := range []bool{false, true} {
				t.Run(fmt.Sprintf("old=%v/%s/gap=%v", old, topic, gap), func(t *testing.T) {
					f := newTopicRelationsFixture(t, old)
					request := relationEvent("$request", relationsRoot, relationsRoot, 300000)
					first := relationEvent("$answer-one", relationsRoot, "$request", 300001)
					second := relationEvent("$answer-two", relationsRoot, "$request", 300002)
					start := ""
					if gap {
						f.pages[""] = relationsResponse{relationsJSON(nil, "after-gap"), 200}
						start = "after-gap"
					}
					f.pages[start] = relationsResponse{relationsJSON([]matrix.Event{second}, "older"), 200}
					f.pages["older"] = relationsResponse{relationsJSON([]matrix.Event{first, request}, ""), 200}
					r := f.read(topic, 10, true).ok(t)
					records := relationsRecords(t, r.stdout)
					if !sameRelationIDs(records, []string{relationsRoot, "$request", "$answer-one", "$answer-two"}) {
						t.Fatalf("expected replies missing or unrelated reply accepted: %s", r.stdout)
					}
					for i, record := range records[1:] {
						parent := relationsRoot
						if i > 0 {
							parent = "$request"
						}
						if record.Root != relationsRoot || record.Re != parent {
							t.Fatalf("thread/native parent changed: %+v", record)
						}
					}
					// The same checker must reject both an omitted reply and a foreign substitute.
					omitted := append([]EventRecord(nil), records[:3]...)
					if sameRelationIDs(omitted, []string{relationsRoot, "$request", "$answer-one", "$answer-two"}) {
						t.Fatal("checker accepted omitted expected reply")
					}
					replaced := append([]EventRecord(nil), records...)
					replaced[3].EventID = "$unrelated"
					if sameRelationIDs(replaced, []string{relationsRoot, "$request", "$answer-one", "$answer-two"}) {
						t.Fatal("checker accepted unrelated substitute")
					}
				})
			}
		}
	}
}

func TestTopicRelationsLongLivedLimits(t *testing.T) {
	for _, topic := range []string{"archive", relationsRoot} {
		for _, limit := range []int{1, 12, 40, 100} {
			t.Run(fmt.Sprintf("%s/%d", topic, limit), func(t *testing.T) {
				f := newTopicRelationsFixture(t, true)
				var newest []matrix.Event
				for i := 40; i >= 1; i-- {
					newest = append(newest, relationEvent(fmt.Sprintf("$reply-%02d", i), relationsRoot, "$request", 300000+int64(i)))
				}
				f.pages[""] = relationsResponse{relationsJSON(newest[:7], "gap"), 200}
				f.pages["gap"] = relationsResponse{relationsJSON(nil, "rest"), 200}
				f.pages["rest"] = relationsResponse{relationsJSON(newest[7:], ""), 200}
				r := f.read(topic, limit, true).ok(t)
				n := min(limit, 40)
				want := []string{relationsRoot}
				for i := 41 - n; i <= 40; i++ {
					want = append(want, fmt.Sprintf("$reply-%02d", i))
				}
				if !sameRelationIDs(relationsRecords(t, r.stdout), want) {
					t.Fatalf("order/limit mismatch: %s", r.stdout)
				}
				text := f.read(topic, limit, false).ok(t).stdout
				if strings.Count(text, "thread="+relationsRoot) != n || !strings.Contains(text, "re=$request") {
					t.Fatalf("text lost replies/parents: %s", text)
				}
			})
		}
	}
}

func TestTopicRelationsRefusals(t *testing.T) {
	valid := relationEvent("$answer", relationsRoot, "$request", 300000)
	foreign := valid
	foreign.RoomID = "!foreign:fixture"
	unrelated := relationEvent("$foreign", "$other-root", "$other-root", 300000)
	malformed := valid
	malformed.Content = json.RawMessage(`{"msgtype":"m.text","body":"bad","m.relates_to":{"rel_type":"m.thread"}}`)
	cases := map[string]relationsResponse{
		"null-cursor":       {`{"chunk":[],"next_batch":null}`, 200},
		"empty-cursor":      {`{"chunk":[],"next_batch":""}`, 200},
		"cursor-case-alias": {`{"chunk":[],"NEXT_BATCH":"older"}`, 200},
		"duplicate-cursor":  {`{"chunk":[],"next_batch":"one","next_batch":"two"}`, 200},
		"duplicate-chunk":   {`{"chunk":[],"chunk":[]}`, 200},
		"trailing-value":    {`{"chunk":[]} {}`, 200},

		"missing-chunk": {`{}`, 200}, "null-chunk": {`{"chunk":null}`, 200}, "malformed-chunk": {`{"chunk":{}}`, 200},
		"foreign-room": {relationsJSON([]matrix.Event{foreign}, ""), 200}, "unrelated-root": {relationsJSON([]matrix.Event{unrelated}, ""), 200},
		"malformed-relation": {relationsJSON([]matrix.Event{malformed}, ""), 200},
		"refused":            {`{"errcode":"M_FORBIDDEN","error":"refused"}`, 403},
		"interrupted":        {`{"chunk":[`, 200}, "malformed-cursor": {`{"chunk":[],"next_batch":123}`, 200},
	}
	for name, response := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTopicRelationsFixture(t, true)
			f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{valid}, "older"), 200}
			f.pages["older"] = response
			r := f.read(relationsRoot, 10, true)
			if r.code == ExitOK || r.stdout != "" {
				t.Fatalf("partial/malformed thread presented as success: %+v", r)
			}
		})
	}
	for _, kind := range []string{"cycle", "bound"} {
		t.Run(kind, func(t *testing.T) {
			f := newTopicRelationsFixture(t, true)
			if kind == "cycle" {
				f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{valid}, "a"), 200}
				f.pages["a"] = relationsResponse{relationsJSON(nil, "a"), 200}
			} else {
				f.repeat = true
			}
			r := f.read(relationsRoot, 10, true)
			if r.code == ExitOK || r.stdout != "" || !strings.Contains(r.stderr, "incomplete topic replies") {
				t.Fatalf("incomplete pagination concealed: %+v", r)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.requests) > 23 {
				t.Fatalf("pagination exceeded request bound: %d", len(f.requests))
			}
		})
	}
}

func TestTopicRelationsCompleteEmpty(t *testing.T) {
	f := newTopicRelationsFixture(t, true)
	f.pages[""] = relationsResponse{relationsJSON(nil, ""), 200}
	r := f.read(relationsRoot, 10, true).ok(t)
	if !sameRelationIDs(relationsRecords(t, r.stdout), []string{relationsRoot}) {
		t.Fatalf("complete empty changed: %s", r.stdout)
	}
}

func TestTopicRelationsEndpointScopeAndRedaction(t *testing.T) {
	f := newTopicRelationsFixture(t, true)
	ordinary := relationEvent("$ordinary", relationsRoot, "$request", 300001)
	ordinary.RoomID = "" // Relations ClientEventWithoutRoomID uses endpoint context.
	redacted := matrix.Event{EventID: "$redacted", Type: "m.room.message", RoomID: relationsRoom, Sender: "@reader:fixture", Content: json.RawMessage(`{}`), Unsigned: json.RawMessage(`{"redacted_because":{}}`)}
	f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{redacted, ordinary}, "older"), 200}
	// Overlapping pages must not spend the limit twice on the same event.
	request := relationEvent("$request", relationsRoot, relationsRoot, 300000)
	f.pages["older"] = relationsResponse{relationsJSON([]matrix.Event{ordinary, request}, ""), 200}
	r := f.read(relationsRoot, 3, true).ok(t)
	records := relationsRecords(t, r.stdout)
	if !sameRelationIDs(records, []string{relationsRoot, "$request", "$ordinary", "$redacted"}) || !records[3].Redacted {
		t.Fatalf("endpoint scope/redaction/overlap changed: %s", r.stdout)
	}
}

func TestTopicRelationsEventScanBound(t *testing.T) {
	f := newTopicRelationsFixture(t, true)
	events := make([]matrix.Event, maxScanEvents+1)
	for i := range events {
		events[i] = relationEvent(fmt.Sprintf("$reply-%d", i), relationsRoot, "$request", int64(i))
	}
	f.pages[""] = relationsResponse{relationsJSON(events, "more"), 200}
	r := f.read(relationsRoot, 10, true)
	if r.code == ExitOK || r.stdout != "" || !strings.Contains(r.stderr, "event scan limit") {
		t.Fatalf("oversize event scan concealed: %+v", r)
	}
}
