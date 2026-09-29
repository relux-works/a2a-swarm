package config

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

func ev(id string) MailEntry { return MailEntry{ID: id, Kind: MailEvent, Room: "!r", Rel: "to"} }

func pendingIDs(m *Mailbox) []string {
	var ids []string
	for _, e := range m.Pending() {
		ids = append(ids, e.ID)
	}
	return ids
}

// Proves append, dedup, shown marks and receipts survive a reload: a duplicate
// id (also one already received) is never appended again, only unreceipted
// entries are pending (in order), and receiving an unknown id changes nothing.
func TestMailboxRoundTrip(t *testing.T) {
	p := testPaths(t)
	m, err := OpenMailbox(p, "a")
	if err != nil || len(m.Pending()) != 0 {
		t.Fatalf("empty mailbox: %v", err)
	}
	if n, err := m.Append([]MailEntry{ev("$1"), ev("$2"), ev("$1")}); err != nil || n != 2 {
		t.Fatalf("append = %d, %v", n, err)
	}
	if err := m.MarkShown([]string{"$2"}); err != nil {
		t.Fatal(err)
	}
	if n, err := m.Receive([]string{"$1", "$nope"}); err != nil || n != 1 {
		t.Fatalf("receive = %d, %v", n, err)
	}
	m, err = OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(pendingIDs(m), ","); got != "$2" || !m.Pending()[0].Shown || !m.Received("$1") {
		t.Fatalf("after reload pending = %s", got)
	}
	if n, _ := m.Append([]MailEntry{ev("$1"), ev("$2"), ev("$3")}); n != 1 {
		t.Fatalf("re-synced duplicates appended: %d", n)
	}
	if st, err := os.Lstat(p.MailboxFile("a")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode(), err)
	}
}

// Proves compaction drops received entries but never an unreceipted one,
// keeps shown marks, and bounds the receipt index to ReceiptIndexMax while the
// newest received ids still deduplicate.
func TestMailboxCompaction(t *testing.T) {
	p := testPaths(t)
	m, _ := OpenMailbox(p, "a")
	var batch []MailEntry
	for i := 0; i < ReceiptIndexMax+1500; i++ {
		batch = append(batch, ev("$e"+strconv.Itoa(i)))
	}
	if _, err := m.Append(batch); err != nil {
		t.Fatal(err)
	}
	m.MarkShown([]string{"$e0"})
	var ids []string
	for i := 1; i < len(batch); i++ {
		ids = append(ids, batch[i].ID)
	}
	if _, err := m.Receive(ids); err != nil {
		t.Fatal(err)
	}
	m, _ = OpenMailbox(p, "a")
	if got := pendingIDs(m); len(got) != 1 || got[0] != "$e0" || !m.Pending()[0].Shown {
		t.Fatalf("pending after compaction = %v", got)
	}
	if len(m.received) != ReceiptIndexMax || m.Received("$e1") || !m.Received(batch[len(batch)-1].ID) {
		t.Fatalf("receipt index = %d", len(m.received))
	}
	data, _ := os.ReadFile(p.MailboxFile("a"))
	if strings.Count(string(data), `"op":"add"`) != 1 {
		t.Fatal("received entries kept after compaction")
	}
}

// Proves a symlinked or 0644 mailbox is refused on open and on write, a torn
// final line (a crashed append) is ignored, and a corrupt middle line is an
// error; a regular 0600 file opens (positive control).
func TestMailboxRefusals(t *testing.T) {
	p := testPaths(t)
	m, _ := OpenMailbox(p, "a")
	m.Append([]MailEntry{ev("$1")})
	path := p.MailboxFile("a")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"op":"add","entry":{"id":"$2"`)
	f.Close()
	if m, err := OpenMailbox(p, "a"); err != nil || len(m.Pending()) != 1 {
		t.Fatalf("torn tail: %v", err)
	}
	os.WriteFile(path, []byte("garbage\n{\"op\":\"rcpt\"}\n"), 0o600)
	if _, err := OpenMailbox(p, "a"); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("corrupt line accepted: %v", err)
	}
	os.Chmod(path, 0o644)
	if _, err := OpenMailbox(p, "a"); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("0644 accepted: %v", err)
	}
	os.Remove(path)
	target := path + ".target"
	os.WriteFile(target, nil, 0o600)
	os.Symlink(target, path)
	if _, err := OpenMailbox(p, "a"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink accepted on open: %v", err)
	}
	m = &Mailbox{path: path, live: map[string]*MailEntry{}, recv: map[string]bool{}}
	if _, err := m.Append([]MailEntry{ev("$9")}); err == nil {
		t.Fatal("append followed a symlink")
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatal("the symlink target was written")
	}
}
