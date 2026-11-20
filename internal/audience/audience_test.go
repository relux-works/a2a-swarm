package audience

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func event(content string) matrix.Event {
	return matrix.Event{EventID: "$message", Type: "m.room.message", Sender: "@sender:example.org", Content: json.RawMessage(content)}
}

// Header precedence and legacy fallbacks admit the intended recipient only;
// malformed peer metadata cannot turn into a general message.
func TestWireAudience(t *testing.T) {
	for _, tc := range []struct{ name, raw, want, source string }{
		{"header-self", `{"ai.relux.a2a":{"to":["alpha"]},"m.mentions":{"user_ids":["@beta:example.org"]}}`, Self, Header},
		{"header-other", `{"ai.relux.a2a":{"to":["@beta:example.org"]},"m.mentions":{"user_ids":["@alpha:example.org"]}}`, Other, Header},
		{"multiple", `{"ai.relux.a2a":{"to":["@beta:example.org","@alpha:example.org"]}}`, Self, Header},
		{"foreign-server", `{"ai.relux.a2a":{"to":["@alpha:elsewhere.example"]}}`, Other, Header},
		{"case-sensitive", `{"ai.relux.a2a":{"to":["Alpha"]}}`, Other, Header},
		{"all-mixed", `{"ai.relux.a2a":{"to":["all","beta"]},"m.mentions":{"user_ids":["@beta:example.org"]}}`, General, Header},
		{"missing", `{}`, General, Legacy},
		{"null", `{"ai.relux.a2a":{"to":null}}`, General, Legacy},
		{"empty", `{"ai.relux.a2a":{"to":[]},"m.mentions":{"user_ids":["@alpha:example.org"]}}`, Self, Mentions},
		{"mentions-other", `{"m.mentions":{"user_ids":["@beta:example.org"]}}`, Other, Mentions},
		{"mentions-room", `{"m.mentions":{"room":true,"user_ids":["@beta:example.org"]}}`, General, Mentions},
		{"empty-mentions", `{"m.mentions":{}}`, General, Legacy},
		{"scalar", `{"ai.relux.a2a":{"to":"alpha"}}`, Invalid, Header},
		{"null-member", `{"ai.relux.a2a":{"to":[null]}}`, Invalid, Header},
		{"invalid-utf8-recipient", "{\"ai.relux.a2a\":{\"to\":[\"@\xff:example.org\"]}}", Invalid, Header},
		{"empty-member", `{"ai.relux.a2a":{"to":[""]}}`, Invalid, Header},
		{"bad-mention", `{"m.mentions":{"user_ids":["alpha"]}}`, Invalid, Mentions},
		{"duplicate-to", `{"ai.relux.a2a":{"to":["alpha"],"to":["beta"]}}`, Invalid, Header},
		{"unrelated-duplicate-body", `{"body":"first","body":"second","ai.relux.a2a":{"to":["beta"]}}`, Other, Header},
		{"wrong-room-type", `{"m.mentions":{"room":"true"}}`, Invalid, Mentions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Parse(event(tc.raw), "!room:example.org", "@alpha:example.org", "alpha", time.Unix(1, 0))
			if got.Decision != tc.want || got.Source != tc.source {
				t.Fatalf("decision/source=%s/%s, want %s/%s", got.Decision, got.Source, tc.want, tc.source)
			}
		})
	}
}

// R1 applies only to invalid metadata, never a well-formed other audience,
// unresolved target or unsupported stored version.
func TestMalformedOwnerTriplet(t *testing.T) {
	for _, tc := range []struct {
		decision    string
		owner, want bool
	}{
		{Invalid, true, true}, {Invalid, false, false}, {Other, true, false},
		{Unresolved, true, false}, {Unavailable, true, false},
		{General, false, true}, {Self, false, true},
	} {
		e := Evidence{Version: Version, Decision: tc.decision}
		if e.Allows(tc.owner) != tc.want {
			t.Fatalf("%+v: owner=%v", e, tc.owner)
		}
	}
	if (Evidence{Version: 99, Decision: Self}).Allows(true) {
		t.Fatal("unknown evidence version admitted")
	}
}
