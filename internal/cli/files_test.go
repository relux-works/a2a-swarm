package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

const (
	uploadRoute   = "POST /_matrix/media/v3/upload"
	downloadRoute = "GET /_matrix/client/v1/media/download/{server}/{media}"
)

// writeFile creates dir/name with data and returns its path.
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func sumHex(data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:])
}

// dirEntries lists the names in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

// attachWorld is world plus topic "alpha" and one attached text file; it
// returns the env, the attachment event id, the file bytes, and the root id.
func attachWorld(t *testing.T) (*env, string, []byte, string) {
	t.Helper()
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	data := []byte("hello attachment\nline two\n")
	p := writeFile(t, t.TempDir(), "notes.txt", data)
	id := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", p, "see", "the", "notes").ok(t).stdout)
	return e, id, data, root
}

// Proves the round trip: attach sends an m.file thread reply with url,
// filename, info and the a2a attachment header (sha256 of the real bytes);
// files lists it; fetch by event id (default name and -o) and by bare mxc with
// --sha256 writes exactly the uploaded bytes.
func TestAttachFilesFetchRoundTrip(t *testing.T) {
	e, id, data, root := attachWorld(t)
	ev, ok := e.hs.eventByID(id)
	if !ok {
		t.Fatalf("event %q not sent", id)
	}
	c := decodeContent(t, ev)
	m := ev.Message()
	h, _ := decodeHeader(m.A2A)
	if h.Attachment == nil {
		t.Fatalf("no attachment header: %s", ev.Content)
	}
	at := *h.Attachment
	want := Attachment{Name: "notes.txt", Size: int64(len(data)), SHA256: sumHex(data), MXC: at.MXC, MimeType: "text/plain; charset=utf-8"}
	if !reflect.DeepEqual(at, want) || !strings.HasPrefix(at.MXC, "mxc://test.local/") {
		t.Fatalf("attachment = %+v, want %+v", at, want)
	}
	if c["msgtype"] != "m.file" || c["url"] != at.MXC || c["filename"] != "notes.txt" {
		t.Fatalf("content = %v", c)
	}
	info, _ := c["info"].(map[string]any)
	if info["size"] != float64(len(data)) || info["mimetype"] != at.MimeType {
		t.Fatalf("info = %v", info)
	}
	tuitestkit.ContainsStr(t, m.Body, "- from: alice\n")
	tuitestkit.ContainsStr(t, m.Body, "- kind: notice\n")
	tuitestkit.ContainsStr(t, m.Body, "\n\nsee the notes")
	if ev.ThreadRoot() != root || h.Topic != root || h.Slug != "alpha" {
		t.Fatalf("not a thread reply to %s: %+v", root, h)
	}

	files := e.as("bob", "files", "proj").ok(t).stdout
	tuitestkit.ContainsStr(t, files, id+"\talice\tnotes.txt\t26\t"+at.SHA256[:12]+"\t"+at.MXC+"\n")
	recs := decodeRecords(t, e.as("bob", "files", "proj", "--json").ok(t).stdout)
	if len(recs) != 1 || recs[0].Attachment == nil || !reflect.DeepEqual(*recs[0].Attachment, at) || recs[0].Body != "see the notes" {
		t.Fatalf("files --json = %+v", recs)
	}
	readTxt := e.as("bob", "read", "proj", "--event", id).ok(t).stdout
	tuitestkit.ContainsStr(t, readTxt, "file=notes.txt size=26")

	dir := t.TempDir()
	t.Chdir(dir)
	r := e.as("bob", "fetch", "proj", id).ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "notes.txt")
	got, err := os.ReadFile(filepath.Join(dir, "notes.txt"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("fetched %q, %v", got, err)
	}
	if ents := dirEntries(t, dir); !reflect.DeepEqual(ents, []string{"notes.txt"}) {
		t.Fatalf("leftovers in dir: %v", ents)
	}

	out := filepath.Join(dir, "copy.bin")
	e.as("bob", "fetch", "proj", id, "-o", out).ok(t)
	got, _ = os.ReadFile(out)
	eq(t, string(got), string(data))

	out2 := filepath.Join(dir, "bare.bin")
	e.as("bob", "fetch", at.MXC, "--sha256", strings.ToUpper(at.SHA256), "-o", out2).ok(t)
	got, _ = os.ReadFile(out2)
	eq(t, string(got), string(data))
}

// Proves image/* content (sniffed from the bytes, not the name) becomes
// m.image; the text file in the round-trip test is the m.file control.
func TestAttachImage(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	png := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), bytes.Repeat([]byte{7}, 40)...)
	p := writeFile(t, t.TempDir(), "shot.dat", png)
	id := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", p).ok(t).stdout)
	ev, _ := e.hs.eventByID(id)
	c := decodeContent(t, ev)
	h, _ := decodeHeader(ev.Message().A2A)
	if c["msgtype"] != "m.image" || h.Attachment.MimeType != "image/png" {
		t.Fatalf("content = %v", c)
	}
	// Without a caption the body text is the file name.
	tuitestkit.ContainsStr(t, ev.Message().Body, "\n\nshot.dat")
}

// Proves attach --pin pins the new event; pin is idempotent (no duplicate,
// a note); unpin removes it; unpin of a non-pinned id is a no-op success with
// a note; pin verifies the event exists; a missing state event is an empty list.
func TestPinUnpin(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	if got := e.hs.pinned("!projRoomID"); got != nil {
		t.Fatalf("precondition: pinned = %v", got)
	}
	p := writeFile(t, t.TempDir(), "a.txt", []byte("aaa"))
	id := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", "--pin", p).ok(t).stdout)
	eq(t, strings.Join(e.hs.pinned("!projRoomID"), ","), id)

	r := e.as("bob", "pin", "proj", id).ok(t)
	tuitestkit.ContainsStr(t, r.stderr, "already pinned")
	eq(t, strings.Join(e.hs.pinned("!projRoomID"), ","), id)

	e.as("bob", "pin", "proj", root).ok(t)
	eq(t, strings.Join(e.hs.pinned("!projRoomID"), ","), id+","+root)

	r = e.as("bob", "unpin", "proj", id).ok(t)
	tuitestkit.NotContainsStr(t, r.stderr, "note")
	eq(t, strings.Join(e.hs.pinned("!projRoomID"), ","), root)
	r = e.as("bob", "unpin", "proj", id).ok(t)
	tuitestkit.ContainsStr(t, r.stderr, "not pinned")
	eq(t, strings.Join(e.hs.pinned("!projRoomID"), ","), root)

	e.as("bob", "pin", "proj", "$nosuch").failsWith(t, ExitError, "not found")
	e.as("bob", "pin", "proj", "nosuch").failsWith(t, ExitUsage, "usage")
	eq(t, strings.Join(e.hs.pinned("!projRoomID"), ","), root)
}

// Proves a non-member (carol) cannot attach (refused by the server before any
// upload) or pin (M_FORBIDDEN); alice attaching the same file is the control.
func TestAttachPinNonMember(t *testing.T) {
	e, _ := world(t)
	root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
	p := writeFile(t, t.TempDir(), "a.txt", []byte("aaa"))
	e.as("carol", "attach", "proj", "--topic", "alpha", p).failsWith(t, ExitError, "M_FORBIDDEN")
	e.as("carol", "attach", "proj", "--topic", root, p).failsWith(t, ExitError, "M_FORBIDDEN")
	e.as("carol", "pin", "proj", root).failsWith(t, ExitError, "M_FORBIDDEN")
	e.as("carol", "unpin", "proj", root).failsWith(t, ExitError, "M_FORBIDDEN")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, uploadRoute, 0)
	e.as("alice", "attach", "proj", "--topic", "alpha", p).ok(t)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, uploadRoute, 1)
}

// Proves a file over m.upload.size is refused before any upload, while a file
// of exactly the limit is uploaded.
func TestAttachOversize(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	e.hs.mu.Lock()
	e.hs.uploadMax = 16
	e.hs.mu.Unlock()
	dir := t.TempDir()
	big := writeFile(t, dir, "big.txt", bytes.Repeat([]byte("x"), 17))
	e.as("alice", "attach", "proj", "--topic", "alpha", big).failsWith(t, ExitError, "over the server's upload limit of 16 bytes")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, uploadRoute, 0)
	ok := writeFile(t, dir, "ok.txt", bytes.Repeat([]byte("x"), 16))
	e.as("alice", "attach", "proj", "--topic", "alpha", ok).ok(t)
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, uploadRoute, 1)
}

// Proves empty files, directories, missing files and unsafe base names are
// refused before any network call; a one-byte file is the positive control.
func TestAttachBadFiles(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	dir := t.TempDir()
	before := len(e.hs.urls())
	empty := writeFile(t, dir, "empty.txt", nil)
	e.as("alice", "attach", "proj", "--topic", "alpha", empty).failsWith(t, ExitUsage, "empty file refused")
	e.as("alice", "attach", "proj", "--topic", "alpha", dir).failsWith(t, ExitUsage, "not a regular file")
	e.as("alice", "attach", "proj", "--topic", "alpha", filepath.Join(dir, "missing")).failsWith(t, ExitError, "no such file")
	dash := writeFile(t, dir, "-rf", []byte("x"))
	e.as("alice", "attach", "proj", "--topic", "alpha", "--", dash).failsWith(t, ExitUsage, "unsafe file name")
	e.as("alice", "attach", "proj", "--topic", "alpha").failsWith(t, ExitUsage, "usage")
	e.as("alice", "attach", "proj", empty).failsWith(t, ExitUsage, "--topic is required")
	if got := len(e.hs.urls()); got != before {
		t.Fatalf("requests sent for refused input: %d", got-before)
	}
	one := writeFile(t, dir, "one.txt", []byte("x"))
	e.as("alice", "attach", "proj", "--topic", "alpha", one).ok(t)
}

// Proves a download whose bytes do not match the event's sha256 is refused
// with "sha256 mismatch" and leaves nothing behind; the same fetch succeeds
// once the server serves the real bytes.
func TestFetchShaMismatch(t *testing.T) {
	e, id, data, _ := attachWorld(t)
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	mediaID := h.Attachment.MXC[strings.LastIndex(h.Attachment.MXC, "/")+1:]
	evil := bytes.Repeat([]byte("z"), len(data))
	e.hs.mu.Lock()
	e.hs.serveAs[mediaID] = evil
	e.hs.mu.Unlock()

	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	e.as("bob", "fetch", "proj", id, "-o", out).failsWith(t, ExitError, "sha256 mismatch")
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	// Bare mxc with a wrong --sha256 is refused the same way.
	e.hs.mu.Lock()
	delete(e.hs.serveAs, mediaID)
	e.hs.mu.Unlock()
	e.as("bob", "fetch", h.Attachment.MXC, "--sha256", sumHex(evil), "-o", out).failsWith(t, ExitError, "sha256 mismatch")
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}

	e.as("bob", "fetch", "proj", id, "-o", out).ok(t)
	got, _ := os.ReadFile(out)
	eq(t, string(got), string(data))
}

// Proves a server that sends more bytes than declared is cut off at the
// declared size and refused, leaving nothing behind; for a bare mxc the cap is
// m.upload.size. Honest serving is the control.
func TestFetchOversend(t *testing.T) {
	e, id, data, _ := attachWorld(t)
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	mxc := h.Attachment.MXC
	mediaID := mxc[strings.LastIndex(mxc, "/")+1:]
	e.hs.mu.Lock()
	e.hs.serveAs[mediaID] = append(append([]byte{}, data...), bytes.Repeat([]byte("!"), 1<<20)...)
	e.hs.mu.Unlock()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.txt")
	e.as("bob", "fetch", "proj", id, "-o", out).failsWith(t, ExitError, "more than 26 bytes")
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	e.hs.mu.Lock()
	e.hs.uploadMax = 100
	e.hs.mu.Unlock()
	e.as("bob", "fetch", mxc, "--sha256", h.Attachment.SHA256, "-o", out).failsWith(t, ExitError, "more than 100 bytes")
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Fatalf("left behind: %v", ents)
	}
	e.hs.mu.Lock()
	delete(e.hs.serveAs, mediaID)
	e.hs.mu.Unlock()
	e.as("bob", "fetch", "proj", id, "-o", out).ok(t)
}

// Proves attachment names that could escape or confuse the current directory
// are refused before any download; a safe name for the same media is the control.
func TestFetchUnsafeNames(t *testing.T) {
	e, id, data, _ := attachWorld(t)
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	dir := t.TempDir()
	t.Chdir(dir)
	inject := func(name string) string {
		at := *h.Attachment
		at.Name = name
		hh := h
		hh.Attachment = &at
		c := threadReply(hh, nil)
		c["msgtype"], c["body"], c["url"], c["filename"] = "m.file", name, at.MXC, name
		return e.hs.inject("!projRoomID", "@alice:test.local", c)
	}
	for _, name := range []string{"../x", "a/b", "..", ".", "-rf", "", `a\b`, "a\x00b", "a\nb"} {
		bad := inject(name)
		e.as("bob", "fetch", "proj", bad).failsWith(t, ExitError, "unsafe file name")
	}
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, downloadRoute, 0)
	if ents := dirEntries(t, dir); len(ents) != 0 {
		t.Fatalf("files written: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "x")); err == nil {
		t.Fatal("../x was written")
	}
	good := inject("safe-name.txt")
	e.as("bob", "fetch", "proj", good).ok(t)
	got, _ := os.ReadFile(filepath.Join(dir, "safe-name.txt"))
	eq(t, string(got), string(data))
}

// Proves fetch usage rules: a bare mxc needs --sha256, malformed mxc URIs and
// bad --sha256 values are usage errors with no request sent, and --sha256 is
// refused with an event id. A well-formed bare fetch is the control.
func TestFetchUsage(t *testing.T) {
	e, id, _, _ := attachWorld(t)
	ev, _ := e.hs.eventByID(id)
	h, _ := decodeHeader(ev.Message().A2A)
	sum := h.Attachment.SHA256
	before := len(e.hs.urls())
	e.as("bob", "fetch", h.Attachment.MXC).failsWith(t, ExitUsage, "needs --sha256")
	for _, bad := range []string{
		"mxc://test.local/../../_matrix/client/v3/account/whoami",
		"mxc://test.local/a/b",
		"mxc://../media1",
		"mxc://test.local/",
		"mxc:///media1",
		"mxc://test.local/id?x=1",
		"mxc://test.local/id%2F..",
		"mxc://user@test.local/id",
		"mxc:test.local/id",
	} {
		e.as("bob", "fetch", bad, "--sha256", sum).failsWith(t, ExitUsage, "invalid mxc URI")
	}
	e.as("bob", "fetch", h.Attachment.MXC, "--sha256", "abc").failsWith(t, ExitUsage, "invalid --sha256")
	e.as("bob", "fetch", "proj", id, "--sha256", sum).failsWith(t, ExitUsage, "only for a bare mxc")
	e.as("bob", "fetch", "proj").failsWith(t, ExitUsage, "usage")
	if got := len(e.hs.urls()); got != before {
		t.Fatalf("requests sent for usage errors: %v", e.hs.urls()[before:])
	}
	e.as("bob", "fetch", h.Attachment.MXC, "--sha256", sum, "-o", filepath.Join(t.TempDir(), "ok")).ok(t)
}

// Proves fetch never overwrites: -o onto an existing file, and a default name
// that already exists, are refused before downloading and leave the file
// untouched. A fresh -o path is the control.
func TestFetchNoOverwrite(t *testing.T) {
	e, id, data, _ := attachWorld(t)
	dir := t.TempDir()
	t.Chdir(dir)
	existing := writeFile(t, dir, "keep.txt", []byte("precious"))
	e.as("bob", "fetch", "proj", id, "-o", existing).failsWith(t, ExitError, "already exists")
	writeFile(t, dir, "notes.txt", []byte("mine"))
	e.as("bob", "fetch", "proj", id).failsWith(t, ExitError, "already exists")
	tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, downloadRoute, 0)
	got, _ := os.ReadFile(existing)
	eq(t, string(got), "precious")
	got, _ = os.ReadFile(filepath.Join(dir, "notes.txt"))
	eq(t, string(got), "mine")
	e.as("bob", "fetch", "proj", id, "-o", "fresh.txt").ok(t)
	got, _ = os.ReadFile(filepath.Join(dir, "fresh.txt"))
	eq(t, string(got), string(data))
}

// Proves files lists only attachment messages, oldest first, honours --last
// and --topic, and that fetch refuses an event without an attachment.
func TestFilesListing(t *testing.T) {
	e, first, _, _ := attachWorld(t)
	e.as("bob", "topic", "new", "proj", "beta", "B").ok(t)
	e.as("bob", "post", "proj", "--topic", "alpha", "plain text").ok(t)
	p := writeFile(t, t.TempDir(), "second.txt", []byte("2"))
	second := strings.TrimSpace(e.as("bob", "attach", "proj", "--topic", "beta", p).ok(t).stdout)

	lines := tuitestkit.LinesFromStr(strings.TrimSpace(e.as("alice", "files", "proj").ok(t).stdout))
	if len(lines) != 2 || !strings.HasPrefix(lines[0], first+"\t") || !strings.HasPrefix(lines[1], second+"\tbob\tsecond.txt\t1\t") {
		t.Fatalf("files = %q", lines)
	}
	last := e.as("alice", "files", "proj", "--last", "1").ok(t).stdout
	tuitestkit.ContainsStr(t, last, second)
	tuitestkit.NotContainsStr(t, last, first)
	alpha := e.as("alice", "files", "proj", "--topic", "alpha").ok(t).stdout
	tuitestkit.ContainsStr(t, alpha, first)
	tuitestkit.NotContainsStr(t, alpha, second)
	e.as("alice", "files", "proj", "--last", "0").failsWith(t, ExitUsage, "--last")

	plain := strings.TrimSpace(e.as("bob", "post", "proj", "--topic", "alpha", "no file").ok(t).stdout)
	e.as("alice", "fetch", "proj", plain, "-o", filepath.Join(t.TempDir(), "x")).failsWith(t, ExitError, "no a2a attachment")
}

func eq(t *testing.T, got, want string) {
	t.Helper()
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
