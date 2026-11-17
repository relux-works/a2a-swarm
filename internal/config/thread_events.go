package config

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// The thread-event window (state/<name>.thread_events.jsonl, mode 0600) holds
// bounded room-scoped thread membership learned from server relations: which
// thread root an observed event belongs to, per room. It grants no follow
// status and is independent of mute/follow changes. The file is an
// append-only log of one ThreadEvent JSON object per line. A sync appends the
// batch's newly learned entries (fsynced) before the State update that
// advances the cursor, so a crash between the two leaves extra entries that a
// replayed sync re-learns idempotently, never a cursor past entries that
// were not stored. Reads deduplicate by (room, event id) with the newest
// occurrence winning and keep only the newest ThreadEventsMax entries.
// Appends deduplicate against the stored window, so the first stored
// observation of an event keeps its root. When the file holds more than
// twice ThreadEventsMax complete lines, an append rewrites it to the newest
// ThreadEventsMax entries (temp file + rename, like mailbox compaction).
//
// Thread-event mutations run under the state lock, like mailbox mutations.
// Read-only snapshots may open the file without that lock; compaction uses
// atomic rename.

// ThreadEventsFile is the thread-event window file of name.
func (p Paths) ThreadEventsFile(name string) string {
	return filepath.Join(p.Base, "state", name+".thread_events.jsonl")
}

// ThreadEventsMax bounds the retained window; the oldest entries are dropped.
const ThreadEventsMax = 2000

// threadEventsCompactLines is the file-size bound that triggers compaction:
// more than twice the retained window in complete lines.
const threadEventsCompactLines = 2 * ThreadEventsMax

// ThreadEvent identifies an observed event's thread in one room.
type ThreadEvent struct {
	Room    string `json:"room"`
	EventID string `json:"event_id"`
	Root    string `json:"root"`
}

func validThreadEvent(e ThreadEvent) bool {
	return e.Room != "" && e.EventID != "" && e.Root != ""
}

type threadEventKey struct{ room, id string }

// ThreadEventWindow is one loaded thread-event window: deduplicated entries
// in learning order (oldest first), at most ThreadEventsMax of them. The
// zero value and a nil window both answer every lookup with "unknown".
type ThreadEventWindow struct {
	path          string
	entries       []ThreadEvent
	index         map[threadEventKey]int // key -> position in entries
	lines         int                    // complete lines in the file this window was parsed from
	completeBytes int64                  // bytes of complete lines in that file
	tailPresent   bool                   // the file ends with an unterminated line
	tailValid     bool                   // that line is a valid entry, already applied
}

// NewThreadEventWindow returns an in-memory window holding entries, learned
// in order with RememberThreadEvent semantics. It is not connected to a file.
func NewThreadEventWindow(entries ...ThreadEvent) *ThreadEventWindow {
	w := &ThreadEventWindow{index: map[threadEventKey]int{}}
	for _, e := range entries {
		w.RememberThreadEvent(e)
	}
	return w
}

// ThreadEventRoot never infers membership from the unscoped follow/mute lists.
func (w *ThreadEventWindow) ThreadEventRoot(room, id string) string {
	if w == nil {
		return ""
	}
	if pos, ok := w.index[threadEventKey{room, id}]; ok {
		return w.entries[pos].Root
	}
	return ""
}

// HasRoot reports whether the window holds any entry of room whose root is
// root (mute topic resolution for roots the server no longer shows).
func (w *ThreadEventWindow) HasRoot(room, root string) bool {
	if w == nil {
		return false
	}
	for _, e := range w.entries {
		if e.Room == room && e.Root == root {
			return true
		}
	}
	return false
}

// Entries returns a copy of the window, oldest first.
func (w *ThreadEventWindow) Entries() []ThreadEvent {
	if w == nil {
		return nil
	}
	return cloneValues(w.entries)
}

// Len is the number of retained entries.
func (w *ThreadEventWindow) Len() int {
	if w == nil {
		return 0
	}
	return len(w.entries)
}

// RememberThreadEvent retains server-derived membership in memory without
// following it and without writing any file: the first observation of a
// (room, event id) pair keeps its root, and beyond ThreadEventsMax the
// oldest entries are dropped. Durable learning goes through
// AppendThreadEvents.
func (w *ThreadEventWindow) RememberThreadEvent(e ThreadEvent) {
	if w == nil || !validThreadEvent(e) {
		return
	}
	if w.index == nil {
		w.index = map[threadEventKey]int{}
	}
	key := threadEventKey{e.Room, e.EventID}
	if _, ok := w.index[key]; ok {
		return
	}
	w.index[key] = len(w.entries)
	w.entries = append(w.entries, e)
	w.trim()
}

// applyNewest records e with newest-wins semantics for file-order replay: a
// repeated (room, event id) replaces the earlier occurrence and takes the
// newest position.
func (w *ThreadEventWindow) applyNewest(e ThreadEvent) {
	if !validThreadEvent(e) {
		return
	}
	if w.index == nil {
		w.index = map[threadEventKey]int{}
	}
	key := threadEventKey{e.Room, e.EventID}
	if pos, ok := w.index[key]; ok {
		w.entries = append(w.entries[:pos], w.entries[pos+1:]...)
		delete(w.index, key)
		for i := pos; i < len(w.entries); i++ {
			w.index[threadEventKey{w.entries[i].Room, w.entries[i].EventID}] = i
		}
	}
	w.index[key] = len(w.entries)
	w.entries = append(w.entries, e)
	w.trim()
}

func (w *ThreadEventWindow) trim() {
	if len(w.entries) <= ThreadEventsMax {
		return
	}
	drop := len(w.entries) - ThreadEventsMax
	w.entries = append(w.entries[:0:0], w.entries[drop:]...)
	w.index = make(map[threadEventKey]int, len(w.entries))
	for i, e := range w.entries {
		w.index[threadEventKey{e.Room, e.EventID}] = i
	}
}

func (w *ThreadEventWindow) has(e ThreadEvent) bool {
	_, ok := w.index[threadEventKey{e.Room, e.EventID}]
	return ok
}

func cloneThreadWindow(src *ThreadEventWindow) *ThreadEventWindow {
	w := *src
	w.entries = cloneValues(src.entries)
	w.index = make(map[threadEventKey]int, len(src.index))
	for k, v := range src.index {
		w.index[k] = v
	}
	return &w
}

// threadFileParse is one full parse of a thread-event file, including the
// digests and offsets an incremental snapshot needs.
type threadFileParse struct {
	window        *ThreadEventWindow // complete lines plus a valid unterminated tail
	base          *ThreadEventWindow // complete lines only
	completeBytes int64
	totalBytes    int64
	prefixSum     [32]byte
	fileSum       [32]byte
}

// parseThreadEventsFile parses path (already protection-checked by the
// caller). Only newline-terminated lines are durable records: an
// unterminated final line that parses as a valid entry is applied to the
// returned window (matching a full mailbox parse), an unparsable torn tail
// is ignored, and a corrupt complete line is an error. retain asks for the
// exact parsed bytes to be kept when the stamped size admits them.
func parseThreadEventsFile(path string, retain bool, stampedSize int64) (*threadFileParse, []byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("thread events: %w", err)
	}
	defer f.Close()
	globalStateStats.threadEventDecodes.Add(1)
	r := bufio.NewReader(f)
	var raw []byte
	retainRaw := retain && stampedSize <= mailboxCacheMaxInput
	if retainRaw {
		raw = make([]byte, 0, int(stampedSize))
	}
	prefixHash := sha256.New()
	totalHash := sha256.New()
	base := &ThreadEventWindow{path: path, index: map[threadEventKey]int{}}
	window := base
	var completeBytes, totalRead int64
	for n := 1; ; n++ {
		line, rerr := r.ReadBytes('\n')
		if retainRaw {
			if len(raw)+len(line) > mailboxCacheMaxInput {
				raw, retainRaw = nil, false
			} else {
				raw = append(raw, line...)
			}
		}
		totalHash.Write(line)
		totalRead += int64(len(line))
		if len(line) > 0 {
			complete := line[len(line)-1] == '\n'
			globalStateStats.threadEventParseBytes.Add(int64(len(line)))
			var e ThreadEvent
			if jerr := json.Unmarshal(bytes.TrimSpace(line), &e); jerr != nil {
				if !complete {
					base.tailPresent = true
					break // a torn final line of a crashed append: never durable
				}
				return nil, nil, fmt.Errorf("thread events %s: line %d is not valid JSON", path, n)
			}
			if complete {
				base.applyNewest(e)
				base.lines++
				prefixHash.Write(line)
				completeBytes += int64(len(line))
				base.completeBytes = completeBytes
			} else {
				base.tailPresent = true
				if validThreadEvent(e) {
					window = cloneThreadWindow(base)
					window.applyNewest(e)
					window.tailPresent = true
					window.tailValid = true
					base.tailValid = true
				}
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, nil, fmt.Errorf("thread events: %w", rerr)
		}
	}
	out := &threadFileParse{window: window, base: base, completeBytes: completeBytes, totalBytes: totalRead}
	copy(out.prefixSum[:], prefixHash.Sum(nil))
	copy(out.fileSum[:], totalHash.Sum(nil))
	return out, raw, nil
}

// OpenThreadEvents loads state/<name>.thread_events.jsonl. A missing file is
// an empty window; anything but a regular 0600 file (a symlink, say) is
// refused, like the mailbox. With a consumer read cache attached, an
// unchanged file costs no decode and a same-inode append parses only the
// appended complete lines.
func OpenThreadEvents(p Paths, name string) (*ThreadEventWindow, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	path := p.ThreadEventsFile(name)
	empty := &ThreadEventWindow{path: path, index: map[threadEventKey]int{}}
	if err := checkSecretFile(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return empty, nil
		}
		return nil, fmt.Errorf("thread events: %w", err)
	}
	var stamp FileStamp
	if p.readCache != nil {
		stamp = StampFile(path)
	}
	if cached, ok := p.readCache.threadHit(name, path, stamp); ok {
		return cached, nil
	}
	if cached, ok := p.readCache.openThreadEventsIncremental(name, path, stamp); ok {
		return cached, nil
	}
	var opened FileStamp
	if info, serr := os.Lstat(path); serr == nil {
		opened = stampInfo(info)
	}
	parsed, raw, err := parseThreadEventsFile(path, stamp.Known, stamp.Size)
	if err != nil {
		return nil, err
	}
	p.readCache.rememberThread(name, path, stamp, raw, parsed.window)
	if p.readCache != nil && stamp.Known && stamp.Present && opened.Known && opened.Dev == stamp.Dev && opened.Ino == stamp.Ino && stamp.Size == parsed.totalBytes && stamp.Equal(StampFile(path)) {
		inc := &threadIncState{path: path, dev: stamp.Dev, ino: stamp.Ino, offset: parsed.completeBytes, prefixSum: parsed.prefixSum, fileSize: parsed.totalBytes, fileSum: parsed.fileSum, base: cloneThreadWindow(parsed.base)}
		if parsed.window != parsed.base {
			inc.visible = cloneThreadWindow(parsed.window)
		}
		p.readCache.rememberThreadInc(name, inc)
	}
	return parsed.window, nil
}

// LoadThreadEventWindow is the reader's view of name's window: the
// thread-event file unioned with any entries an unmigrated State still
// carries, the file winning on conflict. State entries are older, so they
// are applied first. Until the first writer migrates them, lookups answer
// exactly as the in-State window did.
func LoadThreadEventWindow(p Paths, name string, st State) (*ThreadEventWindow, error) {
	w, err := OpenThreadEvents(p, name)
	if err != nil {
		return nil, err
	}
	if len(st.legacyThreadEvents) == 0 {
		return w, nil
	}
	union := &ThreadEventWindow{path: w.path, index: map[threadEventKey]int{}, lines: w.lines, completeBytes: w.completeBytes}
	for _, e := range st.legacyThreadEvents {
		union.applyNewest(e)
	}
	for _, e := range w.entries {
		union.applyNewest(e)
	}
	return union, nil
}

// openThreadEventsIncremental serves a consumer's next read from its
// incremental snapshot when the file is still the same (device, inode), did
// not shrink below the parsed offset, and its already-parsed prefix is
// byte-identical. Only complete lines past the offset are parsed; an
// unterminated final line is kept unparsed and retried on a later read. Any
// other outcome reports a miss so the ordinary full parse runs. It mirrors
// the mailbox incremental read.
func (c *ReadCache) openThreadEventsIncremental(name, path string, stamp FileStamp) (*ThreadEventWindow, bool) {
	inc := c.threadIncSnapshot(name, path)
	if inc == nil || !stamp.Known || !stamp.Present || stamp.Dev != inc.dev || stamp.Ino != inc.ino || stamp.Size < inc.offset {
		return nil, false
	}
	f, err := os.OpenFile(path, os.O_RDONLY|openNoFollow, 0)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false
	}
	opened := stampInfo(info)
	if !opened.Known || opened.Dev != inc.dev || opened.Ino != inc.ino || opened.Size < inc.offset {
		return nil, false
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, inc.offset); err != nil {
		return nil, false
	}
	var prefixSum [32]byte
	copy(prefixSum[:], h.Sum(nil))
	if prefixSum != inc.prefixSum {
		return nil, false
	}
	w := cloneThreadWindow(inc.base)
	w.path = path
	retain := opened.Size <= mailboxCacheMaxInput
	var appended []byte
	newOffset := inc.offset
	prefixSumNew := inc.prefixSum
	var tailLen, parsedBytes int64
	r := bufio.NewReader(f)
	for {
		line, rerr := r.ReadBytes('\n')
		if len(line) > 0 {
			if retain {
				appended = append(appended, line...)
			}
			h.Write(line)
			if line[len(line)-1] == '\n' {
				parsedBytes += int64(len(line))
				var e ThreadEvent
				if json.Unmarshal(bytes.TrimSpace(line), &e) != nil {
					return nil, false // the full parse reproduces the exact error
				}
				w.applyNewest(e)
				w.lines++
				newOffset += int64(len(line))
				copy(prefixSumNew[:], h.Sum(nil))
			} else {
				tailLen = int64(len(line)) // torn: kept unparsed, retried later
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return nil, false
		}
	}
	w.completeBytes = newOffset
	w.tailPresent = tailLen > 0
	w.tailValid = false
	var fileSum [32]byte
	copy(fileSum[:], h.Sum(nil))
	total := newOffset + tailLen
	globalStateStats.threadEventParseBytes.Add(parsedBytes)
	globalStateStats.threadEventIncrementalReads.Add(1)
	result := w
	var newVisible *ThreadEventWindow
	if total == inc.fileSize && fileSum == inc.fileSum {
		newVisible = inc.visible
		if inc.visible != nil {
			result = cloneThreadWindow(inc.visible)
			result.path = path
		}
	}
	if after := StampFile(path); after.Known && after.Present && after.Dev == inc.dev && after.Ino == inc.ino && after.Size == total {
		c.rememberThreadInc(name, &threadIncState{path: path, dev: inc.dev, ino: inc.ino, offset: newOffset, prefixSum: prefixSumNew, fileSize: total, fileSum: fileSum, base: cloneThreadWindow(w), visible: newVisible})
		if retain {
			c.updateThreadRaw(name, path, after, inc, appended, result, total)
		} else {
			c.updateThreadRaw(name, path, after, inc, nil, result, total)
		}
	}
	return result, true
}

// AppendThreadEvents durably appends the entries the stored window does not
// hold yet (by room and event id) and fsyncs the file before it returns; it
// returns how many were added. Only the new entries are written. When the
// append leaves more than threadEventsCompactLines complete lines, the file
// is rewritten to the newest ThreadEventsMax entries. The caller holds the
// state lock. An unparsable torn tail left by a crashed append is truncated
// first so new lines never fuse with a fragment; a valid unterminated tail
// entry is completed with its missing newline. A repeated append of entries
// already stored writes nothing, which is what makes a sync replay after a
// crash between this append and the cursor save idempotent.
func AppendThreadEvents(p Paths, name string, entries []ThreadEvent) (int, error) {
	if err := ValidateName(name); err != nil {
		return 0, err
	}
	cur, err := OpenThreadEvents(p, name)
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	var added []ThreadEvent
	batchSeen := map[threadEventKey]bool{}
	for _, e := range entries {
		if !validThreadEvent(e) {
			continue
		}
		key := threadEventKey{e.Room, e.EventID}
		if cur.has(e) || batchSeen[key] {
			continue
		}
		batchSeen[key] = true
		data, err := json.Marshal(e)
		if err != nil {
			return 0, err
		}
		buf.Write(data)
		buf.WriteByte('\n')
		added = append(added, e)
	}
	if len(added) == 0 {
		return 0, nil
	}
	path := p.ThreadEventsFile(name)
	data := buf.Bytes()
	truncateTo := int64(-1)
	lines := cur.lines
	if cur.tailPresent && cur.tailValid {
		data = append([]byte{'\n'}, data...)
		lines++
	} else if cur.tailPresent {
		truncateTo = cur.completeBytes
	}
	if err := appendThreadEventsBytes(path, data, truncateTo); err != nil {
		return 0, err
	}
	lines += len(added)
	if lines > threadEventsCompactLines {
		for _, e := range added {
			cur.applyNewest(e)
		}
		if err := compactThreadEventsFile(path, cur.entries); err != nil {
			return len(added), fmt.Errorf("compact thread events: %w", err)
		}
	}
	return len(added), nil
}

// compactThreadEventsFile rewrites the file to entries (already deduplicated
// and capped), oldest first, by temp file + rename like mailbox compaction.
func compactThreadEventsFile(path string, entries []ThreadEvent) error {
	var buf bytes.Buffer
	for _, e := range entries {
		data, err := json.Marshal(e)
		if err != nil {
			return err
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	if err := writeSecretAtomic(path, filepath.Base(path), buf.Bytes()); err != nil {
		return err
	}
	globalStateStats.threadEventWriteBytes.Add(int64(buf.Len()))
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}

// appendThreadEventsBytes appends data to the file (created 0600, never
// through a symlink) and fsyncs it; a newly created file also syncs its
// directory. A non-negative truncateTo first drops an unparsable torn tail.
// It mirrors the mailbox append write, including its protection checks.
func appendThreadEventsBytes(path string, data []byte, truncateTo int64) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, statErr := os.Lstat(path)
	created := errors.Is(statErr, fs.ErrNotExist)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|openNoFollow, SecretFileMode)
	if err != nil {
		return fmt.Errorf("thread events: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return fmt.Errorf("thread events: %s is not a regular file; refusing to use it", path)
	}
	if created {
		if err := f.Chmod(SecretFileMode); err != nil {
			f.Close()
			return err
		}
	} else if perm := st.Mode().Perm(); perm != SecretFileMode {
		f.Close()
		return fmt.Errorf("thread events: %s has mode %04o, want 0600; refusing to use it", path, perm)
	}
	if truncateTo >= 0 && truncateTo < st.Size() {
		if err := f.Truncate(truncateTo); err != nil {
			f.Close()
			return fmt.Errorf("truncate thread events: %w", err)
		}
	}
	n, err := f.Write(data)
	globalStateStats.threadEventWriteBytes.Add(int64(n))
	if err != nil {
		f.Close()
		return fmt.Errorf("append thread events: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("sync thread events: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if created {
		if d, err := os.Open(filepath.Dir(path)); err == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	return nil
}

// migrateLegacyThreadEvents writes an unmigrated State's retained entries to
// the thread-event file (deduplicated, capped to the newest
// ThreadEventsMax). The caller holds the state lock and drops the State
// member in the same update, after this append is durable.
func migrateLegacyThreadEvents(p Paths, name string, legacy []ThreadEvent) error {
	w := &ThreadEventWindow{index: map[threadEventKey]int{}}
	for _, e := range legacy {
		w.applyNewest(e)
	}
	_, err := AppendThreadEvents(p, name, w.entries)
	return err
}
