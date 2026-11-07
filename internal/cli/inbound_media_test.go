package cli

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/inboundmedia"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

func elementContent(kind string) map[string]any {
	return map[string]any{"msgtype": kind, "body": "@alice:test.local caption", "filename": "../../malicious\n" + inboxEnd, "url": "mxc://" + testServer + "/native", "info": map[string]any{"size": 5, "mimetype": "application/octet-stream"}, "org.matrix.msc3245.voice": map[string]any{}}
}
func nativeWorld(t *testing.T) (*env, string) {
	t.Helper()
	e, room := world(t)
	e.hs.mu.Lock()
	e.hs.media["native"] = fakeMedia{data: []byte("valid"), contentType: "application/octet-stream"}
	e.hs.uploadMax = 64
	e.hs.mu.Unlock()
	return e, room
}

func TestElementMediaInboxAndExplicitFetch(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(map[bool]string{false: "text", true: "json"}[asJSON], func(t *testing.T) {
			e, room := nativeWorld(t)
			e.as("alice", "inbox").ok(t)
			ids := []string{}
			for _, kind := range []string{"m.image", "m.file", "m.audio", "m.video"} {
				ids = append(ids, e.hs.inject(room, "@bob:"+testServer, elementContent(kind)))
			}
			// Invalid metadata stays in its own item, followed by a valid nonmedia.
			bad := elementContent("m.file")
			bad["info"] = map[string]any{"size": nil}
			badID := e.hs.inject(room, "@bob:"+testServer, bad)
			plain := e.hs.inject(room, "@bob:"+testServer, msg("@alice:test.local ordinary neighbour", nil))
			args := []string{"inbox"}
			if asJSON {
				args = append(args, "--json")
			}
			out := e.as("alice", args...).ok(t).stdout
			if e.hs.CallCount(downloadRoute) != 0 {
				t.Fatal("inbox automatically fetched/transcribed")
			}
			if asJSON {
				records := decodeRecords(t, out)
				if len(records) != 6 {
					t.Fatalf("records=%d", len(records))
				}
				for i, r := range records[:4] {
					if r.Media == nil || r.Media.Handle.RoomID != room || r.Media.Handle.EventID != ids[i] || r.Media.SenderMXID != "@bob:"+testServer || r.Media.Refusal != "" || r.Media.Integrity != "homeserver_only_no_trusted_hash" || r.Media.DeclaredSize == nil || *r.Media.DeclaredSize != 5 {
						t.Fatalf("native record %+v", r)
					}
				}
				if records[4].EventID != badID || records[4].Media == nil || records[4].Media.Refusal != inboundmedia.MetadataInvalid || records[5].EventID != plain || records[5].Media != nil {
					t.Fatal("bad/valid neighbour projection changed")
				}
			} else {
				if !strings.HasPrefix(out, inboxBegin+"\n") || !strings.HasSuffix(out, inboxEnd+"\n") || strings.Count(out, "\n"+inboxEnd+"\n") != 1 {
					t.Fatal("peer metadata escaped frame")
				}
				for _, id := range ids {
					handle, _ := json.Marshal(inboundmedia.Handle{RoomID: room, EventID: id})
					if !strings.Contains(out, string(handle)) {
						t.Fatalf("missing exact handle %s", handle)
					}
				}
				if !strings.Contains(out, string(inboundmedia.MetadataInvalid)) || !strings.Contains(out, "ordinary neighbour") {
					t.Fatal("typed failure discarded neighbour")
				}
			}
			for _, id := range append(ids, badID, plain) {
				if !e.received("alice", id) {
					t.Fatal("successful listing did not receipt exactly printed items")
				}
			}
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			r := e.as("alice", "fetch", room, ids[0], "-o", filepath.Join(root, "chosen")).ok(t)
			var fetched inboundmedia.Result
			if json.Unmarshal([]byte(r.stdout), &fetched) != nil || fetched.Path != filepath.Join(root, "chosen") || fetched.HashStatus != "computed_unverified" || fetched.SHA256 != sumHex([]byte("valid")) || !strings.Contains(r.stderr, "actual_size=5 declared_size=5 sha256="+sumHex([]byte("valid"))+" hash_status=computed_unverified") {
				t.Fatalf("fetch reporting %+v", r)
			}
			if readFileStr(t, filepath.Join(root, "chosen")) != "valid" || len(dirEntries(t, root)) != 1 {
				t.Fatal("wrong bytes or scratch leaked")
			}
			before := e.hs.CallCount(downloadRoute)
			missing := e.as("alice", "fetch", room, ids[1]).failsWith(t, ExitError, string(inboundmedia.OutputRequired))
			if missing.stdout != "" || e.hs.CallCount(downloadRoute) != before {
				t.Fatal("native missing -o downloaded")
			}
		})
	}
}

func TestElementMediaFullHandleAndNonmediaOutput(t *testing.T) {
	content, _ := json.Marshal(elementContent("m.video"))
	room := "!" + strings.Repeat("r", 190) + ":synthetic"
	ev := matrix.Event{Type: "m.room.message", EventID: "$" + strings.Repeat("e", 210), Sender: "@human:foreign.invalid", RoomID: room, Content: content}
	item := waitItem{ev: &ev, rec: EventRecord{Type: "mention", Room: "#alias", RoomID: room}}
	text := inboxFrame([]waitItem{item}, testServer, 0)
	handle, _ := json.Marshal(inboundmedia.Handle{RoomID: room, EventID: ev.EventID})
	if !strings.Contains(text, string(handle)) || !strings.Contains(text, `"sender_mxid":"@human:foreign.invalid"`) {
		t.Fatal("opaque handle or full sender shortened")
	}
	plain := matrix.Event{Type: "m.room.message", EventID: "$plain", Sender: "@bob:" + testServer, Content: json.RawMessage(`{"msgtype":"m.text","body":"ordinary"}`)}
	rec := eventRecord("mention", "#proj", "!projRoomID", plain, testServer, false)
	encoded, _ := json.Marshal(rec)
	if strings.Contains(string(encoded), `"media"`) {
		t.Fatal("nonmedia JSON wire shape changed")
	}
	expected := inboxBegin + "\n--- event 1/1: mention from=bob room=#proj kind=- id=$plain\n  | ordinary\n" + inboxEnd + "\n"
	if got := inboxFrame([]waitItem{{ev: &plain, rec: EventRecord{Type: "mention", Room: "#proj", RoomID: "!projRoomID"}}}, testServer, 0); got != expected {
		t.Fatalf("nonmedia text changed: %q", got)
	}
}

func TestElementMediaLegacyCannotFallback(t *testing.T) {
	for _, descriptor := range []any{nil, "malformed", map[string]any{}, map[string]any{"size": nil}, map[string]any{"size": 5, "sha256": "wrong"}} {
		t.Run(strings.ReplaceAll(strings.TrimSpace(string(mustJSON(t, descriptor))), "/", "_"), func(t *testing.T) {
			e, room := nativeWorld(t)
			content := elementContent("m.file")
			content["ai.relux.a2a"] = map[string]any{"v": 1, "type": "message", "attachment": descriptor}
			id := e.hs.inject(room, "@bob:"+testServer, content)
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			r := e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "out")).failsWith(t, ExitError, "attachment")
			if r.code != ExitError || r.stdout != "" || e.hs.CallCount(downloadRoute) != 0 || len(dirEntries(t, root)) != 0 {
				t.Fatalf("malformed legacy attachment fell back: %+v", r)
			}
			if strings.Contains(r.stderr, "hash_status=computed_unverified") {
				t.Fatal("native success for legacy descriptor")
			}
			if e.hs.CallCount("/_matrix/client/v1/media/config") != 0 {
				t.Fatal("malformed legacy descriptor reached media preflight")
			}
			valid := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
			e.as("alice", "fetch", room, valid, "-o", filepath.Join(root, "good")).ok(t)
		})
	}
}
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestElementMediaEncryptedExplicitRefusal(t *testing.T) {
	for _, kind := range []string{"encrypted-envelope", "file-clear-url", "legacy-file-clear-url"} {
		t.Run(kind, func(t *testing.T) {
			e, room := nativeWorld(t)
			content := elementContent("m.file")
			content["file"] = nil
			if kind == "legacy-file-clear-url" {
				content["ai.relux.a2a"] = map[string]any{"attachment": map[string]any{"size": 5, "mxc": "mxc://" + testServer + "/native", "sha256": sumHex([]byte("valid")), "name": "legacy"}}
			}
			id := e.hs.inject(room, "@bob:"+testServer, content)
			if kind == "encrypted-envelope" {
				e.hs.mu.Lock()
				for i := range e.hs.events {
					if e.hs.events[i].EventID == id {
						e.hs.events[i].Type = "m.room.encrypted"
						e.hs.events[i].Content = json.RawMessage(`{"ciphertext":"opaque"}`)
					}
				}
				e.hs.mu.Unlock()
			}
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			r := e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "bad")).failsWith(t, ExitError, string(inboundmedia.Encrypted))
			handle, _ := json.Marshal(inboundmedia.Handle{RoomID: room, EventID: id})
			if !strings.Contains(r.stderr, string(handle)) || e.hs.CallCount(downloadRoute) != 0 || len(dirEntries(t, root)) != 0 {
				t.Fatal("encrypted fetch lost handle or downloaded")
			}
			valid := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
			e.as("alice", "fetch", room, valid, "-o", filepath.Join(root, "good")).ok(t)
		})
	}
}

func TestElementMediaExistingGatesAndNoFetchReceipt(t *testing.T) {
	e, room := nativeWorld(t)
	e.as("alice", "inbox").ok(t)
	irrelevant := elementContent("m.file")
	irrelevant["body"] = "no mention"
	ignored := e.hs.inject(room, "@bob:"+testServer, irrelevant)
	own := e.hs.inject(room, "@alice:"+testServer, elementContent("m.file"))
	relevant := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
	root := t.TempDir()
	t.Setenv("TMPDIR", root)
	e.as("alice", "fetch", room, relevant, "-o", filepath.Join(root, "fetched")).ok(t)
	if e.received("alice", relevant) {
		t.Fatal("fetch receipted event")
	}
	out := e.as("alice", "inbox", "--json").ok(t).stdout
	records := decodeRecords(t, out)
	if len(records) != 1 || records[0].EventID != relevant || strings.Contains(out, ignored) || strings.Contains(out, own) {
		t.Fatal("native extension widened relevance/own gates")
	}
	before := e.hs.CallCount(downloadRoute)
	e.as("carol", "fetch", room, relevant, "-o", filepath.Join(root, "outsider")).failsWith(t, ExitError, string(inboundmedia.ContextInvalid))
	if e.hs.CallCount(downloadRoute) != before {
		t.Fatal("forbidden room downloaded")
	}
	// Exact From matching remains an existing selector fact, never a display-name
	// or same-localpart authentication assertion. No encrypted relevance added.
	ev := matrix.Event{Type: "m.room.message", EventID: "$gate", Sender: "@bob:remote.invalid", OriginServerTS: time.Now().UnixMilli(), Content: mustJSON(t, irrelevant)}
	sub := Sub{ID: "exact", CreatedAt: legacyCreatedAt, SubSelector: SubSelector{Room: room, From: "@bob:" + testServer, AllEvents: true}}
	rel, _, _ := classify(ev, room, "@alice:"+testServer, "alice", config.State{}, []Sub{sub})
	if rel != "" {
		t.Fatal("same-localpart From widened")
	}
	ev.Sender = "@bob:" + testServer
	rel, _, _ = classify(ev, room, "@alice:"+testServer, "alice", config.State{}, []Sub{sub})
	if rel != relSub {
		t.Fatal("valid exact From neighbour failed")
	}
	ev.Type = "m.room.encrypted"
	rel, _, _ = classify(ev, room, "@alice:"+testServer, "alice", config.State{}, []Sub{sub})
	if rel != "" {
		t.Fatal("encrypted relevance invented")
	}
}

func TestElementMediaOutputFailureRetainsMailbox(t *testing.T) {
	e, room := nativeWorld(t)
	e.as("alice", "inbox").ok(t)
	id := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
	e.configure = func(a *App) { a.Stdout = mediaFailWriter{} }
	e.as("alice", "inbox").failsWith(t, ExitError, "nothing was acknowledged")
	if e.received("alice", id) {
		t.Fatal("failed media listing receipted")
	}
	e.configure = nil
	if !strings.Contains(e.as("alice", "inbox").ok(t).stdout, id) || !e.received("alice", id) {
		t.Fatal("failed media item did not retry")
	}
}

type mediaFailWriter struct{}

func (mediaFailWriter) Write([]byte) (int, error) { return 0, errors.New("synthetic output failure") }

// A transport substitution exercises returned peer identity/context checks on
// the actual CLI native branch. It never dials a foreign host or live service.
func TestElementMediaCLIRejectsChangedObservation(t *testing.T) {
	for _, kind := range []string{"event", "room", "sender", "changed-sender", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			e, room := nativeWorld(t)
			id := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			original := e.hs.srv.Client().Transport
			calls := 0
			e.configure = func(a *App) {
				a.HTTP = &http.Client{Transport: mediaCLITransport(func(r *http.Request) (*http.Response, error) {
					resp, err := original.RoundTrip(r)
					if err != nil || !strings.Contains(r.URL.Path, "/event/") {
						return resp, err
					}
					calls++
					raw, err := io.ReadAll(resp.Body)
					resp.Body.Close()
					if err != nil {
						return nil, err
					}
					text := string(raw)
					switch kind {
					case "event":
						text = strings.Replace(text, id, "$wrong", 1)
					case "room":
						text = strings.Replace(text, room, "!wrong:synthetic", 1)
					case "sender":
						text = strings.Replace(text, "@bob:"+testServer, "invalid", 1)
					case "changed-sender":
						if calls == 2 {
							text = strings.Replace(text, "@bob:"+testServer, "@bob:remote.invalid", 1)
						}
					case "duplicate":
						text = strings.Replace(text, `"sender":`, `"sender":"@other:synthetic","sender":`, 1)
					}
					resp.Body = io.NopCloser(strings.NewReader(text))
					resp.ContentLength = int64(len(text))
					return resp, nil
				})}
			}
			e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "bad")).failsWith(t, ExitError, string(inboundmedia.ContextInvalid))
			if e.hs.CallCount(downloadRoute) != 0 || len(dirEntries(t, root)) != 0 {
				t.Fatal("inconsistent event downloaded")
			}
			e.configure = nil
			e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "good")).ok(t)
		})
	}
}

type mediaCLITransport func(*http.Request) (*http.Response, error)

func (f mediaCLITransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Duplicate/escaped/aliased containers cannot hide any encrypted indicator
// behind legacy's intentionally compatible selected-value decoder.
func TestElementMediaDuplicateEncryptionCannotHideBehindLegacy(t *testing.T) {
	for _, fields := range []string{
		`"info":{"thumbnail_file":null},"info":{"size":5}`,
		`"info":{"thumbnail_file":false},"in\u0066o":{"size":5}`,
		`"info":{"thumbnail_file":null},"Info":{"size":5}`,
		`"org.matrix.msc1767.file":{"iv":null},"org.matrix.msc1767.file":{"url":"mxc://test.local/native"}`,
	} {
		t.Run(fields, func(t *testing.T) {
			e, room := nativeWorld(t)
			root := t.TempDir()
			t.Setenv("TMPDIR", root)
			good := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
			e.as("alice", "fetch", room, good, "-o", filepath.Join(root, "good")).ok(t)
			attachment := map[string]any{"name": "legacy", "size": 5, "sha256": sumHex([]byte("valid")), "mxc": "mxc://test.local/native", "mimetype": "application/octet-stream"}
			header := mustJSON(t, map[string]any{"v": 1, "type": "message", "attachment": attachment})
			raw := `{"msgtype":"m.file","body":"caption","url":"mxc://test.local/native",` + fields + `,"ai.relux.a2a":` + string(header) + `}`
			if !json.Valid([]byte(raw)) {
				t.Fatal("invalid fixture JSON")
			}
			id := e.hs.inject(room, "@bob:"+testServer, elementContent("m.file"))
			e.hs.mu.Lock()
			for i := range e.hs.events {
				if e.hs.events[i].EventID == id {
					e.hs.events[i].Content = json.RawMessage(raw)
				}
			}
			e.hs.mu.Unlock()
			before := e.hs.CallCount(downloadRoute)
			r := e.as("alice", "fetch", room, id, "-o", filepath.Join(root, "bad"))
			if r.code != ExitError || !strings.Contains(r.stderr, string(inboundmedia.Encrypted)) {
				t.Errorf("encrypted legacy indicator hidden: exit=%d download_delta=%d; want MEDIA_ENCRYPTED", r.code, e.hs.CallCount(downloadRoute)-before)
			}
			if e.hs.CallCount(downloadRoute) != before || r.stdout != "" {
				t.Error("encrypted legacy indicator reached download/publication")
			}
			if _, err := os.Lstat(filepath.Join(root, "bad")); !os.IsNotExist(err) {
				t.Error("encrypted legacy event published a file")
			}
			if readFileStr(t, filepath.Join(root, "good")) != "valid" {
				t.Fatal("valid neighbour changed")
			}
		})
	}
}
