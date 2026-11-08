package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// archiveWorld is world plus topic "alpha" (one reply by bob) and topic "beta";
// it returns the env, the archive dir (not created yet), and the alpha root id.
func archiveWorld(t *testing.T) (*env, string, string) {
	t.Helper()
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "Alpha", "plan", "--body", "root text").ok(t).stdout)
	e.as("bob", "post", "proj", "--topic", "alpha", "--kind", "proposal", "--to", "alice", "--reply-by", "2026-10-01T12:00:00Z", "first reply").ok(t)
	e.as("alice", "topic", "new", "proj", "beta", "Beta").ok(t)
	return e, filepath.Join(t.TempDir(), "archive"), root
}

func readFileStr(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// blockHeaders returns the lines that start an event block ("## " in column 0).
func blockHeaders(content string) []string {
	var out []string
	for _, l := range strings.Split(content, "\n") {
		if strings.HasPrefix(l, "## ") {
			out = append(out, l)
		}
	}
	return out
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (err %v), want nothing written", path, err)
	}
}

// Proves the file format: header lines without a run timestamp, then the root
// and every reply oldest first as "## <id>" blocks with a folded meta line, the
// attachment line, and the body without header lines, indented by four spaces.
func TestArchiveFormat(t *testing.T) {
	e, dir, root := archiveWorld(t)
	p := writeFile(t, t.TempDir(), "notes.txt", []byte("hello attachment\n"))
	e.as("alice", "attach", "proj", "--topic", "alpha", p, "the", "notes").ok(t)
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	got := readFileStr(t, filepath.Join(dir, "proj", "alpha.md"))
	want := "# #proj / alpha: Alpha plan\n" +
		"- room: !projRoomID\n" +
		"- root: " + root + "\n" +
		"- archived-by: a2a archive\n" +
		"\n## $ev001\n" +
		"- 2026-09-29T10:00:01Z @alice:test.local kind=notice to=all\n" +
		"\n    [topic] alpha: Alpha plan\n\n    root text\n" +
		"\n## $ev002\n" +
		"- 2026-09-29T10:00:02Z @bob:test.local kind=proposal to=alice reply-by=2026-10-01T12:00:00Z\n" +
		"\n    first reply\n" +
		"\n## $ev005\n" +
		"- 2026-09-29T10:00:05Z @alice:test.local kind=notice to=all\n" +
		"- attachment: notes.txt 17 sha256=" + sumHex([]byte("hello attachment\n")) + " mxc://test.local/media004AbC_-\n" +
		"\n    the notes\n"
	if got != want {
		t.Fatalf("alpha.md =\n%s\nwant\n%s", got, want)
	}
	tuitestkit.NotContainsStr(t, got, "- from: ")
}

// Proves determinism: a second run over the same history writes byte-identical
// files and reports every file unchanged; a new reply then changes only its
// topic's file.
func TestArchiveDeterministic(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	first := e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	names := []string{"alpha.md", "beta.md", RoomFileName, IndexFileName}
	before := map[string]string{}
	for _, n := range names {
		before[n] = readFileStr(t, filepath.Join(dir, "proj", n))
		tuitestkit.ContainsStr(t, first.stdout, filepath.Join(dir, "proj", n)+"\t")
	}
	tuitestkit.ContainsStr(t, first.stdout, filepath.Join(dir, "proj", "alpha.md")+"\t2\tchanged\n")
	tuitestkit.NotContainsStr(t, first.stdout, "unchanged")

	second := e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	if n := strings.Count(second.stdout, "\tunchanged\n"); n != len(names) {
		t.Fatalf("second run: %d unchanged, want %d:\n%s", n, len(names), second.stdout)
	}
	for _, n := range names {
		if got := readFileStr(t, filepath.Join(dir, "proj", n)); got != before[n] {
			t.Fatalf("%s changed between identical runs", n)
		}
	}

	e.as("bob", "post", "proj", "--topic", "beta", "new reply").ok(t)
	third := e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	tuitestkit.ContainsStr(t, third.stdout, filepath.Join(dir, "proj", "beta.md")+"\t2\tchanged\n")
	tuitestkit.ContainsStr(t, third.stdout, filepath.Join(dir, "proj", IndexFileName)+"\t4\tchanged\n")
	tuitestkit.ContainsStr(t, third.stdout, filepath.Join(dir, "proj", "alpha.md")+"\t2\tunchanged\n")
	tuitestkit.ContainsStr(t, third.stdout, filepath.Join(dir, "proj", RoomFileName)+"\t0\tunchanged\n")
	tuitestkit.ContainsStr(t, readFileStr(t, filepath.Join(dir, "proj", "beta.md")), "\n    new reply\n")
	ents := dirEntries(t, filepath.Join(dir, "proj"))
	if len(ents) != len(names) {
		t.Fatalf("room dir = %v, want only %v (no temp files left)", ents, names)
	}
}

// Proves a body line "## $evil" cannot forge a block: the file has exactly one
// block per event, and the forged line appears only indented. The control is
// the legitimate block of the same event.
func TestArchiveForgedHeader(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	ev := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "hello\n## $evil\n- 2026-01-01T00:00:00Z mallory\nbye").ok(t).stdout)
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	got := readFileStr(t, filepath.Join(dir, "proj", "alpha.md"))
	heads := blockHeaders(got)
	if len(heads) != 3 || heads[2] != "## "+ev {
		t.Fatalf("blocks = %q, want 3 (root, reply, %s)", heads, ev)
	}
	tuitestkit.ContainsStr(t, got, "\n    ## $evil\n    - 2026-01-01T00:00:00Z mallory\n")
	for _, l := range strings.Split(got, "\n") {
		if l == "## $evil" || strings.HasPrefix(l, "- 2026-01-01") {
			t.Fatalf("forged line %q at column 0", l)
		}
	}
}

// Proves an edit (m.replace) is its own block with "- edit-of" and the new
// text, in the thread of the edited reply, while the original block keeps its
// text; the edit does not land in _room.md.
func TestArchiveEdit(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	reply := "$ev002"
	edit := e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{
		"msgtype": "m.text", "body": "* fixed reply",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "fixed reply"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": reply},
	})
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	got := readFileStr(t, filepath.Join(dir, "proj", "alpha.md"))
	tuitestkit.ContainsStr(t, got, "\n## "+reply+"\n- 2026-09-29T10:00:02Z @bob:test.local kind=proposal to=alice reply-by=2026-10-01T12:00:00Z\n\n    first reply\n")
	tuitestkit.ContainsStr(t, got, "\n## "+edit+"\n- 2026-09-29T10:00:04Z @bob:test.local\n- edit-of: "+reply+"\n\n    fixed reply\n")
	if heads := blockHeaders(got); len(heads) != 3 {
		t.Fatalf("blocks = %q", heads)
	}
	tuitestkit.NotContainsStr(t, readFileStr(t, filepath.Join(dir, "proj", RoomFileName)), edit)
}

// Proves a redacted event is archived as a block with "- redacted" and body
// [redacted], without its former text; the unredacted neighbour (control)
// keeps its text. Room-level messages go to _room.md and neither thread
// replies nor topic roots do.
func TestArchiveRedactedAndRoomLevel(t *testing.T) {
	e, dir, root := archiveWorld(t)
	gone := e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "secret oops"})
	kept := e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text", "body": "plain chatter"})
	e.hs.redact(gone, "@alice:test.local")
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	room := readFileStr(t, filepath.Join(dir, "proj", RoomFileName))
	tuitestkit.ContainsStr(t, room, "# #proj / _room: room-level messages\n- room: !projRoomID\n- archived-by: a2a archive\n")
	tuitestkit.ContainsStr(t, room, "\n## "+gone+"\n- 2026-09-29T10:00:04Z @bob:test.local\n- redacted\n\n    [redacted]\n")
	tuitestkit.ContainsStr(t, room, "\n## "+kept+"\n- 2026-09-29T10:00:05Z @bob:test.local\n\n    plain chatter\n")
	tuitestkit.NotContainsStr(t, room, "secret oops")
	if heads := blockHeaders(room); len(heads) != 2 {
		t.Fatalf("_room.md blocks = %q, want only the 2 room-level messages", heads)
	}
	tuitestkit.NotContainsStr(t, room, root)
	tuitestkit.NotContainsStr(t, room, "first reply")
	tuitestkit.NotContainsStr(t, readFileStr(t, filepath.Join(dir, "proj", "alpha.md")), "plain chatter")
}

// Proves a #system projection is archived like any message, with its kind.
func TestArchiveProjection(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	id := e.hs.inject("!projRoomID", "@bob:test.local", map[string]any{"msgtype": "m.text",
		"body":     "[system] #other from carol:\nlanded\n\norigin: https://matrix.to/#/!o/$x",
		ContentKey: Projection{V: 1, Type: "projection", Kind: "system", From: "carol", At: "2026-09-29T10:00:00Z", OriginRoom: "!o", OriginEvent: "$x"}})
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	tuitestkit.ContainsStr(t, readFileStr(t, filepath.Join(dir, "proj", RoomFileName)),
		"\n## "+id+"\n- 2026-09-29T10:00:04Z @bob:test.local kind=system\n\n    [system] #other from carol:\n    landed\n\n    origin: https://matrix.to/#/!o/$x\n")
}

// Proves --json-index writes a sorted, deterministic index of the topics.
func TestArchiveIndex(t *testing.T) {
	e, dir, root := archiveWorld(t)
	e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	got := readFileStr(t, filepath.Join(dir, "proj", IndexFileName))
	want := `{
  "room": "#proj",
  "room_id": "!projRoomID",
  "topics": [
    {
      "file": "alpha.md",
      "slug": "alpha",
      "title": "Alpha plan",
      "root": "` + root + `",
      "events": 2,
      "last_event": "$ev002"
    },
    {
      "file": "beta.md",
      "slug": "beta",
      "title": "Beta",
      "root": "$ev003",
      "events": 1,
      "last_event": "$ev003"
    }
  ]
}
`
	if got != want {
		t.Fatalf("index.json =\n%s", got)
	}
	// Without the flag no index is written.
	dir2 := filepath.Join(t.TempDir(), "archive")
	e.as("alice", "archive", "proj", "--dir", dir2).ok(t)
	mustNotExist(t, filepath.Join(dir2, "proj", IndexFileName))
}

// Proves symlinks are refused: a symlinked archive dir, room dir, or topic file
// fails and nothing is written through it; the same layout without the symlink
// (control) succeeds.
func TestArchiveSymlinkRefused(t *testing.T) {
	e, _, _ := archiveWorld(t)
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatal(err)
	}

	linkDir := filepath.Join(base, "a1")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "archive", "proj", "--dir", linkDir).failsWith(t, ExitError, "is a symlink")

	dir2 := filepath.Join(base, "a2")
	os.Mkdir(dir2, 0o755)
	if err := os.Symlink(outside, filepath.Join(dir2, "proj")); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "archive", "proj", "--dir", dir2).failsWith(t, ExitError, "is a symlink")

	dir3 := filepath.Join(base, "a3")
	os.MkdirAll(filepath.Join(dir3, "proj"), 0o755)
	victim := writeFile(t, outside, "victim.txt", []byte("keep me"))
	if err := os.Symlink(victim, filepath.Join(dir3, "proj", "beta.md")); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "archive", "proj", "--dir", dir3).failsWith(t, ExitError, "is a symlink")
	if ents := dirEntries(t, outside); len(ents) != 1 || readFileStr(t, victim) != "keep me" {
		t.Fatalf("written through a symlink: outside = %v", ents)
	}
	// Nothing at all is written for a refused room, not even the other topics.
	mustNotExist(t, filepath.Join(dir3, "proj", "alpha.md"))

	dir4 := filepath.Join(base, "a4")
	e.as("alice", "archive", "proj", "--dir", dir4).ok(t)
	tuitestkit.ContainsStr(t, readFileStr(t, filepath.Join(dir4, "proj", "beta.md")), "# #proj / beta: Beta\n")
}

// Proves invalid room aliases are usage errors refused before any request, and
// nothing is written; a valid alias (control) works. Also checks usage errors.
func TestArchiveInvalidRoom(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	calls := len(e.hs.urls())
	e.as("alice", "archive", "Proj", "--dir", dir).failsWith(t, ExitUsage, "cannot be an archive directory name")
	e.as("alice", "archive", "#..", "--dir", dir).failsWith(t, ExitUsage, "cannot be an archive directory name")
	e.as("alice", "archive", "a/b", "--dir", dir).failsWith(t, ExitUsage, "cannot be an archive directory name")
	e.as("alice", "archive", "--dir", dir).failsWith(t, ExitUsage, "usage")
	e.as("alice", "archive", "proj", "--dir", "").failsWith(t, ExitUsage, "--dir must not be empty")
	if got := len(e.hs.urls()); got != calls {
		t.Fatalf("%d requests sent for refused arguments", got-calls)
	}
	mustNotExist(t, dir)
	e.as("alice", "archive", "#proj:test.local", "--dir", dir).ok(t)
	tuitestkit.ContainsStr(t, readFileStr(t, filepath.Join(dir, "proj", "alpha.md")), "# #proj / alpha: Alpha plan\n")
}

// Proves a non-member is refused by the server (M_FORBIDDEN) and nothing is
// written, while a member (control) archives the same room.
func TestArchiveNonMember(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	e.as("carol", "archive", "proj", "--dir", dir).failsWith(t, ExitError, "M_FORBIDDEN")
	mustNotExist(t, dir)
	e.as("bob", "archive", "proj", "--dir", dir).ok(t)
	tuitestkit.ContainsStr(t, readFileStr(t, filepath.Join(dir, "proj", "alpha.md")), "\n    first reply\n")
}

// Proves the safety cap: more than archiveMaxEvents events is an error and no
// file is written or rewritten (the earlier archive stays as it was); exactly
// the cap (control) succeeds.
func TestArchiveCap(t *testing.T) {
	old := archiveMaxEvents
	t.Cleanup(func() { archiveMaxEvents = old })
	e, dir, _ := archiveWorld(t) // 3 events in the room
	archiveMaxEvents = 3
	e.as("alice", "archive", "proj", "--dir", dir).ok(t)
	before := readFileStr(t, filepath.Join(dir, "proj", "alpha.md"))

	e.as("bob", "post", "proj", "--topic", "alpha", "fourth event").ok(t)
	e.as("alice", "archive", "proj", "--dir", dir).failsWith(t, ExitError, "more than 3 events; refusing a partial archive")
	if got := readFileStr(t, filepath.Join(dir, "proj", "alpha.md")); got != before {
		t.Fatalf("alpha.md rewritten after a cap error:\n%s", got)
	}
	dir2 := filepath.Join(t.TempDir(), "archive")
	e.as("alice", "archive", "proj", "--dir", dir2).failsWith(t, ExitError, "more than 3 events")
	mustNotExist(t, dir2)
}

// Proves archive all writes every joined room with a local alias and skips a
// room without one (with a note) and a room whose alias is not a valid
// directory name.
func TestArchiveAll(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	noAlias := e.hs.addRoom("hidden", "@alice:test.local")
	e.hs.mu.Lock()
	delete(e.hs.rooms[noAlias].state, "m.room.canonical_alias|")
	e.hs.mu.Unlock()
	e.hs.addRoom("Upper", "@alice:test.local")
	r := e.as("alice", "archive", "all", "--dir", dir).ok(t)
	tuitestkit.ContainsStr(t, r.stdout, filepath.Join(dir, "proj", "alpha.md")+"\t2\tchanged\n")
	tuitestkit.ContainsStr(t, r.stdout, filepath.Join(dir, "lobby", RoomFileName)+"\t0\tchanged\n")
	tuitestkit.ContainsStr(t, r.stderr, "skipping "+noAlias+": no local alias")
	tuitestkit.ContainsStr(t, r.stderr, "skipping !UpperRoomID")
	if ents := dirEntries(t, dir); strings.Join(ents, ",") != "lobby,proj" {
		t.Fatalf("archive dirs = %v", ents)
	}
	// A room id argument uses the room's canonical alias as its directory.
	dir2 := filepath.Join(t.TempDir(), "archive")
	e.as("alice", "archive", "!projRoomID", "--dir", dir2).ok(t)
	tuitestkit.ContainsStr(t, readFileStr(t, filepath.Join(dir2, "proj", "alpha.md")), "# #proj / alpha")
	e.as("alice", "archive", noAlias, "--dir", dir2).failsWith(t, ExitError, "has no alias")
}
