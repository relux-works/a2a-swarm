package cli

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func TestBatchFetchVerifiedFilesAndStableLines(t *testing.T) {
	e, first, data, _ := attachWorld(t)
	secondData := []byte("opaque archive-looking bytes; never unpacked\n")
	input := writeFile(t, t.TempDir(), "packet.tar.gz", secondData)
	second := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", input).ok(t).stdout)
	dir := filepath.Join(outputTestDir(t), "fresh")
	r := e.as("bob", "fetch", "proj", first, second, "-o", dir).ok(t)
	want := fmt.Sprintf("%s\tnotes.txt\t%s\t%d\tok\n%s\tpacket.tar.gz\t%s\t%d\tok\n", first, sumHex(data), len(data), second, sumHex(secondData), len(secondData))
	if r.stdout != want {
		t.Fatalf("batch result bytes = %q, want %q", r.stdout, want)
	}
	for name, want := range map[string][]byte{"notes.txt": data, "packet.tar.gz": secondData} {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		st, serr := os.Stat(path)
		if err != nil || serr != nil || !bytes.Equal(got, want) || st.Mode().Perm() != 0o600 {
			t.Fatalf("verified private file %s: %v %v", name, err, serr)
		}
	}
	st, err := os.Stat(dir)
	if err != nil || st.Mode().Perm() != 0o700 || len(dirEntries(t, dir)) != 2 {
		t.Fatal("batch left temporary files or widened directory permissions")
	}
}

func TestBatchFetchUnknownAndChecksumFailureLeaveNoPartialFile(t *testing.T) {
	e, id, data, _ := attachWorld(t)
	att := attachmentFields(t, e, id)
	mediaID, _ := attachmentMedia(t, e, att)
	e.hs.mu.Lock()
	e.hs.serveAs[mediaID] = bytes.Repeat([]byte("x"), len(data))
	e.hs.mu.Unlock()
	dir := filepath.Join(outputTestDir(t), "bad")
	r := e.as("bob", "fetch", "proj", id, "$unknown", "-o", dir).failsWith(t, ExitError, "2 attachment(s) failed")
	tuitestkit.ContainsStr(t, r.stdout, id+"\tnotes.txt\t-\t0\tsha256_mismatch\n")
	tuitestkit.ContainsStr(t, r.stdout, "$unknown\t-\t-\t0\tevent_not_found\n")
	if len(dirEntries(t, dir)) != 0 {
		t.Fatal("failed batch left a partial file")
	}
	// The same bytes succeed after repair; other per-event failures do not
	// roll back a verified file or turn the whole batch into a success.
	e.hs.mu.Lock()
	delete(e.hs.serveAs, mediaID)
	e.hs.mu.Unlock()
	dir = filepath.Join(outputTestDir(t), "repaired")
	r = e.as("bob", "fetch", "proj", "$unknown", id, "-o", dir).failsWith(t, ExitError, "1 attachment(s) failed")
	tuitestkit.ContainsStr(t, r.stdout, "\tok\n")
	if readFileStr(t, filepath.Join(dir, "notes.txt")) != string(data) || len(dirEntries(t, dir)) != 1 {
		t.Fatal("mixed batch lost its successful file")
	}
}

func TestBatchFetchEncodingAndOriginalHashGate(t *testing.T) {
	e, att, original, _ := encodingFixture(t)
	good := injectFileAttachment(e, att)
	input := writeFile(t, t.TempDir(), "other.txt", []byte("other\n"))
	other := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", input).ok(t).stdout)
	dir := filepath.Join(outputTestDir(t), "decoded")
	r := e.as("bob", "fetch", "proj", good, other, "-o", dir).ok(t)
	tuitestkit.ContainsStr(t, r.stdout, fmt.Sprintf("%s\treport.json\t%s\t%d\tok\n", good, sumHex(original), len(original)))
	if readFileStr(t, filepath.Join(dir, "report.json")) != string(original) || len(dirEntries(t, dir)) != 2 {
		t.Fatal("batch changed decoded bytes or retained encoded temporary files")
	}
	originalFields(att)["sha256"] = strings.Repeat("0", 64)
	bad := injectFileAttachment(e, att)
	dir = filepath.Join(outputTestDir(t), "mismatch")
	r = e.as("bob", "fetch", "proj", bad, "$unknown", "-o", dir).failsWith(t, ExitError, "2 attachment(s) failed")
	tuitestkit.ContainsStr(t, r.stdout, "sha256_mismatch\n")
	if len(dirEntries(t, dir)) != 0 {
		t.Fatal("original checksum refusal left stored or decoded bytes")
	}
}

func TestBatchFetchPathRefusalsAndNameCollisions(t *testing.T) {
	e, first, data, _ := attachWorld(t)
	input := writeFile(t, t.TempDir(), "notes.txt", []byte("different bytes\n"))
	second := strings.TrimSpace(e.as("alice", "attach", "proj", "--topic", "alpha", input).ok(t).stdout)
	parent := outputTestDir(t)
	existing := filepath.Join(parent, "exists")
	if err := os.Mkdir(existing, 0o700); err != nil {
		t.Fatal(err)
	}
	keep := writeFile(t, existing, "keep", []byte("unchanged"))
	link := filepath.Join(parent, "link")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	before := len(e.hs.urls())
	for _, path := range []string{existing, link, filepath.Join(link, "fresh")} {
		r := e.as("bob", "fetch", "proj", first, second, "-o", path)
		if r.code != ExitError || r.stdout != "" {
			t.Fatalf("unsafe batch destination accepted: %+v", r)
		}
	}
	if len(e.hs.urls()) != before || readFileStr(t, keep) != "unchanged" || len(dirEntries(t, existing)) != 1 {
		t.Fatal("destination refusal changed files or sent HTTP")
	}
	dir := filepath.Join(parent, "collision")
	beforeDownloads := e.hs.CallCount(downloadRoute)
	r := e.as("bob", "fetch", "proj", first, second, "-o", dir).failsWith(t, ExitError, "1 attachment(s) failed")
	tuitestkit.ContainsStr(t, r.stdout, second+"\tnotes.txt\t-\t0\toutput_error\n")
	if readFileStr(t, filepath.Join(dir, "notes.txt")) != string(data) || len(dirEntries(t, dir)) != 1 || e.hs.CallCount(downloadRoute) != beforeDownloads+1 {
		t.Fatal("duplicate attachment name overwrote or downloaded a second file")
	}
	att := attachmentFields(t, e, first)
	att["name"] = "../escape"
	unsafe := injectFileAttachment(e, att)
	dir = filepath.Join(parent, "unsafe")
	r = e.as("bob", "fetch", "proj", unsafe, "$unknown", "-o", dir).failsWith(t, ExitError, "2 attachment(s) failed")
	tuitestkit.ContainsStr(t, r.stdout, "unsafe_name\n")
	if len(dirEntries(t, dir)) != 0 {
		t.Fatal("unsafe name created a file")
	}
}

func TestBatchFetchSelectionRefusals(t *testing.T) {
	e, _ := world(t)
	dir := filepath.Join(outputTestDir(t), "fresh")
	before := len(e.hs.urls())
	for _, args := range [][]string{
		{"proj", "$one", "$two"},
		{"proj", "$one", "$one", "-o", dir},
		{"proj", "$one", "not-an-event", "-o", dir},
		{"proj", "$one", "$two", "-o", dir, "--sha256", strings.Repeat("0", 64)},
		append(append([]string{"proj"}, numberedEvents(51)...), "-o", dir),
	} {
		r := e.as("bob", append([]string{"fetch"}, args...)...).failsWith(t, ExitUsage, "usage: a2a fetch")
		if r.stdout != "" {
			t.Fatal("selection refusal printed a result")
		}
	}
	if len(e.hs.urls()) != before {
		t.Fatal("invalid batch sent HTTP")
	}
	if _, err := os.Lstat(dir); !os.IsNotExist(err) {
		t.Fatal("invalid batch created a directory")
	}
}

func numberedEvents(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("$synthetic%d", i)
	}
	return ids
}
