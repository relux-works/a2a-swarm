package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Proves a realistic valid pin alone grants no foreign-thread delivery, while
// direct and mention controls in the same sync still reach the durable mailbox.
func TestThreadReworkPinAlone(t *testing.T) {
	for _, mode := range []string{"host", "owner-channel"} {
		t.Run(mode, func(t *testing.T) {
			for _, muted := range []bool{false, true} {
				e, room, f := ownerFixture(t)
				ownerPut(t, f.path("pin"), ownerDMPin{Mode: mode, Owner: "@alice:" + testServer})
				root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
				if muted {
					e.as("bob", "mute", "proj", "--topic", root).ok(t)
				}
				e.as("bob", "wait", "--timeout", "10ms").ok(t)
				blocked := e.hs.inject(room, "@alice:"+testServer, threadContent(root, "pin-only ordinary owner chatter"))
				plain := e.hs.inject(room, "@alice:"+testServer, msg("pin-only ordinary room chatter", nil))
				directContent := threadContent(root, "valid direct fence")
				directContent[ContentKey] = Header{To: []string{"bob"}}
				direct := e.hs.inject(room, "@alice:"+testServer, directContent)
				mention := e.hs.inject(room, "@alice:"+testServer, threadContent(root, "valid mention @bob:test.local"))
				out := e.as("bob", "wait", "--timeout", "2s").ok(t).stdout
				if e.mailbox("bob").Has(blocked) || e.mailbox("bob").Has(plain) || !strings.Contains(out, direct) || !strings.Contains(out, mention) {
					t.Fatalf("pin alone admitted foreign owner traffic or lost priority controls: muted=%v output=%q", muted, out)
				}
			}
		})
	}
}

// Proves the ORIGINAL active owner-DM subscription delivers ordinary owner mail
// only in its configured room; pin/config activation does not follow other rooms.
func TestThreadReworkActiveOwnerDM(t *testing.T) {
	e, room, f := ownerFixture(t)
	c := ownerBind(t, e, room, f)
	ownerActivate(t, e, f, c)
	c = ownerLoad(t, f)
	root := strings.TrimSpace(e.as("alice", "topic", "new", c.Room, "dm-topic", "DM topic").ok(t).stdout)
	e.as("bob", "mute", c.Room, "--topic", root).ok(t)
	e.as("bob", "wait", "--timeout", "10ms").ok(t)
	allowed := e.hs.inject(c.Room, c.Owner, threadContent(root, "ordinary active DM control through thread mute"))
	blocked := e.hs.inject(room, c.Owner, threadContent("$foreign-root", "ordinary foreign thread"))
	out := e.as("bob", "wait", "--timeout", "2s").ok(t).stdout
	if !strings.Contains(out, allowed) || !e.mailbox("bob").Has(allowed) || e.mailbox("bob").Has(blocked) {
		t.Fatalf("active DM scope changed: %q", out)
	}
}

// Proves exact pre-mailbox pending thread IDs drain despite unknown origins;
// acked IDs and later same/unrelated thread mail do not become new obligations.
// Rewinding the cursor after restart cannot replay the normally receipted mail.
func TestThreadReworkPremailboxRecovery(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "inbox").ok(t)
	cursor := e.state("alice").NextBatch
	acked := e.hs.inject(room, "@bob:"+testServer, ping(1))
	pending := e.hs.inject(room, "@bob:"+testServer, threadContent(root, "already pending thread-only obligation"))
	later := e.hs.inject(room, "@bob:"+testServer, threadContent(root, "future same-root rejected"))
	unrelated := e.hs.inject(room, "@bob:"+testServer, threadContent("$other", "future unrelated rejected"))
	legacy := config.State{NextBatch: cursor, Threads: []string{root}, Muted: []string{root}, LegacyAcked: []string{acked}, LegacyPending: json.RawMessage(fmt.Sprintf(`{"next_batch":"not-a-cursor-to-adopt","events":[%q,%q,%q]}`, pending, pending, acked))}
	if err := config.SaveState(e.paths(), "alice", legacy); err != nil {
		t.Fatal(err)
	}
	out := e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(out, pending) || strings.Contains(out, acked) || !e.received("alice", pending) || !e.received("alice", acked) || e.mailbox("alice").Has(later) || e.mailbox("alice").Has(unrelated) {
		t.Fatalf("pre-mailbox pending thread obligation did not drain exactly: %q", out)
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.NextBatch = cursor; return nil }); err != nil {
		t.Fatal(err)
	}
	if out := e.as("alice", "inbox").ok(t).stdout; strings.Contains(out, pending) || strings.Contains(out, acked) {
		t.Fatalf("restart duplicated recovered obligation: %q", out)
	}
	if len(e.state("alice").ThreadFollows) != 0 {
		t.Fatal("recovery fabricated follow intent")
	}
}

// Proves existing local ack/shown/receipt callbacks work before migration even
// during account-data failure. Later online classification still inventories and
// migrates, retaining the offline disposition and admitting a valid direct fence.
func TestThreadReworkOfflineLocalDelivery(t *testing.T) {
	e, room := world(t)
	var app *App
	e.configure = func(a *App) { app = a }
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	e.configure = nil
	s, err := app.session()
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{"$local-ack", "$local-shown", "$local-receipt"}
	_, err = config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.ThreadPolicyVersion = 0
		st.ThreadMigration = nil
		st.Threads = []string{"$unknown"}
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		var entries []config.MailEntry
		for _, id := range ids {
			entries = append(entries, config.MailEntry{ID: id, Room: room, Kind: config.MailEvent, Rel: "thread", Event: json.RawMessage(`{"type":"m.room.message","content":{"body":"local retained"}}`)})
		}
		_, err = mb.Append(entries)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	const route = "GET /_matrix/client/v3/user/{user}/account_data/{type}"
	e.hs.fail(route, 500, "M_UNKNOWN", "offline fixture")
	calls := e.hs.CallCount(route)
	r := e.as("alice", "ack", ids[0])
	if r.code != 0 {
		t.Fatalf("offline local ack required account-data migration: %+v", r)
	}
	_, err = s.lockedDeliver(context.Background(), time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		return nil, mb.MarkShown([]string{ids[1]})
	})
	if err != nil {
		t.Fatalf("offline shown required migration: %v", err)
	}
	_, err = s.lockedDeliver(context.Background(), time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
		_, err := mb.Receive([]string{ids[2]})
		return nil, err
	})
	if err != nil || !e.received("alice", ids[0]) || !e.received("alice", ids[2]) || !e.mailbox("alice").Pending()[0].Shown || e.hs.CallCount(route) != calls || e.state("alice").ThreadPolicyVersion != 0 {
		t.Fatalf("local disposition changed or accessed account data: %v", err)
	}
	delete(e.hs.failures, route)
	control := e.hs.inject(room, "@bob:"+testServer, ping(9))
	out := e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(out, control) || e.state("alice").ThreadPolicyVersion != 1 || !e.received("alice", ids[0]) || !contains(e.state("alice").ThreadMigration.Received, ids[0]) || !contains(e.state("alice").ThreadMigration.Received, ids[2]) {
		t.Fatalf("later online migration/control failed: %q", out)
	}
}

// Proves a readable migration inventory contains pending IDs, not permanent
// copies of raw event bodies. The real durable mailbox stays byte-exact.
func TestThreadReworkMetadataInventory(t *testing.T) {
	e, room := world(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	const body = "fixture-body-must-not-be-in-inventory-118"
	_, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error {
		s.ThreadPolicyVersion = 0
		s.ThreadMigration = nil
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		_, err = mb.Append([]config.MailEntry{{ID: "$metadata-pending", Kind: config.MailEvent, Room: room, Rel: "thread", Event: json.RawMessage(`{"type":"m.room.message","content":{"body":"` + body + `"}}`)}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.paths().MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "mute", "proj", "--topic", root).ok(t)
	b, err := json.Marshal(e.state("alice").ThreadMigration)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(e.paths().MailboxFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(body)) || !bytes.Contains(b, []byte("$metadata-pending")) || !bytes.Equal(before, after) {
		t.Fatal("migration inventory duplicated pending payload or changed mailbox")
	}
}

// Proves malformed or unavailable pre-mailbox obligations fail closed without
// losing their recovery fields/cursor or partially appending even a valid fence.
// Correcting the fixture's pending ID permits ordinary recovery on the next run.
func TestThreadReworkLegacyRefusals(t *testing.T) {
	for _, raw := range []string{`{"events":["$missing"]}`, `{"events":[7]}`, `{"events":[""]}`, `{"payload":"unsupported"}`, `{"events":null}`, `{"events":["$lost"],"events":[]}`, `{"events":["$ok"],"unexpected":"body"}`} {
		t.Run(raw, func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "inbox").ok(t)
			cursor := e.state("alice").NextBatch
			pending := e.hs.inject(room, "@bob:"+testServer, threadContent("$unknown-root", "recoverable control"))
			control := e.hs.inject(room, "@bob:"+testServer, ping(1))
			st := config.State{NextBatch: cursor, Threads: []string{"$unknown-root"}, LegacyPending: json.RawMessage(raw)}
			if err := config.SaveState(e.paths(), "alice", st); err != nil {
				t.Fatal(err)
			}
			st = e.state("alice") // compare the persisted RawMessage representation
			before, _ := os.ReadFile(e.paths().MailboxFile("alice"))
			e.as("alice", "inbox").failsWith(t, ExitError, "legacy pending recovery")
			after, _ := os.ReadFile(e.paths().MailboxFile("alice"))
			got := e.state("alice")
			if got.NextBatch != cursor || !bytes.Equal(got.LegacyPending, st.LegacyPending) || !bytes.Equal(before, after) || e.mailbox("alice").Has(control) {
				t.Fatal("failed legacy recovery advanced or partially wrote mail")
			}
			if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error {
				s.LegacyPending = json.RawMessage(fmt.Sprintf(`{"events":[%q]}`, pending))
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			out := e.as("alice", "inbox").ok(t).stdout
			if !strings.Contains(out, pending) || !strings.Contains(out, control) {
				t.Fatalf("valid recovery/control did not resume: %q", out)
			}
		})
	}
}

// Proves a failure after durable recovery append leaves the old cursor and
// recovery evidence intact; retry drains exactly once without re-following.
func TestThreadReworkLegacyInterruptedAppend(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	cursor := e.state("alice").NextBatch
	pending := e.hs.inject(room, "@bob:"+testServer, threadContent("$root", "pending before mailbox"))
	acked := e.hs.inject(room, "@bob:"+testServer, ping(2))
	if err := config.SaveState(e.paths(), "alice", config.State{NextBatch: cursor, Threads: []string{"$root"}, LegacyAcked: []string{acked}, LegacyPending: json.RawMessage(fmt.Sprintf(`{"events":[%q]}`, pending))}); err != nil {
		t.Fatal(err)
	}
	e.configure = func(a *App) {
		a.afterAppend = func() error { return fmt.Errorf("injected recovery append interruption") }
	}
	e.as("alice", "inbox").failsWith(t, ExitError, "injected recovery append interruption")
	st := e.state("alice")
	if st.NextBatch != cursor || len(st.LegacyPending) == 0 || !e.mailbox("alice").Has(pending) || !e.received("alice", acked) || e.received("alice", pending) {
		t.Fatal("interrupted recovery lost a durable obligation or cursor")
	}
	e.configure = nil
	out := e.as("alice", "inbox").ok(t).stdout
	if strings.Count(out, "id="+pending) != 1 || strings.Contains(out, "id="+acked) || !e.received("alice", pending) || len(e.state("alice").LegacyPending) != 0 {
		t.Fatalf("recovery retry did not drain exactly: %q", out)
	}
	before, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	e.as("alice", "inbox").ok(t)
	after, _ := os.ReadFile(e.paths().MailboxFile("alice"))
	if !bytes.Equal(before, after) {
		t.Fatal("completed recovery repeated mailbox effects")
	}
}

// Proves append-time revalidation retains an addressed control while a mute
// arriving after collection prevents a stale topic match from closing its scope.
func TestThreadReworkDirectStaleClosure(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "wait", "--timeout", "10ms").ok(t)
	sub, _ := e.subscribe("alice", "--room", "proj", "--topic", root, "--until-kind", "answer")
	content := threadContent(root, "valid direct answer")
	content[ContentKey] = Header{Kind: "answer", To: []string{"alice"}}
	id := e.hs.inject(room, "@bob:"+testServer, content)
	e.configure = func(a *App) {
		a.beforeDeliver = func() {
			if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error { s.Mute(root); return nil }); err != nil {
				t.Error(err)
			}
		}
	}
	out := e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
	if !strings.Contains(out, id) || !containsSub(e.serverSubs("alice").Subs, sub) {
		t.Fatalf("stale addressed-topic match closed subscription or lost mail: %q", out)
	}
}

// Proves a pre-mailbox receipt suppresses both replayed mail and replayed
// until-kind closure; a later unreceived event still delivers and closes normally.
func TestThreadReworkLegacyReceiptNoClosure(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "inbox").ok(t)
	sub, _ := e.subscribe("alice", "--room", "proj", "--topic", root, "--until-kind", "answer")
	cursor := e.state("alice").NextBatch
	content := threadContent(root, "already received answer")
	content[ContentKey] = Header{Kind: "answer", To: []string{"alice"}}
	acked := e.hs.inject(room, "@bob:"+testServer, content)
	pending := e.hs.inject(room, "@bob:"+testServer, threadContent(root, "already pending ordinary thread mail"))
	if err := config.SaveState(e.paths(), "alice", config.State{NextBatch: cursor, Threads: []string{root}, LegacyAcked: []string{acked}, LegacyPending: json.RawMessage(fmt.Sprintf(`{"events":[%q]}`, pending))}); err != nil {
		t.Fatal(err)
	}
	out := e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(out, pending) || strings.Contains(out, acked) || !containsSub(e.serverSubs("alice").Subs, sub) {
		t.Fatalf("legacy receipt caused duplicate subscription effect: %q", out)
	}
	valid := e.hs.inject(room, "@bob:"+testServer, content)
	out = e.as("alice", "inbox").ok(t).stdout
	if !strings.Contains(out, valid) || containsSub(e.serverSubs("alice").Subs, sub) {
		t.Fatalf("new valid answer did not close scope: %q", out)
	}
}

// Proves an already-receipted first candidate cannot hide a later valid closing
// event in the same sync. Only the unreceived event delivers and closes once.
func TestThreadReworkReceiptBeforeValidClosingEvent(t *testing.T) {
	e, room := subWorld(t)
	root := strings.TrimSpace(e.as("bob", "topic", "new", "proj", "foreign", "Foreign").ok(t).stdout)
	e.as("alice", "inbox").ok(t)
	sub, _ := e.subscribe("alice", "--room", "proj", "--topic", root, "--until-kind", "answer")
	content := threadContent(root, "answer")
	content[ContentKey] = Header{Kind: "answer"}
	acked := e.hs.inject(room, "@bob:"+testServer, content)
	if _, err := config.UpdateState(e.paths(), "alice", func(s *config.State) error {
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		return mb.AddReceived([]string{acked})
	}); err != nil {
		t.Fatal(err)
	}
	valid := e.hs.inject(room, "@bob:"+testServer, content)
	out := e.as("alice", "inbox").ok(t).stdout
	if strings.Contains(out, acked) || !strings.Contains(out, valid) || containsSub(e.serverSubs("alice").Subs, sub) {
		t.Fatalf("receipted candidate hid later valid closure: %q", out)
	}
}
