package cli

import (
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Proves the rule-10 request flow: sub-request posts kind=subscribe addressed
// to the target with the selector in the structured header; until the target
// accepts, nothing is subscribed and matching traffic is not delivered;
// sub-accept creates the sub (source = the request), replies kind=accept
// re=<request>, and from then on matching events are delivered. Accepting
// twice creates nothing new.
func TestSubRequestAccept(t *testing.T) {
	e, room := subWorld(t)
	e.as("bob", "topic", "new", "proj", "alpha", "A").ok(t)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	req := strings.TrimSpace(e.as("bob", "sub-request", "alice", "--room", "proj", "--topic", "alpha",
		"--subject", "BUG-1", "--until-kind", "system", "--note", "tell me when it lands").ok(t).stdout)
	ev, _ := e.hs.eventByID(req)
	h, _ := decodeHeader(ev.Message().A2A)
	if h.Kind != "subscribe" || strings.Join(h.To, ",") != "alice" || h.SubRequest == nil ||
		h.SubRequest.Subject != "BUG-1" || h.SubRequest.Room != room || h.SubRequest.UntilKind != "system" {
		t.Fatalf("request header = %+v sel %+v", h, h.SubRequest)
	}
	tuitestkit.ContainsStr(t, ev.Message().Body, "subscription request: room=#proj subject=BUG-1 until-kind=system\n\ntell me when it lands")
	recs := decodeRecords(t, e.as("alice", "wait", "--timeout", "2s", "--json").ok(t).stdout)
	if len(recs) != 1 || recs[0].Type != "to" || recs[0].EventID != req || recs[0].SubRequest == nil || recs[0].SubRequest.Subject != "BUG-1" {
		t.Fatalf("request ring = %+v", recs)
	}

	e.hs.inject(room, "@bob:test.local", msg("BUG-1 progress before accept", nil))
	if r := e.as("alice", "wait", "--timeout", "80ms").ok(t); r.stdout != "timeout\n" {
		t.Fatalf("unaccepted request delivered: %q", r.stdout)
	}
	if out := e.as("alice", "subs").ok(t).stdout; out != "" {
		t.Fatalf("unaccepted request created %q", out)
	}

	out := strings.TrimSpace(e.as("alice", "sub-accept", "proj", req).ok(t).stdout)
	m := subLineRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("sub-accept printed %q", out)
	}
	tuitestkit.ContainsStr(t, m[2], "source="+req)
	acc := e.hs.lastEvent()
	ah, _ := decodeHeader(acc.Message().A2A)
	if acc.Sender != "@alice:test.local" || ah.Kind != "accept" || ah.Re != req || strings.Join(ah.To, ",") != "bob" || acc.ThreadRoot() != ev.ThreadRoot() {
		t.Fatalf("accept reply = %+v %+v", acc, ah)
	}
	sends := e.hs.CallCount(sendRoute)
	tuitestkit.ContainsStr(t, e.as("alice", "sub-accept", "proj", req).ok(t).stdout, m[1])
	if n := len(e.serverSubs("alice").Subs); n != 1 || e.hs.CallCount(sendRoute) != sends {
		t.Fatalf("second accept: %d subs, %d new sends", n, e.hs.CallCount(sendRoute)-sends)
	}

	id := e.hs.inject(room, "@bob:test.local", msg("BUG-1 progress after accept", nil))
	tuitestkit.ContainsStr(t, e.as("alice", "wait", "--timeout", "2s").ok(t).stdout, "sub #proj "+id+" from=@bob:test.local sub="+m[1])
}

// Proves sub-accept refuses a request addressed to someone else and an event
// that is not a subscription request, creating nothing; the addressee's own
// accept of the same request succeeds (positive control).
func TestSubAcceptRefused(t *testing.T) {
	e, _ := subWorld(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	req := strings.TrimSpace(e.as("alice", "sub-request", "bob", "--room", "proj", "--topic", "alpha", "--kind", "decision").ok(t).stdout)
	plain := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "--to", "alice", "just a notice").ok(t).stdout)
	sends := e.hs.CallCount(sendRoute)
	e.as("alice", "sub-accept", "proj", req).failsWith(t, ExitError, "addressed to bob, not to alice; refused")
	e.as("alice", "sub-accept", "proj", plain).failsWith(t, ExitError, "is not a subscription request")
	e.as("alice", "sub-accept", "proj", "$nosuch").failsWith(t, ExitError, "not found")
	e.as("alice", "sub-accept", "proj").failsWith(t, ExitUsage, "usage")
	e.as("alice", "sub-accept", "proj", "ev1").failsWith(t, ExitUsage, "usage")
	if n := len(e.serverSubs("alice").Subs); n != 0 || e.hs.CallCount(sendRoute) != sends {
		t.Fatalf("refused accepts created %d subs, sent %d", n, e.hs.CallCount(sendRoute)-sends)
	}
	e.as("bob", "sub-accept", "proj", req).ok(t)
	if n := len(e.serverSubs("bob").Subs); n != 1 {
		t.Fatalf("bob holds %d subs", n)
	}
}

// Proves sub-request refuses incomplete or invalid selectors with a usage
// error before any request; a valid request is sent (positive control).
func TestSubRequestNegative(t *testing.T) {
	e, _ := subWorld(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	before := len(e.hs.Calls())
	cases := []struct {
		args     []string
		fragment string
	}{
		{[]string{"bob", "--room", "proj", "--kind", "decision"}, "usage"},
		{[]string{"bob", "--topic", "alpha", "--kind", "decision"}, "usage"},
		{[]string{"--room", "proj", "--topic", "alpha", "--kind", "decision"}, "usage"},
		{[]string{"bob", "--room", "proj", "--topic", "alpha"}, "name at least one selector"},
		{[]string{"bob", "--room", "proj", "--topic", "alpha", "--kind", "gossip"}, "invalid --kind"},
		{[]string{"bob", "--room", "proj", "--topic", "alpha", "--sub-room", "all", "--all-events"}, "--all-events needs a concrete --room"},
		{[]string{"bad user", "--room", "proj", "--topic", "alpha", "--kind", "decision"}, "invalid user"},
	}
	for _, tc := range cases {
		e.as("alice", append([]string{"sub-request"}, tc.args...)...).failsWith(t, ExitUsage, tc.fragment)
	}
	if n := len(e.hs.Calls()); n != before {
		t.Fatalf("usage errors reached the server: %d requests", n-before)
	}
	e.as("alice", "sub-request", "bob", "--room", "proj", "--topic", "alpha", "--sub-room", "all", "--from", "carol").ok(t)
	h, _ := decodeHeader(e.hs.lastEvent().Message().A2A)
	if h.SubRequest == nil || h.SubRequest.Room != "" || h.SubRequest.From != "@carol:test.local" {
		t.Fatalf("selector = %+v", h.SubRequest)
	}
}
