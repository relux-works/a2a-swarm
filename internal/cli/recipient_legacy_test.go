package cli

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// The shared helper keeps main's historical normalization. Strict outgoing
// validation and full explicit headers belong only to messageAddressees.
func TestRecipientLegacyAddressees(t *testing.T) {
	for _, tc := range []struct {
		target, name, mention string
	}{
		{"@Lead:Alpha.example", "@lead:Alpha.example", "@lead:Alpha.example"},
		{"@Lead:test.local", "lead", "@lead:test.local"},
		{"@Guest:", "guest", "@guest:test.local"},
		{"Guest:Other.invalid", "@guest:Other.invalid", "@guest:Other.invalid"},
	} {
		t.Run(tc.target, func(t *testing.T) {
			names, mentions, err := addressees(tc.target, "test.local")
			if err != nil || !slices.Equal(names, []string{tc.name}) || !slices.Equal(mentions, []string{tc.mention}) {
				t.Fatalf("legacy addressees changed: %v / %v err=%v", names, mentions, err)
			}
		})
	}
}

// Exercise the actual alarm sender, rather than only its shared helper. All
// traffic is handled by the in-memory fake homeserver; no runtime is started.
func TestRecipientLegacyMailAlarm(t *testing.T) {
	for _, tc := range []struct {
		name, target, wantHeader, wantMention string
	}{
		{"uppercase", "@Lead:Alpha.example", "@lead:Alpha.example", "@lead:Alpha.example"},
		{"lowercase", "@lead:alpha.example", "@lead:alpha.example", "@lead:alpha.example"},
		{"local-uppercase", "@Lead:" + testServer, "lead", "@lead:" + testServer},
		{"overlong", "@" + strings.Repeat("A", 256) + ":alpha.example", "@" + strings.Repeat("a", 256) + ":alpha.example", "@" + strings.Repeat("a", 256) + ":alpha.example"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e, a, s, _, sample := alarmFixture(t)
			notificationState(t, e, func(st *config.State) {
				st.MailAlarmConfig.Owner = "off"
				st.MailAlarmConfig.Operator = tc.target
			})
			observeAlarmWindow(a, s, sample)
			if jobs := e.state("alice").MailAlarm.Jobs; len(jobs) != 1 || jobs[0].Target != tc.target {
				t.Fatalf("alarm fixture did not queue the configured target: %+v", jobs)
			}
			before := e.hs.CallCount(sendRoute)
			a.sendMailAlarm(context.Background(), s, "alarm")
			m := e.hs.lastEvent().Message()
			h, ok := decodeHeader(m.A2A)
			if e.hs.CallCount(sendRoute) != before+1 || !ok || !slices.Equal(h.To, []string{tc.wantHeader}) ||
				m.Mentions == nil || !slices.Equal(m.Mentions.UserIDs, []string{tc.wantMention}) {
				t.Fatalf("alarm recipients changed: header=%+v mentions=%+v", h, m.Mentions)
			}
			if len(e.state("alice").MailAlarm.Jobs) != 0 {
				t.Fatal("successful alarm did not finish its queued job")
			}
		})
	}
}

func TestRecipientLegacySubRequest(t *testing.T) {
	for _, tc := range []struct{ target, name, mention string }{
		{"@Guest:Other.invalid", "@guest:Other.invalid", "@guest:Other.invalid"},
		{"@Bob:" + testServer, "bob", "@bob:" + testServer},
	} {
		t.Run(tc.target, func(t *testing.T) {
			e, room := subWorld(t)
			root := strings.TrimSpace(e.as("alice", "topic", "new", room, "root", "Root").ok(t).stdout)
			before := e.hs.CallCount(sendRoute)
			r := e.as("alice", "sub-request", tc.target, "--room", room, "--topic", root, "--kind", "decision").ok(t)
			m := e.hs.lastEvent().Message()
			h, ok := decodeHeader(m.A2A)
			if r.stderr != "" || e.hs.CallCount(sendRoute) != before+1 || !ok || h.Kind != "subscribe" ||
				!slices.Equal(h.To, []string{tc.name}) || m.Mentions == nil || !slices.Equal(m.Mentions.UserIDs, []string{tc.mention}) {
				t.Fatalf("sub-request recipients changed: header=%+v mentions=%+v stderr=%q", h, m.Mentions, r.stderr)
			}
		})
	}
}

// A historical received sender must still get the same normalized accept reply
// as main, after exactly one subscription write. Retrying remains idempotent.
func TestRecipientLegacySubAccept(t *testing.T) {
	for _, tc := range []struct{ sender, name, mention string }{
		{"@Guest:Other.invalid", "@guest:Other.invalid", "@guest:Other.invalid"},
		{"@Bob:" + testServer, "bob", "@bob:" + testServer},
	} {
		t.Run(tc.sender, func(t *testing.T) {
			e, room := subWorld(t)
			h := Header{V: 1, Type: "message", From: "Guest", To: []string{"alice"}, Kind: "subscribe",
				SubRequest: &SubSelector{Room: room, Kinds: []string{"decision"}}}
			req := e.hs.inject(room, tc.sender, msg("historical subscription request", map[string]any{ContentKey: h}))
			before := e.hs.CallCount(sendRoute)
			r := e.as("alice", "sub-accept", room, req).ok(t)
			ev := e.hs.lastEvent()
			m := ev.Message()
			ah, ok := decodeHeader(m.A2A)
			if r.stderr != "" || e.hs.CallCount(sendRoute) != before+1 || !ok || ah.Kind != "accept" || ah.Re != req || ah.Topic != req ||
				!slices.Equal(ah.To, []string{tc.name}) || m.Mentions == nil || !slices.Equal(m.Mentions.UserIDs, []string{tc.mention}) {
				t.Fatalf("historical accept reply changed: header=%+v mentions=%+v stderr=%q", ah, m.Mentions, r.stderr)
			}
			list := e.serverSubs("alice")
			if len(list.Subs) != 1 || list.Subs[0].Source != req || !strings.HasPrefix(r.stdout, list.Subs[0].ID+"\t") {
				t.Fatalf("accept did not persist one matching subscription: %+v stdout=%q", list, r.stdout)
			}
			retry := e.as("alice", "sub-accept", room, req).ok(t)
			if e.hs.CallCount(sendRoute) != before+1 || len(e.serverSubs("alice").Subs) != 1 ||
				!strings.Contains(retry.stderr, "was already accepted") || !strings.HasPrefix(retry.stdout, list.Subs[0].ID+"\t") {
				t.Fatal("accept retry changed state or sent a duplicate reply")
			}
		})
	}
}
