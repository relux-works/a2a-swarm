package audience

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func edit(id, target, sender string) matrix.Event {
	ev := event(fmt.Sprintf(`{"msgtype":"m.text","body":"replacement","m.relates_to":{"rel_type":"m.replace","event_id":%q},"m.new_content":{"msgtype":"m.text","body":"new","m.mentions":{"user_ids":["@alpha:example.org"]},"ai.relux.a2a":{"to":["alpha"]}}}`, target))
	ev.EventID, ev.Sender = id, sender
	return ev
}

// Independent replies use their own recipients; valid same-sender edits and
// redactions inherit only the immutable target ceiling, never replacement text.
func TestRelationAudience(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprintf("included=%t", own), func(t *testing.T) {
			to, want := "beta", Other
			if own {
				to, want = "alpha", Self
			}
			original := event(fmt.Sprintf(`{"ai.relux.a2a":{"to":[%q]}}`, to))
			c := NewCycle("@alpha:example.org", "alpha", time.Unix(1, 0), nil, nil)
			c.Add("!room:example.org", []matrix.Event{original})
			got := c.Resolve("!room:example.org", edit("$edit", original.EventID, original.Sender))
			if got.Decision != want || !got.Inherited {
				t.Fatalf("edit=%+v", got)
			}
			reply := event(`{"m.relates_to":{"m.in_reply_to":{"event_id":"$message"}},"m.mentions":{"user_ids":["@beta:example.org"]}}`)
			reply.EventID = "$reply"
			if got := c.Resolve("!room:example.org", reply); got.Decision != Other || got.Inherited {
				t.Fatalf("reply=%+v", got)
			}
			redaction := matrix.Event{EventID: "$redaction", Type: "m.room.redaction", Sender: original.Sender, Redacts: original.EventID, Content: json.RawMessage(`{}`)}
			if got := c.Resolve("!room:example.org", redaction); got.Decision != want || !got.Inherited {
				t.Fatalf("redaction=%+v", got)
			}
			reaction := matrix.Event{EventID: "$reaction", Type: "m.reaction", Content: json.RawMessage(`{"m.relates_to":{"rel_type":"m.annotation","event_id":"$message"}}`)}
			if got := c.Resolve("!room:example.org", reaction); got.Allows(true) {
				t.Fatal("reaction introduced wake")
			}
			if got := c.Resolve("!room:elsewhere.example", edit("$wrong-room", original.EventID, original.Sender)); got.Decision != Unresolved {
				t.Fatal("cross-room target inherited")
			}
			if got := c.Resolve("!room:example.org", edit("$wrong-sender", original.EventID, "@other:example.org")); got.Decision != Unresolved {
				t.Fatal("cross-sender edit inherited")
			}
		})
	}
}

// Lookup budgets count failures and distinct targets once across a cycle;
// unavailable, cyclic and over-depth relations stay deferred without becoming
// legacy general. A bounded successful lookup is the nearby positive control.
func TestTargetResolutionBounds(t *testing.T) {
	const room = "!room:example.org"
	calls := map[Key]int{}
	fetch := func(k Key) (matrix.Event, error) {
		calls[k]++
		if k.EventID == "$available" {
			e := event(`{"ai.relux.a2a":{"to":["alpha"]}}`)
			e.EventID = k.EventID
			return e, nil
		}
		return matrix.Event{}, errors.New("fixture unavailable")
	}
	c := NewCycle("@alpha:example.org", "alpha", time.Unix(1, 0), nil, fetch)
	if got := c.Resolve(room, edit("$valid", "$available", "@sender:example.org")); got.Decision != Self {
		t.Fatalf("positive=%+v", got)
	}
	for i := 0; i < 9; i++ {
		if got := c.Resolve(room, edit(fmt.Sprintf("$edit%d", i), fmt.Sprintf("$absent%d", i%6), "@sender:example.org")); got.Decision != Unresolved || got.Target == "" {
			t.Fatal("unresolved target widened")
		}
	}
	total := 0
	for _, n := range calls {
		total += n
		if n != 1 {
			t.Fatal("distinct target fetched twice")
		}
	}
	if total != 4 {
		t.Fatalf("fetches=%d, want exactly bounded four", total)
	}
	c = NewCycle("@alpha:example.org", "alpha", time.Unix(1, 0), nil, nil)
	a, b := edit("$a", "$b", "@sender:example.org"), edit("$b", "$a", "@sender:example.org")
	c.Add(room, []matrix.Event{a, b})
	if got := c.Resolve(room, a); got.Decision != Unresolved {
		t.Fatal("cycle widened")
	}
	var chain []matrix.Event
	for i := 0; i < 9; i++ {
		chain = append(chain, edit(fmt.Sprintf("$h%d", i), fmt.Sprintf("$h%d", i+1), "@sender:example.org"))
	}
	end := event(`{"ai.relux.a2a":{"to":["alpha"]}}`)
	end.EventID = "$h9"
	chain = append(chain, end)
	c = NewCycle("@alpha:example.org", "alpha", time.Unix(1, 0), nil, nil)
	c.Add(room, chain)
	if got := c.Resolve(room, chain[0]); got.Decision != Unresolved {
		t.Fatal("over-depth chain admitted")
	}
	for _, start := range []int{1, 2} {
		c = NewCycle("@alpha:example.org", "alpha", time.Unix(1, 0), nil, nil)
		c.Add(room, chain[start:])
		if got := c.Resolve(room, chain[start]); got.Decision != Self {
			t.Fatalf("bounded chain with %d hops rejected", 9-start)
		}
	}
}

// Unsupported evidence is scoped to its event, and a wire payload cannot forge
// the local-only pre-scrub witness or turn an unknown target into general mail.
func TestEvidenceVersionAndLocalWitness(t *testing.T) {
	known := Evidence{Version: 99, Room: "!room:example.org", EventID: "$message", UserID: "@alpha:example.org", Name: "alpha", Decision: Self}
	c := NewCycle(known.UserID, known.Name, time.Unix(1, 0), func(k Key) (Evidence, bool) { return known, k.EventID == known.EventID }, nil)
	if got := c.Resolve(known.Room, event(`{}`)); got.Diagnostic() != AddressStateUnavailable {
		t.Fatal("unknown version opened")
	}
	valid := event(`{"ai.relux.a2a":{"to":["alpha"]}}`)
	valid.EventID = "$unaffected"
	if got := c.Resolve(known.Room, valid); !got.Allows(false) {
		t.Fatal("one unknown version blocked unrelated event")
	}
	var forged matrix.Event
	if err := json.Unmarshal([]byte(`{"event_id":"$forged","type":"m.room.message","content":{"ai.relux.a2a":{"to":["beta"]}},"NotificationAudience":{"decision":"self"}}`), &forged); err != nil {
		t.Fatal(err)
	}
	if len(forged.NotificationAudience) != 0 || c.Resolve(known.Room, forged).Allows(false) {
		t.Fatal("wire forged local witness")
	}
	missing := edit("$deferred", "$late", "@sender:example.org")
	first := NewCycle(known.UserID, known.Name, time.Unix(1, 0), nil, nil)
	deferred := first.Resolve(known.Room, missing)
	deferredID := missing.EventID
	calls := 0
	late := NewCycle(known.UserID, known.Name, time.Unix(2, 0), func(k Key) (Evidence, bool) { return deferred, k.EventID == deferredID }, func(k Key) (matrix.Event, error) {
		calls++
		ev := event(`{"ai.relux.a2a":{"to":["alpha"]}}`)
		ev.EventID = k.EventID
		return ev, nil
	})
	if got := late.Resolve(known.Room, missing.WithoutContent()); got.Target != "$late" || got.Decision != Unresolved || got.Allows(true) || calls != 0 {
		t.Fatal("scrubbed deferred reference was lost or invented selector eligibility")
	}
	missing.EventID = "$intact"
	if got := late.Resolve(known.Room, missing); got.Decision != Self || calls != 1 {
		t.Fatal("intact neighboring reference did not recover")
	}
}

// A pinned sender cannot turn invalid relation structure into the R1 metadata
// exception. Correct same-sender shape beside it still resolves its audience.
func TestMalformedRelationIsNotOwnerMetadataException(t *testing.T) {
	const room = "!room:example.org"
	original := event(`{"ai.relux.a2a":{"to":["alpha"]}}`)
	for _, invalid := range []string{
		`{"m.relates_to":{"rel_type":"m.replace"},"ai.relux.a2a":{"to":"alpha"}}`,
		`{"msgtype":"m.text","body":7,"m.relates_to":{"rel_type":"m.replace","event_id":"$message"},"m.new_content":{"msgtype":"m.text","body":"new"}}`,
		`{"msgtype":"m.text","body":"edit","m.relates_to":{"rel_type":"m.replace","event_id":"$message"},"m.new_content":[]}`,
	} {
		c := NewCycle("@alpha:example.org", "alpha", time.Unix(1, 0), nil, nil)
		c.Add(room, []matrix.Event{original})
		bad := event(invalid)
		bad.EventID = "$bad"
		if got := c.Resolve(room, bad); got.Diagnostic() != AddressTargetUnresolved || got.Allows(true) {
			t.Fatalf("invalid relation rescued by owner exception: %s", got.Diagnostic())
		}
		if got := c.Resolve(room, edit("$valid", original.EventID, original.Sender)); !got.Allows(false) {
			t.Fatal("valid neighboring relation rejected")
		}
	}
}
