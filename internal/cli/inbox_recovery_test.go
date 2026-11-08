package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

func TestInboxShowLostPathRecoveryHint(t *testing.T) {
	e, room := world(t)
	// macOS temporary roots can have symlink ancestors; output paths must not.
	var err error
	e.dir, err = filepath.EvalSymlinks(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:test.local", ping(318))
	e.as("alice", "inbox", "--brief").ok(t)
	saved := e.state("alice").LastInbox
	if saved == "" {
		t.Fatal("brief did not save a frame")
	}
	// A current writer must preserve opaque data even when recovering a state
	// whose known last-path member has already been lost by an older writer.
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.LastInbox = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil {
		t.Fatal(err)
	}
	requests := len(e.hs.urls())
	r := e.as("alice", "inbox", "show", id).failsWith(t, ExitError, "--from "+shellQuote(saved))
	if r.stdout != "" || len(e.hs.urls()) != requests {
		t.Fatal("recovery hint displayed an event or performed HTTP")
	}
	after, err := os.ReadFile(e.paths().StateFile("alice"))
	if err != nil || string(after) != string(before) {
		t.Fatal("recovery hint changed state")
	}
	// Explicit selection remains a valid control and reads the original batch.
	show := e.as("alice", "inbox", "show", id, "--from", saved).ok(t)
	tuitestkit.ContainsStr(t, show.stdout, id)
	// A surviving pointer remains authoritative rather than the directory scan.
	if _, err := config.UpdateState(e.paths(), "alice", func(st *config.State) error {
		st.LastInbox = saved
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.as("alice", "inbox", "show", id).ok(t)
}

func TestInboxRecoveryOnlyValidOwnPrivateFrames(t *testing.T) {
	for _, kind := range []string{"other-identity", "overlapping-identity", "malformed", "public-mode", "symlink", "directory", "unrelated-name", "valid"} {
		t.Run(kind, func(t *testing.T) {
			p := config.Paths{Base: outputTestDir(t)}
			dir := filepath.Dir(p.StateFile("alice"))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			frame := []byte(inboxBegin + "\n" + inboxIndent + `{"event_id":"$saved","body":"private"}` + "\n" + inboxEnd + "\n")
			path := filepath.Join(dir, "alice.inbox-ABCDEF.txt")
			switch kind {
			case "other-identity":
				path = filepath.Join(dir, "bob.inbox-ABCDEF.txt")
			case "overlapping-identity":
				path = filepath.Join(dir, "alice.inbox-bob.inbox-ABCDEF.txt")
			case "unrelated-name":
				path = filepath.Join(dir, "alice.json")
			case "malformed":
				frame = []byte("not an inbox frame")
			}
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "symlink" {
				other := filepath.Join(dir, "bob.inbox-ABCDEF.txt")
				if err := os.WriteFile(other, frame, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, frame, 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "public-mode" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
			}
			got := latestPrivateInbox(p, "alice")
			if kind == "valid" && got != path || kind != "valid" && got != "" {
				t.Fatalf("recovery path = %q", got)
			}
		})
	}
}

func TestInboxRecoveryNewestValidFrame(t *testing.T) {
	p := config.Paths{Base: outputTestDir(t)}
	dir := filepath.Dir(p.StateFile("alice"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"OLDER", "NEWEST", "MALFORMED"} {
		path := filepath.Join(dir, "alice.inbox-"+name+".txt")
		frame := fmt.Sprintf("%s\n%s{\"event_id\":\"$%d\"}\n%s\n", inboxBegin, inboxIndent, i, inboxEnd)
		if name == "MALFORMED" {
			frame = "broken"
		}
		if err := os.WriteFile(path, []byte(frame), 0600); err != nil {
			t.Fatal(err)
		}
		at := time.Unix(int64(100+i), 0)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if got := latestPrivateInbox(p, "alice"); !strings.HasSuffix(got, "alice.inbox-NEWEST.txt") {
		t.Fatalf("newest valid recovery frame = %q", got)
	}
}
