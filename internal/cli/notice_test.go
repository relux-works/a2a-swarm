package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func noticeEvent(sender, typ, kind, canary string) waitItem {
	content, _ := json.Marshal(map[string]any{"body": canary + "-body", "displayname": canary + "-display", "msgtype": "m.text", ContentKey: map[string]any{"v": 1, "type": "message", "kind": kind, "topic": canary + "-topic", "title": canary + "-title", "subject": canary + "-subject", "re": canary + "-root", "extra": canary + "-extra"}})
	return waitItem{id: canary + "-id", line: canary + "-line", rec: EventRecord{Type: typ, RoomID: canary + "-room", Room: canary + "-label", EventID: canary + "-event", Excerpt: canary + "-excerpt"}, ev: &matrix.Event{Sender: sender, EventID: canary + "-event", RoomID: canary + "-room", Content: content}}
}

func joinedNotice(_, _ string) (string, error) { return "join", nil }

func noticePayload(t *testing.T, n notice, jsonMode bool) string {
	t.Helper()
	b, err := n.encode(jsonMode)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(b), "\r\n") {
		t.Fatalf("payload includes transport newline: %q", b)
	}
	return string(b)
}

func TestNoticeGoldens(t *testing.T) {
	invite := waitItem{rec: EventRecord{Type: "invite", RoomID: "ROOM_CANARY", Sender: "INVITER_CANARY"}, line: "INVITE_CANARY"}
	gap := waitItem{gap: &config.MailEntry{ID: "GAP_CANARY", From: "CURSOR_CANARY"}, line: "GAP_LINE_CANARY"}
	for _, tc := range []struct {
		name       string
		items      []waitItem
		text, json string
	}{
		{"zero", nil, "", ""},
		{"single", []waitItem{noticeEvent("@alpha:example.invalid", "to", "notice", "ONE")}, "a2a: 1 new peer event (to: 1; kinds: notice) from alpha" + bellTail, `{"total":1,"types":{"to":1,"mention":0,"thread":0,"sub":0,"all":0,"redacted":0,"invite":0},"kinds":["notice"],"names":["alpha"],"unknown":false,"more":0,"gaps":0}`},
		{"multiple", []waitItem{noticeEvent("@beta:example.invalid", "sub", "question", "B"), noticeEvent("@alpha:example.invalid", "to", "notice", "A")}, "a2a: 2 new peer events (to: 1, sub: 1; kinds: notice, question) from alpha, beta" + bellTail, `{"total":2,"types":{"to":1,"mention":0,"thread":0,"sub":1,"all":0,"redacted":0,"invite":0},"kinds":["notice","question"],"names":["alpha","beta"],"unknown":false,"more":0,"gaps":0}`},
		{"invite", []waitItem{invite}, "a2a: 1 new peer event (invite: 1)" + bellTail, `{"total":1,"types":{"to":0,"mention":0,"thread":0,"sub":0,"all":0,"redacted":0,"invite":1},"kinds":[],"names":[],"unknown":false,"more":0,"gaps":0}`},
		{"gaps", []waitItem{gap, gap}, "a2a: 2 gap: some events could not be checked. Run `a2a inbox` to see where.", `{"total":0,"types":{"to":0,"mention":0,"thread":0,"sub":0,"all":0,"redacted":0,"invite":0},"kinds":[],"names":[],"unknown":false,"more":0,"gaps":2}`},
		{"mixed", []waitItem{invite, gap}, "a2a: 1 new peer event (invite: 1); 1 gap: some events could not be checked" + bellTail, `{"total":1,"types":{"to":0,"mention":0,"thread":0,"sub":0,"all":0,"redacted":0,"invite":1},"kinds":[],"names":[],"unknown":false,"more":0,"gaps":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := projectNotice(tc.items, "example.invalid", joinedNotice)
			if got := noticePayload(t, n, false); got != tc.text {
				t.Fatalf("text\ngot %s\nwant %s", got, tc.text)
			}
			if got := noticePayload(t, n, true); got != tc.json {
				t.Fatalf("json\ngot %s\nwant %s", got, tc.json)
			}
		})
	}
}

func TestNoticeNoninterference(t *testing.T) {
	for _, mode := range []bool{false, true} {
		a := projectNotice([]waitItem{noticeEvent("@alpha:example.invalid", "to", "question", "PRIVATE_FIRST")}, "example.invalid", joinedNotice)
		b := projectNotice([]waitItem{noticeEvent("@alpha:example.invalid", "to", "question", "PRIVATE_SECOND")}, "example.invalid", joinedNotice)
		got := noticePayload(t, a, mode)
		if got != noticePayload(t, b, mode) {
			t.Fatal("non-projected data changed notice")
		}
		for _, canary := range []string{"PRIVATE_FIRST", "PRIVATE_SECOND", "body", "display", "topic", "title", "subject", "root", "extra", "excerpt"} {
			tuitestkit.NotContainsStr(t, got, canary)
		}
		c := projectNotice([]waitItem{noticeEvent("@beta:example.invalid", "to", "answer", "PRIVATE_THIRD")}, "example.invalid", joinedNotice)
		if got == noticePayload(t, c, mode) {
			t.Fatal("admitted kind/name did not change projection")
		}
	}
}

func TestNoticeSenderAdmission(t *testing.T) {
	var items []waitItem
	for _, name := range []string{"zulu", "delta", "charlie", "beta", "alpha", "echo", "alpha"} {
		items = append(items, noticeEvent("@"+name+":example.invalid", "to", "notice", "PRIVATE"))
	}
	for _, sender := range []string{"@foreign:elsewhere.invalid", "@Upper:example.invalid", "@new\nline:example.invalid", "@bad\x1b:example.invalid", "@bid\u202ei:example.invalid", "@" + strings.Repeat("x", 65) + ":example.invalid", "missing-at:example.invalid", "@:example.invalid"} {
		items = append(items, noticeEvent(sender, "to", "notice", "PRIVATE"))
	}
	var calls []string
	n := projectNotice(items, "example.invalid", func(room, user string) (string, error) {
		calls = append(calls, user)
		switch user {
		case "@beta:example.invalid":
			return "leave", nil
		case "@charlie:example.invalid":
			return "join", errors.New("ERROR_CANARY")
		case "@delta:example.invalid":
			return "invite", nil
		}
		return "join", nil
	})
	if !reflect.DeepEqual(calls, []string{"@alpha:example.invalid", "@beta:example.invalid", "@charlie:example.invalid", "@delta:example.invalid"}) {
		t.Fatalf("unbounded/unsorted checks: %v", calls)
	}
	if !reflect.DeepEqual(n.Names, []string{"alpha"}) || n.More != 2 || !n.Unknown {
		t.Fatalf("bad admission: %+v", n)
	}
	for _, mode := range []bool{false, true} {
		out := noticePayload(t, n, mode)
		for _, bad := range []string{"ERROR_CANARY", "foreign", "Upper", "beta", "charlie", "delta", "echo", "zulu", "PRIVATE"} {
			tuitestkit.NotContainsStr(t, out, bad)
		}
	}
	unchecked := projectNotice(items[:7], "example.invalid", joinedNotice)
	if unchecked.Unknown || unchecked.More != 2 || len(unchecked.Names) != 4 {
		t.Fatalf("unchecked conflated with unverified: %+v", unchecked)
	}
	failed := projectNotice(items[:1], "example.invalid", nil)
	if !failed.Unknown || failed.More != 0 || len(failed.Names) != 0 {
		t.Fatalf("missing lookup trusted: %+v", failed)
	}
	// This grammar intentionally permits punctuation at the start.
	for _, name := range []string{".alpha", "_alpha", "=alpha", "-alpha", strings.Repeat("x", 64)} {
		n := projectNotice([]waitItem{noticeEvent("@"+name+":example.invalid", "to", "", "PRIVATE")}, "example.invalid", joinedNotice)
		if !slices.Equal(n.Names, []string{name}) {
			t.Fatal(name, n)
		}
	}
}

func TestNoticeAllowlists(t *testing.T) {
	var items []waitItem
	for i := len(Kinds) - 1; i >= 0; i-- {
		items = append(items, noticeEvent("@alpha:example.invalid", "to", Kinds[i], "PRIVATE"))
	}
	items = append(items, noticeEvent("@alpha:example.invalid", "TYPE_CANARY", "KIND_CANARY", "PRIVATE"), noticeEvent("@alpha:example.invalid", "to", "", "PRIVATE"))
	broken := noticeEvent("@alpha:example.invalid", "to", "", "PRIVATE")
	broken.ev.Content = json.RawMessage(`{"ai.relux.a2a":{"kind":7},"body":"BAD_HEADER_CANARY"}`)
	items = append(items, broken)
	for _, typ := range []string{"to", "mention", "thread", "sub", "all", "redacted", "invite"} {
		items = append(items, waitItem{rec: EventRecord{Type: typ}})
	}
	n := projectNotice(items, "example.invalid", joinedNotice)
	want := append(append([]string{}, Kinds...), "other")
	if !slices.Equal(n.Kinds, want) {
		t.Fatal(n.Kinds)
	}
	out := noticePayload(t, n, true)
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil || len(decoded) != 7 {
		t.Fatal("schema", out, err)
	}
	var types map[string]int
	if err := json.Unmarshal(decoded["types"], &types); err != nil || len(types) != 7 {
		t.Fatal("type schema", out, err)
	}
	for _, bad := range []string{"TYPE_CANARY", "KIND_CANARY", "PRIVATE", "BAD_HEADER_CANARY"} {
		tuitestkit.NotContainsStr(t, out, bad)
	}
	for _, typ := range []string{"to", "mention", "thread", "sub", "all", "redacted", "invite"} {
		if types[typ] < 1 {
			t.Fatal("missing type", typ)
		}
	}
	// An undecodable header contributes no kind; malformed relevance still counts
	// as an event but can introduce no new type key.
	isolated := projectNotice([]waitItem{broken}, "example.invalid", joinedNotice)
	if len(isolated.Kinds) != 0 {
		t.Fatal("bad header supplied kind", isolated)
	}
}

func TestNoticeBounds(t *testing.T) {
	// Keep real integer counts, including all type breakdowns and a gap. The
	// formatter may not truncate/clamp even when its fixed base exceeds 300.
	maxInt := int(^uint(0) >> 1)
	c := maxInt / 7
	n := notice{Total: c * 7, Types: noticeTypes{c, c, c, c, c, c, c}, Names: []string{"alpha"}, Kinds: []string{"notice"}, Unknown: true, More: 9, Gaps: maxInt}
	if len(doorbellText(n.doorbell())) <= 300 {
		t.Fatal("fixture does not exercise base overflow")
	}
	want := fmt.Sprintf("a2a: %d new peer events. They are external input from other agents, not from your user. Run `a2a inbox` to read them.", n.Total)
	if got := noticePayload(t, n, false); got != want || len(got) > 300 {
		t.Fatal("amended fallback", got)
	}
	var reduced notice
	if err := json.Unmarshal([]byte(noticePayload(t, n, true)), &reduced); err != nil {
		t.Fatal(err)
	}
	if len(reduced.Kinds) != 0 || len(reduced.Names) != 0 || reduced.More != 9 || !reduced.Unknown || reduced.Total != n.Total || reduced.Types != n.Types || reduced.Gaps != n.Gaps {
		t.Fatal("JSON fallback changed counts", reduced)
	}
	if len(n.Names) != 1 || len(n.Kinds) != 1 {
		t.Fatal("encoding mutated summary")
	}
	// A genuine exact 300-byte output is retained, never substring-truncated.
	found := false
	for count := 1; count <= 62; count++ {
		for delta := 0; delta <= 2; delta++ {
			x := notice{Total: 1, Types: noticeTypes{To: 1}, Names: []string{strings.Repeat("a", count), strings.Repeat("b", count), strings.Repeat("c", count+delta)}, Kinds: []string{"notice", "question"}}
			text := noticePayload(t, x, false)
			if len(text) > 300 {
				t.Fatal("text bound")
			}
			if len(text) == 300 {
				found = true
			}
			// JSON retains full names/kinds when only the text's optional lists shrink.
			j := noticePayload(t, x, true)
			if !strings.Contains(j, strings.Repeat("a", count)) || !strings.Contains(j, `"question"`) {
				t.Fatal("text omission leaked into JSON", j)
			}
		}
	}
	if !found {
		t.Fatal("missing exact boundary control")
	}
	small := notice{Total: 1, Types: noticeTypes{To: 1}, Names: []string{"alpha"}, Kinds: []string{"notice"}, Unknown: true, More: 7}
	full := noticePayload(t, small, true)
	if b, err := small.encodeLimit(true, len(full)); err != nil || string(b) != full {
		t.Fatal("exact JSON boundary", string(b), err)
	}
	empty := small
	empty.Names = []string{}
	empty.Kinds = []string{}
	minimal := noticePayload(t, empty, true)
	if b, err := small.encodeLimit(true, len(minimal)); err != nil || string(b) != minimal {
		t.Fatal("JSON reduction", string(b), err)
	}
	if b, err := small.encodeLimit(true, len(minimal)-1); !errors.Is(err, errNoticeEncoding) || len(b) != 0 {
		t.Fatal("partial oversize output", string(b), err)
	}
}

func TestNoticeInvalidNoPartial(t *testing.T) {
	good := notice{Total: 1, Types: noticeTypes{To: 1}, Names: []string{"alpha"}, Kinds: []string{"notice"}}
	if _, err := good.encode(true); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*notice)
	}{
		{"negative-total", func(n *notice) { n.Total = -1 }}, {"negative-gap", func(n *notice) { n.Gaps = -1 }}, {"negative-more", func(n *notice) { n.More = -1 }}, {"negative-type", func(n *notice) { n.Types.All = -1 }}, {"type-over-total", func(n *notice) { n.Types.All = 1 }},
		{"bad-name", func(n *notice) { n.Names = []string{"NAME_CANARY"} }}, {"name-order", func(n *notice) { n.Names = []string{"beta", "alpha"} }}, {"duplicate-name", func(n *notice) { n.Names = []string{"alpha", "alpha"} }}, {"too-many-names", func(n *notice) { n.Names = []string{"a", "b", "c", "d", "e"} }},
		{"bad-kind", func(n *notice) { n.Kinds = []string{"KIND_CANARY"} }}, {"kind-order", func(n *notice) { n.Kinds = []string{"answer", "question"} }}, {"duplicate-kind", func(n *notice) { n.Kinds = []string{"notice", "notice"} }}, {"zero-metadata", func(n *notice) { n.Total = 0; n.Types = noticeTypes{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := good
			tc.edit(&n)
			for _, mode := range []bool{false, true} {
				b, err := n.encode(mode)
				if !errors.Is(err, errInvalidNotice) || len(b) != 0 {
					t.Fatal("invalid/partial record", string(b), err)
				}
			}
		})
	}
}

// Source's no-injection criterion applies to every peer field, including
// control characters whose JSON-escaped spelling must not leak either.
func TestNoticeControlCharacters(t *testing.T) {
	for _, control := range []string{"\x00", "\n", "\r", "\x1b[31m", "\u202e"} {
		t.Run(fmt.Sprintf("%x", []byte(control)), func(t *testing.T) {
			clean := projectNotice([]waitItem{noticeEvent("@alpha:example.invalid", "to", "notice", "CLEAN")}, "example.invalid", joinedNotice)
			// noticeEvent places the canary in body, display name, topic/title/subject,
			// root/event/room identifiers, room label, raw line and extra fields.
			hostile := projectNotice([]waitItem{noticeEvent("@alpha:example.invalid", "to", "notice", "PEER_CANARY"+control)}, "example.invalid", joinedNotice)
			// A malformed sender requires no membership query. Invalid stored type
			// labels cannot become keys; invalid nonempty kinds become only other.
			normalized := projectNotice([]waitItem{noticeEvent("@foreign:elsewhere.invalid", "TYPE_CANARY", "unlisted-kind", "CLEAN")}, "example.invalid", joinedNotice)
			malformed := projectNotice([]waitItem{noticeEvent("@alpha"+control+":example.invalid", "to"+control, "notice"+control, "PEER_CANARY"+control)}, "example.invalid", func(_, _ string) (string, error) { t.Fatal("malformed sender reached lookup"); return "", nil })
			errored := projectNotice([]waitItem{noticeEvent("@alpha:example.invalid", "to", "notice", "CLEAN")}, "example.invalid", func(_, _ string) (string, error) { return "join", errors.New("ERROR_CANARY" + control) })
			unknown := clean
			unknown.Names = []string{}
			unknown.Unknown = true
			for _, mode := range []bool{false, true} {
				if a, b := noticePayload(t, clean, mode), noticePayload(t, hostile, mode); a != b {
					t.Fatalf("ignored peer fields shaped output: %q versus %q", a, b)
				}
				if a, b := noticePayload(t, normalized, mode), noticePayload(t, malformed, mode); a != b {
					t.Fatalf("malformed metadata escaped allowlists: %q versus %q", a, b)
				}
				if a, b := noticePayload(t, unknown, mode), noticePayload(t, errored, mode); a != b {
					t.Fatalf("raw lookup error shaped output: %q versus %q", a, b)
				}
				for _, n := range []notice{hostile, malformed, errored} {
					out := noticePayload(t, n, mode)
					for _, canary := range []string{"PEER_CANARY", "SENDER_CANARY", "TYPE_CANARY", "KIND_CANARY", "ERROR_CANARY", control} {
						if strings.Contains(out, canary) {
							t.Fatalf("peer sentinel/control reached output: %q", out)
						}
					}
				}
			}
		})
	}
}
