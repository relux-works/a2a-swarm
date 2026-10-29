package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// assertMuseContext checks the exact supported source envelope, not native
// model receipt. A UI-only Stop must never satisfy this assertion.
func assertMuseContext(t *testing.T, r result, event string) {
	t.Helper()
	var output map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &output); err != nil || len(output) != 1 {
		t.Fatalf("missing eventual context after cooldown: event=%s stdout=%q stderr=%q (%v)", event, r.stdout, r.stderr, err)
	}
	var context map[string]string
	if err := json.Unmarshal(output["hookSpecificOutput"], &context); err != nil || len(context) != 2 || context["hookEventName"] != event || !strings.Contains(context["additionalContext"], "1 new peer event") || r.stderr != "" {
		t.Fatalf("unsupported eventual context: event=%s stdout=%q stderr=%q (%v)", event, r.stdout, r.stderr, err)
	}
}

// Proves a successful terminal-only Muse Stop does not permanently consume
// the same batch's source-context eligibility: no immediate turn inside shared
// spacing, then Prompt/Start context at the real persisted cooldown deadline.
// Source fake clock only; no native model/session, new mail or receipt is used.
func TestMuseStopUIEventualContext(t *testing.T) {
	for _, ev := range hookEvents[:2] {
		t.Run(ev.arg, func(t *testing.T) {
			t.Chdir(t.TempDir())
			e, room := world(t)
			clock := e.clock()
			if r := e.muse("prompt", museInput).ok(t); r.stdout != "" || r.stderr != "" {
				t.Fatalf("seed failed: %+v", r)
			}
			id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "question", "BODY_MUST_REMAIN_IN_INBOX"))
			stop := e.muse("stop", museInput).ok(t)
			var ui map[string]string
			if err := json.Unmarshal([]byte(stop.stdout), &ui); err != nil || len(ui) != 1 || !strings.Contains(ui["systemMessage"], "1 new peer event") || stop.stderr != "" || strings.Contains(stop.stdout, "BODY_MUST_REMAIN_IN_INBOX") {
				t.Fatalf("not a valid nonblocking Stop UI: %+v (%v)", stop, err)
			}
			pending := e.mailbox("alice").Pending()
			if len(pending) != 1 || pending[0].ID != id || pending[0].Shown || e.received("alice", id) {
				t.Fatal("Stop lost/receipted/marked shown the batch")
			}
			mail, err := os.ReadFile(e.paths().MailboxFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			state := e.state("alice")
			due := state.Notification.NotBefore
			if state.Notification.Reservation != nil || !due.Equal(clock.now().Add(config.NotificationEvery)) {
				t.Fatal("Stop did not complete real shared spacing")
			}
			unchanged := func() {
				t.Helper()
				cur := e.state("alice")
				h := cur.HookSession("muse-fixture")
				raw, err := os.ReadFile(e.paths().MailboxFile("alice"))
				if err != nil || !bytes.Equal(raw, mail) || !reflect.DeepEqual(pending, e.mailbox("alice").Pending()) || e.received("alice", id) {
					t.Fatal("context notice changed pending/shown/receipt/intake bytes")
				}
				if h.Blocks != 0 || h.LastBlock != 0 || cur.Lease == nil || cur.Lease.Holder != "muse-fixture" || cur.Runtime != nil || cur.Terminal != nil || !reflect.DeepEqual(cur.Wake, state.Wake) || !reflect.DeepEqual(cur.Liveness, state.Liveness) {
					t.Fatal("UI/context invented Stop block, owner, binding or intake effect")
				}
			}
			unchanged()
			for _, step := range []time.Duration{0, config.NotificationEvery - time.Millisecond} {
				clock.add(step)
				if r := e.muse(ev.arg, museInput).ok(t); r.stdout != "" || r.stderr != "" {
					t.Fatalf("manufactured early context/turn: %+v", r)
				}
				if !e.state("alice").Notification.NotBefore.Equal(due) {
					t.Fatal("suppressed hook slid the real cooldown")
				}
				unchanged()
			}
			t.Logf("valid Stop UI, same pending ID/session and suppressed context through cooldown; due=%s", due.Format(time.RFC3339Nano))
			clock.add(time.Millisecond)
			if !clock.now().Equal(due) {
				t.Fatal("fixture did not release actual cooldown")
			}
			context := e.muse(ev.arg, museInput).ok(t)
			assertMuseContext(t, context, ev.name)
			unchanged()
			h := e.state("alice").HookSession("muse-fixture")
			if len(h.Rung) != 1 || h.Rung[0] != id || !e.state("alice").Notification.NotBefore.Equal(clock.now().Add(config.NotificationEvery)) {
				t.Fatal("context did not record ordinary rung/shared completion")
			}
			clock.add(config.NotificationEvery)
			if r := e.muse(ev.arg, museInput).ok(t); r.stdout != "" || r.stderr != "" {
				t.Fatalf("already-contextualized batch repeated: %+v", r)
			}
			unchanged()
		})
	}
}

// Adjacent positive: ordinary Muse Prompt/Start emits context on fresh mail,
// then remains silent for the same IDs even after cooldown. This control must
// pass on both the accepted R1 candidate and the reverting narrowing plant.
func TestMuseOrdinaryContextOnce(t *testing.T) {
	for _, ev := range hookEvents[:2] {
		t.Run(ev.arg, func(t *testing.T) {
			t.Chdir(t.TempDir())
			e, room := world(t)
			clock := e.clock()
			e.muse(ev.arg, museInput).ok(t)
			id := e.hs.inject(room, "@bob:test.local", ping(1))
			assertMuseContext(t, e.muse(ev.arg, museInput).ok(t), ev.name)
			clock.add(config.NotificationEvery)
			if r := e.muse(ev.arg, museInput).ok(t); r.stdout != "" || r.stderr != "" || e.received("alice", id) || len(e.mailbox("alice").Pending()) != 1 {
				t.Fatalf("ordinary context duplicated/receipted: %+v", r)
			}
		})
	}
}

// A later UI-only Stop must preserve earlier context eligibility history and
// existing shown/receipt state, rather than clearing Rung to force delivery.
// After cooldown only the still-uncontextualized new ID makes context fresh.
func TestMuseStopUIPreservesPriorContext(t *testing.T) {
	t.Chdir(t.TempDir())
	e, room := world(t)
	clock := e.clock()
	e.muse("prompt", museInput).ok(t)
	oldID := e.hs.inject(room, "@bob:test.local", ping(1))
	assertMuseContext(t, e.muse("prompt", museInput).ok(t), "UserPromptSubmit")
	if err := e.mailbox("alice").MarkShown([]string{oldID}); err != nil {
		t.Fatal(err)
	}
	prior := e.state("alice").HookSession("muse-fixture")
	clock.add(config.NotificationEvery)
	newID := e.hs.inject(room, "@bob:test.local", ping(2))
	if r := e.muse("stop", museInput).ok(t); !strings.Contains(r.stdout, `"systemMessage"`) || r.stderr != "" {
		t.Fatalf("valid Stop setup refused: %+v", r)
	}
	if got := e.state("alice").HookSession("muse-fixture"); !reflect.DeepEqual(got, prior) {
		t.Fatalf("UI erased prior context or marked new ID as context: before=%+v after=%+v", prior, got)
	}
	before := e.mailbox("alice").Pending()
	if len(before) != 2 || before[0].ID != oldID || !before[0].Shown || before[1].ID != newID || before[1].Shown {
		t.Fatal("Stop altered shown/intake fixture")
	}
	clock.add(config.NotificationEvery)
	var context hookContextOutput
	r := e.muse("prompt", museInput).ok(t)
	if err := json.Unmarshal([]byte(r.stdout), &context); err != nil || !strings.Contains(context.HookSpecificOutput.AdditionalContext, "2 new peer events") || r.stderr != "" {
		t.Fatalf("new pending ID lost context eligibility: %+v (%v)", r, err)
	}
	if !reflect.DeepEqual(before, e.mailbox("alice").Pending()) || e.received("alice", oldID) || e.received("alice", newID) {
		t.Fatal("context reset shown/received or consumed pending intake")
	}
}
