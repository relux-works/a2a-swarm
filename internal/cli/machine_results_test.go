package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Canonicalize the test-owned directory on macOS too, where /var is a symlink.
// Production deliberately refuses symlink ancestry for new output paths.
func outputTestDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMachinePublicationResults(t *testing.T) {
	for _, command := range []string{"topic", "post", "attach"} {
		for _, mode := range []string{"json", "id-file", "both", "default"} {
			t.Run(command+"/"+mode, func(t *testing.T) {
				e, room := world(t)
				root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t).stdout)
				reply := strings.TrimSpace(e.as("alice", "post", "proj", "--topic", "alpha", "earlier").ok(t).stdout)
				var args []string
				switch command {
				case "topic":
					args = []string{"topic", "new", "proj", "beta", "B"}
				case "post":
					args = []string{"post", "proj", "--topic", "alpha", "--re", reply, "later"}
				case "attach":
					path := writeFile(t, t.TempDir(), "notes.txt", []byte("synthetic attachment\n"))
					args = []string{"attach", "proj", "--topic", "alpha", path}
				}
				dir := outputTestDir(t)
				idPath := filepath.Join(dir, "event-id")
				asJSON := mode == "json" || mode == "both"
				idFile := mode == "id-file" || mode == "both"
				if asJSON {
					args = append(args, "--json")
				}
				if idFile {
					args = append(args, "--id-file", idPath)
				}
				r := e.as("alice", args...).ok(t)
				var result PublishResult
				if asJSON {
					if err := json.Unmarshal([]byte(r.stdout), &result); err != nil {
						t.Fatal(err)
					}
				} else {
					result.EventID = strings.TrimSuffix(r.stdout, "\n")
					if r.stdout != result.EventID+"\n" || !strings.HasPrefix(result.EventID, "$") {
						t.Fatalf("legacy bytes changed: %q", r.stdout)
					}
				}
				ev, ok := e.hs.eventByID(result.EventID)
				if !ok {
					t.Fatal("result does not identify the sent event")
				}
				if asJSON {
					wantRoot, wantReply := root, root
					if command == "topic" {
						wantRoot, wantReply = ev.EventID, ""
					} else if command == "post" {
						wantReply = reply
					}
					want := fmt.Sprintf("{\"event_id\":%q,\"room_id\":%q,\"topic_root\":%q,\"reply_to\":%q}\n", ev.EventID, room, wantRoot, wantReply)
					if r.stdout != want {
						t.Fatalf("stable result = %q, want %q", r.stdout, want)
					}
				}
				if idFile {
					if got := readFileStr(t, idPath); got != ev.EventID+"\n" {
						t.Fatalf("id file = %q", got)
					}
					st, err := os.Lstat(idPath)
					if err != nil || st.Mode().Perm() != 0o600 || len(dirEntries(t, dir)) != 1 {
						t.Fatalf("private id publication: %v %v", st, err)
					}
				}
			})
		}
	}
}

func TestMachinePublicationRefusals(t *testing.T) {
	for _, command := range []string{"topic", "post", "attach"} {
		t.Run(command, func(t *testing.T) {
			e, _ := world(t)
			e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
			path := writeFile(t, t.TempDir(), "notes.txt", []byte("synthetic\n"))
			args := map[string][]string{
				"topic":  {"topic", "new", "proj", "beta", "B"},
				"post":   {"post", "proj", "--topic", "alpha", "text"},
				"attach": {"attach", "proj", "--topic", "alpha", path},
			}[command]
			dir := outputTestDir(t)
			existing := writeFile(t, dir, "keep", []byte("unchanged"))
			link := filepath.Join(dir, "link")
			if err := os.Symlink(existing, link); err != nil {
				t.Fatal(err)
			}
			before := len(e.hs.urls())
			for _, refused := range []struct {
				flags []string
				code  int
				why   string
			}{
				{[]string{"--json=invalid"}, ExitUsage, "invalid boolean value"},
				{[]string{"--id-file", ""}, ExitUsage, "output path"},
				{[]string{"--id-file", existing}, ExitError, "refusing to overwrite"},
				{[]string{"--json", "--id-file", link}, ExitError, "refusing to overwrite"},
				{[]string{"--id-file", filepath.Join(dir, "missing", "out")}, ExitError, "output parent"},
			} {
				invocation := append(append([]string{}, args...), refused.flags...)
				r := e.as("alice", invocation...).failsWith(t, refused.code, refused.why)
				if r.stdout != "" {
					t.Fatal("refusal printed a success result")
				}
			}
			if len(e.hs.urls()) != before || readFileStr(t, existing) != "unchanged" || len(dirEntries(t, dir)) != 2 {
				t.Fatal("refused output touched the server or destination")
			}
		})
	}
	// JSON does not turn a failed publication into a result, nor leave an id file.
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	dir := outputTestDir(t)
	for _, args := range [][]string{
		{"topic", "new", "proj", "alpha", "duplicate"},
		{"post", "proj", "--topic", "$unknown", "text"},
		{"attach", "proj", "--topic", "alpha", filepath.Join(dir, "missing")},
	} {
		before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
		r := e.as("alice", append(args, "--json", "--id-file", filepath.Join(dir, "id"))...)
		if r.code == ExitOK || r.stdout != "" || e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != before || len(dirEntries(t, dir)) != 0 {
			t.Fatalf("failed publication emitted a result or sent: %+v", r)
		}
	}
}

type shortResultWriter struct{}

func (shortResultWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestMachinePublicationBrokenOutputDoesNotRepost(t *testing.T) {
	e, _ := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
	idPath := filepath.Join(outputTestDir(t), "id")
	e.configure = func(a *App) { a.Stdout = shortResultWriter{} }
	r := e.as("alice", "post", "proj", "--topic", "alpha", "text", "--json", "--id-file", idPath).failsWith(t, ExitError, io.ErrShortWrite.Error())
	tuitestkit.ContainsStr(t, r.stderr, "do not repost")
	if e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}") != before+1 || !strings.HasPrefix(readFileStr(t, idPath), "$") {
		t.Fatal("lost successful receipt or retried publication")
	}
}

func TestMachinePublicationOutputFailurePreservesRequestedPin(t *testing.T) {
	e, room := world(t)
	e.as("alice", "topic", "new", "proj", "alpha", "A").ok(t)
	path := writeFile(t, t.TempDir(), "notes.txt", []byte("synthetic\n"))
	idPath := filepath.Join(outputTestDir(t), "id")
	e.configure = func(a *App) { a.Stdout = shortResultWriter{} }
	e.as("alice", "attach", "proj", "--topic", "alpha", path, "--pin", "--json", "--id-file", idPath).failsWith(t, ExitError, "do not repost")
	id := strings.TrimSpace(readFileStr(t, idPath))
	if got := e.hs.pinned(room); len(got) != 1 || got[0] != id {
		t.Fatal("result formatting changed the requested pin")
	}
}

func TestPrivateOutputAtomicNoClobberAndPinnedParent(t *testing.T) {
	dir := outputTestDir(t)
	target := filepath.Join(dir, "result")
	p, err := preparePrivateOutput(target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(target); !os.IsNotExist(err) {
		t.Fatal("published an empty reservation")
	}
	writeFile(t, dir, "result", []byte("winner")) // race after preflight
	if err := p.commit([]byte("loser")); err == nil {
		t.Fatal("overwrote a raced destination")
	}
	p.close()
	if readFileStr(t, target) != "winner" || len(dirEntries(t, dir)) != 1 {
		t.Fatal("race lost data or left a temporary file")
	}
	parent := filepath.Join(dir, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	p, err = preparePrivateOutput(filepath.Join(parent, "saved"))
	if err != nil {
		t.Fatal(err)
	}
	defer p.close()
	moved := filepath.Join(dir, "moved")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	foreign := outputTestDir(t)
	if err := os.Symlink(foreign, parent); err != nil {
		t.Fatal(err)
	}
	if err := p.commit([]byte("complete")); err != nil {
		t.Fatal(err)
	}
	if readFileStr(t, filepath.Join(moved, "saved")) != "complete" || len(dirEntries(t, foreign)) != 0 {
		t.Fatal("a renamed parent redirected publication")
	}
	if _, err := preparePrivateOutput(filepath.Join(parent, "another")); err == nil {
		t.Fatal("accepted symlink ancestry")
	}
}
