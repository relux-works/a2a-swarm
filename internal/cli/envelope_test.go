package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// The envelopes used here are inert carriage fixtures: this layer never parses,
// verifies or authorizes an envelope, so their signatures are intentionally not
// valid and nothing below asserts anything about swarm-envelope validity.
const envelopeCLIJSON = "{\"version\":\"swarm-envelope/2\",\r\n \"body\":{\"type\":\"message/1\",\"note\":\"ж é \U0001F600\"},\n\t\"id\":\"sha256:" +
	"0000000000000000000000000000000000000000000000000000000000000000\",\"signatures\":[]} \r\n\n"

func envelopeAllBytes() []byte {
	b := make([]byte, 0, 1024)
	for i := 0; i < 4; i++ {
		for j := 0; j < 256; j++ {
			b = append(b, byte(j))
		}
	}
	return b
}

func envelopePattern(n int) []byte { // deterministic, incompressible-looking, all byte values
	b := make([]byte, n)
	x := uint32(2463534242)
	for i := range b {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		b[i] = byte(x)
	}
	return b
}

func (e *env) eventCount() int {
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	return len(e.hs.events)
}

func envelopeFile(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "envelope.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readEnvelopeRecord runs the projection and checks the record framing: exactly
// one JSON object and one LF, only the nine contract fields.
func readEnvelopeRecord(t *testing.T, r result) EnvelopeInput {
	t.Helper()
	if strings.Count(r.stdout, "\n") != 1 || !strings.HasSuffix(r.stdout, "\n") {
		t.Fatalf("not exactly one record plus LF: %q", r.stdout)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &keys); err != nil {
		t.Fatal(err)
	}
	var names []string
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	want := []string{"body_text_base64", "envelope_base64", "receiver_mxid", "requested_event_id", "requested_room_id", "returned_event_id", "returned_room_id", "sender_mxid", "type"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("record fields %v, want %v", names, want)
	}
	var rec EnvelopeInput
	dec := json.NewDecoder(strings.NewReader(r.stdout))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

// Proves send then read is an exact-byte round trip: the projection returns the
// very bytes of the file and the very bytes of the event text, for ordinary,
// whitespace/CRLF-heavy, binary-safe and large envelopes, by every text source
// and with or without --re; the stored event carries the key and exact body.
// It proves carriage only, not envelope validity or sender authority.
func TestEnvelopeRoundTripExactBytes(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	textFile := envelopeFile(t, []byte("  file text ж\n\n"))
	cases := []struct {
		name     string
		envelope []byte
		text     string // exact expected event body
		viaFile  bool   // text from @file
		viaStdin bool   // text from stdin ("-")
		reply    bool
	}{
		{name: "json-crlf-unicode-trailing-whitespace", envelope: []byte(envelopeCLIJSON), text: "request: please assign"},
		{name: "binary-all-byte-values", envelope: envelopeAllBytes(), text: "binary envelope", reply: true},
		{name: "single-nul-byte", envelope: []byte{0}, text: "x"},
		{name: "trailing-lf-and-leading-bom", envelope: []byte("\xef\xbb\xbf{}\n"), text: " leading and trailing spaces \n"},
		{name: "large-in-budget", envelope: envelopePattern(40000), text: "large"},
		{name: "text-from-file-is-not-trimmed", envelope: []byte("{}"), text: "  file text ж\n\n", viaFile: true},
		{name: "text-from-stdin-is-not-trimmed", envelope: []byte("{}"), text: "stdin text\n", viaStdin: true, reply: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := envelopeFile(t, tc.envelope)
			text := tc.text
			if tc.viaFile {
				text = "@" + textFile
			}
			if tc.viaStdin {
				text = "-"
				e.stdin = tc.text
				defer func() { e.stdin = "" }()
			}
			args := []string{"post", "proj", "--topic", "alpha", "--kind", "request", "--to", "alice", "--envelope", "@" + path}
			if tc.reply {
				args = append(args, "--re", root)
			}
			id := strings.TrimSpace(e.as("bob", append(args, text)...).ok(t).stdout)
			if !strings.HasPrefix(id, "$") {
				t.Fatalf("no event id: %q", id)
			}

			rec := readEnvelopeRecord(t, e.as("alice", "read", room, "--event", id, "--envelope-input", "--json").ok(t))
			got, err := base64.RawURLEncoding.DecodeString(rec.EnvelopeBase64)
			if err != nil || !bytes.Equal(got, tc.envelope) || sha256.Sum256(got) != sha256.Sum256(tc.envelope) {
				t.Fatalf("envelope bytes changed (%d vs %d): %v", len(got), len(tc.envelope), err)
			}
			body, err := base64.RawURLEncoding.DecodeString(rec.BodyTextBase64)
			if err != nil || string(body) != tc.text {
				t.Fatalf("event text changed: %q vs %q: %v", body, tc.text, err)
			}
			if rec.Type != "a2a.event-envelope-input/1" || rec.SenderMXID != "@bob:"+testServer || rec.ReceiverMXID != "@alice:"+testServer ||
				rec.RequestedRoomID != room || rec.ReturnedRoomID != room || rec.RequestedEventID != id || rec.ReturnedEventID != id {
				t.Fatalf("identifiers or identities wrong: %+v", rec)
			}

			// The stored event: fixed key, unpadded base64url string, exact body,
			// the ordinary thread/mention metadata, and nothing else.
			ev, ok := e.hs.eventByID(id)
			if !ok {
				t.Fatal("event not stored")
			}
			var content map[string]json.RawMessage
			if err := json.Unmarshal(ev.Content, &content); err != nil {
				t.Fatal(err)
			}
			var keys []string
			for k := range content {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if want := []string{"ai.relux.a2a", "body", "m.mentions", "m.relates_to", "msgtype", "works.relux.swarm.envelope"}; !reflect.DeepEqual(keys, want) {
				t.Fatalf("event content keys %v, want %v", keys, want)
			}
			var stored, storedBody string
			json.Unmarshal(content["works.relux.swarm.envelope"], &stored)
			json.Unmarshal(content["body"], &storedBody)
			if stored != base64.RawURLEncoding.EncodeToString(tc.envelope) || strings.ContainsAny(stored, "=+/") {
				t.Fatal("stored value is not the unpadded base64url of the exact bytes")
			}
			if storedBody != tc.text {
				t.Fatalf("stored body %q, want the exact text %q (no header lines)", storedBody, tc.text)
			}
		})
	}
}

// Main-timeline envelope replies keep exact text/envelope bytes and only the
// native reply relation, both for a plain target and an explicitly forced root.
func TestEnvelopeMainReply(t *testing.T) {
	for _, lane := range []string{"plain", "forced-root"} {
		t.Run(lane, func(t *testing.T) {
			forceMain := lane == "forced-root"
			e, room := world(t)
			target := e.hs.inject(room, "@human:test.local", msg("plain", nil))
			if forceMain {
				target = strings.TrimSpace(e.as("alice", "topic", "new", room, "alpha", "A").ok(t).stdout)
			}
			path := envelopeFile(t, []byte(envelopeCLIJSON))
			args := []string{"post", room, "--re", target, "--envelope", "@" + path, " exact text \n"}
			if forceMain {
				args = append(args, "--reply-main")
			}
			id := strings.TrimSpace(e.as("bob", args...).ok(t).stdout)
			ev, ok := e.hs.eventByID(id)
			if !ok {
				t.Fatal("envelope reply not sent")
			}
			c := decodeContent(t, ev)
			want := map[string]any{"m.in_reply_to": map[string]any{"event_id": target}}
			if c["body"] != " exact text \n" || c[matrix.EnvelopeContentKey] != base64.RawURLEncoding.EncodeToString([]byte(envelopeCLIJSON)) ||
				!reflect.DeepEqual(c["m.relates_to"], want) {
				t.Fatalf("main reply changed envelope/text/relation: %v", c)
			}
		})
	}
}

// Proves an ordinary post is unchanged (header lines, no envelope key) and that
// the projection refuses it with the specific missing-key code, so a missing
// key can never look like an empty envelope. Regression neighbour of the
// carriage change.
func TestEnvelopeOrdinaryPostHasNoEnvelope(t *testing.T) {
	e, room := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	id := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "plain  text ").ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	var content map[string]json.RawMessage
	json.Unmarshal(ev.Content, &content)
	var body string
	json.Unmarshal(content["body"], &body)
	if _, present := content["works.relux.swarm.envelope"]; present || !strings.HasPrefix(body, "- from: bob\n") || !strings.HasSuffix(body, "\n\nplain  text") {
		t.Fatalf("ordinary post changed: %s", ev.Content)
	}
	r := e.as("alice", "read", room, "--event", id, "--envelope-input", "--json")
	r.failsWith(t, ExitError, "envelope refused: envelope_missing")
	if r.stdout != "" {
		t.Fatalf("projection on refusal: %q", r.stdout)
	}
}

// Proves the sender refuses every unsafe or ambiguous shape before any request
// is sent (event count unchanged, nothing on stdout) with the intended code or
// usage error, each beside valid posts of the same shape (the "ok-" rows).
func TestPostEnvelopeRefusals(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	small := write("small", []byte("{}"))
	cases := []struct {
		name  string
		args  []string // after: post proj --topic alpha
		stdin string
		code  int    // expected exit
		frag  string // expected stderr fragment; empty for success rows
	}{
		{"ok-small", []string{"--envelope", "@" + small, "text"}, "", ExitOK, ""},
		{"ok-equals-form", []string{"--envelope=@" + small, "text"}, "", ExitOK, ""},
		{"ok-at-spec-limit-boundary-neighbour", []string{"--envelope", "@" + write("n40000", envelopePattern(40000)), "text"}, "", ExitOK, ""},
		{"ok-text-with-tabs-and-lf", []string{"--envelope", "@" + small, "a\tb\nc"}, "", ExitOK, ""},

		{"oversize-by-one", []string{"--envelope", "@" + write("n65537", make([]byte, 65537)), "text"}, "", ExitError, "envelope refused: envelope_too_large"},
		{"oversize-huge", []string{"--envelope", "@" + write("n300000", make([]byte, 300000)), "text"}, "", ExitError, "envelope refused: envelope_too_large"},
		// 65,536 bytes is within the spec limit but cannot fit one event: a different code.
		{"within-spec-limit-over-event-budget", []string{"--envelope", "@" + write("n65536", make([]byte, 65536)), "text"}, "", ExitError, "envelope refused: event_too_large"},
		{"event-budget-50000", []string{"--envelope", "@" + write("n50000", envelopePattern(50000)), "text"}, "", ExitError, "envelope refused: event_too_large"},
		{"envelope-empty", []string{"--envelope", "@" + write("empty", nil), "text"}, "", ExitError, "envelope refused: envelope_empty"},
		{"envelope-missing-file", []string{"--envelope", "@" + filepath.Join(dir, "absent"), "text"}, "", ExitError, "envelope refused: envelope_unreadable"},
		{"envelope-directory", []string{"--envelope", "@" + dir, "text"}, "", ExitError, "envelope refused: envelope_unreadable"},

		{"text-not-utf8", []string{"--envelope", "@" + small, "-"}, "ok\xff", ExitError, "envelope refused: text_invalid"},
		{"text-with-cr", []string{"--envelope", "@" + small, "a\r\nb"}, "", ExitError, "envelope refused: text_invalid"},
		{"text-with-escape", []string{"--envelope", "@" + small, "a\x1b[31mb"}, "", ExitError, "envelope refused: text_invalid"},
		{"text-with-bidi", []string{"--envelope", "@" + small, "a‮b"}, "", ExitError, "envelope refused: text_invalid"},
		{"text-blank", []string{"--envelope", "@" + small, " \n\t "}, "", ExitError, "envelope refused: text_empty"},
		{"text-too-large", []string{"--envelope", "@" + small, "-"}, strings.Repeat("x", MaxBodyBytes+1), ExitError, "body too large"},

		{"flag-without-file-operand", []string{"--envelope", small, "text"}, "", ExitUsage, "--envelope takes a file operand"},
		{"flag-bare-at", []string{"--envelope", "@", "text"}, "", ExitUsage, "--envelope takes a file operand"},
		{"flag-repeated", []string{"--envelope", "@" + small, "--envelope", "@" + small, "text"}, "", ExitUsage, "exactly once"},
		{"flag-with-voice", []string{"--envelope", "@" + small, "--voice", "text"}, "", ExitUsage, "cannot be combined with --voice"},
		{"two-text-arguments", []string{"--envelope", "@" + small, "two", "words"}, "", ExitUsage, "exactly one text argument"},
		{"text-missing", []string{"--envelope", "@" + small}, "", ExitUsage, "empty message body refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := e.eventCount()
			e.stdin = tc.stdin
			defer func() { e.stdin = "" }()
			r := e.as("bob", append([]string{"post", "proj", "--topic", "alpha"}, tc.args...)...)
			if r.code != tc.code {
				t.Fatalf("exit %d, want %d; stderr %q", r.code, tc.code, r.stderr)
			}
			if tc.code == ExitOK {
				if e.eventCount() != before+1 || !strings.HasPrefix(strings.TrimSpace(r.stdout), "$") {
					t.Fatalf("valid post not sent: events %d->%d out %q", before, e.eventCount(), r.stdout)
				}
				return
			}
			if !strings.Contains(r.stderr, tc.frag) {
				t.Fatalf("stderr %q lacks %q", r.stderr, tc.frag)
			}
			if e.eventCount() != before || r.stdout != "" {
				t.Fatalf("refusal had an effect: events %d->%d stdout %q", before, e.eventCount(), r.stdout)
			}
		})
	}
}

// Proves the published event budget is enforced on the actual content and finds
// its edge through the real command: the largest accepted envelope fits the
// budget, the next byte is refused as event_too_large, and nothing is sent for
// the refusal. The edge depends on the header and text, so it is measured, not
// assumed.
func TestPostEnvelopeBudgetEdge(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	try := func(n int) result {
		p := envelopeFile(t, envelopePattern(n))
		return e.as("bob", "post", "proj", "--topic", "alpha", "--envelope", "@"+p, "edge")
	}
	lo, hi := 30000, 60000 // lo is accepted, hi is refused
	if try(lo).code != ExitOK || try(hi).code == ExitOK {
		t.Fatal("search bounds are not on the right sides of the budget")
	}
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if try(mid).code == ExitOK {
			lo = mid
		} else {
			hi = mid
		}
	}
	before := e.eventCount()
	r := try(hi)
	r.failsWith(t, ExitError, "envelope refused: event_too_large")
	if e.eventCount() != before {
		t.Fatal("over-budget post was sent")
	}
	last := e.hs.lastEvent()
	if len(last.Content) > matrix.EnvelopeEventContentBudget || len(last.Content) < matrix.EnvelopeEventContentBudget-8 {
		t.Fatalf("largest accepted event content is %d bytes; budget %d", len(last.Content), matrix.EnvelopeEventContentBudget)
	}
	t.Logf("largest envelope accepted with this header and text: %d bytes (content %d of %d budget)", lo, len(last.Content), matrix.EnvelopeEventContentBudget)
}

const envelopeCLIText = "hi\\r\\n\\u00e9"

func envelopeCLIEvent(content string) string {
	return `{"event_id":"$selected","sender":"@bob:alpha.invalid","type":"m.room.message","room_id":"!selected:fixture","content":` + content + `}`
}

func envelopeCLIContent() string {
	return `{"msgtype":"m.text","body":"` + envelopeCLIText + `","works.relux.swarm.envelope":"e30","ai.relux.a2a":{"v":1}}`
}

// Proves the projection record against a stubbed endpoint for named profiles:
// full foreign sender spelling and the stored receiver, one GET, exact bytes,
// no store or file side effect.
func TestReadEnvelopeInputObservation(t *testing.T) {
	for _, label := range []string{"alpha", "beta"} {
		t.Run(label, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			before := memberFiles(t, f.root)
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != "GET" || r.URL.Path != "/_matrix/client/v3/rooms/!selected:fixture/event/$selected" || r.Header.Get("Authorization") != "Bearer isolated-"+label {
					t.Fatalf("wrong endpoint %s %s", r.Method, r.URL)
				}
				body := strings.Replace(envelopeCLIEvent(envelopeCLIContent()), "@bob:alpha.invalid", "@Historical:elsewhere.invalid", 1)
				return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			code := a.Run(context.Background(), []string{"--profile", label, "--as", "alice", "read", "!selected:fixture", "--event", "$selected", "--envelope-input", "--json"})
			if code != 0 {
				t.Fatalf("exit=%d %s", code, f.err.String())
			}
			rec := readEnvelopeRecord(t, result{stdout: f.out.String()})
			env, _ := base64.RawURLEncoding.DecodeString(rec.EnvelopeBase64)
			text, _ := base64.RawURLEncoding.DecodeString(rec.BodyTextBase64)
			want := EnvelopeInput{Type: "a2a.event-envelope-input/1", SenderMXID: "@Historical:elsewhere.invalid", ReceiverMXID: "@alice:" + label + ".invalid",
				RequestedRoomID: "!selected:fixture", RequestedEventID: "$selected", ReturnedRoomID: "!selected:fixture", ReturnedEventID: "$selected",
				EnvelopeBase64: rec.EnvelopeBase64, BodyTextBase64: rec.BodyTextBase64}
			if rec != want || string(env) != "{}" || string(text) != "hi\r\né" {
				t.Fatalf("record %+v env %q text %q", rec, env, text)
			}
			if calls != 1 || !reflect.DeepEqual(before, memberFiles(t, f.root)) || len(f.store.creates) != 0 {
				t.Fatal("read made an extra request or write")
			}
		})
	}
}

// Proves each response-level refusal reaches the operator with its own code,
// leaves stdout empty and makes exactly one request, including foreign or
// changed sender shapes and a changed connection binding; the first two rows are
// positive controls (a valid response and a foreign sender succeed).
func TestReadEnvelopeInputResponseRefusals(t *testing.T) {
	valid := envelopeCLIEvent(envelopeCLIContent())
	swap := func(old, next string) string {
		if strings.Count(valid, old) != 1 {
			t.Fatalf("anchor %q", old)
		}
		return strings.Replace(valid, old, next, 1)
	}
	cases := []struct{ name, body, code string }{
		{"valid", valid, ""},
		{"foreign-sender", swap("@bob:alpha.invalid", "@bob:other.invalid"), ""},
		{"sender-short", swap("@bob:alpha.invalid", "bob"), "sender_invalid"},
		{"sender-null", swap(`"@bob:alpha.invalid"`, "null"), "sender_invalid"},
		{"sender-control", swap("@bob:alpha.invalid", `@b\nob:alpha.invalid`), "sender_invalid"},
		{"wrong-room", swap("!selected:fixture", "!other:fixture"), "room_id_mismatch"},
		{"wrong-event", swap(`"event_id":"$selected"`, `"event_id":"$other"`), "event_id_mismatch"},
		{"missing-key", swap(`"works.relux.swarm.envelope":"e30",`, ""), "envelope_missing"},
		{"malformed-key", swap(`"e30"`, `"e30="`), "envelope_malformed"},
		{"key-not-a-string", swap(`"e30"`, `{"a":1}`), "envelope_malformed"},
		{"trailing-data", valid + "{}", "trailing_data"},
		{"duplicate-key", swap(`"works.relux.swarm.envelope":"e30",`, `"works.relux.swarm.envelope":"e30","works.relux.swarm.envelope":"e31",`), "duplicate_key"},
		{"case-alias", swap(`"works.relux.swarm.envelope":"e30",`, `"works.relux.swarm.envelope":"e30","Works.Relux.Swarm.Envelope":"e30",`), "key_case_alias"},
		{"text-not-utf8", swap("hi", "h\xff"), "invalid_utf8"},
		{"empty-redaction", swap(envelopeCLIContent(), "{}"), "content_invalid"},
		{"binding-changed", valid, "binding"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
				calls++
				if tc.code == "binding" {
					c := f.r.Profiles["alpha"]
					c.ID = "changed"
					f.r.Profiles["alpha"] = c
					f.save()
				}
				return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(tc.body)), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})}
			code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--envelope-input", "--json"})
			if calls != 1 {
				t.Fatalf("%d requests, want exactly 1 (no refetch)", calls)
			}
			switch tc.code {
			case "":
				if code != 0 {
					t.Fatalf("valid response refused: exit %d %s", code, f.err.String())
				}
				readEnvelopeRecord(t, result{stdout: f.out.String()})
			case "binding":
				// A changed connection binding is not an envelope refusal; it must
				// still leave no record.
				if code == 0 || f.out.Len() != 0 || strings.Contains(f.err.String(), "envelope refused") {
					t.Fatalf("exit%d out%q err%q", code, f.out.String(), f.err.String())
				}
			default:
				if code != ExitError || f.out.Len() != 0 || !strings.Contains(f.err.String(), "envelope refused: "+tc.code) {
					t.Fatalf("got exit%d out%q err%q, want refusal %s", code, f.out.String(), f.err.String(), tc.code)
				}
			}
		})
	}
}

// Proves prohibited selections and identity overrides fail before any request
// and print nothing; the valid explicit selection is the observation test.
func TestReadEnvelopeInputSelectionRefusals(t *testing.T) {
	for _, tail := range [][]string{
		{"#alias:fixture", "--event", "$selected", "--json"},
		{"!selected:fixture", "--json"},
		{"!selected:fixture", "--event", "$selected"},
		{"!selected:fixture", "--event", "selected", "--json"},
		{"!selected:fixture", "--event", "$selected", "--last", "1", "--json"},
		{"!selected:fixture", "--event", "$selected", "--topic=", "--json"},
		{"!selected:fixture", "--event", "$selected", "--event", "$selected", "--json"},
		{"!selected:fixture", "--event", "$selected", "--json", "--json"},
		{"!selected:fixture", "--event", "$selected", "--json", "--envelope-input"},
		{"!selected:fixture", "--event", "$selected", "--json", "--verification-input"},
		{"!selected:fixture", "--event", "$selected", "--json", "--as", "bob"},
		{"!selected:fixture", "--event", "$selected", "--json", "--as", "alice", "--as", "alice"},
		{"!selected:fixture", "--event", "$selected", "--json", "--profile", "alpha", "--profile", "alpha"},
	} {
		t.Run(strings.Join(tail, " "), func(t *testing.T) {
			f := newNamedFixture(t, true)
			a := f.app()
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })}
			code := a.Run(context.Background(), append([]string{"read", "--envelope-input"}, tail...))
			if code == 0 || calls != 0 || f.out.Len() != 0 {
				t.Fatalf("invalid selection passed: exit%d calls%d out%q err%q", code, calls, f.out.String(), f.err.String())
			}
		})
	}
}

// Proves a stdout failure cannot report success, and a short write is an error.
func TestReadEnvelopeInputOutputFailure(t *testing.T) {
	for _, partial := range []bool{false, true} {
		f := newNamedFixture(t, true)
		a := f.app()
		a.Stdout = verificationBrokenWriter{partial: partial}
		a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
			body := envelopeCLIEvent(envelopeCLIContent())
			return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		if code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--envelope-input", "--json"}); code == 0 {
			t.Fatal("broken writer returned success")
		}
	}
}

// Proves a missing or mismatching stored receiver identity is neither repaired
// nor reconstructed and no request precedes the refusal.
func TestReadEnvelopeInputReceiverIdentityRefusals(t *testing.T) {
	for _, kind := range []string{"missing-metadata", "mismatched-metadata", "missing-selector"} {
		t.Run(kind, func(t *testing.T) {
			f := newNamedFixture(t, kind != "missing-metadata")
			a := f.app()
			before := memberFiles(t, f.root)
			if kind == "mismatched-metadata" {
				path := f.paths("alpha", false).AgentFile("alice")
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err = json.Unmarshal(raw, &fields); err != nil {
					t.Fatal(err)
				}
				fields["user_id"] = json.RawMessage(`"@somebody:alpha.invalid"`)
				if raw, err = json.Marshal(fields); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				before = memberFiles(t, f.root)
			}
			if kind == "missing-selector" {
				old := a.Getenv
				a.Getenv = func(key string) string {
					if key == config.EnvAgent {
						return ""
					}
					return old(key)
				}
			}
			calls := 0
			a.HTTP = &http.Client{Transport: memberReadTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected network") })}
			code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--envelope-input", "--json"})
			if code == 0 || calls != 0 || f.out.Len() != 0 || len(f.store.creates) != 0 || !reflect.DeepEqual(before, memberFiles(t, f.root)) {
				t.Fatalf("receiver refusal failed: exit%d calls%d out%q", code, calls, f.out.String())
			}
		})
	}
}

// Proves the existing strict mode is untouched by the new one: it still emits
// its own record type and still refuses to be combined with the new mode.
func TestVerificationInputUnaffectedByEnvelopeMode(t *testing.T) {
	f := newNamedFixture(t, true)
	a := f.app()
	a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
		body := envelopeCLIEvent(envelopeCLIContent())
		return &http.Response{StatusCode: 200, Request: r, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if code := a.Run(context.Background(), []string{"read", "!selected:fixture", "--event", "$selected", "--verification-input", "--json"}); code != 0 {
		t.Fatalf("exit %d %s", code, f.err.String())
	}
	if !strings.Contains(f.out.String(), `"type":"a2a.event-verification-input/1"`) || strings.Contains(f.out.String(), "envelope_base64") {
		t.Fatalf("verification record changed: %s", f.out.String())
	}
}
