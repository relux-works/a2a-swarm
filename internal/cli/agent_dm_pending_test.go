package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Pair filtering leaves opaque ordinary mailbox entries visible. An opaque
// known-pair entry stays unread and hidden; valid peer/principal controls prove
// that the same transaction neither rejects all mail nor weakens a held pair.
func TestAgentDMPendingOpaqueOrdinaryIsolation(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "qualified", true: "records-corrupt"}[corrupt], func(t *testing.T) {
			e, tr, a, b, room := activeAgentFixture(t)
			ownerPut(t, ownerFiles(e.paths(), "bob").path("principals.json"), principalConfig{V: 1, Principals: []string{"@parent:" + testServer}})
			if corrupt {
				if err := os.WriteFile(filepath.Join(agentDMDir(e.paths(), "bob"), "records.json"), []byte(`{`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			s, _ := agentRecoverySession(t, e, tr, "bob")
			peer, err := json.Marshal(agentDirectedEvent(t, "$opaque-peer", a, b))
			if err != nil {
				t.Fatal(err)
			}
			principal, err := json.Marshal(agentDirectedEvent(t, "$opaque-principal", "@parent:"+testServer, "@wrong:"+testServer))
			if err != nil {
				t.Fatal(err)
			}
			entries := []config.MailEntry{
				{ID: "$opaque-ordinary", Kind: config.MailEvent, Room: "!ordinary:" + testServer, Rel: "to"},
				{ID: "$opaque-held", Kind: config.MailEvent, Room: room, Rel: "to"},
				{ID: "$opaque-peer", Kind: config.MailEvent, Room: room, Rel: "to", Event: peer},
				{ID: "$opaque-principal", Kind: config.MailEvent, Room: room, Rel: "to", Event: principal},
			}
			if _, err := e.mailbox("bob").Append(entries); err != nil {
				t.Fatal(err)
			}
			visible := map[string]bool{}
			_, err = s.lockedDeliver(context.Background(), time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) {
				for _, entry := range mb.Pending() {
					visible[entry.ID] = true
				}
				return nil, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !visible["$opaque-ordinary"] || !visible["$opaque-principal"] || visible["$opaque-held"] || visible["$opaque-peer"] != !corrupt || len(visible) != map[bool]int{false: 3, true: 2}[corrupt] {
				t.Fatalf("opaque ordinary/pair isolation assertion: corrupt=%v visible=%v", corrupt, visible)
			}
			mb := e.mailbox("bob")
			for _, entry := range entries {
				if !mb.Has(entry.ID) || mb.Received(entry.ID) {
					t.Fatalf("Pending view removed or receipted %s", entry.ID)
				}
			}
		})
	}
}

// A fresh ack process receipts shown active-pair mail, including all-shown.
// Current admission or policy withdrawal closes implicit selection without
// losing the unread entry. Explicit-ID ack remains the existing explicit act.
func TestAgentDMAckShownCurrentPolicy(t *testing.T) {
	for _, mode := range []string{"explicit-id", "all-shown", "revoked", "state-failed"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, _, b, room := activeAgentFixture(t)
			setAgentCursor(t, e, "bob")
			e.as("alice", "agent-dm", "say", b, "--kind", "question", "shown pair mail").ok(t)
			messages := agentMessageEvents(e, room)
			id := messages[len(messages)-1].EventID
			shown := e.as("bob", "wait", "--timeout", "3s").ok(t)
			if !strings.Contains(shown.stdout, id) && !strings.Contains(shown.stdout, "shown pair mail") {
				t.Fatal("setup did not show pair mail")
			}
			if mode == "revoked" {
				stageAgentAdmission(t, e, "bob", b, "@other:"+testServer)
			}
			if mode == "state-failed" {
				tr.setFault("state")
			}
			ack := "all-shown"
			if mode == "explicit-id" {
				ack = id
			}
			result := e.as("bob", "ack", ack).ok(t)
			mb, err := config.OpenMailbox(e.paths(), "bob")
			if err != nil {
				t.Fatal(err)
			}
			want := mode == "explicit-id" || mode == "all-shown"
			if mb.Received(id) != want || !want && (!mb.Has(id) || !strings.Contains(result.stdout, "nothing pending")) {
				t.Fatalf("shown ack policy assertion: mode=%s received=%v stdout=%q", mode, mb.Received(id), result.stdout)
			}
		})
	}
}

// The common Pending boundary used by notices, binding wakes and first-sync
// inbox refreshes policy in a reused session even when local files do not
// change. This tests selection, not a real terminal wake or daemon lifecycle.
func TestAgentDMPendingConsumersRefresh(t *testing.T) {
	e, tr, _, b, room := activeAgentFixture(t)
	setAgentCursor(t, e, "bob")
	e.as("alice", "agent-dm", "say", b, "--kind", "question", "retained pending").ok(t)
	e.as("bob", "wait", "--timeout", "3s").ok(t)
	var output bytes.Buffer
	app := &App{as: "bob", Stdout: &output, Stderr: &output, HTTP: &http.Client{Transport: tr}, Now: time.Now, Getenv: func(k string) string {
		if k == config.EnvConfigDir {
			return e.dir
		}
		return ""
	}}
	s, err := app.session()
	if err != nil {
		t.Fatal(err)
	}
	defer app.closeAdmission()
	count := func() int {
		t.Helper()
		n := 0
		_, err := s.lockedDeliver(context.Background(), time.Time{}, false, func(_ *config.State, mb *config.Mailbox) ([]string, error) { n = len(mb.Pending()); return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count() != 1 {
		t.Fatal("pending refresh assertion: valid pair hidden")
	}
	stamp := s.agentGateStamp()
	tr.setFault("state")
	if count() != 0 || s.agentGateStamp() != stamp {
		t.Fatal("pending refresh assertion: stale pair policy admitted mail")
	}
	tr.setFault("")
	if count() != 1 {
		t.Fatal("pending refresh assertion: recovery lost unreceived pair mail")
	}
	if len(agentMessageEvents(e, room)) != 1 {
		t.Fatal("pending refresh mutated remote messages")
	}
}
