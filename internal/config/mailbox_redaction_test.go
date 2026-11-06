package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func TestMailboxRedactionRetentionAndScope(t *testing.T) {
	p := testPaths(t)
	m, err := OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	entry := func(id, room, body string) MailEntry {
		raw, _ := json.Marshal(matrix.Event{EventID: id, Type: "m.room.message", Content: json.RawMessage(`{"body":"` + body + `","private":"` + body + `"}`)})
		return MailEntry{ID: id, Kind: MailEvent, Room: room, Event: raw}
	}
	if _, err = m.Append([]MailEntry{entry("$old", "!r", "removed-fixture"), entry("$neighbour", "!r", "positive-control")}); err != nil {
		t.Fatal(err)
	}
	if err = m.MarkShown([]string{"$old"}); err != nil {
		t.Fatal(err)
	}
	if err = m.Redact([]Redaction{{Room: "!r", EventID: "$old"}, {Room: "!other", EventID: "$future"}}); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Pending()) != 2 || !m.Pending()[0].Shown || m.Received("$old") {
		t.Fatal("pending delivery changed")
	}
	data, _ := os.ReadFile(p.MailboxFile("a"))
	if strings.Contains(string(data), "removed-fixture") || !strings.Contains(string(data), "positive-control") {
		t.Fatal("payload removal or neighbour preservation failed")
	}
	if _, err = m.Append([]MailEntry{entry("$future", "!r", "different-room-control")}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(m.Pending()[2].Event), "[redacted]") || !strings.Contains(string(m.Pending()[2].Event), "different-room-control") {
		t.Fatal("tombstone crossed room scope")
	}
	// Receipts and compaction do not prune redaction knowledge.
	if _, err = m.Receive([]string{"$old"}); err != nil {
		t.Fatal(err)
	}
	if err = m.compact(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !m.IsRedacted("!r", "$old") || !m.IsRedacted("!other", "$future") {
		t.Fatal("receipt or restart erased tombstone")
	}
	if _, err = m.Append([]MailEntry{entry("$old", "!r", "removed-fixture")}); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(p.MailboxFile("a"))
	if strings.Contains(string(data), "removed-fixture") {
		t.Fatal("received replay resurrected content")
	}
}

// Upgrade a mailbox written by a client that did not scrub replacement edits.
// Opening remains read-only; the next persist must rewrite stale payloads even
// if the original tombstone is already known and no new redactions arrive.
func TestMailboxRedactionStoredEdits(t *testing.T) {
	p := testPaths(t)
	target := Redaction{Room: "!r", EventID: "$gone"}
	entry := func(id, room, rel string) MailEntry {
		content, _ := json.Marshal(map[string]any{"body": "stored-edit-sentinel", "m.new_content": map[string]string{"body": "stored-edit-sentinel"}, "m.relates_to": map[string]string{"rel_type": rel, "event_id": "$gone"}})
		raw, _ := json.Marshal(matrix.Event{Type: "m.room.message", EventID: id, Content: content})
		return MailEntry{Kind: MailEvent, ID: id, Room: room, Event: raw}
	}
	m, _ := OpenMailbox(p, "a")
	if _, err := m.Append([]MailEntry{entry("$edit", "!r", "m.replace"), entry("$other-room", "!other", "m.replace"), entry("$reply", "!r", "m.thread")}); err != nil {
		t.Fatal(err)
	}
	// Append old-format redaction knowledge without rewriting the edit.
	var line strings.Builder
	b, _ := json.Marshal(mailLine{Op: "redact", Targets: []Redaction{target}})
	line.Write(b)
	line.WriteByte('\n')
	f, err := os.OpenFile(p.MailboxFile("a"), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(line.String()); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	pending := m.Pending()
	if len(pending) != 3 || strings.Contains(string(pending[0].Event), "stored-edit-sentinel") {
		t.Fatal("restart exposed stale edit")
	}
	for _, e := range pending[1:] {
		if !strings.Contains(string(e.Event), "stored-edit-sentinel") {
			t.Fatal("room scope or unrelated relation suppressed")
		}
	}
	if err = m.Redact(nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.MailboxFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range strings.Split(string(data), "\n") {
		var l mailLine
		_ = json.Unmarshal([]byte(s), &l)
		if l.Entry != nil && l.Entry.ID == "$edit" && strings.Contains(string(l.Entry.Event), "stored-edit-sentinel") {
			t.Fatal("old edit persisted after no-new-target sync")
		}
	}
	if err = m.MarkShown([]string{"$edit"}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Receive([]string{"$edit"}); err != nil {
		t.Fatal(err)
	}
	if err = m.compact(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = m.Append([]MailEntry{entry("$fresh-edit", "!r", "m.replace")}); err != nil {
		t.Fatal(err)
	}
	pending = m.Pending()
	if !m.Received("$edit") || len(pending) != 3 || strings.Contains(string(pending[2].Event), "stored-edit-sentinel") {
		t.Fatal("receipt/compaction/replayed target state lost")
	}
}
