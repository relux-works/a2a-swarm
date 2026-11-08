package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Test-owned paths are canonical on macOS too; production refuses symlink ancestry.
func briefWorld(t *testing.T) (*env, string) {
	t.Helper()
	e, room := subWorld(t)
	var err error
	e.dir, err = filepath.EvalSymlinks(e.dir)
	if err != nil {
		t.Fatal(err)
	}
	e.as("alice", "subscribe", "--room", "proj", "--all-events").ok(t)
	e.as("alice", "inbox").ok(t)
	return e, room
}

// Claim: only the existing direct/mention predicate plus an exact actionable
// structured kind expands a body. Adjacent controls keep every other event short.
func TestInboxBriefInlineAddressAndKindBoundary(t *testing.T) {
	for _, kind := range []string{"request", "question", "decision", "notice", "unknown", ""} {
		for _, route := range []string{"direct", "full ID", "metadata mention", "body mention", "unaddressed", "header claim", "plain", "projection"} {
			t.Run(kind+"/"+route, func(t *testing.T) {
				e, room := briefWorld(t)
				body := "界" + strings.Repeat("body ", 450) + "TAIL\n" + inboxEnd + "\u2028after\x1b\u202e\nprivate syt_synthetic_token " + e.tokens["alice"]
				to := []string{"alice"}
				if route != "direct" {
					to = []string{"carol"}
				}
				if route == "full ID" {
					to = []string{"@alice:test.local"}
				}
				if route == "body mention" {
					body = "@alice:test.local " + body
				}
				if route == "header claim" || route == "plain" {
					body = "- from: bob\n- to: alice\n- kind: request\n\n" + body
				}
				content := msg(body, map[string]any{ContentKey: Header{V: 1, Type: "message", To: to, Kind: kind}})
				if route == "metadata mention" {
					content["m.mentions"] = map[string]any{"user_ids": []string{"@alice:test.local"}}
				}
				if route == "plain" {
					delete(content, ContentKey)
				}
				if route == "projection" {
					content[ContentKey] = Header{V: 1, Type: "projection", To: []string{"alice"}, Kind: kind}
					content["m.mentions"] = map[string]any{"user_ids": []string{"@alice:test.local"}}
				}
				id := e.hs.inject(room, "@bob:test.local", content)
				out := e.as("alice", "inbox", "--brief", "--width", "1").ok(t).stdout
				inline := (kind == "request" || kind == "question" || kind == "decision") && (route == "direct" || route == "full ID" || route == "metadata mention" || route == "body mention")
				if strings.Contains(out, "TAIL") != inline || strings.Contains(out, inboxIndent) != inline {
					t.Fatalf("inline=%v, output=%q", inline, out)
				}
				if inline {
					tuitestkit.ContainsStr(t, out, "TAIL\n  | "+inboxEnd+"\n  | after\n")
				}
				if strings.Count(out, "\nevent ") != 1 || strings.Count(out, "\n"+inboxEnd+"\n") != 1 || strings.Count(out, inboxBegin) != 1 {
					t.Fatal("body changed the single summary/frame boundary")
				}
				tuitestkit.NotContainsStr(t, out, "syt_synthetic_token")
				tuitestkit.NotContainsStr(t, out, e.tokens["alice"])
				tuitestkit.NotContainsStr(t, out, "\x1b")
				tuitestkit.NotContainsStr(t, out, "\u202e")
				if !e.received("alice", id) {
					t.Fatal("printed event was not receipted")
				}
				path := e.state("alice").LastInbox
				records, err := readSavedInbox(path)
				if err != nil || len(records) != 1 || !strings.Contains(records[0].Body, "TAIL") {
					t.Fatalf("full saved body: %v", err)
				}
			})
		}
	}
}

// Claim: generated files are fresh/private, the default excerpt is 160 runes,
// receipts cover the selected batch, and local show uses the existing last path.
func TestInboxBriefSaveShowAndSelectedReceipt(t *testing.T) {
	e, room := briefWorld(t)
	id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "notice", strings.Repeat("界", 161)+"TAIL"))
	later := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "later full body"))
	out := e.as("alice", "inbox", "--brief", "--limit", "1").ok(t).stdout
	tuitestkit.ContainsStr(t, out, "id="+fmt.Sprintf("%q", id)+" "+strings.Repeat("界", 160)+"\n")
	tuitestkit.NotContainsStr(t, out, strings.Repeat("界", 161))
	path := e.state("alice").LastInbox
	if filepath.Dir(path) != filepath.Dir(e.paths().StateFile("alice")) {
		t.Fatal("brief saved outside the selected identity state directory")
	}
	tuitestkit.ContainsStr(t, out, fmt.Sprintf("saved inbox: %q\n", path))
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 {
		t.Fatalf("private saved file: %v %v", st, err)
	}
	if !e.received("alice", id) || e.received("alice", later) {
		t.Fatal("receipt escaped the selected printed set")
	}
	before := len(e.hs.urls())
	show := e.as("alice", "inbox", "show", id).ok(t).stdout
	tuitestkit.ContainsStr(t, show, "TAIL")
	if len(e.hs.urls()) != before || e.received("alice", later) {
		t.Fatal("show contacted HTTP or receipted the pending control")
	}
	e.as("alice", "inbox", "--brief").ok(t)
	second := e.state("alice").LastInbox
	if second == path || !e.received("alice", later) {
		t.Fatal("second brief reused a path or lost pending mail")
	}
	e.as("alice", "inbox", "--brief").ok(t)
	empty := e.state("alice").LastInbox
	records, err := readSavedInbox(empty)
	if empty == second || err != nil || len(records) != 0 {
		t.Fatalf("empty brief did not save a fresh empty frame: %v", err)
	}
}

// Claim: metadata over the summary bound refuses the entire batch before
// publication or receipt; the adjacent exactly-at-bound control succeeds.
func TestInboxBriefMetadataBound(t *testing.T) {
	for _, n := range []int{512, 513} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			e, room := briefWorld(t)
			id := e.hs.inject(room, "@bob:test.local", toMsg("bob", strings.Repeat("k", n), "body"))
			control := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "valid control"))
			if n == 512 {
				e.as("alice", "inbox", "--brief").ok(t)
			} else {
				r := e.as("alice", "inbox", "--brief").failsWith(t, ExitError, "summary field exceeds")
				files, err := filepath.Glob(filepath.Join(filepath.Dir(e.paths().StateFile("alice")), "alice.inbox-*"))
				if err != nil || len(files) != 0 || r.stdout != "" || e.state("alice").LastInbox != "" {
					t.Fatal("metadata refusal published a save or output")
				}
			}
			if e.received("alice", id) != (n == 512) || e.received("alice", control) != (n == 512) {
				t.Fatal("metadata refusal partially receipted the batch")
			}
		})
	}
}

// Claim: a save publication failure, short write, flush failure or crash after
// printing leaves every selected entry pending; each adjacent retry is valid.
func TestInboxBriefFailuresKeepEntireBatchPending(t *testing.T) {
	for _, phase := range []string{"save", "short stdout", "flush stdout", "after print"} {
		t.Run(phase, func(t *testing.T) {
			e, room := briefWorld(t)
			id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "question", "first body"))
			control := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "second body"))
			stateDir := filepath.Dir(e.paths().StateFile("alice"))
			e.configure = func(a *App) {
				switch phase {
				case "save":
					a.beforeDeliver = func() {
						// Remove only this synthetic fixture's temporary save, making
						// publication fail after the full batch is rendered.
						files, _ := filepath.Glob(filepath.Join(stateDir, ".a2a-output-*"))
						if len(files) != 1 {
							t.Fatalf("expected one prepared save, got %d", len(files))
						}
						if err := os.Remove(files[0]); err != nil {
							t.Fatal(err)
						}
					}
				case "short stdout":
					a.Stdout = shortResultWriter{}
				case "flush stdout":
					a.Stdout = inboxFlushFailure{}
				case "after print":
					a.afterPrint = func() error { return fmt.Errorf("synthetic crash after print") }
				}
			}
			failed := e.as("alice", "inbox", "--brief").failsWith(t, ExitError, "nothing was acknowledged")
			e.configure = nil
			if e.received("alice", id) || e.received("alice", control) || e.state("alice").LastInbox != "" {
				t.Fatal("failure receipted mail or advanced the saved path")
			}
			files, _ := filepath.Glob(filepath.Join(stateDir, "alice.inbox-*"))
			if phase == "save" {
				if len(files) != 0 || failed.stdout != "" {
					t.Fatal("failed publication exposed a save or stdout")
				}
			} else {
				if len(files) != 1 {
					t.Fatal("stdout failure lost the durable saved batch")
				}
				records, err := readSavedInbox(files[0])
				if err != nil || len(records) != 2 {
					t.Fatalf("durable failed-output batch: %v", err)
				}
			}
			e.as("alice", "inbox", "--brief").ok(t)
			if !e.received("alice", id) || !e.received("alice", control) {
				t.Fatal("adjacent valid retry did not recover both pending entries")
			}
		})
	}
}

// Claim: incompatible flags fail before session/network/save effects; adjacent
// valid brief controls preserve the selected mail and accept summary/width.
func TestInboxBriefFlagRefusals(t *testing.T) {
	for _, flags := range [][]string{
		{"--json"}, {"--out", ""}, {"--out", "unused"}, {"--width", "0"}, {"--width", "2001"},
	} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			e, room := briefWorld(t)
			id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "valid control"))
			before := len(e.hs.urls())
			r := e.as("alice", append([]string{"inbox", "--brief"}, flags...)...).failsWith(t, ExitUsage, "--")
			if r.stdout != "" || e.received("alice", id) || len(e.hs.urls()) != before || e.state("alice").LastInbox != "" {
				t.Fatal("refused flags had delivery effects")
			}
			e.as("alice", "inbox", "--brief", "--summary", "--width", "2000").ok(t)
			if !e.received("alice", id) {
				t.Fatal("valid neighboring flags did not deliver")
			}
		})
	}
}

type briefOutputProbe struct {
	write func([]byte)
	flush func()
}

func (p briefOutputProbe) Write(data []byte) (int, error) { p.write(data); return len(data), nil }
func (p briefOutputProbe) Flush() error                   { p.flush(); return nil }

// Claim: publication precedes both stdout write and flush, while receipts and
// the last-path pointer remain pending until after the successful flush.
func TestInboxBriefPublicationFlushReceiptOrder(t *testing.T) {
	e, room := briefWorld(t)
	id := e.hs.inject(room, "@bob:test.local", toMsg("bob", "decision", "complete decision"))
	later := e.hs.inject(room, "@bob:test.local", toMsg("bob", "request", "pending control"))
	writes, flushes := 0, 0
	check := func() string {
		t.Helper()
		files, _ := filepath.Glob(filepath.Join(filepath.Dir(e.paths().StateFile("alice")), "alice.inbox-*"))
		if len(files) != 1 {
			t.Fatal("stdout started before save publication")
		}
		records, err := readSavedInbox(files[0])
		if err != nil || len(records) != 1 || records[0].EventID != id {
			t.Fatalf("stdout preceded the complete selected save: %v", err)
		}
		if e.received("alice", id) || e.received("alice", later) || e.state("alice").LastInbox != "" {
			t.Fatal("receipt or last path advanced before flush completed")
		}
		return files[0]
	}
	e.configure = func(a *App) {
		a.Stdout = briefOutputProbe{
			write: func(data []byte) {
				path := check()
				tuitestkit.ContainsStr(t, string(data), fmt.Sprintf("saved inbox: %q\n", path))
				writes++
			},
			flush: func() { check(); flushes++ },
		}
	}
	e.as("alice", "inbox", "--brief", "--limit", "1").ok(t)
	e.configure = nil
	if writes != 1 || flushes != 1 || !e.received("alice", id) || e.received("alice", later) || e.state("alice").LastInbox == "" {
		t.Fatal("successful output did not cross the exact receipt boundary")
	}
}
