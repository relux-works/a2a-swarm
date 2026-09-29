package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func decodeRecords(t *testing.T, out string) []EventRecord {
	t.Helper()
	var recs []EventRecord
	for _, l := range tuitestkit.LinesFromStr(strings.TrimRight(out, "\n")) {
		var r EventRecord
		dec := json.NewDecoder(strings.NewReader(l))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&r); err != nil {
			t.Fatalf("line is not one EventRecord: %q: %v", l, err)
		}
		recs = append(recs, r)
	}
	return recs
}

// Proves read --event --json returns exactly that event with every header
// field and the full body without the header lines; the text form of --event
// prints the same single event.
func TestReadEventJSON(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "Alpha title", "--body", "root text").ok(t).stdout)
	e.as("bob", "post", "proj", "--topic", "alpha", "decoy").ok(t)
	long := strings.Repeat("x", 500) + " tail"
	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "question", "--to", "alice,carol",
		"--re", root, "--reply-by", "2026-10-01T00:00:00Z", "--trunk", "abcdef1", "line one\n"+long).ok(t).stdout)

	recs := decodeRecords(t, e.as("alice", "read", "proj", "--event", id, "--json").ok(t).stdout)
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1", len(recs))
	}
	r := recs[0]
	want := EventRecord{Type: "message", Room: "#proj", RoomID: "!projRoomID", EventID: id, Sender: "bob", At: r.At,
		Kind: "question", Topic: "alpha", Root: root, To: []string{"alice", "carol"}, Re: root,
		ReplyBy: "2026-10-01T00:00:00Z", Trunk: "abcdef1", Body: "line one\n" + long}
	if !reflect.DeepEqual(r, want) {
		t.Fatalf("record\n got %+v\nwant %+v", r, want)
	}
	if r.At == "" {
		t.Fatal("at missing")
	}

	txt := e.as("alice", "read", "proj", "--event", id).ok(t).stdout
	tuitestkit.ContainsStr(t, txt, id+" kind=question")
	tuitestkit.NotContainsStr(t, txt, "decoy")

	rootRec := decodeRecords(t, e.as("alice", "read", "proj", "--event", root, "--json").ok(t).stdout)[0]
	if rootRec.Type != "topic" || rootRec.Topic != "alpha" || rootRec.Title != "Alpha title" || rootRec.Root != "" {
		t.Fatalf("topic root record = %+v", rootRec)
	}
}

// Proves read --json emits one record per event in the same order and
// selection as the text form, with control characters stripped from text.
func TestReadJSONList(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	for _, s := range []string{"one", "two"} {
		e.as("bob", "post", "proj", "--topic", "alpha", s).ok(t)
	}
	e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "evil \x1b[31mred\x07"})
	recs := decodeRecords(t, e.as("alice", "read", "proj", "--last", "3", "--json").ok(t).stdout)
	if len(recs) != 3 || recs[0].Body != "one" || recs[1].Body != "two" {
		t.Fatalf("records = %+v", recs)
	}
	if strings.ContainsAny(recs[2].Body, "\x1b\x07") || !strings.Contains(recs[2].Body, "evil") {
		t.Fatalf("unsanitized body %q", recs[2].Body)
	}
	if recs[2].Kind != "" || recs[2].Topic != "" {
		t.Fatalf("plain message got header fields: %+v", recs[2])
	}
}

// Proves --event rejects bad combinations and unknown or non-message events,
// and a non-member cannot read an event by id.
func TestReadEventNegative(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	e.as("alice", "read", "proj", "--event", root, "--topic", "alpha").failsWith(t, ExitUsage, "--event cannot be combined")
	e.as("alice", "read", "proj", "--event", root, "--last", "5").failsWith(t, ExitUsage, "--event cannot be combined")
	e.as("alice", "read", "proj", "--event", "ev001").failsWith(t, ExitUsage, "--event must be an event id")
	e.as("alice", "read", "proj", "--event", "$nosuch").failsWith(t, ExitError, "not found")
	e.as("carol", "read", "proj", "--event", root).failsWith(t, ExitError, "M_FORBIDDEN")
	e.as("alice", "read", "proj", "--event", root).ok(t) // positive control
}

// Proves wait --json reports each relevance type as one record with the
// fields a bridge needs, keeps the excerpt bounded, and that timeout and
// invites have JSON forms too. The line format stays the default.
func TestWaitJSON(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	r := e.as("alice", "wait", "--timeout", "10ms", "--json").ok(t)
	if r.stdout != "{\"type\":\"timeout\"}\n" {
		t.Fatalf("timeout = %q", r.stdout)
	}

	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "request", "--to", "alice", strings.Repeat("y", 400)).ok(t).stdout)
	e.hs.inject(room, "@bob:test.local", msg("unrelated", nil))
	recs := decodeRecords(t, e.as("alice", "wait", "--timeout", "2s", "--json").ok(t).stdout)
	if len(recs) != 1 {
		t.Fatalf("records = %+v", recs)
	}
	got := recs[0]
	if got.Type != "to" || got.Room != "#proj" || got.RoomID != room || got.EventID != id || got.Sender != "bob" ||
		got.Kind != "request" || got.Topic != "alpha" || got.Root != root || !reflect.DeepEqual(got.To, []string{"alice"}) || got.Body != "" {
		t.Fatalf("record = %+v", got)
	}
	if n := len([]rune(got.Excerpt)); n == 0 || n > SnippetMax {
		t.Fatalf("excerpt length %d", n)
	}

	e.as("alice", "invite", "proj", "carol").ok(t)
	inv := decodeRecords(t, e.as("carol", "wait", "--timeout", "2s", "--json").ok(t).stdout)
	if len(inv) != 1 || !reflect.DeepEqual(inv[0], EventRecord{Type: "invite", Room: "#proj", RoomID: "!projRoomID", Sender: "alice"}) {
		t.Fatalf("invite = %+v", inv)
	}

	e.hs.inject(room, "@bob:test.local", msg("@alice:test.local plain", nil))
	line := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	if !strings.HasPrefix(line, "mention #proj ") {
		t.Fatalf("default output is not the line format: %q", line)
	}
}
