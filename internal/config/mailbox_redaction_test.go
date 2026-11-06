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
