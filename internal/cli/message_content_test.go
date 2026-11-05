package cli

import (
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/yuin/goldmark/text"
	xhtml "golang.org/x/net/html"
)

func requireTextContent(t *testing.T, body string) map[string]any {
	t.Helper()
	c, err := textContent(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if c["body"] != body || c["msgtype"] != "m.text" {
		t.Fatalf("plaintext changed: %#v", c)
	}
	return c
}

// Proves supported formatting survives the sanitizer, including GFM tables and
// the allowed HTTPS, matrix.to and MXC destinations; body stays byte-identical.
func TestTextContentFormatting(t *testing.T) {
	for _, tc := range []struct{ source, want string }{
		{"**bold**", "<strong>bold</strong>"}, {"`code`", "<code>code</code>"},
		{"```go\nx := 1\n```", "<pre><code class=\"language-go\">x := 1\n</code></pre>"},
		{"- one\n- two", "<ul>"}, {"3. three", `<ol start="3">`}, {"| A | B |\n| - | - |\n| 1 | 2 |", "<table>"},
		{"[web](https://example.org/a)", `<a href="https://example.org/a">web</a>`},
		{"[room](https://matrix.to/#/!room:server)", `<a href="https://matrix.to/#/!room:server">room</a>`},
		{"[media](mxc://server/id)", `<a href="mxc://server/id">media</a>`},
	} {
		t.Run(tc.source, func(t *testing.T) {
			c := requireTextContent(t, tc.source)
			if c["format"] != "org.matrix.custom.html" || !strings.Contains(fmt.Sprint(c["formatted_body"]), tc.want) {
				t.Fatalf("missing %s: %#v", tc.want, c)
			}
		})
	}
}

// Proves ordinary paragraphs (including multiline Unicode and escaping) do not
// acquire redundant HTML, while preserving whitespace exactly.
func TestTextContentPlain(t *testing.T) {
	for _, body := range []string{"ordinary message", "line one\nline two", "界 & quotes ' \"", " trailing space "} {
		c := requireTextContent(t, body)
		if c["format"] != nil || c["formatted_body"] != nil {
			t.Fatalf("redundant HTML for %q: %#v", body, c)
		}
	}
}

// Proves hostile source has no executable tags, attributes or unsafe links in
// parsed HTML, including source in code spans/fences and link labels. Escaped
// text in code is intentionally retained, never interpreted as HTML.
func TestTextContentHostileSource(t *testing.T) {
	for _, hostile := range []string{`<script>alert(1)</script>`, `<img src=x onerror=alert(1)>`, `<b onclick="x">raw</b>`, `[bad](javascript:alert%281%29)`, `[bad](data:text/html,boom)`, `[local](/private/path)`, `[local](file:///private/path)`, `[bad](http://example.org)`, `[bad](JaVaScRiPt:alert%281%29)`, `[bad](jav&#x61;script:alert%281%29)`, `[bad](d&#97;ta:text/html,boom)`, `[bad](HTTP://example.org)`, "```go\"\tonmouseover=alert(1)\ncode\n```"} {
		for _, source := range []string{hostile, "**control** " + hostile, "`" + hostile + "`", "[" + hostile + "](https://example.org)", "```html\n" + hostile + "\n```"} {
			c := requireTextContent(t, source)
			assertSafeMessageHTML(t, fmt.Sprint(c["formatted_body"]))
		}
	}
	for _, target := range []string{"javascript:alert(1)", "data:text/html,x", "/tmp/x", "file:///tmp/x", "http://example.org", "//matrix.to/#/x", "https://user:pass@matrix.to/x", "JaVaScRiPt:alert(1)", "jav&#x61;script:alert(1)", "d&#97;ta:text/html,x", "HTTP://example.org"} {
		c := requireTextContent(t, "[label]("+target+")")
		if strings.Contains(fmt.Sprint(c["formatted_body"]), "<a ") {
			t.Fatalf("unsafe link: %#v", c)
		}
	}
}

// Independent test oracle parses the decoded DOM attribute, never calls the
// production URL policy. Only absolute HTTPS/MXC with no credentials is valid.
func independentSafeLink(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.IsAbs() && u.Hostname() != "" && u.User == nil &&
		(strings.EqualFold(u.Scheme, "https") || strings.EqualFold(u.Scheme, "mxc"))
}

func assertSafeMessageHTML(t *testing.T, source string) {
	t.Helper()
	doc, err := xhtml.Parse(strings.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	var walk func(*xhtml.Node)
	walk = func(n *xhtml.Node) {
		if n.Type == xhtml.CommentNode {
			t.Fatal("comment survived")
		}
		if n.Type == xhtml.ElementNode {
			if !strings.Contains("|html|head|body|p|br|hr|blockquote|pre|code|em|strong|del|ul|ol|li|h1|h2|h3|h4|h5|h6|table|thead|tbody|tr|th|td|a|", "|"+n.Data+"|") || n.Namespace != "" {
				t.Fatalf("forbidden tag: %s", n.Data)
			}
			for _, a := range n.Attr {
				_, startErr := strconv.ParseUint(a.Val, 10, 32)
				if a.Namespace != "" || !(n.Data == "ol" && a.Key == "start" && startErr == nil || n.Data == "a" && a.Key == "href" && independentSafeLink(a.Val) || n.Data == "code" && a.Key == "class" && strings.HasPrefix(a.Val, "language-") && !strings.ContainsAny(a.Val, " \t\r\n")) {
					t.Fatalf("forbidden attribute: %+v", a)
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
}

// Proves the sanitizer itself strips forbidden renderer output, independently
// of goldmark's literal-HTML renderer (a bounded control for the allowlist).
func TestMessageHTMLAllowlist(t *testing.T) {
	got := sanitizeMessageHTML(`<p onclick="x" style="color:red">ok<strong id="x">bold</strong><img src="mxc://x/y" onerror="x"><script>evil</script><a href="javascript:x">label</a><code class="language-go" onmouseover="x">code</code><svg><a href="https://example.org">x</a></svg><!-- secret --></p>`)
	assertSafeMessageHTML(t, got)
	if strings.Contains(got, "evil") || strings.Contains(got, "secret") || !strings.Contains(got, "<strong>bold</strong>") || !strings.Contains(got, "label") {
		t.Fatalf("bad sanitization: %s", got)
	}
}

// Proves HTML size accounting includes JSON escaping and metadata, falls back
// without changing body/fields, and imposes no new 48 KiB limit on plain events.
func TestTextContentSize(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		fields     map[string]any
		plain      bool
	}{
		{"formatted", "**ok**", nil, false},
		{"HTML expansion", "**" + strings.Repeat("&", 5000) + "**", nil, true},
		{"plain above HTML threshold", strings.Repeat("<&", 8000), nil, true},
		{"metadata", "**ok**", map[string]any{"extra": strings.Repeat("x", maxTextContentBytes)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := textContent(tc.body, tc.fields)
			if err != nil || c["body"] != tc.body {
				t.Fatalf("plain event refused or changed: %v", err)
			}
			for k, v := range tc.fields {
				if c[k] != v {
					t.Fatalf("metadata changed: %s", k)
				}
			}
			if tc.plain {
				if _, ok := c["format"]; ok {
					t.Fatal("format survived fallback")
				}
				if _, ok := c["formatted_body"]; ok {
					t.Fatal("HTML survived fallback")
				}
			} else if c["format"] != "org.matrix.custom.html" {
				t.Fatal("valid formatting lost")
			}
		})
	}
}

// Proves the inclusive HTML threshold using serialized metadata overhead;
// crossing it by one byte removes both HTML fields, including stale input HTML.
func TestTextContentHTMLSizeBoundary(t *testing.T) {
	base := requireTextContent(t, "**ok**")
	base["extra"] = ""
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{0, 1} {
		fields := map[string]any{"extra": strings.Repeat("x", maxTextContentBytes-len(raw)+delta), "format": "stale", "formatted_body": "stale"}
		c, err := textContent("**ok**", fields)
		if err != nil {
			t.Fatal(err)
		}
		if (c["format"] != nil) != (delta == 0) || (c["formatted_body"] != nil) != (delta == 0) {
			t.Fatalf("wrong boundary decision at delta %d", delta)
		}
		if fields["format"] != "stale" || fields["formatted_body"] != "stale" {
			t.Fatal("mutated input fields")
		}
	}
}

// Proves invalid plain metadata is still rejected without a partial content map,
// beside a serializable positive control; HTML cannot bypass JSON validation.
func TestTextContentInvalidMetadata(t *testing.T) {
	requireTextContent(t, "**ok**")
	c, err := textContent("**ok**", map[string]any{"invalid": make(chan int)})
	if c != nil || err == nil || !strings.Contains(err.Error(), "encode message") {
		t.Fatalf("invalid plain event accepted: %v", err)
	}
}

// Proves boundaries that used to fall inside fitting closed/unclosed, tilde and
// nested fences move outside them; each chunk renders balanced code HTML and
// concatenated bodies retain the exact source bytes.
func TestMirrorFenceChunks(t *testing.T) {
	for _, block := range []string{"```go\nx := 1\n```\n", "```\n```\n", "~~~\ncode\n~~~\n", "```\n" + strings.Repeat("x", 3991) + "\n```\n", "```\nunclosed\n", "> ```\n> code\n> ```\n", "- ```\n  code\n  ```\n"} {
		source := strings.Repeat("x", 3990) + "\n\n" + block + "after"
		chunks := ownerDMContent(source)
		if strings.Join(chunks, "") != source {
			t.Fatal("chunking changed plaintext")
		}
		found := false
		for _, chunk := range chunks {
			hasFence := strings.Contains(chunk, block)
			if hasFence {
				found = true
			}
			if utf8.RuneCountInString(chunk) > 4000 {
				t.Fatal("chunk exceeds target")
			}
			c := requireTextContent(t, chunk)
			rendered, _ := c["formatted_body"].(string)
			assertBalancedMessageHTML(t, rendered)
			if hasFence && !strings.Contains(rendered, "<pre><code") {
				t.Fatalf("fence chunk lacks code HTML: %q", rendered)
			}
		}
		if !found {
			t.Fatalf("split fence: %q", block)
		}
	}
}

// Proves topic/post keep their exact historical body construction and metadata
// at the wire; read --json continues to expose plaintext, never formatted_body.
func TestRichTopicAndPostBodies(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "rich", "**Title**", "--body", "**bold**").ok(t).stdout)
	ev, _ := e.hs.eventByID(root)
	if ev.Message().Body != "[topic] rich: **Title**\n\n**bold**" {
		t.Fatal(ev.Message().Body)
	}
	if !strings.Contains(fmt.Sprint(decodeContent(t, ev)["formatted_body"]), "<strong>bold</strong>") {
		t.Fatal("topic HTML absent")
	}
	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "rich", "**reply**").ok(t).stdout)
	ev, _ = e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	// Before HTML support, omitted --to already defaulted to all.
	want := fmt.Sprintf("- from: bob\n- to: all\n- at: %s\n- re: -\n- kind: notice\n\n**reply**", h.At)
	if ev.Message().Body != want {
		t.Fatalf("post body changed: got %q, want %q", ev.Message().Body, want)
	}
	if ev.ThreadRoot() != root {
		t.Fatalf("post thread changed: got %q, want %q", ev.ThreadRoot(), root)
	}
	out := e.as("alice", "read", "proj", "--topic", "rich", "--json").ok(t).stdout
	if !strings.Contains(out, "**reply**") || strings.Contains(out, "<strong>") {
		t.Fatal(out)
	}

	// Addressed delivery must also read body even when sender supplies HTML.
	// The first inbox establishes the sync cursor; only later sends are delivered.
	e.as("alice", "inbox").ok(t)
	e.as("bob", "post", "proj", "--topic", "rich", "--to", "alice", "**mail**").ok(t)
	inbox := e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(inbox, "**mail**") || strings.Contains(inbox, "<strong>") {
		t.Fatal(inbox)
	}
}

// Proves projection keeps the exact original body, ignores source HTML and
// preserves projection metadata while rendering only the plaintext source.
func TestRichProjectionBody(t *testing.T) {
	raw, _ := json.Marshal(map[string]any{"body": "**source**", "formatted_body": "EVIL HTML"})
	ev := matrix.Event{EventID: "$event", Sender: "@bob:test.local", OriginServerTS: 1000, Content: raw}
	c, err := projection(ev, Header{Kind: "system"}, "!room:test.local", "#proj", "test.local", "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := "[system] from bob · #proj › $event\n**source**\n\norigin: https://matrix.to/#/!room:test.local/$event"
	wantMetadata := Projection{V: 1, Type: "projection", Kind: "system", From: "bob", At: "1970-01-01T00:00:01Z", OriginRoom: "!room:test.local", OriginAlias: "#proj", OriginEvent: "$event", AuthorMXID: "@bob:test.local", Author: "bob"}
	if !reflect.DeepEqual(c[ContentKey], wantMetadata) || !reflect.DeepEqual(c["m.mentions"], map[string]any{}) {
		t.Fatalf("projection metadata changed: %#v", c)
	}
	if c["body"] != want || strings.Contains(fmt.Sprint(c["formatted_body"]), "EVIL") || !strings.Contains(fmt.Sprint(c["formatted_body"]), "<strong>source</strong>") {
		t.Fatalf("projection changed: %#v", c)
	}
}

// Proves actual owner confirmation and mirror sends keep their historical body,
// redact secrets before either representation, and retain clean formatted text.
func TestRichOwnerDMBodies(t *testing.T) {
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	ownerActivate(t, e, f, c)
	c = ownerLoad(t, f)
	want := fmt.Sprintf("owner-dm active: bob; tag=%t (forced off in S1), mirror=%t, render=%s; config %s; unbind: a2a owner-dm unbind bob", c.Tag, c.Mirror, c.Render, ownerFingerprint(c))
	bodies := ownerBodies(e, c.Room)
	if len(bodies) != 2 || bodies[0] != want || bodies[1] != "ok" {
		t.Fatalf("confirmation changed: %q", bodies)
	}
	ownerMirror(t, e, "codex", ownerPayload(t, "codex", "rich", "**safe** ghp_fixture"))
	bodies = ownerBodies(e, c.Room)
	if len(bodies) != 3 || bodies[2] != "**safe** [redacted]" {
		t.Fatalf("mirror changed: %q", bodies)
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	for _, ev := range e.hs.events {
		if ev.RoomID == c.Room && ev.Message().Body == bodies[2] {
			rendered := fmt.Sprint(decodeContent(t, ev)["formatted_body"])
			if strings.Contains(rendered, "ghp_fixture") || !strings.Contains(rendered, "<strong>safe</strong>") || !strings.Contains(rendered, "[redacted]") {
				t.Fatal(rendered)
			}
		}
	}
}

// Proves liveness sends retain the exact transition body and routing metadata;
// these ordinary sentences do not acquire redundant HTML.
func TestRichLivenessBody(t *testing.T) {
	e, _ := world(t)
	now := time.Date(2026, 10, 7, 18, 0, 0, 0, time.UTC)
	e.now = func() time.Time { return now }
	e.as("alice", "status", "init").ok(t)
	e.as("bob", "join", "status").ok(t)
	e.as("alice", statusArgs("idle")...).ok(t)
	e.as("bob", statusArgs("working")...).ok(t)
	now = now.Add(4 * time.Minute)
	e.as("alice", "who", "--stale-after", "3m", "--product", "swarm", "--alert", "--alert-room", "proj", "--keeper", "bob").ok(t)
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	count := 0
	for _, ev := range e.hs.events {
		c := decodeContent(t, ev)
		if c["ai.relux.a2a.liveness_observation"] == nil {
			continue
		}
		body := ev.Message().Body
		want := "Liveness transition host:" + machineLabel() + ": host unavailable; agents=alice,bob. Keeper action required; waiter freshness, native wake reachability and reply age are separate evidence."
		if body != want {
			t.Fatalf("liveness changed: %q", body)
		}
		h, ok := decodeHeader(ev.Message().A2A)
		wantHeader := Header{V: 1, Type: "message", From: "alice", To: []string{"bob"}, At: now.UTC().Format(time.RFC3339), Kind: "system", Subtype: "incident"}
		if !ok || !reflect.DeepEqual(h, wantHeader) {
			t.Fatalf("liveness A2A header changed: %+v", h)
		}
		mentions, _ := c["m.mentions"].(map[string]any)
		if !reflect.DeepEqual(mentions["user_ids"], []any{"@bob:test.local"}) {
			t.Fatalf("liveness mentions changed: %#v", c["m.mentions"])
		}
		if c["format"] != nil || c["formatted_body"] != nil {
			t.Fatal("redundant HTML")
		}
		count++
	}
	if count != 1 {
		t.Fatalf("alerts = %d", count)
	}
}

// Proves escaped source is inert when deliberately placed inside code.
func TestRichEscapedCode(t *testing.T) {
	c := requireTextContent(t, "`<script>`")
	if !strings.Contains(fmt.Sprint(c["formatted_body"]), html.EscapeString("<script>")) {
		t.Fatal(c)
	}
}

// Proves HTML-only oversize posts deliver the exact framed body as plain text,
// while the existing CLI body limit rejects 64 KiB plaintext before any send,
// without recording any event. fakeHS has no message-size gate.
func TestRichPostSizeContract(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "sizes", "Sizes").ok(t)
	e.as("bob", "post", "proj", "--topic", "sizes", "**ok**").ok(t)
	body := "**" + strings.Repeat("&", 5000) + "**"
	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "sizes", body).ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	// Keep the historical default recipient even when HTML is too large.
	want := fmt.Sprintf("- from: bob\n- to: all\n- at: %s\n- re: -\n- kind: notice\n\n%s", h.At, body)
	content := decodeContent(t, ev)
	if ev.Message().Body != want {
		t.Fatalf("HTML fallback changed body: got %q, want %q", ev.Message().Body, want)
	}
	if content["format"] != nil || content["formatted_body"] != nil {
		t.Fatal("oversize HTML did not fall back to plain content")
	}
	e.hs.mu.Lock()
	before := len(e.hs.events)
	e.hs.mu.Unlock()
	const sendRoute = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
	beforeSends := e.hs.CallCount(sendRoute)
	e.as("bob", "post", "proj", "--topic", "sizes", strings.Repeat("x", 64<<10)).failsWith(t, ExitError, "body too large (limit 32768 bytes)")
	if e.hs.CallCount(sendRoute) != beforeSends {
		t.Fatal("oversize body reached the send endpoint")
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	if len(e.hs.events) != before {
		t.Fatal("rejected plain event was partially recorded")
	}
}

// Proves HTML expansion in a later chunk falls back to plain without withholding
// earlier chunks or changing any body; the complete mirror is journaled as sent.
func TestRichMirrorSizeFallback(t *testing.T) {
	if requireTextContent(t, "<https://example.org/?&&>")["format"] != "org.matrix.custom.html" {
		t.Fatal("small autolink lost formatting")
	}
	e, c, f := ownerActiveFixture(t)
	before := len(ownerBodies(e, c.Room))
	link := "<https://example.org/?" + strings.Repeat("&", 3900) + ">"
	source := strings.Repeat("x", 3998) + "\n\n" + link
	ownerMirror(t, e, "codex", ownerPayload(t, "codex", "HTML-oversize", source))
	bodies := ownerBodies(e, c.Room)[before:]
	if !reflect.DeepEqual(bodies, []string{source[:4000], link}) {
		t.Fatalf("mirror changed: %q", bodies)
	}
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	found := false
	for _, ev := range e.hs.events {
		if ev.RoomID == c.Room && ev.Message().Body == link {
			content := decodeContent(t, ev)
			if content["format"] != nil || content["formatted_body"] != nil {
				t.Fatal("HTML-only oversize chunk did not fall back")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("fallback event absent")
	}
	entries, err := f.journal(time.Now())
	if err != nil || len(entries) != 1 || entries[0].State != "sent 2/2" {
		t.Fatalf("mirror incomplete: %+v %v", entries, err)
	}
}

// Proves long closed/unclosed and Unicode fences use historical target splits,
// deliver every byte through the mirror, and independently emit balanced HTML.
// Later chunks can be ordinary prose: code formatting need not continue.
func TestRichMirrorLongFences(t *testing.T) {
	for _, block := range []string{
		"```\n" + strings.Repeat("x", 3992) + "\n```\n", // 4001 runes: immediately over target.
		"```\n" + strings.Repeat("&", 20000) + "\n```",
		"~~~\n" + strings.Repeat("界", 5000),
	} {
		e, c, f := ownerActiveFixture(t)
		before := len(ownerBodies(e, c.Room))
		source := strings.Repeat("x", 3990) + "\n\n" + block
		ownerMirror(t, e, "codex", ownerPayload(t, "codex", "long-fence", source))
		bodies := ownerBodies(e, c.Room)[before:]
		if !reflect.DeepEqual(bodies, historicalMirrorChunks(source)) || strings.Join(bodies, "") != source {
			t.Fatal("long fence text lost or boundaries changed")
		}
		sawCode := false
		e.hs.mu.Lock()
		for _, ev := range e.hs.events {
			if ev.RoomID != c.Room {
				continue
			}
			rendered, _ := decodeContent(t, ev)["formatted_body"].(string)
			assertBalancedMessageHTML(t, rendered)
			sawCode = sawCode || strings.Contains(rendered, "<pre><code")
		}
		e.hs.mu.Unlock()
		if !sawCode {
			t.Fatal("long fence produced no code HTML")
		}
		entries, err := f.journal(time.Now())
		if err != nil || len(entries) != 1 || entries[0].State != fmt.Sprintf("sent %d/%d", len(bodies), len(bodies)) {
			t.Fatalf("long fence incomplete: %+v %v", entries, err)
		}
	}
}

// Proves fence-free ASCII/Unicode messages retain each historical chunk and
// joined body byte, including boundaries at and immediately past the target.
func TestMirrorChunksWithoutFences(t *testing.T) {
	for _, source := range []string{"", strings.Repeat("x", 4000), strings.Repeat("界\n", 4001)} {
		if got := mirrorChunks(source); !reflect.DeepEqual(got, historicalMirrorChunks(source)) || strings.Join(got, "") != source {
			t.Fatal("fence-free chunks changed")
		}
	}
}

func historicalMirrorChunks(source string) []string {
	var chunks []string
	chars := []rune(source)
	for len(chars) > 0 {
		n := min(4000, len(chars))
		chunks = append(chunks, string(chars[:n]))
		chars = chars[n:]
	}
	return chunks
}

func assertBalancedMessageHTML(t *testing.T, source string) {
	t.Helper()
	var stack []string
	z := xhtml.NewTokenizer(strings.NewReader(source))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			if z.Err() != io.EOF || len(stack) != 0 {
				t.Fatalf("unbalanced HTML: %q", source)
			}
			return
		case xhtml.StartTagToken:
			name, _ := z.TagName()
			if string(name) != "br" && string(name) != "hr" {
				stack = append(stack, string(name))
			}
		case xhtml.EndTagToken:
			name, _ := z.TagName()
			if len(stack) == 0 || stack[len(stack)-1] != string(name) {
				t.Fatalf("unmatched end tag: %s", name)
			}
			stack = stack[:len(stack)-1]
		}
	}
}

// Proves source strong tags remain literal visible text while Markdown strong
// still produces formatting; neither raw text nor the allowed link label is lost.
func TestRichRawHTMLDisabled(t *testing.T) {
	c := requireTextContent(t, "**control** <strong>raw</strong> [ok](https://example.org)")
	rendered := fmt.Sprint(c["formatted_body"])
	if strings.Contains(rendered, "<strong>raw</strong>") || !strings.Contains(rendered, "<strong>control</strong>") || strings.TrimSpace(visibleMessageText(rendered)) != "control <strong>raw</strong> ok" {
		t.Fatal(rendered)
	}
}

// Proves inline/block HTML, comments and placeholders remain literal visible
// text; images retain alt or source URL, with no source element or comment.
func TestRichLiteralSourceFidelity(t *testing.T) {
	for _, tc := range []struct{ source, visible string }{
		{"a2a post <room> then wait", "a2a post <room> then wait"},
		{"<div>deploy output</div>", "<div>deploy output</div>"},
		{"<!-- keep these words -->", "<!-- keep these words -->"},
		{"<!--\nkeep these words\n-->", "<!--\nkeep these words\n-->"},
		{"before <!-- keep words --> after", "before <!-- keep words --> after"},
		{"<div>\ndeploy output\n</div>", "<div>\ndeploy output\n</div>"},
		{"use <placeholder> in prose", "use <placeholder> in prose"},
		{"![diagram](mxc://srv/id)", "diagram"},
		{"![](mxc://srv/id)", "mxc://srv/id"},
	} {
		c := requireTextContent(t, tc.source)
		rendered, ok := c["formatted_body"].(string)
		if !ok || strings.TrimSpace(visibleMessageText(rendered)) != tc.visible {
			t.Fatalf("visible content lost for %q: %#v", tc.source, c)
		}
		assertSafeMessageHTML(t, rendered)
		if strings.Contains(rendered, "<div>") || strings.Contains(rendered, "<room>") || strings.Contains(rendered, "<placeholder>") || strings.Contains(rendered, "<!--") || strings.Contains(rendered, "<img") {
			t.Fatalf("source HTML interpreted: %s", rendered)
		}
	}
}

// Proves the retention guard rejects empty output and missing/repeated words,
// with positive controls for Markdown delimiters, link targets and fence info.
func TestMessageWordRetention(t *testing.T) {
	for _, tc := range []struct {
		source, rendered string
		want             bool
	}{
		{"**keep** [label](https://unused.example)", "<p><strong>keep</strong> label</p>", true},
		{"```go\nkeep\n```", "<pre><code>keep\n</code></pre>", true},
		{"a2a post <room> then wait", "<p>a2a post then wait</p>", false},
		{"<div>deploy output</div>", "", false},
		{"keep keep", "<p>keep</p>", false},
		{"**keep**", "<p></p>", false},
		{"**caf&#233;**", "<p>café</p>", true},
	} {
		source := []byte(tc.source)
		doc := messageMarkdown.Parser().Parse(text.NewReader(source))
		if got := retainsMessageWords(messageNodeText(doc, source), tc.rendered); got != tc.want {
			t.Fatalf("retention(%q, %q) = %v", tc.source, tc.rendered, got)
		}
	}
}

// Proves the review's shortcut reference and subtype link cannot style sender
// framing; user Markdown still renders, and wire body stays byte-identical.
func TestRichFramingIsolation(t *testing.T) {
	e, _ := world(t)
	body := "**control**\n\n[topic]: https://phish.example"
	id := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "framing", "Title", "--body", body).ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	c := decodeContent(t, ev)
	rendered := fmt.Sprint(c["formatted_body"])
	if c["body"] != "[topic] framing: Title\n\n"+body || strings.Contains(rendered, "<a") || !strings.Contains(rendered, "[topic] framing: Title") || !strings.Contains(rendered, "<strong>control</strong>") {
		t.Fatalf("topic framing styled: %#v", c)
	}
	raw, _ := json.Marshal(map[string]any{"body": "**control**"})
	c, err := projection(matrix.Event{EventID: "$e", Sender: "@bob:test.local", Content: raw}, Header{Kind: "system", Subtype: "x](https://phish.example)"}, "!room:test.local", "#proj", "test.local", "", "")
	if err != nil {
		t.Fatal(err)
	}
	// The historical [%s] wrapper adds ] after the complete subtype.
	wantFrame := "[system/x](https://phish.example)] from bob · #proj › $e"
	rendered = fmt.Sprint(c["formatted_body"])
	if want := wantFrame + "\n**control**\n\norigin: https://matrix.to/#/!room:test.local/$e"; c["body"] != want {
		t.Fatalf("projection body changed: got %q, want %q", c["body"], want)
	}
	if strings.Contains(rendered, "<a") || !strings.Contains(visibleMessageText(rendered), wantFrame) || !strings.Contains(rendered, "<strong>control</strong>") {
		t.Fatalf("projection framing styled: %#v", c)
	}
}

// Proves fence movement cannot exceed eight mirror sends: the adversarial
// fitting fences fall back to every historical boundary without losing bytes.
func TestMirrorFenceChunkLimit(t *testing.T) {
	fence := "```\n" + strings.Repeat("x", 3991) + "\n```\n" // Exactly 4000 runes.
	source := strings.Repeat("\n"+fence, 7)
	for _, source := range []string{source, strings.Repeat("\n"+fence, 8), strings.Repeat("\n"+fence, 9)} {
		got := ownerDMContent(source)
		// Reproduce the existing truncation rule, independently of fence movement.
		chars := []rune(source)
		if len(chars) > 32000 {
			chars = append(chars[:28000:28000], []rune(fmt.Sprintf("\n[omitted %d characters]\n", len(chars)-28000))...)
		}
		bounded := string(chars)
		if len(got) > 8 || !reflect.DeepEqual(got, historicalMirrorChunks(bounded)) || strings.Join(got, "") != bounded {
			t.Fatalf("mirror chunk bound failed: %d chunks", len(got))
		}
	}
}

// Proves fence info containing an attribute injection still emits code HTML
// with no injected attribute; a normal language class remains as a control.
func TestRichFenceInfoInjection(t *testing.T) {
	for _, info := range []string{"go", "go\"\tonmouseover=alert(1)", "go&quote;\tonmouseover=x"} {
		c := requireTextContent(t, "```"+info+"\nkeep\n```")
		rendered, ok := c["formatted_body"].(string)
		if !ok || !strings.Contains(rendered, "<pre><code") || strings.TrimSpace(visibleMessageText(rendered)) != "keep" {
			t.Fatalf("code lost for %q: %#v", info, c)
		}
		assertSafeMessageHTML(t, rendered)
		if info == "go" && !strings.Contains(rendered, `class="language-go"`) {
			t.Fatal(rendered)
		}
	}
}

// Proves empty semantic rendering sends neither HTML field and retains the
// exact body; an adjacent alt-bearing image does receive nonempty HTML.
func TestRichEmptyImageFallback(t *testing.T) {
	c := requireTextContent(t, "![]()")
	if c["format"] != nil || c["formatted_body"] != nil {
		t.Fatal(c)
	}
	c = requireTextContent(t, "![keep]()")
	if c["format"] == nil || strings.TrimSpace(visibleMessageText(fmt.Sprint(c["formatted_body"]))) != "keep" {
		t.Fatal(c)
	}
}
