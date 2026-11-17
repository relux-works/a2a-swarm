package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func incAppendLine(t *testing.T, p Paths, entry MailEntry) int64 {
	t.Helper()
	plain := p.WithReadCache(nil)
	m, err := OpenMailbox(plain, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(p.MailboxFile("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append([]MailEntry{entry}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p.MailboxFile("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return after.Size() - before.Size()
}

func incAppendRaw(t *testing.T, path string, raw []byte) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func incEventEntry(id, body string) MailEntry {
	raw, _ := json.Marshal(matrix.Event{EventID: id, Type: "m.room.message", Content: json.RawMessage(`{"body":"` + body + `"}`)})
	return MailEntry{ID: id, Kind: MailEvent, Room: "!fixture", Event: raw}
}

// TestMailboxIncrementalAppend proves a same-inode append is served by
// parsing only the appended bytes, beside an unchanged control that parses
// nothing; the appended entry and the earlier entry are both visible.
func TestMailboxIncrementalAppend(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	if got := pendingIDs(cachedMail(t, p)); len(got) != 1 || got[0] != "$one" {
		t.Fatalf("warm pending = %v", got)
	}
	before := ReadStateDecodeStats()
	if got := pendingIDs(cachedMail(t, p)); len(got) != 1 || got[0] != "$one" {
		t.Fatalf("unchanged control pending = %v", got)
	}
	if after := ReadStateDecodeStats(); after.MailboxDecodes != before.MailboxDecodes || after.MailboxParseBytes != before.MailboxParseBytes {
		t.Fatal("unchanged control parsed mailbox bytes")
	}
	appended := incAppendLine(t, p, incEventEntry("$two", "two"))
	before = ReadStateDecodeStats()
	got := pendingIDs(cachedMail(t, p))
	after := ReadStateDecodeStats()
	if len(got) != 2 || got[0] != "$one" || got[1] != "$two" {
		t.Fatalf("pending after append = %v", got)
	}
	if after.MailboxDecodes != before.MailboxDecodes {
		t.Fatal("append took a full mailbox parse")
	}
	if d := after.MailboxParseBytes - before.MailboxParseBytes; d != appended {
		t.Fatalf("parsed bytes = %d, appended = %d", d, appended)
	}
	if after.MailboxIncrementalReads <= before.MailboxIncrementalReads {
		t.Fatal("append was not an incremental read")
	}
}

// TestMailboxIncrementalNewInode proves a replaced mailbox (new inode) takes
// exactly one full parse and shows the new content, beside a same-inode
// append control that stays incremental.
func TestMailboxIncrementalNewInode(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	cachedMail(t, p)
	incAppendLine(t, p, incEventEntry("$two", "two"))
	before := ReadStateDecodeStats()
	if got := pendingIDs(cachedMail(t, p)); len(got) != 2 {
		t.Fatalf("append control pending = %v", got)
	}
	if ReadStateDecodeStats().MailboxDecodes != before.MailboxDecodes {
		t.Fatal("same-inode append control took a full parse")
	}
	path := p.MailboxFile("fixture")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var extra bytes.Buffer
	if err := writeLine(&extra, mailLine{Op: "add", Entry: func() *MailEntry { e := incEventEntry("$three", "three"); return &e }()}); err != nil {
		t.Fatal(err)
	}
	tmp := path + ".replacement"
	if err := os.WriteFile(tmp, append(raw, extra.Bytes()...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	before = ReadStateDecodeStats()
	got := pendingIDs(cachedMail(t, p))
	if len(got) != 3 || got[2] != "$three" {
		t.Fatalf("pending after replacement = %v", got)
	}
	if d := ReadStateDecodeStats().MailboxDecodes - before.MailboxDecodes; d != 1 {
		t.Fatalf("new-inode replacement full parses = %d, want 1", d)
	}
}

// TestMailboxIncrementalPrefixMismatch proves a same-inode truncate-and-
// rewrite to a longer file with a different prefix takes a full parse and
// shows the new prefix, beside unchanged and post-rewrite append controls
// that stay incremental. The rewritten first line has exactly the parsed first line's
// length, so the old offset lands on a line boundary: without the prefix
// check an incremental read would silently keep the old first entry.
func TestMailboxIncrementalPrefixMismatch(t *testing.T) {
	p := cachedPaths(t)
	plain := p.WithReadCache(nil)
	m, err := OpenMailbox(plain, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append([]MailEntry{incEventEntry("$one", "one!")}); err != nil {
		t.Fatal(err)
	}
	cachedMail(t, p)
	before := ReadStateDecodeStats()
	cachedMail(t, p)
	if ReadStateDecodeStats().MailboxDecodes != before.MailboxDecodes {
		t.Fatal("unchanged control took a full parse")
	}
	path := p.MailboxFile("fixture")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	oldRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstLineLen := bytes.IndexByte(oldRaw, '\n') + 1
	var buf bytes.Buffer
	for _, e := range []MailEntry{incEventEntry("$six", "six!"), incEventEntry("$pad", strings.Repeat("p", 4096))} {
		e := e
		if err := writeLine(&buf, mailLine{Op: "add", Entry: &e}); err != nil {
			t.Fatal(err)
		}
	}
	if got := bytes.IndexByte(buf.Bytes(), '\n') + 1; got != firstLineLen {
		t.Fatalf("rewrite first line = %d bytes, parsed first line = %d", got, firstLineLen)
	}
	if int64(buf.Len()) <= info.Size() {
		t.Fatal("rewrite control is not longer than the parsed file")
	}
	if err := os.WriteFile(path, buf.Bytes(), 0600); err != nil { // O_TRUNC: same inode
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, afterInfo) {
		t.Fatal("rewrite control changed inode")
	}
	before = ReadStateDecodeStats()
	got := pendingIDs(cachedMail(t, p))
	if len(got) != 2 || got[0] != "$six" || got[1] != "$pad" {
		t.Fatalf("pending after prefix rewrite = %v", got)
	}
	if d := ReadStateDecodeStats().MailboxDecodes - before.MailboxDecodes; d != 1 {
		t.Fatalf("prefix mismatch full parses = %d, want 1", d)
	}
	incAppendLine(t, p, incEventEntry("$zz2", "two!"))
	before = ReadStateDecodeStats()
	if got := pendingIDs(cachedMail(t, p)); len(got) != 3 || got[2] != "$zz2" {
		t.Fatalf("post-rewrite append pending = %v", got)
	}
	if ReadStateDecodeStats().MailboxDecodes != before.MailboxDecodes {
		t.Fatal("post-rewrite append control took a full parse")
	}
}

// TestMailboxIncrementalTornLine proves the incremental path keeps an
// unterminated final line unparsed — first an incomplete JSON fragment,
// then a complete JSON line missing its newline — and parses each exactly
// once when completed, while the earlier entry stays visible throughout.
func TestMailboxIncrementalTornLine(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	cachedMail(t, p)
	path := p.MailboxFile("fixture")
	var line bytes.Buffer
	e2 := incEventEntry("$two", "two")
	if err := writeLine(&line, mailLine{Op: "add", Entry: &e2}); err != nil {
		t.Fatal(err)
	}
	full := line.Bytes()
	half := len(full) / 2
	incAppendRaw(t, path, full[:half])
	if got := pendingIDs(cachedMail(t, p)); len(got) != 1 || got[0] != "$one" {
		t.Fatalf("pending with torn fragment = %v", got)
	}
	incAppendRaw(t, path, full[half:])
	if got := pendingIDs(cachedMail(t, p)); len(got) != 2 || got[0] != "$one" || got[1] != "$two" {
		t.Fatalf("pending after completing torn line = %v", got)
	}
	line.Reset()
	e3 := incEventEntry("$three", "three")
	if err := writeLine(&line, mailLine{Op: "add", Entry: &e3}); err != nil {
		t.Fatal(err)
	}
	incAppendRaw(t, path, bytes.TrimSuffix(line.Bytes(), []byte("\n")))
	if got := pendingIDs(cachedMail(t, p)); len(got) != 2 {
		t.Fatalf("unterminated complete line became visible early: %v", got)
	}
	incAppendRaw(t, path, []byte("\n"))
	got := pendingIDs(cachedMail(t, p))
	if len(got) != 3 || got[2] != "$three" {
		t.Fatalf("pending after terminating line = %v", got)
	}
	if n := strings.Count(strings.Join(got, ","), "$three"); n != 1 {
		t.Fatalf("$three parsed %d times", n)
	}
}

// TestMailboxIncrementalCompaction proves compaction under a consumer that
// holds an incremental offset takes a full parse with no duplicate or lost
// entry, beside a pre-compaction append control that stays incremental.
func TestMailboxIncrementalCompaction(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	incAppendLine(t, p, incEventEntry("$two", "two"))
	cachedMail(t, p)
	incAppendLine(t, p, incEventEntry("$three", "three"))
	before := ReadStateDecodeStats()
	if got := pendingIDs(cachedMail(t, p)); len(got) != 3 {
		t.Fatalf("append control pending = %v", got)
	}
	if ReadStateDecodeStats().MailboxDecodes != before.MailboxDecodes {
		t.Fatal("append control took a full parse")
	}
	plain := p.WithReadCache(nil)
	other, err := OpenMailbox(plain, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Receive([]string{"$one"}); err != nil {
		t.Fatal(err)
	}
	if err := other.compact(); err != nil {
		t.Fatal(err)
	}
	before = ReadStateDecodeStats()
	m := cachedMail(t, p)
	got := pendingIDs(m)
	if len(got) != 2 || got[0] != "$two" || got[1] != "$three" || !m.Received("$one") {
		t.Fatalf("pending after compaction = %v received-$one=%v", got, m.Received("$one"))
	}
	if d := ReadStateDecodeStats().MailboxDecodes - before.MailboxDecodes; d != 1 {
		t.Fatalf("compaction full parses = %d, want 1", d)
	}
}

// TestMailboxIncrementalRedaction proves a redaction line appended after an
// entry was parsed scrubs that entry on the next incremental read, while a
// neighbouring entry keeps its content.
func TestMailboxIncrementalRedaction(t *testing.T) {
	p := cachedPaths(t)
	plain := p.WithReadCache(nil)
	m, err := OpenMailbox(plain, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append([]MailEntry{incEventEntry("$victim", "remove-this"), incEventEntry("$neighbour", "keep-this")}); err != nil {
		t.Fatal(err)
	}
	warm := cachedMail(t, p)
	if !strings.Contains(string(warm.live["$victim"].Event), "remove-this") {
		t.Fatal("control entry content missing before redaction")
	}
	var line bytes.Buffer
	if err := writeLine(&line, mailLine{Op: "redact", Targets: []Redaction{{Room: "!fixture", EventID: "$victim"}}}); err != nil {
		t.Fatal(err)
	}
	incAppendRaw(t, p.MailboxFile("fixture"), line.Bytes())
	before := ReadStateDecodeStats()
	got := cachedMail(t, p)
	if ReadStateDecodeStats().MailboxDecodes != before.MailboxDecodes {
		t.Fatal("redaction append took a full parse")
	}
	if strings.Contains(string(got.live["$victim"].Event), "remove-this") {
		t.Fatal("redacted entry still shows its content after the next read")
	}
	if !strings.Contains(string(got.live["$neighbour"].Event), "keep-this") {
		t.Fatal("neighbouring entry lost its content")
	}
	if !got.IsRedacted("!fixture", "$victim") {
		t.Fatal("redaction tombstone not visible after incremental read")
	}
}

// BenchmarkOpenMailboxAppend measures reopening a mailbox after one small
// same-inode append to a multi-megabyte file, through a consumer read cache.
func BenchmarkOpenMailboxAppend(b *testing.B) {
	p := Paths{Base: b.TempDir()}.WithReadCache(NewReadCache("fixture"))
	plain := p.WithReadCache(nil)
	m, err := OpenMailbox(plain, "fixture")
	if err != nil {
		b.Fatal(err)
	}
	var batch []MailEntry
	for i := 0; i < 300; i++ {
		batch = append(batch, incEventEntry(fmt.Sprintf("$bench%04d", i), strings.Repeat("x", 10000)))
	}
	if _, err := m.Append(batch); err != nil {
		b.Fatal(err)
	}
	ids := make([]string, 0, len(batch))
	for _, e := range batch {
		ids = append(ids, e.ID)
	}
	if _, err := m.Receive(ids); err != nil {
		b.Fatal(err)
	}
	if _, err := OpenMailbox(p, "fixture"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		pad := append([]byte(`{"op":"padding"}`), bytes.Repeat([]byte(" "), 1024)...)
		pad = append(pad, '\n')
		f, err := os.OpenFile(p.MailboxFile("fixture"), os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := f.Write(pad); err != nil {
			b.Fatal(err)
		}
		if err := f.Close(); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := OpenMailbox(p, "fixture"); err != nil {
			b.Fatal(err)
		}
	}
}
