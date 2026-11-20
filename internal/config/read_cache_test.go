package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func cachedPaths(t *testing.T) Paths {
	t.Helper()
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache("fixture"))
	if err := SaveState(p, "fixture", State{NextBatch: "s0", Threads: []string{"$root"}, Terminal: &TerminalBinding{Generation: "fixture"}}); err != nil {
		t.Fatal(err)
	}
	return p
}

func useRepeatedUnqualifiedDirectoryTime(t *testing.T) {
	t.Helper()
	saved := fileChangeTimeNS
	fileChangeTimeNS = func(os.FileInfo) (int64, bool) { return 17, false }
	t.Cleanup(func() { fileChangeTimeNS = saved })
}

func cachedLoad(t *testing.T, p Paths) State {
	t.Helper()
	s, err := LoadState(p, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func cachedMail(t *testing.T, p Paths) *Mailbox {
	t.Helper()
	m, err := OpenMailbox(p, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	return m
}
func cacheMailSeed(t *testing.T, p Paths) {
	t.Helper()
	m := cachedMail(t, p)
	if _, err := m.Append([]MailEntry{{ID: "$one", Kind: MailEvent, Room: "!fixture", Event: json.RawMessage(`{"type":"m.room.message","content":{"body":"one"}}`)}}); err != nil {
		t.Fatal(err)
	}
}
func cacheReplace(t *testing.T, path string, raw []byte) {
	t.Helper()
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	before, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	tmp := path + ".replacement"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("replacement control did not preserve size and exact mtime with a new inode")
	}
}

// TestReadCacheReplacement proves same-size, exact-mtime State and mailbox renames miss even with identical bytes, beside unchanged-stamp decode-free hits.
func TestReadCacheReplacement(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	cachedLoad(t, p)
	cachedMail(t, p)
	before := ReadStateDecodeStats()
	cachedLoad(t, p)
	cachedMail(t, p)
	after := ReadStateDecodeStats()
	if after.UnmarshalStatePasses != before.UnmarshalStatePasses || after.MailboxDecodes != before.MailboxDecodes {
		t.Fatal("unchanged control decoded")
	}
	for _, path := range []string{p.StateFile("fixture"), p.MailboxFile("fixture")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		cacheReplace(t, path, raw)
	}
	cachedLoad(t, p)
	cachedMail(t, p)
	after = ReadStateDecodeStats()
	if after.UnmarshalStatePasses-before.UnmarshalStatePasses != 1 || after.MailboxDecodes-before.MailboxDecodes != 1 {
		t.Fatal("same-byte new-inode replacements did not miss exactly once")
	}
	for _, tc := range []struct{ path, old, next string }{{p.StateFile("fixture"), "s0", "s1"}, {p.MailboxFile("fixture"), "one", "two"}} {
		raw, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		changed := bytes.ReplaceAll(raw, []byte(tc.old), []byte(tc.next))
		if bytes.Equal(raw, changed) {
			t.Fatal("replacement did not change content")
		}
		cacheReplace(t, tc.path, changed)
	}
	before = ReadStateDecodeStats()
	if cachedLoad(t, p).NextBatch != "s1" {
		t.Fatal("stale State after rename")
	}
	if pending := cachedMail(t, p).Pending(); len(pending) != 1 || !strings.Contains(string(pending[0].Event), "two") {
		t.Fatal("stale mailbox after rename")
	}
	after = ReadStateDecodeStats()
	if after.UnmarshalStatePasses-before.UnmarshalStatePasses != 1 || after.MailboxDecodes-before.MailboxDecodes != 1 {
		t.Fatal("changed stamps did not decode exactly once")
	}
}

func cacheRestamp(t *testing.T, path string, raw []byte) {
	t.Helper()
	before := StampFile(path)
	if !before.Known || before.Size != int64(len(raw)) {
		t.Fatal("same-stamp control requires a known stamp and equal size")
	}
	// Overwrite the incumbent inode, simulating a foreign version with a
	// colliding stamp without depending on filesystem inode reuse timing.
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := f.Write(raw); err != nil || n != len(raw) {
		f.Close()
		t.Fatalf("same-stamp write: n=%d err=%v", n, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(0, before.ModNS)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	if !before.Equal(StampFile(path)) {
		t.Fatal("overwrite did not preserve every stamp field, including inode and exact mtime")
	}
}

// TestReadCacheByteConfirmation proves same-inode, same-size, exact-mtime byte changes miss for State and mailbox, beside decode-free unchanged-byte hits.
func TestReadCacheByteConfirmation(t *testing.T) {
	for _, name := range []string{"state", "mailbox"} {
		t.Run(name, func(t *testing.T) {
			p := cachedPaths(t)
			cacheMailSeed(t, p)
			path, old, next := p.StateFile("fixture"), "s0", "s1"
			read := func() string { return cachedLoad(t, p).NextBatch }
			decodes := func() int64 { return ReadStateDecodeStats().UnmarshalStatePasses }
			if name == "mailbox" {
				path, old, next = p.MailboxFile("fixture"), "one", "two"
				read = func() string {
					var event struct{ Content struct{ Body string } }
					entry := cachedMail(t, p).live["$one"]
					if entry == nil {
						t.Fatal("mailbox entry is missing")
					}
					if err := json.Unmarshal(entry.Event, &event); err != nil {
						t.Fatal(err)
					}
					return event.Content.Body
				}
				decodes = func() int64 { return ReadStateDecodeStats().MailboxDecodes }
			}
			if read() != old {
				t.Fatal("initial content differs")
			}
			before := decodes()
			if read() != old || decodes() != before {
				t.Fatal("unchanged-byte hit control decoded or returned different content")
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			needle, replacement := []byte(old), []byte(next)
			if name == "mailbox" {
				needle, replacement = []byte(`"body":"one"`), []byte(`"body":"two"`)
			}
			changed := bytes.ReplaceAll(raw, needle, replacement)
			if bytes.Equal(raw, changed) {
				t.Fatal("same-stamp replacement did not change bytes")
			}
			cacheRestamp(t, path, changed)
			if read() != next || decodes()-before != 1 {
				t.Fatal("same-stamp byte change did not miss and decode the fresh content exactly once")
			}
			before = decodes()
			if read() != next || decodes() != before {
				t.Fatal("refreshed unchanged-byte hit control decoded or returned stale content")
			}
		})
	}
}

// TestReadCacheMalformedInputDoesNotUseStaleState proves a cached valid value
// cannot mask a same-stamp malformed known field, and that a locked update
// leaves the malformed source untouched without running its callback.
func TestReadCacheMalformedInputDoesNotUseStaleState(t *testing.T) {
	p := cachedPaths(t)
	if got := cachedLoad(t, p); got.NextBatch != "s0" {
		t.Fatal("valid cached neighbor did not load")
	}
	path := p.StateFile("fixture")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := bytes.Replace(raw, []byte(`"s0"`), []byte(`{  }`), 1)
	if bytes.Equal(raw, bad) || len(raw) != len(bad) {
		t.Fatal("malformed same-size fixture was not prepared")
	}
	cacheRestamp(t, path, bad)
	if _, err := LoadState(p, "fixture"); err == nil || !strings.Contains(err.Error(), "not valid state JSON") {
		t.Fatalf("stale cache hid malformed source: %v", err)
	}
	called := false
	if _, err := UpdateState(p, "fixture", func(*State) error { called = true; return nil }); err == nil || called {
		t.Fatalf("malformed source reached update callback: called=%v err=%v", called, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, bad) {
		t.Fatalf("malformed input was changed: err=%v", err)
	}
}

// TestReadCacheMalformedMailboxDoesNotUseStaleData proves exact-byte
// confirmation does not mask a malformed complete mailbox line under a
// colliding stamp, and a restored valid neighbor remains readable.
func TestReadCacheMalformedMailboxDoesNotUseStaleData(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	if got := cachedMail(t, p).Pending(); len(got) != 1 {
		t.Fatal("valid mailbox neighbor did not load")
	}
	path := p.MailboxFile("fixture")
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatalf("read mailbox fixture: %v", err)
	}
	bad := bytes.Clone(raw)
	bad[0] = '!'
	cacheRestamp(t, path, bad)
	if _, err := OpenMailbox(p, "fixture"); err == nil || !strings.Contains(err.Error(), "is not valid JSON") {
		t.Fatalf("stale mailbox hid malformed source: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, bad) {
		t.Fatalf("malformed mailbox was changed: %v", err)
	}
	cacheRestamp(t, path, raw)
	if got, err := OpenMailbox(p, "fixture"); err != nil || len(got.Pending()) != 1 {
		t.Fatalf("valid restored neighbor: entries=%d err=%v", len(got.Pending()), err)
	}
}

// TestReadCacheOversizeMailboxFallsBack proves the raw-retention bound cannot
// reject or truncate a valid larger mailbox, and that it is reparsed rather
// than retained beyond the configured cache limit.
func TestReadCacheOversizeMailboxFallsBack(t *testing.T) {
	p := Paths{Base: t.TempDir()}.WithReadCache(NewReadCache("fixture"))
	if err := os.MkdirAll(filepath.Dir(p.MailboxFile("fixture")), 0o700); err != nil {
		t.Fatal(err)
	}
	entry := MailEntry{ID: "$large", Kind: MailEvent, Room: "!fixture", Event: json.RawMessage(`{"body":"` + strings.Repeat("x", mailboxCacheMaxInput) + `"}`)}
	var data bytes.Buffer
	if err := writeLine(&data, mailLine{Op: "add", Entry: &entry}); err != nil {
		t.Fatal(err)
	}
	if int64(data.Len()) <= mailboxCacheMaxInput {
		t.Fatal("oversize fixture is not above the retention bound")
	}
	if err := os.WriteFile(p.MailboxFile("fixture"), data.Bytes(), SecretFileMode); err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	for i := 0; i < 2; i++ {
		m, err := OpenMailbox(p, "fixture")
		if err != nil || len(m.Pending()) != 1 || len(m.live["$large"].Event) != len(entry.Event) {
			t.Fatalf("oversize mailbox read %d: err=%v", i, err)
		}
	}
	if after := ReadStateDecodeStats(); after.MailboxDecodes-before.MailboxDecodes != 2 || p.readCache.mailbox != nil || p.readCache.mailboxRaw != nil {
		t.Fatalf("oversize input was cached or refused: decodes=%d cache=%p", after.MailboxDecodes-before.MailboxDecodes, p.readCache.mailbox)
	}
}

// TestReadCacheByteConfirmationOnUpdate proves a locked update sees a same-stamp foreign sealed change and both stable and foreign no-ops use the ordinary durability path when namespace proof is unavailable.
func TestReadCacheByteConfirmationOnUpdate(t *testing.T) {
	p := cachedPaths(t)
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	if _, err := UpdateState(p, "fixture", func(s *State) error {
		if s.NextBatch != "s0" {
			t.Fatal("own durable control loaded different content")
		}
		return nil
	}); err != nil || len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("stable own-write positive control: %v, syncs=%+v durable=%v", err, rec, p.readCache.durable)
	}
	rec = syncRecord{}
	foreign := cachedLoad(t, p)
	foreign.NextBatch = "s1"
	raw, err := sealedState(foreign)
	if err != nil {
		t.Fatal(err)
	}
	foreignBytes := append(raw, '\n')
	cacheRestamp(t, p.StateFile("fixture"), foreignBytes)
	got, err := UpdateState(p, "fixture", func(s *State) error {
		if s.NextBatch != "s1" {
			t.Fatal("locked update loaded stale cached State")
		}
		return nil
	})
	finalBytes, readErr := os.ReadFile(p.StateFile("fixture"))
	if err != nil || readErr != nil || got.NextBatch != "s1" || !got.Writer.ForeignSeen.IsZero() || !validWriterSeal(got) || !bytes.Equal(finalBytes, foreignBytes) || len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("same-stamp compatible foreign change: err=%v readErr=%v State=%q bytesPreserved=%v syncs=%+v durable=%v", err, readErr, got.NextBatch, bytes.Equal(finalBytes, foreignBytes), rec, p.readCache.durable)
	}
}

// TestReadCacheUnqualifiedDirectoryTimeNeverAuthorizesDurability proves a repeated but unqualified timestamp cannot enable the zero-sync path.
func TestReadCacheUnqualifiedDirectoryTimeNeverAuthorizesDurability(t *testing.T) {
	useRepeatedUnqualifiedDirectoryTime(t)
	p := cachedPaths(t)
	old := cachedLoad(t, p)
	path := p.StateFile("fixture")
	if p.readCache.durable || p.readCache.unchangedDurable("fixture", path, old, old) {
		t.Fatal("unqualified repeated directory time authorized zero-sync durability")
	}
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("repeated unqualified time did not use ordinary syncs: %+v durable=%v", rec, p.readCache.durable)
	}
	cacheRestamp(t, path, bytes.ReplaceAll(old.rawState, []byte("s0"), []byte("s1")))
	if p.readCache.unchangedDurable("fixture", path, old, old) {
		t.Fatal("unqualified directory time authorized durability after a same-stamp byte change")
	}
}

// TestReadCacheRepeatedDirectoryTimestampsDoNotProveNamespace proves that equal directory timestamps are not treated as a namespace version and cannot skip syncs.
func TestReadCacheRepeatedDirectoryTimestampsDoNotProveNamespace(t *testing.T) {
	useRepeatedUnqualifiedDirectoryTime(t)
	p := cachedPaths(t)
	dir := filepath.Dir(p.StateFile("fixture"))
	info, err := os.Lstat(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, firstKnown := fileChangeTimeNS(info)
	second, secondKnown := fileChangeTimeNS(info)
	if first != second || firstKnown || secondKnown || stampStateDirectory(dir).known {
		t.Fatalf("repeated timestamp was treated as qualified: first=(%d,%v) second=(%d,%v) stamp=%+v", first, firstKnown, second, secondKnown, stampStateDirectory(dir))
	}
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("repeated directory timestamp skipped ordinary durability: %+v durable=%v", rec, p.readCache.durable)
	}
}

// TestReadCacheStableOwnWriteUsesOrdinarySync proves an unchanged own-written State succeeds while retaining inode and bytes through the verified-file and directory sync path.
func TestReadCacheStableOwnWriteUsesOrdinarySync(t *testing.T) {
	p := cachedPaths(t)
	if p.readCache.durable {
		t.Fatal("unqualified platform timestamp minted own-durable evidence")
	}
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("stable own-write positive did not use ordinary durability: %+v durable=%v", rec, p.readCache.durable)
	}
}

// TestReadCacheNamespaceABADurability proves same-inode, same-byte rename-away
// and rename-back cannot reuse durability for the restored directory entry,
// with and without observing the missing filename; the ordinary path syncs
// the verified file and both directories before refreshing its proof.
func TestReadCacheNamespaceABADurability(t *testing.T) {
	for _, observeMissing := range []bool{false, true} {
		name := "without missing-file read"
		if observeMissing {
			name = "with missing-file read"
		}
		t.Run(name, func(t *testing.T) {
			useRepeatedUnqualifiedDirectoryTime(t)
			p := cachedPaths(t)
			if p.readCache.durable {
				t.Fatal("unqualified repeated directory time minted own-durable evidence")
			}
			var rec syncRecord
			swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))

			path := p.StateFile("fixture")
			before := StampFile(path)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			moved := path + ".moved"
			if err := os.Rename(path, moved); err != nil {
				t.Fatal(err)
			}
			if err := syncStateDirectory(filepath.Dir(path)); err != nil {
				t.Fatal(err)
			}
			if observeMissing {
				missing, err := LoadState(p, "fixture")
				if err != nil || missing.NextBatch != "" {
					t.Fatalf("missing-file read = State(%q), %v", missing.NextBatch, err)
				}
				if p.readCache.durable {
					t.Fatal("observed missing path retained own-write durability")
				}
			}
			if err := os.Rename(moved, path); err != nil {
				t.Fatal(err)
			}
			after := StampFile(path)
			restored, err := os.ReadFile(path)
			if err != nil || !before.Equal(after) || !bytes.Equal(original, restored) {
				t.Fatalf("ABA control did not restore identical file identity and bytes: err=%v sameStamp=%v sameBytes=%v", err, before.Equal(after), bytes.Equal(original, restored))
			}

			rec = syncRecord{}
			if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if len(rec.files) != 1 || len(rec.dirs) != 2 {
				t.Fatalf("restored namespace no-op must sync the verified file and both directories: %+v", rec)
			}
			if p.readCache.durable {
				t.Fatal("ordinary sync minted unqualified directory evidence")
			}

			rec = syncRecord{}
			if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if len(rec.files) != 1 || len(rec.dirs) != 2 {
				t.Fatalf("later no-op skipped ordinary durability after an ABA: %+v", rec)
			}
		})
	}
}

// TestReadCacheDurableByteConfirmation proves the own-durable shortcut rejects changed disk bytes even when the stamp and typed arguments still match, beside an unchanged-byte control.
func TestReadCacheDurableByteConfirmation(t *testing.T) {
	p := cachedPaths(t)
	old := cachedLoad(t, p)
	path := p.StateFile("fixture")
	if !p.readCache.unchangedDurable("fixture", path, old, old) {
		t.Fatal("own durable unchanged-byte control missed")
	}
	cacheRestamp(t, path, bytes.ReplaceAll(old.rawState, []byte("s0"), []byte("s1")))
	if p.readCache.unchangedDurable("fixture", path, old, old) {
		t.Fatal("own durable shortcut trusted a stamp collision without byte confirmation")
	}
}

// TestReadCacheByteReadFailure proves candidate hits fail closed when the byte read is denied after the stamp pre-check, beside readable controls before and after.
func TestReadCacheByteReadFailure(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	cachedLoad(t, p)
	cachedMail(t, p)
	for _, mail := range []bool{false, true} {
		path := p.StateFile("fixture")
		if mail {
			path = p.MailboxFile("fixture")
		}
		stamp := StampFile(path)
		hit := func() bool {
			if mail {
				_, ok := p.readCache.mailboxHit("fixture", path, stamp)
				return ok
			}
			_, ok := p.readCache.stateHit("fixture", path, stamp)
			return ok
		}
		if !hit() {
			t.Fatal("readable candidate control missed")
		}
		if err := os.Chmod(path, 0000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.Chmod(path, 0600) })
		if _, err := os.ReadFile(path); !errors.Is(err, os.ErrPermission) {
			t.Fatal("permission control did not deny the byte read", err)
		}
		if hit() {
			t.Fatal("unreadable candidate returned cached success")
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if !hit() {
			t.Fatal("restored readable control missed")
		}
	}
}

// TestReadCacheClone proves mutated State and mailbox results cannot alter later hits, while real writes remain visible.
func TestReadCacheClone(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	original := cachedLoad(t, p)
	want, _ := cloneState(&original)
	first := cachedLoad(t, p)
	before := ReadStateDecodeStats()
	first.Threads[0] = "$mutated"
	first.Terminal.Generation = "mutated"
	first.rawState[0] = '!'
	if got := cachedLoad(t, p); !reflect.DeepEqual(got, want) || ReadStateDecodeStats().UnmarshalStatePasses != before.UnmarshalStatePasses {
		t.Fatal("State cache shares mutable data or decoded after caller mutation")
	}
	seed := cachedMail(t, p)
	if _, err := seed.Append([]MailEntry{{ID: "$two", Kind: MailEvent, Room: "!fixture", Event: json.RawMessage(`{"type":"m.room.message","content":{"body":"two"}}`)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Receive([]string{"$one"}); err != nil {
		t.Fatal(err)
	}
	wantMail := cachedMail(t, p) // Includes receipt order plus one pending entry.
	mail := cachedMail(t, p)
	mail.live["$two"].Event[0] = '!'
	mail.live["$two"].Shown = true
	mail.recv["$other"] = true
	mail.redacted[Redaction{"!fixture", "$one"}] = true
	mail.order[1] = "$other"
	mail.received[0] = "$other"
	mail.dead++
	got := cachedMail(t, p)
	if !reflect.DeepEqual(got, wantMail) || len(got.Pending()) != 1 || got.live["$two"].Shown || !json.Valid(got.live["$two"].Event) || got.recv["$other"] || len(got.redacted) != 0 || got.received[0] != "$one" {
		t.Fatal("mailbox cache shares mutable data")
	}
	if _, err := UpdateState(p, "fixture", func(s *State) error { s.Threads[0] = "$written"; return nil }); err != nil {
		t.Fatal(err)
	}
	if cachedLoad(t, p).Threads[0] != "$written" {
		t.Fatal("real State write hidden")
	}
	if _, err := got.Receive([]string{"$two"}); err != nil {
		t.Fatal(err)
	}
	if len(cachedMail(t, p).Pending()) != 0 {
		t.Fatal("real mailbox append hidden")
	}
}

// TestReadCacheStampUncertainty proves changed/unknown/stat-error stamps miss beside a known-stamp hit, and directory permission failures are refused by the public read guards.
func TestReadCacheStampUncertainty(t *testing.T) {
	p := cachedPaths(t)
	cacheMailSeed(t, p)
	cachedLoad(t, p)
	cachedMail(t, p)
	c := p.readCache
	for _, mail := range []bool{false, true} {
		path := p.StateFile("fixture")
		if mail {
			path = p.MailboxFile("fixture")
		}
		stamp := StampFile(path)
		hit := func(s FileStamp) bool {
			if mail {
				_, ok := c.mailboxHit("fixture", path, s)
				return ok
			}
			_, ok := c.stateHit("fixture", path, s)
			return ok
		}
		if !hit(stamp) {
			t.Fatal("known-stamp control missed")
		}
		variants := []FileStamp{stamp, stamp, stamp, stamp, stamp, {}, stampFile(path, func(string) (os.FileInfo, error) { return nil, os.ErrPermission })}
		variants[0].Dev++
		variants[1].Ino++
		variants[2].Size++
		variants[3].ModNS++
		variants[4].Mode = 0644
		for i, s := range variants {
			if hit(s) {
				t.Fatalf("stamp uncertainty %d returned stale data", i)
			}
		}
	}
	dir := filepath.Join(p.Base, "state")
	if err := os.Chmod(dir, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	if _, err := LoadState(p, "fixture"); err == nil {
		t.Error("State stat error returned stale success")
	}
	if _, err := OpenMailbox(p, "fixture"); err == nil {
		t.Error("mailbox stat error returned stale success")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	cachedLoad(t, p)
	cachedMail(t, p)
}

// TestReadCacheWriteRefresh proves a successful changed write refreshes decoded State without a following decode, and a failed callback changes neither disk nor cache.
func TestReadCacheWriteRefresh(t *testing.T) {
	p := cachedPaths(t)
	if _, err := UpdateState(p, "fixture", func(s *State) error { s.NextBatch = "s1"; return nil }); err != nil {
		t.Fatal(err)
	}
	before := ReadStateDecodeStats()
	if cachedLoad(t, p).NextBatch != "s1" {
		t.Fatal("write did not refresh")
	}
	if ReadStateDecodeStats().UnmarshalStatePasses != before.UnmarshalStatePasses {
		t.Fatal("post-write read decoded")
	}
	sentinel := errors.New("rollback")
	if _, err := UpdateState(p, "fixture", func(s *State) error { s.NextBatch = "s2"; return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if cachedLoad(t, p).NextBatch != "s1" {
		t.Fatal("failed write poisoned cache")
	}
	plain := p.WithReadCache(nil)
	if cachedLoad(t, plain).NextBatch != "s1" {
		t.Fatal("failed callback changed disk")
	}
}

// TestReadCacheDurableNoop proves a stable own-write no-op preserves inode, mtime and bytes after one comparison encode while syncing the verified file and directories, beside one changed atomic replacement.
func TestReadCacheDurableNoop(t *testing.T) {
	p := cachedPaths(t)
	path := p.StateFile("fixture")
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	first, err := held.Stat()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	before := ReadStateDecodeStats()
	if _, err := UpdateState(p, "fixture", func(s *State) error { s.Terminal.Generation = "fixture"; return nil }); err != nil {
		t.Fatal(err)
	}
	after := ReadStateDecodeStats()
	same, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sameFile := os.SameFile(first, same)
	sameMtime := first.ModTime().Equal(same.ModTime())
	sameBytes := bytes.Equal(raw, data)
	encodeDelta := after.StateEncodes - before.StateEncodes
	writeByteDelta := after.StateWriteBytes - before.StateWriteBytes
	if !sameFile || !sameMtime || !sameBytes || encodeDelta != 1 || writeByteDelta != 0 || len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("stable no-op failed ordinary-sync contract: sameFile=%v sameMtime=%v sameBytes=%v encodeDelta=%d writeByteDelta=%d syncs=%+v durable=%v", sameFile, sameMtime, sameBytes, encodeDelta, writeByteDelta, rec, p.readCache.durable)
	}
	if _, err := UpdateState(p, "fixture", func(s *State) error { s.Terminal.Generation = "changed"; return nil }); err != nil {
		t.Fatal(err)
	}
	changed, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	after = ReadStateDecodeStats()
	if os.SameFile(first, changed) || after.SealedStates-before.SealedStates != 1 || after.StateWriteBytes-before.StateWriteBytes != changed.Size() || cachedLoad(t, p).Terminal.Generation != "changed" {
		t.Fatal("one-field change did not replace State exactly once")
	}
	if matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*.tmp")); err != nil || len(matches) != 0 {
		t.Fatal("completed write left temporary files")
	}
}

// TestReadCacheForeignDurability proves both cold compatible and own-written State no-ops use the ordinary path while directory provenance is unqualified.
func TestReadCacheForeignDurability(t *testing.T) {
	p := cachedPaths(t).WithReadCache(NewReadCache("fixture"))
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	cachedLoad(t, p)
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 {
		t.Fatal("foreign compatible seal was treated as proof of a completed fsync")
	}
	if _, err := UpdateState(p, "fixture", func(s *State) error { s.LastInbox = "written"; return nil }); err != nil {
		t.Fatal(err)
	}
	rec = syncRecord{}
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 || p.readCache.durable {
		t.Fatalf("own-write control did not use ordinary syncs: %+v durable=%v", rec, p.readCache.durable)
	}
}

// TestReadCacheWrittenInode proves an unknown directory timestamp cannot mint own-write durability even beside the matching file inode, and replacement still uses ordinary syncs.
func TestReadCacheWrittenInode(t *testing.T) {
	p := cachedPaths(t)
	path := p.StateFile("fixture")
	s := cachedLoad(t, p)
	evidence := stateWriteEvidence{file: StampFile(path), directory: stampStateDirectory(filepath.Dir(path))}
	p.readCache.rememberWrittenState(p, "fixture", s, s.rawState, evidence)
	if p.readCache.durable {
		t.Fatal("unqualified directory evidence minted own-write durability")
	}
	cacheReplace(t, path, s.rawState)
	p.readCache.rememberWrittenState(p, "fixture", s, s.rawState, evidence)
	if p.readCache.durable {
		t.Fatal("replacement inherited a different inode's durability evidence")
	}
	var rec syncRecord
	swapOrdinaryStateSync(t, recordingSyncer(&rec, nil, -1, nil))
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(rec.files) != 1 || len(rec.dirs) != 2 {
		t.Fatal("replacement skipped the ordinary durability check")
	}
}

// TestReadCacheStrictRefresh proves ordinary and strict Host writes refresh decoded data without minting unqualified directory durability.
func TestReadCacheStrictRefresh(t *testing.T) {
	for _, version := range []int{1, 2} {
		p, name := hostElisionNeighbour(t, version)
		p = p.WithReadCache(NewReadCache(name))
		if _, err := UpdateState(p, name, func(s *State) error { s.LastInbox = "written"; return nil }); err != nil {
			t.Fatal(err)
		}
		before := ReadStateDecodeStats()
		s, err := LoadState(p, name)
		if err != nil || s.LastInbox != "written" || ReadStateDecodeStats().UnmarshalStatePasses != before.UnmarshalStatePasses {
			t.Fatalf("version %d post-write cache: %v", version, err)
		}
		if p.readCache.durable {
			t.Fatalf("version %d used the wrong durability provenance", version)
		}
	}
}

// TestReadCacheMonitorRefusal proves a cached foreign State still refuses monitor admission, beside successful admission of compatible cached State.
func TestReadCacheMonitorRefusal(t *testing.T) {
	p := monitorSeed(t).WithReadCache(NewReadCache("a"))
	monitorLoad(t, p)
	monitorLoad(t, p)
	called := false
	if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { called = true; return nil }); err != nil || !called {
		t.Fatalf("compatible cached control: %v, called=%v", err, called)
	}
	monitorOldWrite(t, p, func(m map[string]json.RawMessage) { m["next_batch"] = json.RawMessage(`"foreign"`) })
	monitorLoad(t, p)
	monitorLoad(t, p)
	called = false
	if err := monitorRun(p, MonitorAutomatic, func(*MonitorGeneration) error { called = true; return nil }); !errors.Is(err, ErrIncompatibleWriter) || called {
		t.Fatalf("foreign cached admission: %v, called=%v", err, called)
	}
}

// TestReadCacheFenceOnHit proves cached reads do not skip per-transaction seal verification or fencing of foreign State, beside a valid unfenced control.
func TestReadCacheFenceOnHit(t *testing.T) {
	p := cachedPaths(t)
	cachedLoad(t, p)
	before := ReadStateDecodeStats()
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if ReadStateDecodeStats().WriterSealChecks-before.WriterSealChecks != 1 || !cachedLoad(t, p).Writer.ForeignSeen.IsZero() {
		t.Fatal("valid cached control skipped verification or was fenced")
	}
	raw, err := os.ReadFile(p.StateFile("fixture"))
	if err != nil {
		t.Fatal(err)
	}
	cacheReplace(t, p.StateFile("fixture"), bytes.ReplaceAll(raw, []byte("s0"), []byte("s1")))
	cachedLoad(t, p) // Prime the foreign input itself, not only the earlier State.
	before = ReadStateDecodeStats()
	if _, err := UpdateState(p, "fixture", func(*State) error { return nil }); err != nil {
		t.Fatal(err)
	}
	got := cachedLoad(t, p)
	if ReadStateDecodeStats().WriterSealChecks-before.WriterSealChecks != 1 || got.Writer.ForeignSeen.IsZero() || !validWriterSeal(got) || got.NextBatch != "s1" {
		t.Fatal("foreign cached input escaped the writer fence")
	}
	if time.Since(got.Writer.ForeignSeen) > time.Minute {
		t.Fatal("fence did not record this transaction")
	}
}
