package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func decodeContent(t *testing.T, ev matrix.Event) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(ev.Content, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Proves topic new sends a root whose body starts "[topic] <slug>: <title>",
// carries the structured header, prints the event id, and records the thread
// in the agent's wait state.
func TestTopicNew(t *testing.T) {
	e, _ := world(t)
	e.stdin = "line one\nline two\n"
	r := e.as("alice", "topic", "new", "proj", "api-design", "Design", "the", "API", "--body", "-").ok(t)
	id := strings.TrimSpace(r.stdout)
	ev, ok := e.hs.eventByID(id)
	if !ok {
		t.Fatalf("event %q not sent", id)
	}
	m := ev.Message()
	if m.Body != "[topic] api-design: Design the API\n\nline one\nline two" {
		t.Fatalf("body = %q", m.Body)
	}
	h, ok := decodeHeader(m.A2A)
	if !ok || h.Type != "topic" || h.Slug != "api-design" || h.From != "alice" || h.Title != "Design the API" {
		t.Fatalf("header = %+v", h)
	}
	st, _ := config.LoadState(e.paths(), "alice")
	if !st.HasThread(id) {
		t.Fatal("topic root not recorded in state")
	}
}

// Proves topic new refuses bad slugs, empty titles, empty bodies, and duplicate
// slugs; none of these send a message.
func TestTopicNewNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "dup", "First").ok(t)
	sends := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
	e.as("alice", "topic", "new", "proj", "dup", "Again").failsWith(t, ExitError, `topic "dup" already exists`)
	e.as("alice", "topic", "new", "proj", "Bad_Slug", "T").failsWith(t, ExitUsage, "invalid slug")
	e.as("alice", "topic", "new", "proj", "ok", "  ").failsWith(t, ExitUsage, "empty topic title")
	e.as("alice", "topic", "new", "proj", "ok", "T", "--body", "   ").failsWith(t, ExitUsage, "empty message body refused")
	e.as("alice", "topic", "new", "proj", "ok").failsWith(t, ExitUsage, "usage")
	e.as("alice", "topic", "old", "proj", "ok", "T").failsWith(t, ExitUsage, "usage")
	e.as("carol", "topic", "new", "proj", "ok", "T").failsWith(t, ExitError, "M_FORBIDDEN")
	// carol is refused by the server while listing topics, before any send.
	if got := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"); got != sends {
		t.Fatalf("sends = %d, want %d", got, sends)
	}
}

// Proves topics lists every topic root with slug, id, and reply count, oldest
// first, including topics with no replies yet (which /threads omits).
func TestTopicsList(t *testing.T) {
	e, _ := world(t)
	a := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	b := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "beta", "B").ok(t).stdout)
	e.as("bob", "post", "proj", "--topic", "alpha", "one").ok(t)
	e.as("alice", "post", "proj", "--topic", "alpha", "two").ok(t)
	e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "plain chatter"})
	r := e.as("alice", "topics", "proj").ok(t)
	lines := tuitestkit.LinesFromStr(strings.TrimSpace(r.stdout))
	if len(lines) != 2 {
		t.Fatalf("lines = %q", lines)
	}
	tuitestkit.ContainsStr(t, lines[0], "alpha\t"+a+"\t2\tA")
	tuitestkit.ContainsStr(t, lines[1], "beta\t"+b+"\t0\tB")
}

// Proves the fallback path: when /threads is unsupported, topics are found and
// replies are counted from the /messages scan.
func TestTopicsFallback(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	e.as("bob", "post", "proj", "--topic", "alpha", "one").ok(t)
	e.hs.mu.Lock()
	e.hs.threadsAPI = false
	e.hs.mu.Unlock()
	r := e.as("alice", "topics", "proj").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "alpha\t")
	tuitestkit.ContainsStr(t, r.stdout, "\t1\tA")
	tuitestkit.AssertCalled(t, &e.hs.MockCallRecorder, "GET /_matrix/client/v1/rooms/{room}/threads")
	// Posting by slug still works through the fallback.
	e.as("alice", "post", "proj", "--topic", "alpha", "two").ok(t)
}

// Proves post sends a thread reply whose body has the protocol header lines and
// whose content carries the same header as structured JSON, plus m.mentions for
// named addressees.
func TestPost(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	r := e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "proposal", "--to", "alice,@carol",
		"--trunk", "abc1234", "--reply-by", "2026-10-01T12:00:00+02:00", "Let's", "do", "it").ok(t)
	ev, _ := e.hs.eventByID(strings.TrimSpace(r.stdout))
	m := ev.Message()
	for _, line := range []string{"- from: bob\n", "- to: alice, carol\n", "- re: -\n", "- kind: proposal\n", "- reply-by: 2026-10-01T10:00:00Z\n", "- trunk: abc1234\n", "\n\nLet's do it"} {
		tuitestkit.ContainsStr(t, m.Body, line)
	}
	h, _ := decodeHeader(m.A2A)
	if h.Kind != "proposal" || h.Topic != root || h.Slug != "alpha" || strings.Join(h.To, ",") != "alice,carol" || h.Trunk != "abc1234" {
		t.Fatalf("header = %+v", h)
	}
	if m.RelatesTo == nil || m.RelatesTo.RelType != "m.thread" || m.RelatesTo.EventID != root || !m.RelatesTo.IsFallingBack {
		t.Fatalf("relates_to = %+v", m.RelatesTo)
	}
	if m.Mentions == nil || strings.Join(m.Mentions.UserIDs, ",") != "@alice:test.local,@carol:test.local" {
		t.Fatalf("mentions = %+v", m.Mentions)
	}
	st, _ := config.LoadState(e.paths(), "bob")
	if !st.HasThread(root) {
		t.Fatal("thread not recorded for poster")
	}
}

// Proves --re makes a real reply (in_reply_to the given event, not a fallback),
// the default kind is notice, the default addressee is all, and a question gets
// the protocol default reply-by of 12 h.
func TestPostDefaultsAndRe(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	q := strings.TrimSpace(e.as("alice", "post", "proj", "--topic", root, "--kind", "question", "why?").ok(t).stdout)
	ev, _ := e.hs.eventByID(q)
	h, _ := decodeHeader(ev.Message().A2A)
	rb, err := time.Parse(time.RFC3339, h.ReplyBy)
	if err != nil || rb.Sub(time.Now()) < 11*time.Hour || rb.Sub(time.Now()) > 13*time.Hour {
		t.Fatalf("reply_by = %q", h.ReplyBy)
	}
	a := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--re", q, "because").ok(t).stdout)
	ev, _ = e.hs.eventByID(a)
	m := ev.Message()
	h, _ = decodeHeader(m.A2A)
	if h.Kind != "notice" || strings.Join(h.To, ",") != "all" || h.Re != q || h.ReplyBy != "" {
		t.Fatalf("header = %+v", h)
	}
	if m.RelatesTo.IsFallingBack || m.RelatesTo.InReplyTo.EventID != q {
		t.Fatalf("relates_to = %+v", m.RelatesTo)
	}
	if m.Mentions != nil && len(m.Mentions.UserIDs) != 0 {
		t.Fatalf("mentions for 'all' = %+v", m.Mentions)
	}
}

// Proves bodies come from @file and from stdin ("-").
func TestPostBodySources(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	f := filepath.Join(t.TempDir(), "msg.md")
	os.WriteFile(f, []byte("from a file\n"), 0o644)
	id := strings.TrimSpace(e.as("alice", "post", "proj", "--topic", "alpha", "@"+f).ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	tuitestkit.ContainsStr(t, ev.Message().Body, "\n\nfrom a file")
	e.stdin = "from stdin"
	id = strings.TrimSpace(e.as("alice", "post", "proj", "-", "--topic", "alpha").ok(t).stdout)
	ev, _ = e.hs.eventByID(id)
	tuitestkit.ContainsStr(t, ev.Message().Body, "\n\nfrom stdin")
}

// Proves post refuses empty bodies (literal, whitespace, empty stdin, empty
// file), unknown topics, and malformed flags, and sends nothing in those cases.
func TestPostNegative(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	reply := strings.TrimSpace(e.as("alice", "post", "proj", "--topic", "alpha", "x").ok(t).stdout)
	send := "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
	before := e.hs.CallCount(send)
	empty := filepath.Join(t.TempDir(), "empty")
	os.WriteFile(empty, nil, 0o644)
	e.stdin = ""
	cases := []struct {
		args     []string
		code     int
		fragment string
	}{
		{[]string{"proj", "--topic", "alpha", ""}, ExitUsage, "empty message body refused"},
		{[]string{"proj", "--topic", "alpha", "  \n\t "}, ExitUsage, "empty message body refused"},
		{[]string{"proj", "--topic", "alpha", "-"}, ExitUsage, "empty message body refused"},
		{[]string{"proj", "--topic", "alpha", "@" + empty}, ExitUsage, "empty message body refused"},
		{[]string{"proj", "--topic", "alpha"}, ExitUsage, "empty message body refused"},
		{[]string{"proj", "hello"}, ExitUsage, "--topic is required"},
		{[]string{"proj", "--topic", "nosuch", "hello"}, ExitError, `topic "nosuch" not found in #proj`},
		{[]string{"proj", "--topic", "$missing", "hello"}, ExitError, "topic event $missing not found"},
		{[]string{"proj", "--topic", reply, "hello"}, ExitError, "is a thread reply, not a topic root"},
		{[]string{"proj", "--topic", "alpha", "--kind", "gossip", "hi"}, ExitUsage, "invalid --kind"},
		{[]string{"proj", "--topic", "alpha", "--re", "notanid", "hi"}, ExitUsage, "invalid --re"},
		{[]string{"proj", "--topic", "alpha", "--trunk", "xyz", "hi"}, ExitUsage, "invalid --trunk"},
		{[]string{"proj", "--topic", "alpha", "--reply-by", "tomorrow", "hi"}, ExitUsage, "invalid --reply-by"},
		{[]string{"proj", "--topic", "alpha", "--to", "bad user", "hi"}, ExitUsage, "invalid user"},
		{[]string{"proj", "--topic", "alpha", "--bogus", "hi"}, ExitUsage, "flag provided but not defined"},
		{[]string{"proj", "--topic", "alpha", "@/does/not/exist"}, ExitError, "read body"},
		{[]string{"proj", "--topic", "alpha", "-", "extra"}, ExitUsage, "exactly one body"},
		{[]string{"proj", "--topic", "alpha", strings.Repeat("x", MaxBodyBytes+1)}, ExitError, "body too large"},
	}
	for _, tc := range cases {
		e.as("alice", append([]string{"post"}, tc.args...)...).failsWith(t, tc.code, tc.fragment)
	}
	if got := e.hs.CallCount(send); got != before {
		t.Fatalf("refused posts sent %d messages", got-before)
	}
	_ = root
}

// Proves read prints room messages oldest first, honours --last, strips
// control characters, and hides header lines behind a compact meta line.
func TestReadRoom(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	for _, s := range []string{"one", "two", "three"} {
		e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "answer", s).ok(t)
	}
	e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "evil \x1b[31mred\x1b[0m ‮txt\x07"})
	r := e.as("alice", "read", "proj", "--last", "3").ok(t)
	out := r.stdout
	tuitestkit.NotContainsStr(t, out, "one")
	if !(strings.Index(out, "two") < strings.Index(out, "three") && strings.Index(out, "three") < strings.Index(out, "evil")) {
		t.Fatalf("not oldest first:\n%s", out)
	}
	tuitestkit.ContainsStr(t, out, "bob $ev003 kind=answer to=all thread=$ev001")
	tuitestkit.NotContainsStr(t, out, "- from:")
	for _, bad := range []string{"\x1b", "‮", "\x07"} {
		tuitestkit.NotContainsStr(t, out, bad)
	}
}

// Proves read --topic shows the root first, then the last N replies oldest first,
// and nothing from other threads.
func TestReadTopic(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	e.as("alice", "topic", "new", "proj", "beta", "B").ok(t)
	e.as("bob", "post", "proj", "--topic", "beta", "other thread").ok(t)
	for i := 0; i < 150; i++ {
		e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "reply " + string(rune('A'+i%26)),
			"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$ev001"}})
	}
	e.as("bob", "post", "proj", "--topic", "alpha", "latest").ok(t)
	r := e.as("alice", "read", "proj", "--topic", "alpha", "--last", "120").ok(t)
	lines := tuitestkit.LinesFromStr(r.stdout)
	tuitestkit.ContainsStr(t, lines[0], "$ev001")
	tuitestkit.ContainsStr(t, lines[1], "[topic] alpha: A")
	tuitestkit.NotContainsStr(t, r.stdout, "other thread")
	if n := strings.Count(r.stdout, "thread=$ev001"); n != 120 {
		t.Fatalf("replies shown = %d, want 120", n)
	}
	tuitestkit.ContainsStr(t, lines[len(lines)-1], "latest")
	e.as("alice", "read", "proj", "--topic", "nosuch").failsWith(t, ExitError, "not found")
	e.as("alice", "read", "proj", "--last", "0").failsWith(t, ExitUsage, "--last")
	e.as("carol", "read", "proj").failsWith(t, ExitError, "M_FORBIDDEN")
}

// Proves the rule-24 kinds owner-needed and system are accepted and stored in
// the structured header, while an unknown kind is still refused.
func TestPostOwnerNeededAndSystemKinds(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	for _, k := range []string{"owner-needed", "system"} {
		id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--kind", k, "--to", "alice", "x").ok(t).stdout)
		ev, _ := e.hs.eventByID(id)
		if h, _ := decodeHeader(ev.Message().A2A); h.Kind != k {
			t.Fatalf("kind = %q, want %q", h.Kind, k)
		}
	}
	e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "owner", "x").failsWith(t, ExitUsage, "invalid --kind")
}

// Proves post --subtype/--subject store the structured subtype and subject in
// the header, add them to the header lines of the body, and surface them in
// read meta, read --json, the wait line and record, and the projection.
func TestPostSubtypeSubject(t *testing.T) {
	e, room, system := projWorld(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	e.project().ok(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	subject := "relux-works/skill-project-management@main"
	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "system", "--subtype", "landing",
		"--subject", subject, "--to", "alice", "PR #12 landed").ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	if h.Subtype != "landing" || h.Subject != subject {
		t.Fatalf("header = %+v", h)
	}
	tuitestkit.ContainsStr(t, ev.Message().Body, "- kind: system\n- subtype: landing\n- subject: "+subject+"\n\nPR #12 landed")
	tuitestkit.ContainsStr(t, e.as("alice", "read", "proj", "--event", id).ok(t).stdout, "kind=system to=alice subtype=landing subject="+subject)
	rec := decodeRecords(t, e.as("alice", "read", "proj", "--event", id, "--json").ok(t).stdout)[0]
	if rec.Subtype != "landing" || rec.Subject != subject {
		t.Fatalf("read record = %+v", rec)
	}
	line := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	tuitestkit.ContainsStr(t, line, "to #proj "+id+" from=bob kind=system subtype=landing subject="+subject+" topic=alpha")
	e.project().ok(t)
	got := e.projections(system)
	if len(got) != 1 {
		t.Fatalf("projections = %d", len(got))
	}
	if p := projectionOf(t, got[0]); p.Subtype != "landing" || p.Subject != subject {
		t.Fatalf("projection = %+v", p)
	}
	tuitestkit.ContainsStr(t, got[0].Message().Body, "[system/landing] from bob · #proj › topic alpha › "+id+" · subject "+subject+"\n")
	_ = room
}

// Proves every system subtype of rules 24, 28 and 31 (and queue) is accepted
// and stored in the header, while a near-miss spelling ("impact", "redline_closed")
// and a subtype on another kind are refused without sending.
func TestPostNewSubtypes(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	for _, st := range []string{"queue", "redline", "redline-closed", "impact-start", "impact-end"} {
		id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "system", "--subtype", st,
			"--subject", "runner-pool", "x").ok(t).stdout)
		ev, _ := e.hs.eventByID(id)
		if h, _ := decodeHeader(ev.Message().A2A); h.Subtype != st {
			t.Fatalf("subtype %s stored as %+v", st, h)
		}
	}
	before := e.hs.CallCount(sendRoute)
	for _, bad := range []string{"impact", "redline_closed", "Redline"} {
		e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "system", "--subtype", bad, "x").failsWith(t, ExitUsage, "invalid --subtype")
	}
	e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "notice", "--subtype", "redline", "x").failsWith(t, ExitUsage, "--subtype needs --kind system")
	if n := e.hs.CallCount(sendRoute); n != before {
		t.Fatalf("refused posts sent %d messages", n-before)
	}
	e.subscribe("alice", "--room", "proj", "--subtype", "impact-start")
}

// Proves --subtype needs --kind system and a known subtype, --subject must be
// 1..128 token characters, and refused posts send nothing; a plain --subject
// on another kind is accepted (positive control).
func TestPostSubtypeSubjectNegative(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	before := e.hs.CallCount(sendRoute)
	e.as("bob", "post", "proj", "--topic", "alpha", "--subtype", "landing", "x").failsWith(t, ExitUsage, "--subtype needs --kind system")
	e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "notice", "--subtype", "landing", "x").failsWith(t, ExitUsage, "--subtype needs --kind system")
	e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "system", "--subtype", "party", "x").failsWith(t, ExitUsage, "invalid --subtype")
	e.as("bob", "post", "proj", "--topic", "alpha", "--subject", "has space", "x").failsWith(t, ExitUsage, "invalid --subject")
	e.as("bob", "post", "proj", "--topic", "alpha", "--subject", strings.Repeat("a", 129), "x").failsWith(t, ExitUsage, "invalid --subject")
	e.as("bob", "post", "proj", "--topic", "alpha", "--subject", "a\x1bb", "x").failsWith(t, ExitUsage, "invalid --subject")
	if n := e.hs.CallCount(sendRoute); n != before {
		t.Fatalf("refused posts sent %d", n-before)
	}
	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "decision", "--subject", "BUG-260928-2qon2t", "x").ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	if h, _ := decodeHeader(ev.Message().A2A); h.Subject != "BUG-260928-2qon2t" || h.Subtype != "" {
		t.Fatalf("header = %+v", h)
	}
}
