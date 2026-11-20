package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// All identities and protocol values here belong to an in-process homeserver
// using a reserved example domain, with a fresh private configuration root.
func addressedFixture(t *testing.T) (*env, string) {
	t.Helper()
	e := newEnv(t)
	// Subscriptions predate the fake server's deterministic event timestamps.
	e.now = func() time.Time { return time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC) }
	e.hs.server = "example.org"
	data := fmt.Sprintf("A2A_MATRIX_URL=%s\nA2A_MATRIX_SERVER_NAME=example.org\nA2A_MATRIX_REGISTRATION_TOKEN=%s\n", e.hs.srv.URL, e.hs.regToken)
	if err := os.WriteFile(filepath.Join(e.dir, "matrix.env"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	a := e.agent("alpha")
	b := e.agent("beta")
	e.agent("sender")
	e.agent("owner")
	room := e.hs.addRoom("audience-fixture", a, b, "@sender:example.org", "@owner:example.org")
	e.sequentialSessions()
	return e, room
}

// The real sync/mailbox/hook/inbox path must deliver the positive room control,
// while a to-other event cannot add hook context, count, Shown or a receipt.
// This test intentionally also compiles on the unchanged maintenance base.
func TestAddressedContractRoomControl(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprintf("included=%t", own), func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "subscribe", "--room", room, "--all-events").ok(t)
			e.as("alpha", "inbox").ok(t)
			to := "beta"
			if own {
				to = "alpha"
			}
			id := e.hs.inject(room, "@sender:example.org", msg("fixture body", map[string]any{ContentKey: Header{V: 1, Type: "message", Kind: "notice", To: []string{to}}}))
			r := e.hook("alpha", "prompt", `{"session_id":"audience-fixture"}`).ok(t)
			if (r.stdout != "") != own {
				t.Fatalf("NONRECIPIENT HOOK OR POSITIVE REJECTED: included=%t output=%q", own, r.stdout)
			}
			st := e.state("alpha")
			for _, h := range st.HookSessions {
				if !own && (len(h.Rung) != 0 || h.Blocks != 0 || h.LastBlock != 0) {
					t.Fatal("excluded hook changed Rung/Stop state")
				}
			}
			for _, entry := range e.mailbox("alpha").Pending() {
				if entry.ID == id && entry.Shown {
					t.Fatal("hook marked mail shown")
				}
			}
			if e.received("alpha", id) {
				t.Fatal("hook receipted mail")
			}
			r = e.as("alpha", "inbox").ok(t)
			if strings.Contains(r.stdout, id) != own || e.received("alpha", id) != own {
				t.Fatal("NONRECIPIENT INBOX OR POSITIVE REJECTED")
			}
			if !own {
				mb := e.mailbox("alpha")
				for _, entry := range mb.Pending() {
					if entry.ID == id && (entry.Shown || mb.Received(id)) {
						t.Fatal("suppression manufactured delivery")
					}
				}
				// The ordinary explicit read path remains available.
				r = e.as("alpha", "read", room, "--event", id).ok(t)
				if !strings.Contains(r.stdout, "fixture body") {
					t.Fatal("notification filter hid authorized read")
				}
			}
		})
	}
}

// A followed thread remains an existing positive selector for general replies,
// but cannot admit a reply whose own recipients exclude this identity.
func TestAddressedContractThreadControl(t *testing.T) {
	for _, own := range []bool{true, false} {
		t.Run(fmt.Sprintf("general=%t", own), func(t *testing.T) {
			e, room := addressedFixture(t)
			e.as("alpha", "inbox").ok(t)
			_, err := config.UpdateState(e.paths(), "alpha", func(st *config.State) error { st.AddThread("$followed"); return nil })
			if err != nil {
				t.Fatal(err)
			}
			extra := map[string]any{"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$followed"}}
			if !own {
				extra[ContentKey] = Header{V: 1, Type: "message", To: []string{"beta"}}
			}
			id := e.hs.inject(room, "@sender:example.org", msg("reply", extra))
			r := e.as("alpha", "inbox").ok(t)
			if strings.Contains(r.stdout, id) != own || e.received("alpha", id) != own {
				t.Fatal("NONRECIPIENT THREAD OR POSITIVE REJECTED")
			}
		})
	}
}
