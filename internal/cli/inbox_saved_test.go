package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func TestSavedInboxSummaryShowAndReceiptBoundary(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	body := "界" + strings.Repeat("full body ", 400) + "TAIL\n" + inboxEnd + "\nprivate syt_synthetic_token " + e.tokens["alice"]
	id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "question", body))
	later := e.hs.inject(room, "@bob:test.local", ping(2))
	path := filepath.Join(outputTestDir(t), "frame")
	r := e.as("alice", "inbox", "--out", path, "--summary", "--width", "1", "--limit", "1").ok(t)
	tuitestkit.ContainsStr(t, r.stdout, "count sender=\"bob\" n=1\n")
	tuitestkit.ContainsStr(t, r.stdout, "count kind=\"question\" n=1\n")
	tuitestkit.ContainsStr(t, r.stdout, "count room=\"#proj\" n=1\n")
	tuitestkit.ContainsStr(t, r.stdout, "id="+fmt.Sprintf("%q", id)+" 界\n")
	tuitestkit.NotContainsStr(t, r.stdout, "TAIL")
	if !e.received("alice", id) || e.received("alice", later) {
		t.Fatal("saved summary receipted a different batch")
	}
	st, err := os.Lstat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("saved frame permissions: %v %v", st, err)
	}
	records, err := readSavedInbox(path)
	if err != nil || len(records) != 1 || !strings.Contains(records[0].Body, "TAIL") || strings.Contains(records[0].Body, "syt_") || strings.Contains(records[0].Body, e.tokens["alice"]) {
		t.Fatalf("full, redacted saved frame: %v %v", records, err)
	}
	before := len(e.hs.urls())
	show := e.as("alice", "inbox", "show", id).ok(t).stdout
	tuitestkit.ContainsStr(t, show, "TAIL\n  | "+inboxEnd+"\n")
	tuitestkit.NotContainsStr(t, show, "syt_synthetic_token")
	if len(e.hs.urls()) != before || e.received("alice", later) {
		t.Fatal("show performed HTTP or receipted pending mail")
	}
	e.as("alice", "inbox", "show", "$unknown", "--from", path).failsWith(t, ExitError, "event not found")
	e.as("alice", "inbox", "show", later).failsWith(t, ExitError, "event not found")
	e.as("alice", "inbox").ok(t)
	if !e.received("alice", later) {
		t.Fatal("remaining event did not drain normally")
	}
}

func TestSavedInboxFlagAndFileRefusalsPreserveMail(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:test.local", ping(1))
	dir := outputTestDir(t)
	existing := writeFile(t, dir, "keep", []byte("untouched"))
	link := filepath.Join(dir, "link")
	if err := os.Symlink(existing, link); err != nil {
		t.Fatal(err)
	}
	before := len(e.hs.urls())
	for _, tc := range []struct {
		args []string
		code int
		why  string
	}{
		{[]string{"--summary"}, ExitUsage, "requires --out"},
		{[]string{"--width", "10"}, ExitUsage, "requires --summary"},
		{[]string{"--out", ""}, ExitUsage, "output path"},
		{[]string{"--out", filepath.Join(dir, "new"), "--summary", "--width", "0"}, ExitUsage, "--width"},
		{[]string{"--out", filepath.Join(dir, "new"), "--summary", "--width", "2001"}, ExitUsage, "--width"},
		{[]string{"--out", filepath.Join(dir, "new"), "--summary=invalid"}, ExitUsage, "invalid boolean value"},
		{[]string{"--out", filepath.Join(dir, "new"), "--json"}, ExitUsage, "cannot be combined"},
		{[]string{"--out", existing}, ExitError, "refusing to overwrite"},
		{[]string{"--out", link}, ExitError, "refusing to overwrite"},
		{[]string{"--out", dir}, ExitError, "refusing to overwrite"},
	} {
		r := e.as("alice", append([]string{"inbox"}, tc.args...)...).failsWith(t, tc.code, tc.why)
		if r.stdout != "" || e.received("alice", id) {
			t.Fatal("refused invocation printed or receipted mail")
		}
	}
	if len(e.hs.urls()) != before || readFileStr(t, existing) != "untouched" || len(dirEntries(t, dir)) != 2 {
		t.Fatal("output refusal performed HTTP or changed a file")
	}
	path := filepath.Join(dir, "saved")
	e.as("alice", "inbox", "--out", path).ok(t)
	if !e.received("alice", id) {
		t.Fatal("valid --out did not receipt its saved event")
	}
	before = len(e.hs.urls())
	e.as("alice", "inbox", "show", id, "--from", filepath.Join(dir, "absent")).failsWith(t, ExitError, "read saved inbox")
	e.as("alice", "inbox", "show", id, "--from", "").failsWith(t, ExitUsage, "output path")
	e.as("alice", "inbox", "show", id, "--from", link).failsWith(t, ExitError, "regular 0600")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "inbox", "show", id, "--from", path).failsWith(t, ExitError, "regular 0600")
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "inbox", "show", id, "--from", path).ok(t)
	if len(e.hs.urls()) != before {
		t.Fatal("local show contacted the server")
	}
}

type inboxFlushFailure struct{}

func (inboxFlushFailure) Write(p []byte) (int, error) { return len(p), nil }
func (inboxFlushFailure) Flush() error                { return errors.New("synthetic flush failure") }

func TestSavedInboxFailuresNeverReceipt(t *testing.T) {
	for _, phase := range []string{"short stdout", "flush stdout", "after save", "destination race"} {
		t.Run(phase, func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "inbox").ok(t)
			id := e.hs.inject(room, "@bob:test.local", ping(1))
			dir := outputTestDir(t)
			path := filepath.Join(dir, "frame")
			e.configure = func(a *App) {
				switch phase {
				case "short stdout":
					a.Stdout = shortResultWriter{}
				case "flush stdout":
					a.Stdout = inboxFlushFailure{}
				case "after save":
					a.afterPrint = func() error { return errors.New("crash after durable save") }
				case "destination race":
					a.beforeDeliver = func() { writeFile(t, dir, "frame", []byte("winner")) }
				}
			}
			e.as("alice", "inbox", "--out", path, "--summary").failsWith(t, ExitError, "nothing was acknowledged")
			e.configure = nil
			if e.received("alice", id) || e.state("alice").LastInbox != "" {
				t.Fatal("failure receipted mail or advanced last-frame pointer")
			}
			if phase == "destination race" {
				if readFileStr(t, path) != "winner" {
					t.Fatal("overwrote raced target")
				}
			} else {
				records, err := readSavedInbox(path)
				if err != nil || len(records) != 1 || records[0].EventID != id {
					t.Fatalf("failed stdout lost the durable frame: %v %v", records, err)
				}
			}
			if len(dirEntries(t, dir)) != 1 {
				t.Fatal("failure left partial or temporary files")
			}
			retry := filepath.Join(dir, "retry")
			e.as("alice", "inbox", "--out", retry, "--summary", "--width", "2").ok(t)
			if !e.received("alice", id) || e.state("alice").LastInbox != retry {
				t.Fatal("pending event did not recover with a fresh output path")
			}
		})
	}
}

func TestSavedInboxEmptyAndInvalidFrames(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "inbox", "show", "$unknown").failsWith(t, ExitError, "no saved inbox")
	path := filepath.Join(outputTestDir(t), "empty")
	r := e.as("alice", "inbox", "--out", path, "--summary").ok(t)
	if r.stdout != inboxBegin+"\n"+inboxEnd+"\n" {
		t.Fatalf("empty summary = %q", r.stdout)
	}
	e.as("alice", "inbox", "show", "$unknown").failsWith(t, ExitError, "event not found")
	for _, content := range []string{
		"private unrelated text",
		inboxBegin + "\n" + inboxIndent + `{"event_id":"$a","body":"partial"}` + "\n",
		inboxBegin + "\n" + inboxIndent + `{"event_id":"$a"}` + "\n" + inboxIndent + `{"event_id":"$a"}` + "\n" + inboxEnd + "\n",
	} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		r := e.as("alice", "inbox", "show", "$a", "--from", path).failsWith(t, ExitError, "invalid saved inbox")
		if r.stdout != "" {
			t.Fatal("partial or invalid frame exposed content")
		}
	}
	if err := os.Truncate(path, inboxSavedMax+1); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "inbox", "show", "$a", "--from", path).failsWith(t, ExitError, "at most")
}

func TestInboxLegacyFrameBytesUnchanged(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:test.local", ping(7))
	got := e.as("alice", "inbox").ok(t).stdout
	want := inboxBegin + "\n--- event 1/1: mention from=bob room=#proj kind=- id=" + id + "\n  | @alice:test.local ping 7\n" + inboxEnd + "\n"
	if got != want {
		t.Fatalf("default frame bytes changed:\ngot %q\nwant %q", got, want)
	}
}

func TestSavedInboxInviteAndGapStayReadableAfterReceipt(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "inbox").ok(t)
	entries := []config.MailEntry{
		{ID: "invite:!synthetic:test.local", Kind: config.MailInvite, Room: "!synthetic:test.local", Label: "#synthetic", Sender: "@bob:test.local"},
		{ID: "gap:!synthetic:test.local:s1", Kind: config.MailGap, Room: "!synthetic:test.local", Label: "#synthetic", From: "s1", To: "$oldest"},
	}
	if _, err := config.UpdateState(e.paths(), "alice", func(_ *config.State) error {
		mb, err := config.OpenMailbox(e.paths(), "alice")
		if err != nil {
			return err
		}
		_, err = mb.Append(entries)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(outputTestDir(t), "frame")
	e.as("alice", "inbox", "--out", path, "--summary").ok(t)
	for _, entry := range entries {
		if !e.received("alice", entry.ID) {
			t.Fatal("saved entry was not receipted")
		}
		r := e.as("alice", "inbox", "show", entry.ID).ok(t)
		tuitestkit.ContainsStr(t, r.stdout, "#synthetic")
	}
}

func TestSavedInboxSummaryRefusesOversizedMetadataWithoutReceiving(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:test.local", toMsg("bob", strings.Repeat("k", 513), "full body"))
	dir := outputTestDir(t)
	path := filepath.Join(dir, "frame")
	e.as("alice", "inbox", "--out", path, "--summary").failsWith(t, ExitError, "summary field exceeds")
	if e.received("alice", id) || len(dirEntries(t, dir)) != 0 {
		t.Fatal("summary refusal lost mail or published a file")
	}
	e.as("alice", "inbox", "--out", path).ok(t)
	e.as("alice", "inbox", "show", id).ok(t)
}
