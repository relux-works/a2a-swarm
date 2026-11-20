package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/audience"
	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func addressHeader(to ...string) map[string]any {
	return map[string]any{ContentKey: Header{V: 1, Type: "message", Kind: "notice", Subject: "fixture-subject", To: to}}
}

func addressMentions(ids ...string) map[string]any {
	return map[string]any{"m.mentions": map[string]any{"user_ids": ids}}
}

func addressSession(t *testing.T, e *env, name string) (*App, *session) {
	t.Helper()
	a := &App{as: name, Stdout: io.Discard, Stderr: io.Discard, HTTP: e.hs.srv.Client(), Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return e.extra[k]
	}}
	if e.now != nil {
		a.Now = e.now
	}
	s, err := a.session()
	if err != nil {
		t.Fatal(err)
	}
	s.principals = s.readDeliveryPrincipals()
	return a, s
}

// Each named pair uses real sync, subscriptions, durable mailbox, hook
// reservation and inbox receipt paths. Rejection must preserve mail without
// Shown/Rung/receipts or until-kind expiry; the adjacent positive must deliver.
func TestAudienceContractSelectors(t *testing.T) {
	self, other := "@alpha:example.org", "@beta:example.org"
	for _, tc := range []struct {
		name               string
		positive, negative map[string]any
		selector           []string
		owner              bool
		diagnostic         string
	}{
		{"A1-room", addressHeader("alpha"), addressHeader("beta"), []string{"--all-events"}, false, ""},
		{"A1-sender-owner", nil, addressHeader("beta"), []string{"--from", "@owner:example.org"}, true, ""},
		{"A1-kind-subject", addressHeader("all"), addressHeader("beta"), []string{"--kind", "notice", "--subject", "fixture-subject"}, false, ""},
		{"A2-multiple", addressHeader("alpha", "beta"), addressHeader("beta", "gamma"), nil, false, ""},
		{"A2-foreign-server", addressHeader(self), addressHeader("@alpha:elsewhere.example"), nil, false, ""},
		{"A2-case", addressHeader("alpha"), addressHeader("Alpha"), nil, false, ""},
		{"A3-legacy", nil, addressHeader("beta"), []string{"--all-events"}, false, ""},
		{"A3-all", addressHeader("all"), addressHeader("beta"), []string{"--all-events"}, false, ""},
		{"A4-body", nil, addressHeader("beta"), nil, false, ""},
		{"A8-intentional", addressMentions(self, other), addressMentions(other), []string{"--all-events"}, false, ""},
		{"A8-header-precedence", map[string]any{ContentKey: Header{To: []string{"alpha"}}, "m.mentions": map[string]any{"user_ids": []string{other}}}, map[string]any{ContentKey: Header{To: []string{"beta"}}, "m.mentions": map[string]any{"user_ids": []string{self}}}, []string{"--all-events"}, false, ""},
		{"A8-room-general", map[string]any{"m.mentions": map[string]any{"room": true, "user_ids": []string{other}}}, addressMentions(other), []string{"--all-events"}, false, ""},
		{"A8-old-clients", map[string]any{"m.mentions": map[string]any{}}, addressHeader("beta"), []string{"--all-events"}, false, ""},
		{"R1-peer-malformed", addressHeader("alpha"), map[string]any{ContentKey: map[string]any{"to": "alpha"}}, []string{"--all-events"}, false, audience.AddressMetadataInvalid},
		{"R1-owner-malformed", map[string]any{ContentKey: map[string]any{"to": "alpha"}}, addressHeader("beta"), nil, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, positive := range []bool{true, false} {
				t.Run(fmt.Sprintf("positive=%t", positive), func(t *testing.T) {
					e, room := addressedFixture(t)
					if tc.owner {
						e.as("alpha", "host", "principals", "set", "@owner:example.org").ok(t)
					}
					if len(tc.selector) > 0 {
						args := append([]string{"subscribe", "--room", room}, tc.selector...)
						e.as("alpha", args...).ok(t)
					}
					e.as("alpha", "inbox").ok(t)
					beforeSubs := e.as("alpha", "subs").ok(t).stdout
					extra := tc.negative
					if positive {
						extra = tc.positive
					}
					body := "fixture body"
					if tc.name == "A4-body" {
						body += " " + self
					}
					sender := "@sender:example.org"
					if tc.owner {
						sender = "@owner:example.org"
					}
					id := e.hs.inject(room, sender, msg(body, extra))
					r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t)
					if (r.stdout != "") != positive {
						t.Fatalf("hook eligibility=%t: %q stderr=%q", positive, r.stdout, r.stderr)
					}
					if !positive && tc.diagnostic != "" && !strings.Contains(r.stderr, tc.diagnostic) {
						t.Fatalf("missing diagnostic %s", tc.diagnostic)
					}
					if positive && tc.name == "R1-owner-malformed" && !strings.Contains(r.stderr, audience.AddressMetadataInvalid) {
						t.Fatal("R1 omitted diagnostic")
					}
					st := e.state("alpha")
					if !positive {
						for _, h := range st.HookSessions {
							if len(h.Rung) > 0 || h.Blocks != 0 || h.LastBlock != 0 {
								t.Fatal("rejection changed hook state")
							}
						}
						if st.Notification.Reservation != nil || len(st.Wake.Reservations) > 0 || len(st.MailAlarm.Jobs) > 0 {
							t.Fatal("rejection charged or queued effect")
						}
						if got := e.as("alpha", "subs").ok(t).stdout; got != beforeSubs {
							t.Fatal("rejection changed subscription")
						}
					}
					r = e.as("alpha", "inbox").ok(t)
					if strings.Contains(r.stdout, id) != positive || e.received("alpha", id) != positive {
						t.Fatal("inbox/receipt disagrees with expected audience")
					}
					if !positive {
						_, s := addressSession(t, e, "alpha")
						mb, err := s.openAudienceMailbox()
						if err != nil {
							t.Fatal(err)
						}
						if len(mb.Pending()) != 0 {
							t.Fatal("excluded pending count")
						}
						for _, entry := range mb.RawPending() {
							if entry.Shown || mb.Received(entry.ID) {
								t.Fatal("excluded retained mail marked delivered")
							}
						}
					}
				})
			}
		})
	}
}

// Retained legacy mail is reclassified on every delivery view after restart;
// unknown evidence versions block that event only and remain visible in status.
func TestAudienceRetainedAndUnknownVersion(t *testing.T) {
	e, room := addressedFixture(t)
	e.as("alpha", "inbox").ok(t)
	_, s := addressSession(t, e, "alpha")
	mb, err := config.OpenMailbox(e.paths(), "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, own := range []bool{true, false} {
		to, id := "beta", "$retained-other"
		if own {
			to, id = "alpha", "$retained-self"
		}
		raw, _ := json.Marshal(matrix.Event{EventID: id, Type: "m.room.message", Sender: "@sender:example.org", Content: addressJSON(msg("fixture retained", addressHeader(to)))})
		if _, err := mb.Append([]config.MailEntry{{ID: id, Kind: config.MailEvent, Room: room, Rel: "to", Event: raw}}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		view, err := s.openAudienceMailbox()
		if err != nil {
			t.Fatal(err)
		}
		pending := view.Pending()
		if len(pending) != 1 || pending[0].ID != "$retained-self" {
			t.Fatalf("retained view has %d entries instead of the one eligible control", len(pending))
		}
	}
	unknown := audience.Evidence{Version: 99, Room: room, EventID: "$retained-other", UserID: s.creds.UserID, Name: s.name, Sender: "@sender:example.org", Decision: audience.Self, ObservedAt: s.app.Now()}
	if err := mb.SaveAudiences([]audience.Evidence{unknown}); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"text", "json"} {
		args := []string{"bind", "status"}
		if format == "json" {
			args = append(args, "--json")
		}
		r := e.as("alpha", args...).ok(t)
		if !strings.Contains(r.stdout, audience.AddressStateUnavailable) {
			t.Fatal("unknown version invisible in bind status")
		}
	}
	r := e.as("alpha", "inbox").ok(t)
	if strings.Contains(r.stdout, "$retained-other") || !strings.Contains(r.stdout, "$retained-self") || e.received("alpha", "$retained-other") {
		t.Fatal("unknown event opened or blocked unrelated positive")
	}
	t.Run("unreadable-owner", func(t *testing.T) {
		f, room := addressedFixture(t)
		f.as("alpha", "host", "principals", "set", "@owner:example.org").ok(t)
		f.as("alpha", "inbox").ok(t)
		f.hs.inject(room, "@sender:example.org", msg("excluded", addressHeader("beta")))
		addressSync(t, f, "alpha")
		before := f.state("alpha").NextBatch
		path := f.paths().MailboxFile("alpha")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		id := f.hs.inject(room, "@owner:example.org", msg("owner fixture", map[string]any{ContentKey: map[string]any{"to": "alpha"}}))
		result := f.hook("alpha", "stop", `{"session_id":"audience-fixture"}`).ok(t)
		if result.stdout != "" || !strings.Contains(result.stderr, audience.AddressStateUnavailable) {
			t.Fatal("unreadable store rescued by R1 or missing fixed diagnostic")
		}
		st := f.state("alpha")
		if st.NextBatch != before || st.Notification.Reservation != nil {
			t.Fatal("unreadable store advanced cursor or reserved a wake")
		}
		for _, h := range st.HookSessions {
			if len(h.Rung) > 0 || h.Blocks != 0 {
				t.Fatal("unreadable store changed delivery state")
			}
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(data) {
			t.Fatal("failed store read wrote mailbox")
		}
		if status := f.as("alpha", "bind", "status", "--json").ok(t); !strings.Contains(status.stdout, audience.AddressStateUnavailable) {
			t.Fatal("unreadable store invisible in bind status")
		}
		if err = os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if result := f.as("alpha", "inbox").ok(t); !strings.Contains(result.stdout, id) || !f.received("alpha", id) {
			t.Fatal("repaired-store R1 positive failed")
		}
	})
}

// Reopening a cached audience view performs no State decode and no mailbox
// rewrite. Deferred references also avoid repeated evidence append records on
// unchanged empty cycles; target retries remain bounded by the cycle resolver.
func TestAudienceIdleViewNoDecodeOrRewrite(t *testing.T) {
	e, room := addressedFixture(t)
	e.as("alpha", "inbox").ok(t)
	_, s := addressSession(t, e, "alpha")
	s.paths = s.paths.WithReadCache(config.NewReadCache("alpha"))
	id := e.hs.inject(room, "@sender:example.org", msg("fixture", addressHeader("beta")))
	addressSync(t, e, "alpha")
	view, err := s.openAudienceMailbox()
	if err != nil || len(view.Pending()) != 0 {
		t.Fatal("initial excluded view", err)
	}
	before, err := os.ReadFile(s.paths.MailboxFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	stats := config.ReadStateDecodeStats()
	for i := 0; i < 4; i++ {
		view, err = s.openAudienceMailbox()
		if err != nil || len(view.Pending()) != 0 {
			t.Fatal("cached exclusion", err)
		}
	}
	afterStats := config.ReadStateDecodeStats()
	after, err := os.ReadFile(s.paths.MailboxFile(s.name))
	if err != nil || string(before) != string(after) || afterStats.UnmarshalStatePasses != stats.UnmarshalStatePasses || afterStats.MailboxDecodes != stats.MailboxDecodes {
		t.Fatal("idle view decoded State/mailbox or rewrote evidence")
	}
	if e.received("alpha", id) {
		t.Fatal("cached exclusion manufactured receipt")
	}
	// Exercise an unresolved relation over several independent empty cycles.
	extra := addressHeader("alpha")
	extra["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": "$absent"}
	extra["m.new_content"] = msg("replacement", nil)
	e.hs.inject(room, "@sender:example.org", msg("edit", extra))
	addressSync(t, e, "alpha")
	before, err = os.ReadFile(s.paths.MailboxFile(s.name))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		at := e.now().Add(time.Second)
		e.now = func() time.Time { return at }
		addressSync(t, e, "alpha")
	}
	after, err = os.ReadFile(s.paths.MailboxFile(s.name))
	if err != nil || string(before) != string(after) {
		t.Fatal("unchanged deferred cycle grew mailbox log")
	}
}

// Unrelated legacy-general traffic does not create a new metadata-only log.
// Excluded addressed mail is still durably retained, and own addressed mail
// supplies the adjacent positive delivery control.
func TestAudienceUnrelatedGeneralDoesNotGrowLog(t *testing.T) {
	e, room := addressedFixture(t)
	e.as("alpha", "inbox").ok(t)
	e.hs.inject(room, "@sender:example.org", msg("excluded", addressHeader("beta")))
	addressSync(t, e, "alpha")
	path := e.paths().MailboxFile("alpha")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		e.hs.inject(room, "@sender:example.org", msg("unrelated general", nil))
		addressSync(t, e, "alpha")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) {
		t.Fatal("unrelated general mail grew audience-only log")
	}
	id := e.hs.inject(room, "@sender:example.org", msg("positive", addressHeader("alpha")))
	if r := e.as("alpha", "inbox").ok(t); !strings.Contains(r.stdout, id) {
		t.Fatal("addressed positive rejected")
	}
}

func addressJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}

// Release documentation states both the hard exclusion and legacy/read rules.
func TestAudienceSkillRelease(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "skill", "files", "a2a", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(data)), " ")
	for _, required := range []string{"Messages addressed to other users never notify you", "An explicit a2a recipient list wins over Matrix mentions", "Room/thread read remains available"} {
		if !strings.Contains(text, required) {
			t.Fatalf("shipped skill missing %q", required)
		}
	}
}

// An audience rejection at the shared reservation boundary invokes no effect
// callback and allocates no notification token. It still admits a positive ID.
func TestAudienceReservationBoundary(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprint(own), func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "inbox").ok(t)
			to := "beta"
			if own {
				to = "alpha"
			}
			id := e.hs.inject(room, "@sender:example.org", msg("fixture", addressHeader(to)))
			a, s := addressSession(t, e, "alpha")
			st, subs, err := s.startDelivery(context.Background(), a.Now())
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.syncNow(context.Background(), st, subs.Subs, time.Time{}); err != nil {
				t.Fatal(err)
			}
			effects := 0
			token, err := a.reserveNotification(context.Background(), s, time.Time{}, "hook", "fixture", "", false, func(_ *config.State, mb *config.Mailbox, _ string) (bool, error) {
				if len(mb.Pending()) == 0 {
					return false, nil
				}
				effects++
				return true, nil
			})
			if err != nil || (token != "") != own || effects != map[bool]int{true: 1, false: 0}[own] {
				t.Fatalf("token=%q effects=%d err=%v", token, effects, err)
			}
			if e.received("alpha", id) {
				t.Fatal("reservation receipted mail")
			}
		})
	}
}
