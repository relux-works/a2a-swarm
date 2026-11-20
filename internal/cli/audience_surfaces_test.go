package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func addressSync(t *testing.T, e *env, name string) {
	t.Helper()
	a, s := addressSession(t, e, name)
	st, subs, err := s.startDelivery(context.Background(), a.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.syncNow(context.Background(), st, subs.Subs, time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func addressVariant(t *testing.T, e *env, room, variant, target string) string {
	t.Helper()
	sender := "@sender:example.org"
	switch variant {
	case "reply":
		extra := addressMentions("@" + target + ":example.org")
		extra["m.relates_to"] = map[string]any{"rel_type": "m.thread", "event_id": "$followed"}
		return e.hs.inject(room, sender, msg("reply quoting @alpha:example.org", extra))
	case "edit":
		id := e.hs.inject(room, sender, msg("original", addressHeader(target)))
		addressSync(t, e, "alpha")
		// Receive the original only when eligible, so this observes the edit.
		e.as("alpha", "inbox").ok(t)
		extra := addressHeader("alpha")
		extra["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": id}
		extra["m.new_content"] = msg("replacement", addressHeader("alpha"))
		return e.hs.inject(room, sender, msg("edited @alpha:example.org", extra))
	case "redaction":
		id := e.hs.inject(room, sender, msg("body to scrub", addressHeader(target)))
		addressSync(t, e, "alpha")
		e.hs.redact(id, sender)
		e.hs.mu.Lock()
		e.hs.appendLocked(room, sender, "m.room.redaction", map[string]any{"redacts": id})
		e.hs.mu.Unlock()
		return id
	case "retained-restart":
		id := "$retained-" + target
		raw := addressJSON(matrix.Event{EventID: id, Type: "m.room.message", Sender: sender, Content: addressJSON(msg("retained", addressHeader(target)))})
		mb, err := config.OpenMailbox(e.paths(), "alpha")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = mb.Append([]config.MailEntry{{ID: id, Kind: config.MailEvent, Room: room, Rel: "to", Event: raw}}); err != nil {
			t.Fatal(err)
		}
		return id
	case "mixed":
		e.hs.inject(room, sender, msg("excluded @alpha:example.org", addressHeader("beta")))
		return e.hs.inject(room, sender, msg("mixed", addressHeader(target)))
	default:
		return e.hs.inject(room, sender, msg("original", addressHeader(target)))
	}
}

// SessionStart/Prompt/Stop use the real shared mailbox and reservation path
// across Claude, Codex and Muse rendering. Every relation/restart/mixed negative
// leaves no context, Stop block, Rung, new count, alarm or subscription closure.
// Linux fixtures certify selection and fake effects, never a Mac/provider UI.
func TestAudienceHookLifecycleMatrix(t *testing.T) {
	for _, runtime := range []string{"claude", "codex", "muse"} {
		for _, hook := range []string{"session-start", "prompt", "stop"} {
			for _, variant := range []string{"original", "reply", "edit", "redaction", "retained-restart", "mixed"} {
				t.Run(runtime+"/"+hook+"/"+variant, func(t *testing.T) {
					t.Chdir(t.TempDir())
					for _, own := range []bool{true, false} {
						t.Run(fmt.Sprint(own), func(t *testing.T) {
							e, room := addressedFixture(t)
							e.as("alpha", "subscribe", "--room", room, "--all-events", "--until-kind", "notice").ok(t)
							e.as("alpha", "inbox").ok(t)
							_, err := config.UpdateState(e.paths(), "alpha", func(st *config.State) error { st.AddThread("$followed"); return nil })
							if err != nil {
								t.Fatal(err)
							}
							target := "beta"
							if own {
								target = "alpha"
							}
							id := addressVariant(t, e, room, variant, target)
							// An edit's original may already close the one-shot positive row;
							// compare after preparation, before the event being tested syncs.
							beforeSubs := e.as("alpha", "subs").ok(t).stdout
							e.stdin = `{"session_id":"audience-fixture"}`
							r := e.as("alpha", "hook", hook, "--runtime", runtime).ok(t)
							if (r.stdout != "") != own {
								t.Fatalf("eligibility=%t output=%q stderr=%q", own, r.stdout, r.stderr)
							}
							st := e.state("alpha")
							if !own {
								for _, h := range st.HookSessions {
									if len(h.Rung) > 0 || h.Blocks != 0 || h.LastBlock != 0 {
										t.Fatal("excluded hook changed delivery state")
									}
								}
								if st.Notification.Reservation != nil || len(st.Wake.Reservations) > 0 || len(st.MailAlarm.Jobs) > 0 {
									t.Fatal("excluded hook charged/queued effects")
								}
								if got := e.as("alpha", "subs").ok(t).stdout; got != beforeSubs {
									t.Fatal("excluded relation closed subscription")
								}
								_, s := addressSession(t, e, "alpha")
								mb, err := s.openAudienceMailbox()
								if err != nil || len(mb.Pending()) != 0 {
									t.Fatal("excluded retained count", err)
								}
								for _, entry := range mb.RawPending() {
									if entry.Shown || mb.Received(entry.ID) {
										t.Fatal("suppression fabricated delivery")
									}
								}
							}
							if e.received("alpha", id) {
								t.Fatal("hook receipted tested event")
							}
							if variant == "redaction" {
								for _, entry := range e.mailbox("alpha").RawPending() {
									if strings.Contains(string(entry.Event), "body to scrub") {
										t.Fatal("redaction leaked excluded cached body")
									}
								}
							}
						})
					}
				})
			}
		}
	}
}

// Reactions never notify either audience. A nearby ordinary addressed event
// still notifies, so rejecting every event cannot pass this check.
func TestAudienceReactionNoWake(t *testing.T) {
	for _, target := range []string{"alpha", "beta"} {
		t.Run(target, func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "subscribe", "--room", room, "--all-events").ok(t)
			e.as("alpha", "inbox").ok(t)
			id := e.hs.inject(room, "@sender:example.org", msg("ordinary", addressHeader(target)))
			e.as("alpha", "inbox").ok(t)
			e.hs.mu.Lock()
			reaction := e.hs.appendLocked(room, "@sender:example.org", "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": id, "key": "fixture"}})
			e.hs.mu.Unlock()
			r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t)
			if r.stdout != "" || e.received("alpha", reaction) {
				t.Fatal("reaction introduced notification/receipt")
			}
			e.hs.inject(room, "@sender:example.org", msg("positive", addressHeader("alpha")))
			if r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t); r.stdout == "" {
				t.Fatal("ordinary positive rejected")
			}
		})
	}
}

// Inherited audience is only a ceiling: an originally addressed recipient gains
// no positive selector from a silent edit's nested replacement metadata.
func TestAudienceSilentEditStaysSilent(t *testing.T) {
	e, room := addressedFixture(t)
	e.as("alpha", "inbox").ok(t)
	id := e.hs.inject(room, "@sender:example.org", msg("original", addressHeader("alpha")))
	e.as("alpha", "inbox").ok(t)
	extra := map[string]any{"m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": id}, "m.new_content": msg("nested @alpha:example.org", addressHeader("alpha"))}
	edited := e.hs.inject(room, "@sender:example.org", msg("silent edit", extra))
	if r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t); r.stdout != "" || e.received("alpha", edited) {
		t.Fatal("inheritance added positive selector")
	}
	positive := e.hs.inject(room, "@sender:example.org", msg("eligible edit", map[string]any{ContentKey: Header{To: []string{"alpha"}}, "m.relates_to": extra["m.relates_to"], "m.new_content": extra["m.new_content"]}))
	if r := e.as("alpha", "inbox").ok(t); !strings.Contains(r.stdout, positive) {
		t.Fatal("existing eligible edit rejected")
	}
}

// Both standard/all-event selection and retained count notices intersect the
// audience view before subscription expiry, Shown state or output.
func TestAudienceAllProjectionAndNewCount(t *testing.T) {
	for _, projection := range []bool{false, true} {
		t.Run(fmt.Sprint(projection), func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "subscribe", "--room", room, "--all-events", "--until-kind", "notice").ok(t)
			e.as("alpha", "inbox").ok(t)
			before := e.as("alpha", "subs").ok(t).stdout
			h := Header{V: 1, Type: "message", Kind: "notice", To: []string{"beta"}}
			if projection {
				h.Type = "projection"
			}
			id := e.hs.inject(room, "@sender:example.org", msg("@alpha:example.org", map[string]any{ContentKey: h}))
			a, s := addressSession(t, e, "alpha")
			st, subs, err := s.startDelivery(context.Background(), a.Now())
			if err != nil {
				t.Fatal(err)
			}
			resp, err := s.client.Sync(context.Background(), st.NextBatch, 0, syncFilter)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.ingest(context.Background(), resp, st.NextBatch, subs.Subs, map[string]bool{room: true}, time.Time{}); err != nil {
				t.Fatal(err)
			}
			if n, err := a.showNew(context.Background(), s, deliveryFilter{all: true}, false); err != nil || n != 0 {
				t.Fatal("--all exposed excluded new count", n, err)
			}
			if got := e.as("alpha", "subs").ok(t).stdout; got != before {
				t.Fatal("excluded until-kind closure")
			}
			for _, entry := range e.mailbox("alpha").RawPending() {
				if entry.ID == id && entry.Shown {
					t.Fatal("excluded shown")
				}
			}
			h.To = []string{"alpha"}
			valid := e.hs.inject(room, "@sender:example.org", msg("positive", map[string]any{ContentKey: h}))
			addressSync(t, e, "alpha")
			if n, err := a.showNew(context.Background(), s, deliveryFilter{all: true}, false); err != nil || n != 1 {
				t.Fatal("positive new count", n, err)
			}
			if e.received("alpha", valid) || e.received("alpha", id) {
				t.Fatal("wait-style count created receipt")
			}
		})
	}
}

// Preserve exact evidence bytes through a missing-target retry; unavailable
// targets remain durable/nonwaking and a later known target resolves an
// already-eligible edit without turning an unrelated legacy message off.
func TestAudienceDeferredRelationRecovery(t *testing.T) {
	e, room := addressedFixture(t)
	e.as("alpha", "subscribe", "--room", room, "--all-events").ok(t)
	e.as("alpha", "inbox").ok(t)
	extra := addressHeader("alpha")
	extra["m.relates_to"] = map[string]any{"rel_type": "m.replace", "event_id": "$delayed"}
	extra["m.new_content"] = msg("new", nil)
	id := e.hs.inject(room, "@sender:example.org", msg("edit", extra))
	if r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t); r.stdout != "" || !strings.Contains(r.stderr, "ADDRESS_TARGET_UNRESOLVED") {
		t.Fatal("unknown target woke or lacked diagnostic")
	}
	addressSync(t, e, "alpha")
	mb := e.mailbox("alpha")
	if mb.Received(id) || len(mb.RawPending()) != 1 {
		t.Fatal("deferred reference lost/received")
	}
	// Supply the missing room-scoped target through the in-process endpoint.
	e.hs.mu.Lock()
	e.hs.events = append(e.hs.events, matrix.Event{EventID: "$delayed", Type: "m.room.message", Sender: "@sender:example.org", RoomID: room, OriginServerTS: 1, Content: addressJSON(msg("original", addressHeader("alpha")))})
	e.hs.mu.Unlock()
	addressSync(t, e, "alpha")
	r := e.as("alpha", "inbox").ok(t)
	if !strings.Contains(r.stdout, id) || !e.received("alpha", id) {
		t.Fatal("resolved eligible edit did not recover")
	}
	legacy := e.hs.inject(room, "@sender:example.org", msg("legacy", nil))
	if r := e.as("alpha", "inbox").ok(t); !strings.Contains(r.stdout, legacy) {
		t.Fatal("deferred policy suppressed legacy general")
	}
}

// Excluded mail stays available to authorized explicit read, while a verified
// redaction scrubs both the suppressed durable copy and subsequent read output.
func TestAudienceExplicitReadRedaction(t *testing.T) {
	e, room := addressedFixture(t)
	e.as("alpha", "inbox").ok(t)
	sender := "@sender:example.org"
	id := e.hs.inject(room, sender, msg("fixture body to scrub", addressHeader("beta")))
	if r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t); r.stdout != "" {
		t.Fatal("excluded original rang")
	}
	if r := e.as("alpha", "read", room, "--event", id).ok(t); !strings.Contains(r.stdout, "fixture body to scrub") {
		t.Fatal("excluded read unavailable")
	}
	e.hs.redact(id, sender)
	e.hs.mu.Lock()
	e.hs.appendLocked(room, sender, "m.room.redaction", map[string]any{"redacts": id})
	e.hs.mu.Unlock()
	if r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t); r.stdout != "" {
		t.Fatal("excluded redaction rang")
	}
	if r := e.as("alpha", "read", room, "--event", id).ok(t); strings.Contains(r.stdout, "fixture body to scrub") {
		t.Fatal("explicit read leaked redacted body")
	}
	for _, entry := range e.mailbox("alpha").RawPending() {
		if strings.Contains(string(entry.Event), "fixture body to scrub") || entry.Shown {
			t.Fatal("excluded cache retained redacted content or changed shown")
		}
	}
	if e.received("alpha", id) {
		t.Fatal("read or suppression created receipt")
	}
}

// A room mention preserves general selection but creates no selector by itself;
// copied projection recipients likewise do not acquire direct priority.
func TestAudienceNoNewRoomOrProjectionSelector(t *testing.T) {
	for _, variant := range []string{"room", "projection"} {
		t.Run(variant, func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "inbox").ok(t)
			extra := map[string]any{"m.mentions": map[string]any{"room": true, "user_ids": []string{"@beta:example.org"}}}
			if variant == "projection" {
				extra = map[string]any{ContentKey: Header{V: 1, Type: "projection", To: []string{"alpha"}}}
			}
			id := e.hs.inject(room, "@sender:example.org", msg("fixture", extra))
			if r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t); r.stdout != "" || e.received("alpha", id) {
				t.Fatal("metadata introduced a positive selector")
			}
			e.as("alpha", "subscribe", "--room", room, "--all-events").ok(t)
			// The fixture clock predates both events, so the newly added selector
			// can match the next independent general/projection positive.
			positive := e.hs.inject(room, "@sender:example.org", msg("positive", extra))
			if r := e.as("alpha", "inbox").ok(t); !strings.Contains(r.stdout, positive) {
				t.Fatal("existing general/projection selector rejected")
			}
		})
	}
}
