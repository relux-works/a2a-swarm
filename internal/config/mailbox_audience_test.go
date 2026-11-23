package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/audience"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// A narrowing pending view survives reopen without Shown/receipt mutations;
// raw access still lets authorized redaction scrub excluded content.
func TestMailboxAudienceViewAndScrub(t *testing.T) {
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache("alpha"))
	m, err := OpenMailbox(p, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	ev := matrix.Event{EventID: "$message", Type: "m.room.message", Sender: "@sender:example.org", Content: json.RawMessage(`{"body":"sensitive fixture body"}`)}
	raw, _ := json.Marshal(ev)
	entry := MailEntry{ID: ev.EventID, Room: "!room:example.org", Kind: MailEvent, Event: raw}
	e := audience.Evidence{Version: 1, Room: entry.Room, EventID: entry.ID, UserID: "@alpha:example.org", Name: "alpha", Sender: ev.Sender, Decision: audience.Other, Source: audience.Header, ObservedAt: time.Now()}
	if err = m.SaveAudiences([]audience.Evidence{e}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Append([]MailEntry{entry}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		m, err = OpenMailbox(p, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		m.SetPendingFilter(func(e MailEntry) bool { return e.ID != entry.ID })
		if len(m.Pending()) != 0 || len(m.RawPending()) != 1 || m.RawPending()[0].Shown || m.Received(entry.ID) {
			t.Fatal("exclusion mutated delivery state")
		}
		// The nearby valid view still exposes an allowed entry.
		m.SetPendingFilter(func(MailEntry) bool { return true })
		if len(m.Pending()) != 1 {
			t.Fatal("view rejects everything")
		}
	}
	if err = m.Redact([]Redaction{{Room: entry.Room, EventID: entry.ID}}); err != nil {
		t.Fatal(err)
	}
	if p.readCache.mailbox != nil || len(p.readCache.mailboxRaw) != 0 {
		t.Fatal("redaction retained an authorized decoded/raw cache copy")
	}
	data, err := os.ReadFile(p.MailboxFile("alpha"))
	if err != nil || strings.Contains(string(data), "sensitive fixture body") {
		t.Fatal("redaction retained body")
	}
	for i := 0; i < 2; i++ {
		m, err = OpenMailbox(p, "alpha")
		if err != nil {
			t.Fatal(err)
		}
		got, ok := m.Audience(audience.Key{Room: entry.Room, EventID: entry.ID})
		if !ok || got.Decision != audience.Other || m.Received(entry.ID) {
			t.Fatal("redaction lost audience or receipted excluded mail")
		}
	}
}

// Evidence is kept with live mail regardless of age; receipt starts a fresh
// fourteen-day minimum and compaction removes only an expired orphan.
func TestMailboxAudienceRetentionAndCacheIsolation(t *testing.T) {
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache("alpha"))
	m, err := OpenMailbox(p, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	k := audience.Key{Room: "!room:example.org", EventID: "$old"}
	e := audience.Evidence{Version: 1, Room: k.Room, EventID: k.EventID, Decision: audience.Self, ObservedAt: time.Now().Add(-30 * 24 * time.Hour)}
	if err = m.SaveAudiences([]audience.Evidence{e}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Append([]MailEntry{{ID: k.EventID, Room: k.Room, Kind: MailEvent}}); err != nil {
		t.Fatal(err)
	}
	if err = m.compact(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Audience(k); !ok {
		t.Fatal("expired live evidence removed")
	}
	before := time.Now()
	if _, err = m.Receive([]string{k.EventID}); err != nil {
		t.Fatal(err)
	}
	if err = m.compact(); err != nil {
		t.Fatal(err)
	}
	m, err = OpenMailbox(p, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := m.Audience(k)
	if !ok || got.RetainUntil.Before(before.Add(14*24*time.Hour)) {
		t.Fatal("receipt lost minimum retention")
	}
	clone := cloneMailbox(m)
	clone.audiences[k] = audience.Evidence{Version: 99}
	if got, _ := m.Audience(k); got.Version != 1 {
		t.Fatal("cache clone aliases evidence")
	}
	got.RetainUntil = time.Now().Add(-time.Hour)
	m.audiences[k] = got
	if err = m.compact(); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Audience(k); ok {
		t.Fatal("expired orphan evidence not compacted")
	}
}
