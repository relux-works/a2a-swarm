package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/relux-works/a2a-swarm/internal/matrix"
)

// archiveMaxEvents is the hard safety cap on the events read for one thread or
// one room scan. Exceeding it is an error and nothing is written for the room,
// so an archive is never silently truncated. Tests lower it.
var archiveMaxEvents = 100000

// archiveMaxPages bounds scans even when filters return no matching events.
const archiveMaxPages = 10000

// archiveRoomRE is the room directory name: the alias localpart, lowercase.
var archiveRoomRE = regexp.MustCompile(`^[a-z0-9._=-]+$`)

// archiveBodyIndent prefixes every non-empty body line in an archive file. A
// block starts only at a line beginning with "## " in column 0, and meta lines
// with "- ", so no body line can forge either.
const archiveBodyIndent = "    "

// RoomFileName is the archive file of the room-level messages (not in any thread).
const RoomFileName = "_room.md"

// IndexFileName is the optional JSON index of a room's archived topics.
const IndexFileName = "index.json"

// archiveDirName validates a room directory name.
func archiveDirName(local string) error {
	if !archiveRoomRE.MatchString(local) || local == "." || local == ".." {
		return fmt.Errorf("room alias %q cannot be an archive directory name (want [a-z0-9._=-]+, not . or ..)", sanitizeLine(local))
	}
	return nil
}

// archiveThread is one thread root and its events, oldest first (root first).
type archiveThread struct {
	topic  topicInfo
	events []matrix.Event
}

// archiveFile is one rendered file of a room archive.
type archiveFile struct {
	name    string
	content []byte
	events  int
}

// archiveIndexTopic is one entry of index.json.
type archiveIndexTopic struct {
	File      string `json:"file"`
	Slug      string `json:"slug,omitempty"`
	Title     string `json:"title"`
	Root      string `json:"root"`
	Events    int    `json:"events"`
	LastEvent string `json:"last_event"`
}

type archiveIndex struct {
	Room   string              `json:"room"`
	RoomID string              `json:"room_id"`
	Topics []archiveIndexTopic `json:"topics"`
}

// pageAllEvents pages newest-first to the end of a timeline and returns the
// events oldest first. Event/page bounds or a repeated token are errors, never
// evidence that the timeline ended. Empty filtered pages may have successors.
func pageAllEvents(what string, page func(from string) (matrix.Page, error)) ([]matrix.Event, error) {
	var out []matrix.Event
	from := ""
	seen := map[string]bool{}
	for pages := 1; ; pages++ {
		p, err := page(from)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Chunk...)
		if len(out) > archiveMaxEvents {
			return nil, fmt.Errorf("archive: %s has more than %d events; refusing a partial archive", what, archiveMaxEvents)
		}
		next := firstNonEmpty(p.End, p.NextBatch)
		if next == "" {
			break
		}
		if seen[next] {
			return nil, fmt.Errorf("archive: %s has a repeated pagination token; refusing a partial archive", what)
		}
		if pages == archiveMaxPages {
			return nil, fmt.Errorf("archive: %s has unresolved history after %d pages; refusing a partial archive", what, archiveMaxPages)
		}
		seen[next] = true
		from = next
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func isEdit(ev matrix.Event) (string, bool) {
	m := ev.Message()
	if m.RelatesTo != nil && m.RelatesTo.RelType == "m.replace" && m.RelatesTo.EventID != "" {
		return m.RelatesTo.EventID, true
	}
	return "", false
}

// isRedacted is shared with all message renderers and the local mailbox.
func isRedacted(ev matrix.Event) bool { return ev.IsRedacted() }

// collectArchive reads the whole history of a room: every thread (root plus
// all replies, plus edits of its events) and the room-level messages. Nothing
// is written here, so a failure leaves no partial archive.
func (s *session) collectArchive(ctx context.Context, roomID string) ([]archiveThread, []matrix.Event, error) {
	topics, err := s.listTopics(ctx, roomID)
	if err != nil {
		return nil, nil, err
	}
	scan, err := pageAllEvents("room "+roomID, func(from string) (matrix.Page, error) {
		return s.client.MessagesWithRedactions(ctx, roomID, from, "b", 100)
	})
	if err != nil {
		return nil, nil, err
	}
	roots := map[string]bool{}
	for _, t := range topics {
		roots[t.EventID] = true
	}
	// Thread roots the bounded topic listing missed (older than its scan).
	var extra []string
	for _, ev := range scan {
		if ev.Type != "m.room.message" {
			continue
		}
		root := ev.ThreadRoot()
		if root == "" && strings.HasPrefix(ev.Message().Body, TopicPrefix) {
			root = ev.EventID
		}
		if root != "" && !roots[root] {
			roots[root] = true
			extra = append(extra, root)
		}
	}
	for _, id := range extra {
		ev, err := s.client.GetEvent(ctx, roomID, id)
		if err != nil {
			return nil, nil, fmt.Errorf("archive: thread root %s: %w", sanitizeLine(id), err)
		}
		topics = append(topics, topicOf(ev, 0))
	}
	sort.SliceStable(topics, func(i, j int) bool {
		if topics[i].TS != topics[j].TS {
			return topics[i].TS < topics[j].TS
		}
		return topics[i].EventID < topics[j].EventID
	})

	threads := make([]archiveThread, 0, len(topics))
	threadOf := map[string]int{} // event id -> index in threads
	for _, t := range topics {
		rootEv, err := s.client.GetEvent(ctx, roomID, t.EventID)
		if err != nil {
			return nil, nil, fmt.Errorf("archive: thread root %s: %w", sanitizeLine(t.EventID), err)
		}
		replies, err := pageAllEvents("thread "+t.EventID, func(from string) (matrix.Page, error) {
			return s.client.ThreadReplies(ctx, roomID, t.EventID, from, "b", 100)
		})
		if err != nil {
			return nil, nil, err
		}
		if rootEv.IsRedacted() {
			t.Slug, t.Title = "", "[redacted]"
		}
		i := len(threads)
		evs := []matrix.Event{rootEv}
		threadOf[rootEv.EventID] = i
		for _, ev := range replies {
			if ev.Type == "m.room.message" {
				evs = append(evs, ev)
				threadOf[ev.EventID] = i
			}
		}
		threads = append(threads, archiveThread{topic: t, events: evs})
	}

	var room []matrix.Event
	for _, ev := range scan {
		if ev.Type != "m.room.message" || ev.ThreadRoot() != "" || roots[ev.EventID] {
			continue
		}
		if target, ok := isEdit(ev); ok {
			if i, ok := threadOf[target]; ok {
				threads[i].events = append(threads[i].events, ev)
				threadOf[ev.EventID] = i
				continue
			}
		}
		room = append(room, ev)
	}
	// Root first, then replies and edits by origin time (stable: API order on ties).
	for i := range threads {
		rest := threads[i].events[1:]
		sort.SliceStable(rest, func(a, b int) bool { return rest[a].OriginServerTS < rest[b].OriginServerTS })
	}
	return threads, room, nil
}

// renderArchive renders one archive file: the header lines, then one block per event.
func renderArchive(title string, header []string, events []matrix.Event, server string) []byte {
	var b strings.Builder
	b.WriteString("# " + sanitizeLine(title) + "\n")
	for _, h := range header {
		b.WriteString("- " + sanitizeLine(h) + "\n")
	}
	for _, ev := range events {
		writeArchiveBlock(&b, ev, server)
	}
	return []byte(b.String())
}

// writeArchiveBlock renders "## <event id>", the meta lines, and the indented body.
func writeArchiveBlock(b *strings.Builder, ev matrix.Event, server string) {
	redacted := isRedacted(ev)
	if redacted {
		ev = ev.WithoutContent()
	}
	m := ev.Message()
	b.WriteString("\n## " + sanitizeLine(ev.EventID) + "\n")
	meta := []string{}
	h, hok := decodeHeader(m.A2A)
	if hok && !redacted {
		if h.Kind != "" {
			meta = append(meta, "kind="+h.Kind)
		}
		if len(h.To) > 0 {
			meta = append(meta, "to="+strings.Join(h.To, ","))
		}
		if h.Re != "" {
			meta = append(meta, "re="+h.Re)
		}
		if h.ReplyBy != "" {
			meta = append(meta, "reply-by="+h.ReplyBy)
		}
		if h.Trunk != "" {
			meta = append(meta, "trunk="+h.Trunk)
		}
		if h.Subtype != "" {
			meta = append(meta, "subtype="+h.Subtype)
		}
		if h.Subject != "" {
			meta = append(meta, "subject="+h.Subject)
		}
	}
	b.WriteString("- " + time.UnixMilli(ev.OriginServerTS).UTC().Format(time.RFC3339) + " " + senderText(ev.Sender))
	if len(meta) > 0 {
		b.WriteString(" " + sanitizeLine(strings.Join(meta, " ")))
	}
	b.WriteByte('\n')
	if at := h.Attachment; hok && at != nil && !redacted {
		fmt.Fprintf(b, "- attachment: %s %d sha256=%s %s%s\n", sanitizeLine(at.Name), at.Size, sanitizeLine(at.SHA256), sanitizeLine(at.MXC), attachmentEncodingText(at, " "))
	}
	if target, ok := isEdit(ev); ok {
		b.WriteString("- edit-of: " + sanitizeLine(target) + "\n")
	}
	body := ""
	if redacted {
		b.WriteString("- redacted\n")
		body = "[redacted]"
	} else {
		body = m.Body
		var nc struct {
			NewContent *struct {
				Body string `json:"body"`
			} `json:"m.new_content"`
		}
		if _, ok := isEdit(ev); ok && json.Unmarshal(ev.Content, &nc) == nil && nc.NewContent != nil {
			body = nc.NewContent.Body
		}
		body = sanitizeText(body)
		if hok {
			body = stripHeaderBody(body)
		}
		body = strings.TrimRight(body, "\n")
	}
	if body == "" {
		return
	}
	b.WriteString("\n")
	for _, l := range strings.Split(body, "\n") {
		if l != "" {
			b.WriteString(archiveBodyIndent + l)
		}
		b.WriteString("\n")
	}
}

// renderRoomArchive renders every file of one room archive.
func renderRoomArchive(label, roomID string, threads []archiveThread, room []matrix.Event, server string, withIndex bool) ([]archiveFile, error) {
	var files []archiveFile
	idx := archiveIndex{Room: label, RoomID: roomID, Topics: []archiveIndexTopic{}}
	used := map[string]bool{RoomFileName: true, IndexFileName: true}
	for _, th := range threads {
		t := th.topic
		slug := t.Slug
		name := slug + ".md"
		if !slugRE.MatchString(slug) || used[name] {
			// No slug, an invalid one, or a later duplicate: a stable name from the root id.
			sum := sha256.Sum256([]byte(t.EventID))
			name = "_thread-" + hex.EncodeToString(sum[:8]) + ".md"
			if !slugRE.MatchString(slug) {
				slug = ""
			}
		}
		used[name] = true
		shown := firstNonEmpty(slug, "-")
		content := renderArchive(label+" / "+shown+": "+t.Title,
			[]string{"room: " + roomID, "root: " + t.EventID, "archived-by: a2a archive"}, th.events, server)
		files = append(files, archiveFile{name: name, content: content, events: len(th.events)})
		idx.Topics = append(idx.Topics, archiveIndexTopic{File: name, Slug: slug, Title: sanitizeLine(t.Title),
			Root: sanitizeLine(t.EventID), Events: len(th.events), LastEvent: sanitizeLine(th.events[len(th.events)-1].EventID)})
	}
	files = append(files, archiveFile{name: RoomFileName, events: len(room),
		content: renderArchive(label+" / _room: room-level messages", []string{"room: " + roomID, "archived-by: a2a archive"}, room, server)})
	if withIndex {
		sort.Slice(idx.Topics, func(i, j int) bool { return idx.Topics[i].File < idx.Topics[j].File })
		data, err := json.MarshalIndent(idx, "", "  ")
		if err != nil {
			return nil, err
		}
		total := 0
		for _, t := range idx.Topics {
			total += t.Events
		}
		files = append(files, archiveFile{name: IndexFileName, content: append(data, '\n'), events: total})
	}
	return files, nil
}

// checkNoSymlink refuses a path that is a symlink, or exists as something
// other than wantDir (a directory) / a regular file. A missing path is fine.
func checkNoSymlink(path string, wantDir bool) error {
	st, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	switch {
	case st.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("archive: %s is a symlink; refusing to write through it", sanitizeLine(path))
	case wantDir && !st.IsDir():
		return fmt.Errorf("archive: %s is not a directory", sanitizeLine(path))
	case !wantDir && !st.Mode().IsRegular():
		return fmt.Errorf("archive: %s is not a regular file", sanitizeLine(path))
	}
	return nil
}

// writeAtomic writes data to path through a temporary file in the same
// directory and a rename, so readers see the old or the new file, never a mix.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".a2a-archive-*")
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	ok = true
	return nil
}

// writeRoomArchive checks every target first (no symlinks anywhere below dir),
// then writes the changed files atomically and prints one line per file.
func (a *App) writeRoomArchive(dir, roomDir string, files []archiveFile) error {
	roomPath := filepath.Join(dir, roomDir)
	if err := checkNoSymlink(dir, true); err != nil {
		return err
	}
	if err := checkNoSymlink(roomPath, true); err != nil {
		return err
	}
	for _, f := range files {
		if err := checkNoSymlink(filepath.Join(roomPath, f.name), false); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(roomPath, 0o755); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	for _, f := range files {
		p := filepath.Join(roomPath, f.name)
		state := "changed"
		if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, f.content) {
			state = "unchanged"
		} else if err := writeAtomic(p, f.content); err != nil {
			return err
		}
		a.printf("%s\t%d\t%s\n", sanitizeLine(p), f.events, state)
	}
	return nil
}

// archiveRoom reads, renders, and writes the archive of one room.
func (s *session) archiveRoom(ctx context.Context, roomID, label, roomDir, dir string, withIndex bool) error {
	threads, room, err := s.collectArchive(ctx, roomID)
	if err != nil {
		return err
	}
	// Reapply after all pages: a redaction may arrive on a later page than
	// its original. Topic metadata is derived again from the filtered root.
	for i := range threads {
		threads[i].events, err = s.client.FilterEvents(roomID, threads[i].events)
		if err != nil {
			return err
		}
		if threads[i].events[0].IsRedacted() {
			threads[i].topic.Slug, threads[i].topic.Title = "", "[redacted]"
		}
	}
	room, err = s.client.FilterEvents(roomID, room)
	if err != nil {
		return err
	}
	files, err := renderRoomArchive(label, roomID, threads, room, s.server, withIndex)
	if err != nil {
		return err
	}
	if err := scrubPriorRedactedTopics(dir, roomDir, roomID, label, s.server, threads, room, files); err != nil {
		return err
	}
	return s.app.writeRoomArchive(dir, roomDir, files)
}

// cmdArchive: a2a archive <room|all> [--dir archive] [--json-index]
func cmdArchive(ctx context.Context, a *App, args []string) error {
	const use = "usage: a2a archive <room|all> [--dir archive] [--json-index]"
	fset := newFlagSet("archive")
	dir := fset.String("dir", "archive", "archive root directory")
	withIndex := fset.Bool("json-index", false, "also write <dir>/<room>/index.json")
	pos, err := parseInterspersed(fset, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef(use)
	}
	if *dir == "" {
		return usagef("--dir must not be empty")
	}
	fmt.Fprintln(a.Stderr, archiveRedactionWarning)
	all := pos[0] == "all"
	if !all && !strings.HasPrefix(pos[0], "!") {
		local, _, _ := strings.Cut(strings.TrimPrefix(pos[0], "#"), ":")
		if err := archiveDirName(local); err != nil {
			return usageError{err.Error()}
		}
	}
	s, err := a.session()
	if err != nil {
		return err
	}
	excluded, err := ownerFiles(s.paths, s.name).exclusions()
	if err != nil {
		return err
	}
	if !all {
		roomID, label, err := s.resolveRoom(ctx, pos[0])
		if err != nil {
			return err
		}
		if excluded[roomID] || s.adminRoomExcluded(roomID) {
			return nil
		}
		if strings.HasPrefix(label, "!") {
			if label = s.roomLabel(ctx, roomID, nil); strings.HasPrefix(label, "!") {
				return fmt.Errorf("archive: room %s has no alias to name its directory", sanitizeLine(roomID))
			}
		}
		local, _, _ := strings.Cut(strings.TrimPrefix(label, "#"), ":")
		if err := archiveDirName(local); err != nil {
			return fmt.Errorf("archive: %w", err)
		}
		return s.archiveRoom(ctx, roomID, label, local, *dir, *withIndex)
	}
	ids, err := s.client.JoinedRooms(ctx)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if excluded[id] || s.adminRoomExcluded(id) {
			continue
		}
		label := s.roomLabel(ctx, id, nil)
		if !strings.HasPrefix(label, "#") || strings.Contains(label, ":") {
			fmt.Fprintf(a.Stderr, "a2a: note: skipping %s: no local alias\n", sanitizeLine(id))
			continue
		}
		local := strings.TrimPrefix(label, "#")
		if err := archiveDirName(local); err != nil {
			fmt.Fprintf(a.Stderr, "a2a: note: skipping %s: %v\n", sanitizeLine(id), err)
			continue
		}
		if err := s.archiveRoom(ctx, id, label, local, *dir, *withIndex); err != nil {
			return fmt.Errorf("archive %s: %w", label, err)
		}
	}
	return nil
}

// A redacted topic no longer has its content-derived slug. Replace earlier
// a2a-generated files for that root as well as writing its new stable filename;
// otherwise the old slug file would keep exporting the removed root. Only
// bounded generated headers in the explicitly selected export directory are
// read; no git history or unrelated file is changed or printed.
func scrubPriorRedactedTopics(dir, roomDir, roomID, label, server string, threads []archiveThread, room []matrix.Event, current []archiveFile) error {
	currentNames := map[string]bool{}
	for _, f := range current {
		currentNames[f.name] = true
	}
	roots := map[string]bool{}
	for _, ev := range room {
		if ev.IsRedacted() {
			roots[ev.EventID] = true
		}
	}
	for _, th := range threads {
		if len(th.events) > 0 && th.events[0].IsRedacted() {
			roots[th.topic.EventID] = true
		}
	}
	if len(roots) == 0 {
		return nil
	}
	if err := checkNoSymlink(dir, true); err != nil {
		return err
	}
	path := filepath.Join(dir, roomDir)
	if err := checkNoSymlink(path, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("archive: cannot inspect previous exports")
	}
	// A previous index is still a published export when this run omits
	// --json-index. Replace it from this run's filtered snapshot as well.
	if !currentNames[IndexFileName] {
		indexPath := filepath.Join(path, IndexFileName)
		if err := checkNoSymlink(indexPath, false); err != nil {
			return err
		}
		if _, err := os.Stat(indexPath); err == nil {
			files, err := renderRoomArchive(label, roomID, threads, room, server, true)
			if err != nil {
				return err
			}
			// Re-rendering needs no payload from the old index.
			for _, file := range files {
				if file.name == IndexFileName {
					if err := writeAtomic(indexPath, file.content); err != nil {
						return errors.New("archive: cannot replace previous redacted topic index")
					}
				}
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return errors.New("archive: cannot inspect previous index")
		}
	}
	for _, entry := range entries {
		if currentNames[entry.Name()] || !strings.HasSuffix(entry.Name(), ".md") || !entry.Type().IsRegular() {
			continue
		}
		p := filepath.Join(path, entry.Name())
		f, err := os.Open(p)
		if err != nil {
			return errors.New("archive: cannot inspect previous export header")
		}
		// A valid Matrix event can contribute a long topic title to the
		// first header line. Bound the header scan above the event size,
		// rather than truncating a valid title before its root marker.
		scanner := bufio.NewScanner(io.LimitReader(f, 128<<10))
		scanner.Buffer(make([]byte, 4096), 128<<10)
		var header []string
		for len(header) < 4 && scanner.Scan() {
			header = append(header, scanner.Text())
		}
		f.Close()
		if len(header) != 4 || !strings.HasPrefix(header[0], "# ") || header[1] != "- room: "+roomID || header[3] != "- archived-by: a2a archive" {
			continue
		}
		root := strings.TrimPrefix(header[2], "- root: ")
		if roots[root] {
			data := renderArchive("[redacted topic]", []string{"room: " + roomID, "root: " + root, "archived-by: a2a archive"}, nil, "")
			if err := writeAtomic(p, data); err != nil {
				return errors.New("archive: cannot replace previous redacted topic export")
			}
		}
	}
	return nil
}
