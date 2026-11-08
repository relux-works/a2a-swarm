package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// Pure fixture coverage: no account, network or invite lifecycle operation.
// Each selected server sees both same-localpart inviters, plus quoted and
// expanded senders. JSON decoding, mailbox storage and saved-frame rereading
// must retain the original decoded value; only human output quotes it.
func TestSenderInviteConsumers(t *testing.T) {
	for _, server := range []string{"alpha.example", "beta.example"} {
		t.Run(server, func(t *testing.T) {
			me := "@alice:" + server
			cases := append([]senderCase{
				{"sam-alpha", "@sam:alpha.example", "@sam:alpha.example"},
				{"sam-beta", "@sam:beta.example", "@sam:beta.example"},
			}, senderCases()...)
			var entries []config.MailEntry
			var items []waitItem
			var ids []string
			var wantText strings.Builder
			for i, tc := range cases {
				roomID := fmt.Sprintf("!invite%d:%s", i, server)
				// Include a long, untrusted label to exercise the pre-sender line cap.
				alias := "#" + strings.Repeat("r", 410) + ":" + server
				aliasJSON, err := json.Marshal(map[string]string{"alias": alias})
				if err != nil {
					t.Fatal(err)
				}
				original := matrix.Event{Type: "m.room.member", Sender: tc.raw, StateKey: &me, Content: json.RawMessage(`{"membership":"invite"}`)}
				wire, err := json.Marshal(original)
				if err != nil {
					t.Fatal(err)
				}
				var decoded matrix.Event
				if err := json.Unmarshal(wire, &decoded); err != nil {
					t.Fatal(err)
				}
				state := []matrix.Event{{Type: "m.room.canonical_alias", Content: aliasJSON}, decoded}
				room, inviter := inviteInfo(roomID, state, me, server)
				entry := inviteEntry(roomID, "seed", state, me, server)
				item := itemOf(entry, server)
				if inviter != tc.raw || entry.Sender != tc.raw || item.rec.Sender != tc.raw || item.rec.Type != "invite" || entry.Kind != config.MailInvite {
					t.Fatalf("invite sender changed: case=%s inviter=%q mailbox=%q record=%q want=%q", tc.name, inviter, entry.Sender, item.rec.Sender, tc.raw)
				}
				want := snippet("invite "+room+" "+roomID, 400) + " from=" + tc.text
				if item.line != want || inviteLine(roomID, state, me, server) != want || decodedSender(t, strings.TrimPrefix(item.line, snippet("invite "+room+" "+roomID, 400)+" from=")) != tc.raw {
					t.Fatalf("invite text sender changed: case=%s got=%q want=%q", tc.name, item.line, want)
				}
				entries = append(entries, entry)
				items = append(items, item)
				ids = append(ids, entry.ID)
				wantText.WriteString(want + "\n")
			}
			paths := config.Paths{Base: t.TempDir()}
			mb, err := config.OpenMailbox(paths, "alice")
			if err != nil {
				t.Fatal(err)
			}
			if n, err := mb.Append(entries); err != nil || n != len(entries) {
				t.Fatal("invite mailbox append changed")
			}
			mb, err = config.OpenMailbox(paths, "alice")
			if err != nil {
				t.Fatal(err)
			}
			pending := mb.Pending()
			if len(pending) != len(cases) {
				t.Fatal("invite mailbox count changed")
			}
			for i, entry := range pending {
				if entry.Sender != cases[i].raw || mb.Received(entry.ID) {
					t.Fatal("persisted invite sender or receipt changed")
				}
			}
			items = itemsOf(pending, server)
			var out bytes.Buffer
			a := &App{Stdout: &out}
			if err := a.emit(items, false); err != nil {
				t.Fatal(err)
			}
			if out.String() != wantText.String() {
				t.Fatal("invite text emission changed")
			}
			out.Reset()
			if err := a.emit(items, true); err != nil {
				t.Fatal(err)
			}
			records := decodeRecords(t, out.String())
			if len(records) != len(cases) {
				t.Fatal("invite JSON count changed")
			}
			for i, rec := range records {
				if rec.Type != "invite" || rec.Sender != cases[i].raw {
					t.Fatal("invite JSON sender changed")
				}
			}
			frame := inboxFrame(items, server, 0)
			wantFrame := inboxBegin + "\n"
			for i, item := range items {
				wantFrame += fmt.Sprintf("--- event %d/%d: %s\n", i+1, len(items), item.line)
			}
			wantFrame += inboxEnd + "\n"
			if frame != wantFrame {
				t.Fatal("invite inbox sender clipped or frame changed")
			}
			inbox, err := inboxJSON(items, server)
			if err != nil {
				t.Fatal(err)
			}
			records = decodeRecords(t, inbox)
			for i, rec := range records {
				if rec.Sender != cases[i].raw {
					t.Fatal("invite inbox JSON sender changed")
				}
			}
			records, saved, err := savedInboxFrame(a, items, server)
			if err != nil {
				t.Fatal(err)
			}
			assertSenderRecords(t, records, ids, cases)
			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "invites.frame")
			if err := os.WriteFile(path, saved, 0600); err != nil {
				t.Fatal(err)
			}
			reread, err := readSavedInbox(path)
			if err != nil {
				t.Fatal(err)
			}
			assertSenderRecords(t, reread, ids, cases)
			summary, err := inboxSummary(reread, 160, 0, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, tc := range cases {
				if !strings.Contains(summary, "count sender="+tc.text+" n=1\n") {
					t.Fatal("invite summary sender changed")
				}
			}
			for i, id := range ids {
				out.Reset()
				if err := cmdInboxShow(a, []string{id, "--from", path}); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(out.String(), "from="+cases[i].text+" room=") {
					t.Fatalf("invite saved show sender changed: %s", cases[i].name)
				}
			}
		})
	}
}

// Historical saved identifiers stay exactly as recorded: no migration or
// fabricated server is permitted when the old frame contains a localpart.
func TestSenderInviteHistoricalFrame(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "historical.frame")
	data := []byte(inboxBegin + "\n" + inboxIndent + `{"type":"invite","room_id":"!old:alpha.example","event_id":"invite:old","sender":"sam","body":"invite old from=sam"}` + "\n" + inboxEnd + "\n")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	records, err := readSavedInbox(path)
	if err != nil || len(records) != 1 || records[0].Sender != "sam" {
		t.Fatal("historical invite sender changed")
	}
	var out bytes.Buffer
	if err := cmdInboxShow(&App{Stdout: &out}, []string{"invite:old", "--from", path}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `from="sam"`) {
		t.Fatal("historical invite sender fabricated")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, after) {
		t.Fatal("historical saved frame was modified")
	}
}
