package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func threadEntry(room string, n int) ThreadEvent {
	return ThreadEvent{Room: room, EventID: fmt.Sprintf("$event-%d", n), Root: "$root"}
}

func threadFileLines(t *testing.T, p Paths, name string) int {
	t.Helper()
	data, err := os.ReadFile(p.ThreadEventsFile(name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte("\n"))
}

func appendRawThread(t *testing.T, path string, raw []byte) {
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

func threadLine(t *testing.T, e ThreadEvent) []byte {
	t.Helper()
	data, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// Claim: appended entries survive a reload with room-scoped lookups, a repeat
// of a stored (room, event id) pair (even with another root) and invalid
// entries are never stored, and the file is a regular 0600 file. Distinct
// rooms holding the same event id are the controls.
func TestThreadEventsRoundTripDedup(t *testing.T) {
	p := testPaths(t)
	w, err := OpenThreadEvents(p, "a")
	if err != nil || w.Len() != 0 {
		t.Fatalf("empty window: %v", err)
	}
	n, err := AppendThreadEvents(p, "a", []ThreadEvent{
		{Room: "!room", EventID: "$child", Root: "$root"},
		{Room: "!room", EventID: "$child", Root: "$replacement"},
		{Room: "!other", EventID: "$child", Root: "$other-root"},
		{Room: "", EventID: "$bad", Root: "$root"},
		{Room: "!room", EventID: "$child", Root: "$root"},
	})
	if err != nil || n != 2 {
		t.Fatalf("append = %d, %v", n, err)
	}
	w, err = OpenThreadEvents(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if w.Len() != 2 || w.ThreadEventRoot("!room", "$child") != "$root" || w.ThreadEventRoot("!other", "$child") != "$other-root" || w.ThreadEventRoot("!unknown", "$child") != "" {
		t.Fatalf("reloaded window = %+v", w.Entries())
	}
	if !w.HasRoot("!room", "$root") || w.HasRoot("!room", "$other-root") || w.HasRoot("!other", "$root") {
		t.Fatal("root presence is not room-scoped")
	}
	if n, err := AppendThreadEvents(p, "a", []ThreadEvent{{Room: "!room", EventID: "$child", Root: "$root"}}); err != nil || n != 0 {
		t.Fatalf("replayed append = %d, %v", n, err)
	}
	if got := threadFileLines(t, p, "a"); got != 2 {
		t.Fatalf("file lines = %d, want 2", got)
	}
	if st, err := os.Lstat(p.ThreadEventsFile("a")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", st.Mode(), err)
	}
}

// Claim: when a stored file already holds two lines for one (room, event id)
// pair, lookup answers the newest line's root; a single-occurrence neighbour
// answers its only root.
func TestThreadEventsNewestLineWins(t *testing.T) {
	p := testPaths(t)
	path := p.ThreadEventsFile("a")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	buf.Write(threadLine(t, ThreadEvent{Room: "!room", EventID: "$child", Root: "$old"}))
	buf.Write(threadLine(t, ThreadEvent{Room: "!room", EventID: "$neighbour", Root: "$keep"}))
	buf.Write(threadLine(t, ThreadEvent{Room: "!room", EventID: "$child", Root: "$new"}))
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := OpenThreadEvents(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if w.Len() != 2 || w.ThreadEventRoot("!room", "$child") != "$new" || w.ThreadEventRoot("!room", "$neighbour") != "$keep" {
		t.Fatalf("window = %+v", w.Entries())
	}
}

// Claim: entry 2001 evicts only the oldest entry: the second-oldest and the
// newest still answer, and the retained window stays at the cap.
func TestThreadEventsEntry2001DropsOldestOnly(t *testing.T) {
	p := testPaths(t)
	var batch []ThreadEvent
	for i := 0; i < ThreadEventsMax; i++ {
		batch = append(batch, threadEntry("!room", i))
	}
	if n, err := AppendThreadEvents(p, "a", batch); err != nil || n != ThreadEventsMax {
		t.Fatalf("seed append = %d, %v", n, err)
	}
	if n, err := AppendThreadEvents(p, "a", []ThreadEvent{threadEntry("!room", ThreadEventsMax)}); err != nil || n != 1 {
		t.Fatalf("entry 2001 append = %d, %v", n, err)
	}
	w, err := OpenThreadEvents(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if w.Len() != ThreadEventsMax {
		t.Fatalf("window len = %d, want %d", w.Len(), ThreadEventsMax)
	}
	if w.ThreadEventRoot("!room", "$event-0") != "" {
		t.Fatal("oldest entry was not dropped")
	}
	if w.ThreadEventRoot("!room", "$event-1") != "$root" || w.ThreadEventRoot("!room", fmt.Sprintf("$event-%d", ThreadEventsMax)) != "$root" {
		t.Fatal("entry 2001 dropped more than the oldest entry")
	}
}

// Claim: appends write only the new entries (three lines for three learned
// entries over a full window), and replaying the same append writes nothing.
// The valid control is the first append's byte count matching its lines.
func TestThreadEventsAppendOnlyNewBytes(t *testing.T) {
	p := testPaths(t)
	var batch []ThreadEvent
	for i := 0; i < ThreadEventsMax; i++ {
		batch = append(batch, threadEntry("!room", i))
	}
	if _, err := AppendThreadEvents(p, "a", batch); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p.ThreadEventsFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	learned := []ThreadEvent{threadEntry("!room", 5001), threadEntry("!room", 5002), threadEntry("!room", 5003)}
	wantBytes := int64(0)
	for _, e := range learned {
		wantBytes += int64(len(threadLine(t, e)))
	}
	before := ReadStateDecodeStats()
	if n, err := AppendThreadEvents(p, "a", learned); err != nil || n != 3 {
		t.Fatalf("append 3 = %d, %v", n, err)
	}
	after := ReadStateDecodeStats()
	if got := after.ThreadEventWriteBytes - before.ThreadEventWriteBytes; got != wantBytes {
		t.Fatalf("bytes written for 3 entries = %d, want %d (3 lines)", got, wantBytes)
	}
	info2, err := os.Stat(p.ThreadEventsFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	if info2.Size()-info.Size() != wantBytes {
		t.Fatalf("file grew by %d, want %d", info2.Size()-info.Size(), wantBytes)
	}
	before = ReadStateDecodeStats()
	if n, err := AppendThreadEvents(p, "a", learned); err != nil || n != 0 {
		t.Fatalf("replay append = %d, %v", n, err)
	}
	if got := ReadStateDecodeStats().ThreadEventWriteBytes - before.ThreadEventWriteBytes; got != 0 {
		t.Fatalf("replay wrote %d bytes, want 0", got)
	}
}

// Claim: once appends leave more than twice the retained window in complete
// lines, the file is rewritten to exactly the newest ThreadEventsMax
// entries; lookups before and after compaction agree on the retained set.
func TestThreadEventsCompactionBound(t *testing.T) {
	p := testPaths(t)
	appendRange := func(from, count int) {
		t.Helper()
		var batch []ThreadEvent
		for i := from; i < from+count; i++ {
			batch = append(batch, threadEntry("!room", i))
		}
		if _, err := AppendThreadEvents(p, "a", batch); err != nil {
			t.Fatal(err)
		}
	}
	appendRange(0, ThreadEventsMax)
	appendRange(ThreadEventsMax, ThreadEventsMax)
	if got := threadFileLines(t, p, "a"); got != 2*ThreadEventsMax {
		t.Fatalf("lines before bound = %d, want %d (no early compaction)", got, 2*ThreadEventsMax)
	}
	appendRange(2*ThreadEventsMax, 1)
	if got := threadFileLines(t, p, "a"); got != ThreadEventsMax {
		t.Fatalf("lines after compaction = %d, want %d", got, ThreadEventsMax)
	}
	w, err := OpenThreadEvents(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	oldestKept := fmt.Sprintf("$event-%d", ThreadEventsMax+1)
	if w.Len() != ThreadEventsMax || w.ThreadEventRoot("!room", oldestKept) != "$root" || w.ThreadEventRoot("!room", "$event-0") != "" || w.ThreadEventRoot("!room", fmt.Sprintf("$event-%d", 2*ThreadEventsMax)) != "$root" {
		t.Fatalf("compacted window len=%d", w.Len())
	}
}

// Claim: a symlinked or group/world-writable thread-event file is refused on
// open and on append, a torn final line is ignored, and a corrupt complete
// line is an error; a regular 0600 file opens and appends (controls).
func TestThreadEventsRefusals(t *testing.T) {
	p := testPaths(t)
	if _, err := AppendThreadEvents(p, "a", []ThreadEvent{{Room: "!r", EventID: "$1", Root: "$t"}}); err != nil {
		t.Fatal(err)
	}
	path := p.ThreadEventsFile("a")
	appendRawThread(t, path, []byte(`{"room":"!r","event_id":"$2"`))
	if w, err := OpenThreadEvents(p, "a"); err != nil || w.Len() != 1 {
		t.Fatalf("torn tail: len=%v err=%v", w.Len(), err)
	}
	if err := os.WriteFile(path, []byte("garbage\n{\"room\":\"!r\",\"event_id\":\"$1\",\"root\":\"$t\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenThreadEvents(p, "a"); err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("corrupt line accepted: %v", err)
	}
	if err := os.WriteFile(path, threadLine(t, ThreadEvent{Room: "!r", EventID: "$1", Root: "$t"}), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o644, 0o660, 0o622} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenThreadEvents(p, "a"); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("mode %04o accepted on open: %v", mode, err)
		}
		if _, err := AppendThreadEvents(p, "a", []ThreadEvent{{Room: "!r", EventID: "$2", Root: "$t"}}); err == nil {
			t.Fatalf("mode %04o accepted on append", mode)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if w, err := OpenThreadEvents(p, "a"); err != nil || w.Len() != 1 {
		t.Fatalf("0600 control: len=%v err=%v", w.Len(), err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := path + ".target"
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenThreadEvents(p, "a"); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink accepted on open: %v", err)
	}
	if _, err := AppendThreadEvents(p, "a", []ThreadEvent{{Room: "!r", EventID: "$9", Root: "$t"}}); err == nil {
		t.Fatal("append followed a symlink")
	}
	if data, _ := os.ReadFile(target); len(data) != 0 {
		t.Fatal("the symlink target was written")
	}
}

func cachedThread(t *testing.T, p Paths) *ThreadEventWindow {
	t.Helper()
	w, err := OpenThreadEvents(p, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func seedCachedThread(t *testing.T, p Paths, entries ...ThreadEvent) {
	t.Helper()
	if _, err := AppendThreadEvents(p.WithReadCache(nil), "fixture", entries); err != nil {
		t.Fatal(err)
	}
}

// Claim: a same-inode append is served by parsing only the appended bytes,
// beside an unchanged control that parses nothing; both entries are visible.
func TestThreadEventsIncrementalAppend(t *testing.T) {
	p := cachedPaths(t)
	seedCachedThread(t, p, ThreadEvent{Room: "!r", EventID: "$one", Root: "$t"})
	if got := cachedThread(t, p); got.Len() != 1 || got.ThreadEventRoot("!r", "$one") != "$t" {
		t.Fatalf("warm window = %+v", got.Entries())
	}
	before := ReadStateDecodeStats()
	if got := cachedThread(t, p); got.Len() != 1 {
		t.Fatalf("unchanged control = %+v", got.Entries())
	}
	if after := ReadStateDecodeStats(); after.ThreadEventDecodes != before.ThreadEventDecodes || after.ThreadEventParseBytes != before.ThreadEventParseBytes {
		t.Fatal("unchanged control parsed thread-event bytes")
	}
	path := p.ThreadEventsFile("fixture")
	st1, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	seedCachedThread(t, p, ThreadEvent{Room: "!r", EventID: "$two", Root: "$t"})
	st2, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	appended := st2.Size() - st1.Size()
	before = ReadStateDecodeStats()
	got := cachedThread(t, p)
	after := ReadStateDecodeStats()
	if got.Len() != 2 || got.ThreadEventRoot("!r", "$two") != "$t" {
		t.Fatalf("window after append = %+v", got.Entries())
	}
	if after.ThreadEventDecodes != before.ThreadEventDecodes {
		t.Fatal("append took a full thread-event parse")
	}
	if d := after.ThreadEventParseBytes - before.ThreadEventParseBytes; d != appended {
		t.Fatalf("parsed bytes = %d, appended = %d", d, appended)
	}
	if after.ThreadEventIncrementalReads <= before.ThreadEventIncrementalReads {
		t.Fatal("append was not an incremental read")
	}
}

// Claim: a replaced thread-event file (new inode) takes exactly one full
// parse and shows the new content, beside a same-inode append control that
// stays incremental.
func TestThreadEventsIncrementalNewInode(t *testing.T) {
	p := cachedPaths(t)
	seedCachedThread(t, p, ThreadEvent{Room: "!r", EventID: "$one", Root: "$t"})
	cachedThread(t, p)
	seedCachedThread(t, p, ThreadEvent{Room: "!r", EventID: "$two", Root: "$t"})
	before := ReadStateDecodeStats()
	if got := cachedThread(t, p); got.Len() != 2 {
		t.Fatalf("append control = %+v", got.Entries())
	}
	if ReadStateDecodeStats().ThreadEventDecodes != before.ThreadEventDecodes {
		t.Fatal("same-inode append control took a full parse")
	}
	path := p.ThreadEventsFile("fixture")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".replacement"
	if err := os.WriteFile(tmp, append(raw, threadLine(t, ThreadEvent{Room: "!r", EventID: "$three", Root: "$t"})...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	before = ReadStateDecodeStats()
	got := cachedThread(t, p)
	if got.Len() != 3 || got.ThreadEventRoot("!r", "$three") != "$t" {
		t.Fatalf("window after replacement = %+v", got.Entries())
	}
	if d := ReadStateDecodeStats().ThreadEventDecodes - before.ThreadEventDecodes; d != 1 {
		t.Fatalf("new-inode replacement full parses = %d, want 1", d)
	}
}

// Claim: a same-inode truncate-and-rewrite (shorter file, different prefix)
// takes a full parse and shows only the new content; an append after the
// rewrite is incremental again.
func TestThreadEventsIncrementalTruncated(t *testing.T) {
	p := cachedPaths(t)
	seedCachedThread(t, p,
		ThreadEvent{Room: "!r", EventID: "$one", Root: "$t"},
		ThreadEvent{Room: "!r", EventID: "$two", Root: "$t"},
		ThreadEvent{Room: "!r", EventID: "$three", Root: "$t"})
	cachedThread(t, p)
	path := p.ThreadEventsFile("fixture")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, threadLine(t, ThreadEvent{Room: "!r", EventID: "$only", Root: "$kept"}), 0o600); err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, afterInfo) || afterInfo.Size() >= info.Size() {
		t.Fatal("truncate control changed inode or did not shrink")
	}
	before := ReadStateDecodeStats()
	got := cachedThread(t, p)
	if got.Len() != 1 || got.ThreadEventRoot("!r", "$only") != "$kept" || got.ThreadEventRoot("!r", "$one") != "" {
		t.Fatalf("window after truncation = %+v", got.Entries())
	}
	if d := ReadStateDecodeStats().ThreadEventDecodes - before.ThreadEventDecodes; d != 1 {
		t.Fatalf("truncation full parses = %d, want 1", d)
	}
	seedCachedThread(t, p, ThreadEvent{Room: "!r", EventID: "$next", Root: "$kept"})
	before = ReadStateDecodeStats()
	if got := cachedThread(t, p); got.Len() != 2 || got.ThreadEventRoot("!r", "$next") != "$kept" {
		t.Fatalf("post-truncation append = %+v", got.Entries())
	}
	if ReadStateDecodeStats().ThreadEventDecodes != before.ThreadEventDecodes {
		t.Fatal("post-truncation append took a full parse")
	}
}

// Claim: the incremental path keeps an unterminated final line unparsed —
// first an incomplete JSON fragment, then a complete JSON line missing its
// newline — and parses each exactly once when completed, while the earlier
// entry stays visible throughout.
func TestThreadEventsIncrementalTornLine(t *testing.T) {
	p := cachedPaths(t)
	seedCachedThread(t, p, ThreadEvent{Room: "!r", EventID: "$one", Root: "$t"})
	cachedThread(t, p)
	path := p.ThreadEventsFile("fixture")
	full := threadLine(t, ThreadEvent{Room: "!r", EventID: "$two", Root: "$t"})
	half := len(full) / 2
	appendRawThread(t, path, full[:half])
	if got := cachedThread(t, p); got.Len() != 1 {
		t.Fatalf("torn fragment became visible: %+v", got.Entries())
	}
	appendRawThread(t, path, full[half:])
	if got := cachedThread(t, p); got.Len() != 2 || got.ThreadEventRoot("!r", "$two") != "$t" {
		t.Fatalf("completed torn line = %+v", got.Entries())
	}
	line3 := threadLine(t, ThreadEvent{Room: "!r", EventID: "$three", Root: "$t"})
	appendRawThread(t, path, bytes.TrimSuffix(line3, []byte("\n")))
	if got := cachedThread(t, p); got.Len() != 2 {
		t.Fatalf("unterminated complete line became visible early: %+v", got.Entries())
	}
	appendRawThread(t, path, []byte("\n"))
	got := cachedThread(t, p)
	if got.Len() != 3 || got.ThreadEventRoot("!r", "$three") != "$t" {
		t.Fatalf("terminated line = %+v", got.Entries())
	}
}

// writeOldState writes an old-format State file carrying entries in its
// thread_events member, beside next_batch, with no writer seal.
func writeOldState(t *testing.T, p Paths, name string, entries []ThreadEvent) {
	t.Helper()
	data, err := json.MarshalIndent(map[string]any{"thread_events": entries, "next_batch": "s-old"}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := p.StateFile(name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Claim: a reader of an unmigrated old State sees the union of the file and
// the State member with the file winning on conflict, deduplicated across
// both; reading alone creates no thread-event file.
func TestThreadEventsOldStateReaderUnion(t *testing.T) {
	p := testPaths(t)
	legacy := []ThreadEvent{
		{Room: "!r", EventID: "$shared", Root: "$state-root"},
		{Room: "!r", EventID: "$state-only", Root: "$state-root"},
		{Room: "!r", EventID: "$shared", Root: "$state-root"},
	}
	writeOldState(t, p, "a", legacy)
	st, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(st.LegacyThreadEvents()) != len(legacy) {
		t.Fatalf("legacy decoded = %d, want %d", len(st.LegacyThreadEvents()), len(legacy))
	}
	win, err := LoadThreadEventWindow(p, "a", st)
	if err != nil {
		t.Fatal(err)
	}
	if win.ThreadEventRoot("!r", "$state-only") != "$state-root" || win.Len() != 2 {
		t.Fatalf("old-State-only window = %+v", win.Entries())
	}
	if _, err := os.Lstat(p.ThreadEventsFile("a")); !os.IsNotExist(err) {
		t.Fatal("a pure reader created the thread-event file")
	}
	if _, err := AppendThreadEvents(p, "a", []ThreadEvent{
		{Room: "!r", EventID: "$shared", Root: "$file-root"},
		{Room: "!r", EventID: "$file-only", Root: "$file-root"},
	}); err != nil {
		t.Fatal(err)
	}
	win, err = LoadThreadEventWindow(p, "a", st)
	if err != nil {
		t.Fatal(err)
	}
	if win.Len() != 3 || win.ThreadEventRoot("!r", "$shared") != "$file-root" || win.ThreadEventRoot("!r", "$state-only") != "$state-root" || win.ThreadEventRoot("!r", "$file-only") != "$file-root" {
		t.Fatalf("union window = %+v", win.Entries())
	}
}

// Claim: the first writer that loads an old State migrates its window to the
// file (deduplicated, capped) and drops the member in the same update; a
// second writer does not rewrite the file, and deleting the file afterwards
// is not undone by later updates (the member is gone).
func TestThreadEventsMigrationOnce(t *testing.T) {
	p := testPaths(t)
	legacy := []ThreadEvent{
		{Room: "!r", EventID: "$a", Root: "$root"},
		{Room: "!r", EventID: "$a", Root: "$root"},
		{Room: "!r", EventID: "$b", Root: "$root"},
		{Room: "", EventID: "$bad", Root: "$root"},
	}
	writeOldState(t, p, "a", legacy)
	if _, err := UpdateState(p, "a", func(s *State) error { s.LastInbox = "x"; return nil }); err != nil {
		t.Fatal(err)
	}
	st, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if st.LastInbox != "x" || len(st.LegacyThreadEvents()) != 0 || bytes.Contains(st.rawState, []byte("thread_events")) {
		t.Fatal("migration did not drop the member in the same update")
	}
	win, err := OpenThreadEvents(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if win.Len() != 2 || win.ThreadEventRoot("!r", "$a") != "$root" || win.ThreadEventRoot("!r", "$b") != "$root" {
		t.Fatalf("migrated window = %+v", win.Entries())
	}
	rawBefore, err := os.ReadFile(p.ThreadEventsFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	if _, err := LoadState(p, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, "a", func(s *State) error { s.LastInbox = "y"; return nil }); err != nil {
		t.Fatal(err)
	}
	rawAfter, err := os.ReadFile(p.ThreadEventsFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawBefore, rawAfter) || ReadStateDecodeStats().ThreadEventWriteBytes != before.ThreadEventWriteBytes {
		t.Fatal("second load/update rewrote the migrated window")
	}
	if err := os.Remove(p.ThreadEventsFile("a")); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, "a", func(s *State) error { s.LastInbox = "z"; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(p.ThreadEventsFile("a")); !os.IsNotExist(err) {
		t.Fatal("migration ran again after the member was dropped")
	}
}

// Claim: when the migration append cannot run (the thread-event path is a
// symlink), the State update refuses and the old file keeps both its cursor
// and its thread_events member; with the obstruction removed the same update
// succeeds and migrates.
func TestThreadEventsMigrationAppendFailureKeepsState(t *testing.T) {
	p := testPaths(t)
	writeOldState(t, p, "a", []ThreadEvent{{Room: "!r", EventID: "$a", Root: "$root"}})
	path := p.ThreadEventsFile("a")
	target := path + ".target"
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "s-new"; return nil }); err == nil {
		t.Fatal("update succeeded despite a refused migration append")
	}
	st, err := LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if st.NextBatch != "s-old" || len(st.LegacyThreadEvents()) != 1 {
		t.Fatalf("failed migration moved the cursor or dropped the member: next=%q legacy=%d", st.NextBatch, len(st.LegacyThreadEvents()))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, "a", func(s *State) error { s.NextBatch = "s-new"; return nil }); err != nil {
		t.Fatal(err)
	}
	st, err = LoadState(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	win, err := OpenThreadEvents(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if st.NextBatch != "s-new" || len(st.LegacyThreadEvents()) != 0 || win.ThreadEventRoot("!r", "$a") != "$root" {
		t.Fatal("repaired update did not migrate and persist")
	}
}

// Claim (counting witness): with a full 2000-entry window, the sealed State
// and its seal-digest input shrink by the window's share once the window
// lives in its file, and learning 3 entries writes only their lines. The
// logged sizes are the witness table; the assertions bound them.
func TestThreadWindowCountingWitness(t *testing.T) {
	p := testPaths(t)
	var entries []ThreadEvent
	for i := 0; i < ThreadEventsMax; i++ {
		entries = append(entries, ThreadEvent{Room: syntheticID("!", "room", i%16), EventID: syntheticID("$", "event", i), Root: syntheticID("$", "root", i%64)})
	}
	writeOldState(t, p, "a", entries)
	oldRaw, err := os.ReadFile(p.StateFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	oldCanon, err := canonicalState(oldRaw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateState(p, "a", func(s *State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	newRaw, err := os.ReadFile(p.StateFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	newCanon, err := canonicalState(newRaw)
	if err != nil {
		t.Fatal(err)
	}
	threadInfo, err := os.Stat(p.ThreadEventsFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("State bytes: old-format=%d current=%d; seal digest input: old=%d current=%d; thread-event file=%d", len(oldRaw), len(newRaw), len(oldCanon), len(newCanon), threadInfo.Size())
	if len(newRaw)*4 > len(oldRaw) || len(newCanon)*4 > len(oldCanon) {
		t.Fatalf("window share not gone from State: %d -> %d bytes, digest input %d -> %d", len(oldRaw), len(newRaw), len(oldCanon), len(newCanon))
	}
	if bytes.Contains(newRaw, []byte("thread_events")) {
		t.Fatal("current State still carries thread_events")
	}
	learned := []ThreadEvent{
		{Room: syntheticID("!", "room", 1), EventID: syntheticID("$", "event", 90001), Root: syntheticID("$", "root", 1)},
		{Room: syntheticID("!", "room", 1), EventID: syntheticID("$", "event", 90002), Root: syntheticID("$", "root", 1)},
		{Room: syntheticID("!", "room", 1), EventID: syntheticID("$", "event", 90003), Root: syntheticID("$", "root", 1)},
	}
	before := ReadStateDecodeStats()
	if _, err := AppendThreadEvents(p, "a", learned); err != nil {
		t.Fatal(err)
	}
	after := ReadStateDecodeStats()
	wrote := after.ThreadEventWriteBytes - before.ThreadEventWriteBytes
	t.Logf("bytes written learning 3 entries: thread file=%d, State=%d (old-format State rewrite would be %d)", wrote, after.StateWriteBytes-before.StateWriteBytes, len(oldRaw))
	if wrote <= 0 || wrote > 1024 || after.StateWriteBytes != before.StateWriteBytes {
		t.Fatalf("learning 3 entries wrote thread=%d state=%d bytes", wrote, after.StateWriteBytes-before.StateWriteBytes)
	}
}

// BenchmarkAppendThreadEventsThree measures one sync-sized durable learn
// step: appending three new entries to a full window (fsync included).
func BenchmarkAppendThreadEventsThree(b *testing.B) {
	p := Paths{Base: b.TempDir()}
	var seed []ThreadEvent
	for i := 0; i < ThreadEventsMax; i++ {
		seed = append(seed, threadEntry("!bench", i))
	}
	if _, err := AppendThreadEvents(p, "fixture", seed); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		base := ThreadEventsMax + i*3
		if _, err := AppendThreadEvents(p, "fixture", []ThreadEvent{threadEntry("!bench", base), threadEntry("!bench", base+1), threadEntry("!bench", base+2)}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkOpenThreadEventsUnchanged measures reopening a full window
// through a consumer read cache with no change (no decode).
func BenchmarkOpenThreadEventsUnchanged(b *testing.B) {
	p := Paths{Base: b.TempDir()}.WithReadCache(NewReadCache("fixture"))
	var seed []ThreadEvent
	for i := 0; i < ThreadEventsMax; i++ {
		seed = append(seed, threadEntry("!bench", i))
	}
	if _, err := AppendThreadEvents(p.WithReadCache(nil), "fixture", seed); err != nil {
		b.Fatal(err)
	}
	if _, err := OpenThreadEvents(p, "fixture"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w, err := OpenThreadEvents(p, "fixture")
		if err != nil || w.Len() != ThreadEventsMax {
			b.Fatal(err)
		}
	}
}
