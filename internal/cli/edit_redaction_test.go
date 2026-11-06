package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

const editRemoved = "EDIT-REMOVED-SENTINEL"

func editContent(target, text string) map[string]any {
	return map[string]any{"msgtype": "m.text", "body": "@alice:test.local * " + text,
		"formatted_body": text, "m.new_content": map[string]any{"msgtype": "m.text", "body": text, "formatted_body": text},
		"m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": target}}
}

// Model both server stripping and the independently arriving redaction event.
func observeEditTargetRedaction(e *env, room, target string) {
	e.hs.redact(target, "@alice:test.local")
	e.hs.mu.Lock()
	defer e.hs.mu.Unlock()
	e.hs.appendLocked(room, "@alice:test.local", "m.room.redaction", map[string]string{"redacts": target})
}

// Edits remain independent server events. Local knowledge must suppress both
// fallback and new content, while preserving unrelated edits and ordinary text.
func TestRedactionEditsReadArchive(t *testing.T) {
	e, dir, _ := archiveWorld(t)
	e.sequentialSessions()
	e.as("alice", "inbox").ok(t)
	room := "!projRoomID"
	target := e.hs.inject(room, "@bob:test.local", msg("original removed", nil))
	edit := e.hs.inject(room, "@bob:test.local", editContent(target, editRemoved))
	good := e.hs.inject(room, "@bob:test.local", editContent("$ev002", "valid-edit-control"))
	e.hs.inject(room, "@bob:test.local", msg("ordinary-control", nil))
	observeEditTargetRedaction(e, room, target)
	// Start with a complete room read so the same response contains both the
	// redacted original and its still-unredacted independent edit.
	r := e.as("alice", "read", "proj", "--json").ok(t)
	if strings.Contains(r.stdout+r.stderr, editRemoved) {
		t.Fatal("read leaked edit sentinel after target redaction")
	}
	for _, control := range []string{"valid-edit-control", "ordinary-control"} {
		if !strings.Contains(r.stdout, control) {
			t.Fatalf("read lost %s", control)
		}
	}
	e.as("alice", "archive", "proj", "--dir", dir, "--json-index").ok(t)
	var archive strings.Builder
	files, err := os.ReadDir(filepath.Join(dir, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		archive.WriteString(readFileStr(t, filepath.Join(dir, "proj", f.Name())))
	}
	if strings.Contains(archive.String(), editRemoved) {
		t.Fatal("archive leaked edit sentinel after target redaction")
	}
	for _, control := range []string{edit, good, "valid-edit-control", "ordinary-control"} {
		if !strings.Contains(archive.String(), control) {
			t.Fatalf("archive lost %s", control)
		}
	}
	// Learn the redaction durably; fresh sessions must suppress direct-event
	// JSON as well as every content-bearing read endpoint.
	syncRedactionFixture(t, e, "alice")
	for _, args := range [][]string{{"read", room}, {"read", room, "--event", edit}, {"read", room, "--event", edit, "--json"}, {"files", room, "--json"}, {"fetch", room, edit, "-o", filepath.Join(t.TempDir(), "file")}} {
		r := e.as("alice", args...)
		if strings.Contains(r.stdout+r.stderr, editRemoved) {
			t.Fatalf("%v leaked edit sentinel", args)
		}
	}
	ev, _ := e.hs.eventByID(edit)
	if ev.IsRedacted() || !strings.Contains(string(ev.Content), editRemoved) {
		t.Fatal("local suppression changed independent server edit")
	}
}

func TestRedactionEditsSync(t *testing.T) {
	for _, order := range []string{"stored-before-redaction", "redaction-before-edit", "same-batch", "backfill", "cross-page", "crash-before-cursor"} {
		t.Run(order, func(t *testing.T) {
			e, room := world(t)
			e.sequentialSessions()
			e.as("alice", "inbox").ok(t)
			target := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local target", nil))
			if order == "redaction-before-edit" {
				observeEditTargetRedaction(e, room, target)
				syncRedactionFixture(t, e, "alice")
			}
			edit := e.hs.inject(room, "@bob:test.local", editContent(target, editRemoved))
			if order == "stored-before-redaction" {
				syncRedactionFixture(t, e, "alice")
				if !strings.Contains(string(e.mailbox("alice").Pending()[1].Event), editRemoved) {
					t.Fatal("initial stored-edit control missing")
				}
			}
			if order != "redaction-before-edit" {
				observeEditTargetRedaction(e, room, target)
			}
			if order == "cross-page" {
				// The newer edit is returned on a page before knowledge of an older
				// redaction reaches the client. Reapply knowledge to all kept pages.
				ev, _ := e.hs.eventByID(edit)
				e.hs.mu.Lock()
				red := e.hs.events[len(e.hs.events)-1]
				original := e.hs.events[0]
				e.hs.messagePages = map[string]map[string]fakeMessagePage{room: {
					"m1":    {page: matrix.Page{Chunk: []matrix.Event{ev}, End: "older"}},
					"older": {page: matrix.Page{Chunk: []matrix.Event{red, original}}},
				}}
				e.hs.mu.Unlock()
			}
			good := e.hs.inject(room, "@bob:test.local", editContent("$unredacted", "valid-edit-control"))
			neighbour := e.hs.inject(room, "@bob:test.local", msg("@alice:test.local ordinary-control", nil))
			if order == "backfill" || order == "cross-page" {
				e.hs.mu.Lock()
				e.hs.syncLimit = 1
				e.hs.mu.Unlock()
			}
			if order == "cross-page" {
				ev, _ := e.hs.eventByID(good)
				e.hs.mu.Lock()
				page := e.hs.messagePages[room]["m1"]
				page.page.Chunk = append([]matrix.Event{ev}, page.page.Chunk...)
				e.hs.messagePages[room]["m1"] = page
				e.hs.mu.Unlock()
			}
			if order == "crash-before-cursor" {
				before, err := config.LoadState(e.paths(), "alice")
				if err != nil {
					t.Fatal(err)
				}
				e.configure = func(a *App) { a.afterAppend = func() error { return errors.New("fixture crash before cursor") } }
				e.as("alice", "inbox").failsWith(t, ExitError, "fixture crash before cursor")
				after, err := config.LoadState(e.paths(), "alice")
				if err != nil {
					t.Fatal(err)
				}
				if before.NextBatch != after.NextBatch {
					t.Fatal("failed edit persist advanced cursor")
				}
				e.configure = nil
			} else {
				syncRedactionFixture(t, e, "alice")
			}
			mb := e.mailbox("alice")
			pending := mb.Pending()
			if len(pending) != 4 {
				t.Fatalf("pending=%d, want target, edit, valid edit, neighbour", len(pending))
			}
			for i, id := range []string{target, edit, good, neighbour} {
				if pending[i].ID != id || mb.Received(id) {
					t.Fatal("arrival/order/receipt changed")
				}
			}
			disk, err := os.ReadFile(e.paths().MailboxFile("alice"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(disk), editRemoved) {
				t.Fatal("mailbox retained edit sentinel")
			}
			for _, jsonMode := range []bool{false, true} {
				args := []string{"wait", "--timeout", "20ms"}
				if jsonMode {
					args = append(args, "--json")
				}
				r := e.as("alice", args...).ok(t)
				if strings.Contains(r.stdout+r.stderr, editRemoved) {
					t.Fatal("wait leaked edit sentinel")
				}
			}
			for _, jsonMode := range []bool{false, true} {
				args := []string{"inbox"}
				if jsonMode {
					args = append(args, "--json")
				}
				e.configure = func(a *App) { a.afterPrint = func() error { return errors.New("fixture crash before receipt") } }
				r := e.as("alice", args...)
				if strings.Contains(r.stdout+r.stderr, editRemoved) {
					t.Fatal("inbox leaked edit sentinel")
				}
				for _, control := range []string{"[redacted]", "valid-edit-control", "ordinary-control"} {
					if !strings.Contains(r.stdout, control) {
						t.Fatalf("inbox lost %s", control)
					}
				}
			}
			e.configure = nil
			// Replaying the server's unredacted edit after restart cannot resurrect it.
			ev, _ := e.hs.eventByID(edit)
			e.hs.mu.Lock()
			e.hs.events = append(e.hs.events, ev)
			e.hs.mu.Unlock()
			r := e.as("alice", "inbox", "--json").ok(t)
			if strings.Contains(r.stdout+r.stderr, editRemoved) {
				t.Fatal("replay leaked edit sentinel")
			}
			mb = e.mailbox("alice")
			if len(mb.Pending()) != 0 || !mb.Received(edit) {
				t.Fatal("explicit receipt failed")
			}
		})
	}
}

func TestRedactionEditsProjectKnownCache(t *testing.T) {
	for _, mode := range []string{"live", "limited-backfill"} {
		t.Run(mode, func(t *testing.T) {
			e, room, system := projWorld(t)
			e.project().ok(t)
			target := e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "original"))
			content := editContent(target, editRemoved)
			content[ContentKey] = kindMsg("bob", "system", "x")[ContentKey]
			e.hs.inject(room, "@bob:test.local", content)
			// Knowledge belongs to the projector's selected identity, not another account.
			_, err := config.UpdateState(e.paths(), "proj", func(_ *config.State) error {
				mb, err := config.OpenMailbox(e.paths(), "proj")
				if err != nil {
					return err
				}
				return mb.Redact([]config.Redaction{{Room: room, EventID: target}})
			})
			if err != nil {
				t.Fatal(err)
			}
			e.hs.inject(room, "@bob:test.local", kindMsg("bob", "system", "projection-control"))
			if mode == "limited-backfill" {
				e.hs.mu.Lock()
				e.hs.syncLimit = 1
				e.hs.mu.Unlock()
			}
			r := e.project().ok(t)
			if strings.Contains(r.stdout+r.stderr, editRemoved) {
				t.Fatal("project output leaked edit sentinel")
			}
			projected := e.projections(system)
			if len(projected) != 1 || !strings.Contains(projected[0].Message().Body, "projection-control") {
				t.Fatal("projector failed known-cache suppression or positive control")
			}

		})
	}
}
