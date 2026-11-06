package matrix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRedactionWireEncoding(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PUT" || r.URL.EscapedPath() != "/_matrix/client/v3/rooms/%21room%2Fpart/redact/$event%2Fpart/txn.id-42" {
			t.Errorf("unexpected method or escaped path: %s %s", r.Method, r.URL.EscapedPath())
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Error("caller token missing")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body["reason"] != "π \"quote\" \\ <tag>" || len(body) != 1 {
			t.Error("reason JSON encoding changed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"event_id":"$redaction"}`))
	}))
	defer server.Close()
	c := New(server.URL, "fixture-token")
	id, err := c.RedactEventTxn(context.Background(), "!room/part", "$event/part", "txn.id-42", "π \"quote\" \\ <tag>")
	if err != nil || id != "$redaction" || calls != 1 {
		t.Fatalf("result=%q error=%v attempts=%d", id, err, calls)
	}
}

func TestRedactionFormatsAndSafeDecode(t *testing.T) {
	for _, raw := range []string{
		`{"type":"m.room.redaction","redacts":"$target","content":{"reason":"removed-fixture"}}`,
		`{"type":"m.room.redaction","content":{"redacts":"$target","reason":"removed-fixture"}}`,
		`{"type":"m.room.redaction","content":{"redacts":"$target"},"unsigned":{"redacted_because":{}}}`,
	} {
		var ev Event
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.RedactionTarget() != "$target" || ev.WithoutContent().RedactionTarget() != "$target" {
			t.Fatal("redaction target lost across room formats or redaction-of-redaction")
		}
	}
	var ev Event
	json.Unmarshal([]byte(`{"type":"m.room.message","content":{"body":"removed-fixture","ai.relux.a2a":{"attachment":{"mxc":"removed-fixture"}}},"unsigned":{"redacted_because":{"content":{"reason":"removed-fixture"}},"m.relations":{"private":"removed-fixture"}}}`), &ev)
	if ev.Message().Body != "[redacted]" || len(ev.Message().A2A) != 0 {
		t.Fatal("unsigned redaction content decoded")
	}
	raw, _ := json.Marshal(ev.WithoutContent())
	if strings.Contains(string(raw), "removed-fixture") {
		t.Fatal("raw content survived tombstone")
	}
}

func TestRedactionReplacementTarget(t *testing.T) {
	for _, tc := range []struct{ typ, content, want string }{
		{"m.room.message", `{"m.relates_to":{"rel_type":"m.replace","event_id":"$gone"}}`, "$gone"},
		{"m.room.message", `{"m.relates_to":{"rel_type":"m.thread","event_id":"$gone"}}`, ""},
		{"m.room.message", `{"m.relates_to":{"m.in_reply_to":{"event_id":"$gone"}}}`, ""},
		{"m.room.message", `{"m.relates_to":{"event_id":"$gone"},"body":"copy"}`, ""},
		{"m.reaction", `{"m.relates_to":{"rel_type":"m.replace","event_id":"$gone"}}`, ""},
		{"m.room.message", `{"m.relates_to":{"rel_type":"m.replace","event_id":42}}`, ""},
	} {
		e := Event{Type: tc.typ, Content: json.RawMessage(tc.content)}
		if got := e.ReplacementTarget(); got != tc.want {
			t.Fatalf("type=%s content=%s target=%q want=%q", tc.typ, tc.content, got, tc.want)
		}
	}
}
