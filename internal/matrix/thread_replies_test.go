package matrix

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestThreadRepliesNativeRelations(t *testing.T) {
	content := json.RawMessage(`{"msgtype":"m.text","body":"answer","m.relates_to":{"rel_type":"m.thread","event_id":"$root","m.in_reply_to":{"event_id":"$request"}}}`)
	expected := Event{EventID: "$answer", Type: "m.room.message", Content: content}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/_matrix/client/v1/rooms/!room:fixture/relations/$root/m.thread" || r.URL.Query().Get("from") != "before" || r.URL.Query().Get("dir") != "b" || r.URL.Query().Get("limit") != "12" {
			t.Error("relations endpoint or query changed")
		}
		json.NewEncoder(w).Encode(Page{Chunk: []Event{expected}, NextBatch: "older"})
	}))
	defer server.Close()
	c := New(server.URL, "")
	p, err := c.ThreadReplies(context.Background(), "!room:fixture", "$root", "before", "b", 12)
	if err != nil || len(p.Chunk) != 1 || !reflect.DeepEqual(p.Chunk[0].Message(), expected.Message()) || p.NextBatch != "older" {
		t.Fatalf("native relation lost: %+v %v", p, err)
	}
}

func TestThreadRepliesPageEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"complete-empty", `{"chunk":[]}`, true},
		{"empty-continuing", `{"chunk":[],"next_batch":"older"}`, true},
		{"future-field", `{"chunk":[],"future":{}}`, true},
		{"peer-content-compatibility", `{"chunk":[{"content":{"body":"first","body":"last\ud800"}}]}`, true},
		{"missing", `{}`, false}, {"null", `{"chunk":null}`, false},
		{"partial", `{"chunk":[`, false}, {"trailing", `{"chunk":[]} {}`, false},
		{"null-cursor", `{"chunk":[],"next_batch":null}`, false},
		{"cursor-case", `{"chunk":[],"Next_Batch":"more"}`, false},
		{"duplicate", `{"chunk":[],"next_batch":"a","next_batch":"b"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeRelationsPage([]byte(tc.raw))
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
		})
	}
}

func TestThreadRepliesIncompleteBody(t *testing.T) {
	for _, kind := range []string{"short", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if kind == "short" {
					w.Header().Set("Content-Length", "100")
					fmt.Fprint(w, `{"chunk":[]}`)
					return
				}
				fmt.Fprint(w, `{"chunk":[],"future":"`)
				block := make([]byte, 4096)
				for i := range block {
					block[i] = 'a'
				}
				for i := 0; i < maxRelationsPageBytes/len(block)+1; i++ {
					if _, err := w.Write(block); err != nil {
						return
					}
				}
				fmt.Fprint(w, `"}`)
			}))
			defer server.Close()
			p, err := New(server.URL, "").ThreadReplies(context.Background(), "!room:fixture", "$root", "", "b", 100)
			if err == nil || p.Chunk != nil {
				t.Fatalf("incomplete body accepted: %+v %v", p, err)
			}
		})
	}
}
