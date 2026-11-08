package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Model post --re replies as native m.thread + m.in_reply_to and explicit a2a
// headers. The old/young synthetic roots do not reproduce any private server.
func nestedTopicFixture(t *testing.T, old bool) *topicRelationsFixture {
	t.Helper()
	f := newTopicRelationsFixture(t, old)
	request := relationEvent("$request", relationsRoot, relationsRoot, 300000)
	first := relationEvent("$answer-one", relationsRoot, "$request", 300001)
	second := relationEvent("$answer-two", relationsRoot, "$request", 300002)
	for i, ev := range []*matrix.Event{&request, &first, &second} {
		m := ev.Message()
		kind := "answer"
		if i == 0 {
			kind = "request"
		}
		m.A2A = json.RawMessage(fmt.Sprintf(`{"v":1,"kind":%q,"re":%q}`, kind, m.RelatesTo.InReplyTo.EventID))
		var err error
		ev.Content, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Native target and thread root are independent fixture facts, not inferred
	// from the display header's re field.
	for i, ev := range []matrix.Event{request, first, second} {
		parent := relationsRoot
		if i > 0 {
			parent = "$request"
		}
		m := ev.Message()
		if ev.ThreadRoot() != relationsRoot || m.RelatesTo.InReplyTo.EventID != parent {
			t.Fatal("invalid native fixture relation")
		}
	}
	f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{second}, "older"), 200}
	f.pages["older"] = relationsResponse{relationsJSON([]matrix.Event{first, request}, ""), 200}
	f.timeline = []matrix.Event{second, first, request, f.root}
	return f
}

// Check the real HTTP request sequence, including next_batch consumption and
// absence of a broad timeline fallback when selecting the explicit root ID.
func nestedRead(t *testing.T, f *topicRelationsFixture, args []string, wantCursors []string) result {
	t.Helper()
	f.mu.Lock()
	start := len(f.requests)
	f.mu.Unlock()
	r := f.readArgs(args...)
	f.mu.Lock()
	requests := append([]string(nil), f.requests[start:]...)
	f.mu.Unlock()
	var cursors []string
	for _, request := range requests {
		u, err := url.Parse(strings.TrimPrefix(request, "GET "))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(u.Path, "/relations/") {
			if u.Path != "/_matrix/client/v1/rooms/"+relationsRoom+"/relations/"+relationsRoot+"/m.thread" {
				t.Fatalf("wrong selected relations endpoint: %s", request)
			}
			q := u.Query()
			if q.Get("dir") != "b" || q.Get("limit") != "100" {
				t.Fatalf("wrong query: %s", request)
			}
			if q.Get("from") == "" {
				if q.Has("from") {
					t.Fatal("empty from parameter sent")
				}
			} else if len(q["from"]) != 1 {
				t.Fatal("ambiguous from parameter")
			}
			cursors = append(cursors, q.Get("from"))
		}
		if len(args) > 3 && args[2] == "--topic" && args[3] == relationsRoot && strings.HasSuffix(u.Path, "/messages") {
			t.Fatal("root read used room-history fallback")
		}
	}
	t.Logf("argv=%q exit=%d requests=%q stdout=%s stderr=%s", args, r.code, requests, r.stdout, r.stderr)
	if !reflect.DeepEqual(cursors, wantCursors) {
		t.Fatalf("relations pagination: got %q want %q", cursors, wantCursors)
	}
	return r
}

// On unchanged production, both topic selectors must return root/request/two
// nested answers exactly, next to the independent bounded room timeline read.
// This proves the supplied exhausted fixture's shape, not server completeness.
func TestTopicReadNestedRepliesQualification(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(fmt.Sprintf("old=%v", old), func(t *testing.T) {
			f := nestedTopicFixture(t, old)
			want := []string{relationsRoot, "$request", "$answer-one", "$answer-two"}
			for _, args := range [][]string{
				{"read", relationsRoom, "--topic", "archive", "--last", "10", "--json"},
				{"read", relationsRoom, "--topic", relationsRoot, "--last", "10", "--json"},
				{"read", relationsRoom, "--last", "12", "--json"},
			} {
				var cursors []string
				if args[2] == "--topic" {
					cursors = []string{"", "older"}
				}
				r := nestedRead(t, f, args, cursors).ok(t)
				records := relationsRecords(t, r.stdout)
				if !sameRelationIDs(records, want) {
					t.Fatalf("nested answer order/identity mismatch: got %s want %q", r.stdout, want)
				}
				for i, record := range records[1:] {
					parent := relationsRoot
					kind := "request"
					if i > 0 {
						parent = "$request"
						kind = "answer"
					}
					if record.Root != relationsRoot || record.Re != parent || record.Kind != kind {
						t.Fatalf("header/thread projection changed: %+v", record)
					}
				}
			}
		})
	}
}

// A native reply target without an a2a header must not manufacture display re;
// root membership remains valid and both nested answers are still returned.
func TestTopicReadNestedReplyProvenance(t *testing.T) {
	f := nestedTopicFixture(t, true)
	first := relationEvent("$answer-one", relationsRoot, "$request", 300001)
	second := relationEvent("$answer-two", relationsRoot, "$request", 300002)
	for _, ev := range []*matrix.Event{&first, &second} {
		m := ev.Message()
		m.A2A = nil
		var err error
		ev.Content, err = json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := relationEvent("$request", relationsRoot, relationsRoot, 300000)
	f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{second}, "older"), 200}
	f.pages["older"] = relationsResponse{relationsJSON([]matrix.Event{first, request}, ""), 200}
	records := relationsRecords(t, nestedRead(t, f, []string{"read", relationsRoom, "--topic", relationsRoot, "--last", "10", "--json"}, []string{"", "older"}).ok(t).stdout)
	if !sameRelationIDs(records, []string{relationsRoot, "$request", "$answer-one", "$answer-two"}) {
		t.Fatal("native-only answers omitted")
	}
	for _, r := range records[2:] {
		if r.Re != "" || r.Root != relationsRoot {
			t.Fatalf("native parent invented as header re: %+v", r)
		}
	}
}

// Foreign-thread contradictions refuse atomically, non-message relations are
// skipped, and redaction metadata suppresses even a retained synthetic body.
// The valid nested qualification and each unmodified request/answer are controls.
func TestTopicReadNestedReplyNeighbors(t *testing.T) {
	for _, kind := range []string{"valid", "foreign-thread", "non-message", "redacted"} {
		t.Run(kind, func(t *testing.T) {
			f := nestedTopicFixture(t, true)
			ev := relationEvent("$answer-two", relationsRoot, "$request", 300002)
			switch kind {
			case "foreign-thread":
				ev = relationEvent("$foreign", "$other-root", "$other-request", 300002)
			case "non-message":
				ev.Type = "m.reaction"
			case "redacted":
				ev.Unsigned = json.RawMessage(`{"redacted_because":{}}`)
			}
			f.pages[""] = relationsResponse{relationsJSON([]matrix.Event{ev}, "older"), 200}
			cursors := []string{"", "older"}
			if kind == "foreign-thread" {
				cursors = []string{""}
			}
			r := nestedRead(t, f, []string{"read", relationsRoom, "--topic", relationsRoot, "--last", "10", "--json"}, cursors)
			if kind == "foreign-thread" {
				if r.code != ExitError || r.stdout != "" || !strings.Contains(r.stderr, "incomplete topic replies") {
					t.Fatalf("foreign relation not refused atomically: %+v", r)
				}
				return
			}
			r.ok(t)
			records := relationsRecords(t, r.stdout)
			want := []string{relationsRoot, "$request", "$answer-one", "$answer-two"}
			if kind == "non-message" {
				want = want[:3]
			}
			if !sameRelationIDs(records, want) {
				t.Fatalf("neighbor order/filter mismatch: %s", r.stdout)
			}
			if kind == "redacted" && (!records[3].Redacted || records[3].Body != "[redacted]" || strings.Contains(r.stdout, `"body":"$answer-two"`)) {
				t.Fatalf("redacted body resurrected: %s", r.stdout)
			}
		})
	}
}

// The last-two bound spans both relation pages and excludes the older request,
// while keeping the root plus the two newest answers in chronological order.
func TestTopicReadNestedReplyLimit(t *testing.T) {
	for _, topic := range []string{"archive", relationsRoot} {
		t.Run(topic, func(t *testing.T) {
			f := nestedTopicFixture(t, true)
			r := nestedRead(t, f, []string{"read", relationsRoom, "--topic", topic, "--last", "2", "--json"}, []string{"", "older"}).ok(t)
			if !sameRelationIDs(relationsRecords(t, r.stdout), []string{relationsRoot, "$answer-one", "$answer-two"}) {
				t.Fatalf("last-two bound across pages: %s", r.stdout)
			}
		})
	}
}
