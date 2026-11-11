package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Confirmed history is bounded across repeated successful sends at capacity;
// an uncertain attempt at either end blocks without a send or evidence edit.
// Fixtures preseed confirmed ACKs; no live server or key material is used.
func TestAgentDMSendEvidenceRing(t *testing.T) {
	for _, mode := range []string{"255-confirmed", "256-confirmed", "uncertain-first", "uncertain-last", "new-uncertain"} {
		t.Run(mode, func(t *testing.T) {
			e, tr, _, b, room := activeAgentFixture(t)
			dir := agentDMDir(e.paths(), "alice")
			var st agentDMRecords
			raw, err := agentRead(dir, "records.json")
			if err != nil {
				t.Fatal(err)
			}
			if err := agentDecode(raw, &st); err != nil {
				t.Fatal(err)
			}
			d := st.Pairs[b]
			n := 256
			if mode == "255-confirmed" {
				n = 255
			}
			for i := 0; i < n; i++ {
				d.SendAttempts = append(d.SendAttempts, agentDMSend{Transaction: fmt.Sprintf("a2a-adm-old%d", i), State: "confirmed", Event: fmt.Sprintf("$old%d", i)})
			}
			if mode == "uncertain-first" {
				d.SendAttempts[0].State = "started"
				d.SendAttempts[0].Event = ""
			}
			if mode == "uncertain-last" {
				d.SendAttempts[n-1].State = "started"
				d.SendAttempts[n-1].Event = ""
			}
			st.Pairs[b] = d
			if err := agentWrite(context.Background(), dir, "records.json", st); err != nil {
				t.Fatal(err)
			}
			beforeRaw, err := os.ReadFile(filepath.Join(dir, "records.json"))
			if err != nil {
				t.Fatal(err)
			}
			before := len(agentMessageEvents(e, room))
			if strings.HasPrefix(mode, "uncertain-") {
				e.as("alice", "agent-dm", "say", b, "--kind", "question", "must not send").failsWith(t, ExitError, "uncertain send retained")
				afterRaw, err := os.ReadFile(filepath.Join(dir, "records.json"))
				if err != nil {
					t.Fatal(err)
				}
				if string(afterRaw) != string(beforeRaw) || len(agentMessageEvents(e, room)) != before {
					t.Fatal("send ring uncertainty assertion: changed evidence or sent")
				}
				return
			}
			if mode == "new-uncertain" {
				tr.setFault("send-ack")
				e.as("alice", "agent-dm", "say", b, "--kind", "question", "uncertain at capacity").failsWith(t, ExitError, "uncertain outcome retained")
				tr.setFault("")
				d = agentDMEntry(t, e, "alice", b)
				if len(d.SendAttempts) != 256 || d.SendAttempts[255].State != "started" {
					t.Fatal("send ring uncertainty assertion: missing new fence")
				}
				e.as("alice", "agent-dm", "say", b, "--kind", "question", "must not retry").failsWith(t, ExitError, "uncertain send retained")
				if len(agentMessageEvents(e, room)) != before+1 {
					t.Fatal("send ring uncertainty assertion: repeated uncertain send")
				}
				return
			}
			for i := 0; i < 3; i++ {
				e.as("alice", "agent-dm", "say", b, "--kind", "question", "after confirmed capacity").ok(t)
			}
			d = agentDMEntry(t, e, "alice", b)
			if len(agentMessageEvents(e, room)) != before+3 || len(d.SendAttempts) != 256 || d.SendAttempts[0].Transaction == "a2a-adm-old0" {
				t.Fatal("send ring progress assertion: lost send, unbounded history or no compaction")
			}
			for _, attempt := range d.SendAttempts {
				if attempt.State != "confirmed" || attempt.Event == "" {
					t.Fatal("send ring progress assertion: lost ACK")
				}
			}
		})
	}
}
