package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/config"
	"github.com/relux-works/a2a-swarm/internal/matrix"
)

type senderCase struct{ name, raw, text string }

func senderCases() []senderCase {
	long := "@" + strings.Repeat("a", 255-len("@:alpha.example")) + ":alpha.example"
	// Exactly 255 UTF-8 bytes, with ASCII quoting expanding beyond 512 bytes.
	expanded := "@" + strings.Repeat("é", 120) + ":alpha.example"
	return []senderCase{
		{"alpha", "@same:alpha.example", "@same:alpha.example"},
		{"beta", "@same:beta.example", "@same:beta.example"},
		{"case", "@Same:Alpha.example", "@Same:Alpha.example"},
		{"unicode", "@界:alpha.example", `"@\u754c:alpha.example"`},
		{"space", "@same name:alpha.example", `"@same name:alpha.example"`},
		{"double-space", "@same  name:alpha.example", `"@same  name:alpha.example"`},
		{"control", "@same\t\n\x1b\u202e:alpha.example", `"@same\t\n\x1b\u202e:alpha.example"`},
		{"literal-escape", `@same\t\n\x1b\u202e:alpha.example`, `"@same\\t\\n\\x1b\\u202e:alpha.example"`},
		{"quote-equals", "@same\"=name:alpha.example", `"@same\"=name:alpha.example"`},
		{"long", long, long},
		{"expanded", expanded, `"@` + strings.Repeat(`\u00e9`, 120) + `:alpha.example"`},
	}
}

func TestSenderText(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range senderCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := senderText(tc.raw)
			if got != tc.text || seen[got] {
				t.Fatalf("sender representation changed or collided: got %q want %q", got, tc.text)
			}
			seen[got] = true
			if tc.name == "long" || tc.name == "expanded" {
				if len(tc.raw) != 255 {
					t.Fatal("boundary fixture is not 255 bytes")
				}
			}
			if decodedSender(t, got) != tc.raw {
				t.Fatal("sender is lossy")
			}
			for i := 0; i < len(got); i++ {
				if got[i] < 0x20 || got[i] > 0x7e {
					t.Fatal("sender can alter a display line")
				}
			}
		})
	}
}

func decodedSender(t *testing.T, text string) string {
	t.Helper()
	if strings.HasPrefix(text, "\"") {
		value, err := strconv.Unquote(text)
		if err != nil {
			t.Fatalf("sender is not reversible: %q: %v", text, err)
		}
		return value
	}
	if !strings.HasPrefix(text, "@") {
		t.Fatalf("bare sender is not a full MXID: %q", text)
	}
	return text
}

func senderWorld(t *testing.T, server string) (*env, string) {
	t.Helper()
	e := newEnv(t)
	e.hs.server = server
	alice, bob := e.agent("alice"), e.agent("bob")
	return e, e.hs.addRoom("proj", alice, bob)
}

func assertSenderRecords(t *testing.T, records []EventRecord, ids []string, cases []senderCase) {
	t.Helper()
	if len(records) != len(ids) {
		t.Fatalf("sender record count: got %d want %d", len(records), len(ids))
	}
	for i, r := range records {
		if r.EventID != ids[i] || r.Sender != cases[i].raw {
			t.Fatalf("event sender changed: event=%q sender=%q want event=%q sender=%q", r.EventID, r.Sender, ids[i], cases[i].raw)
		}
	}
}

// The two selected servers must yield the same identities. Equal display labels
// and a forged identical From cannot replace the actual event sender, including
// when the event itself is a projection. No profile lookup participates.
func TestSenderReadConsumers(t *testing.T) {
	for _, server := range []string{"alpha.example", "beta.example"} {
		t.Run(server, func(t *testing.T) {
			e, room := senderWorld(t, server)
			var ids []string
			cases := senderCases()
			for _, tc := range cases {
				id := e.hs.inject(room, tc.raw, toMsg("@claimed:origin.example", "question", "same display label\nbody\x1b"))
				ids = append(ids, id)
				records := decodeRecords(t, e.as("alice", "read", room, "--event", id, "--json").ok(t).stdout)
				assertSenderRecords(t, records, []string{id}, []senderCase{tc})
				if records[0].Body != "same display label\nbody" {
					t.Fatal("read body sanitization changed")
				}
				text := e.as("alice", "read", room, "--event", id).ok(t).stdout
				line := strings.SplitN(text, "\n", 2)[0]
				_, rest, ok := strings.Cut(line, "] ")
				field, _, found := strings.Cut(rest, " "+id)
				if !ok || !found || field != tc.text || decodedSender(t, field) != tc.raw {
					t.Fatalf("read sender changed: %q", line)
				}
			}
			assertSenderRecords(t, decodeRecords(t, e.as("alice", "read", room, "--last", "100", "--json").ok(t).stdout), ids, cases)
			projection := msg("[system] claimed origin", map[string]any{ContentKey: Header{V: 1, Type: "projection", From: "@claimed:origin.example", Kind: "system"}})
			id := e.hs.inject(room, "@projector:bridge.example", projection)
			r := decodeRecords(t, e.as("alice", "read", room, "--event", id, "--json").ok(t).stdout)
			if len(r) != 1 || r[0].Sender != "@projector:bridge.example" {
				t.Fatal("projection sender replaced by claimed origin")
			}
			e.hs.redact(ids[0], "@alice:"+server)
			redacted := decodeRecords(t, e.as("alice", "read", room, "--event", ids[0], "--json").ok(t).stdout)
			assertSenderRecords(t, redacted, ids[:1], cases[:1])
			if !redacted[0].Redacted || redacted[0].Body != "[redacted]" {
				t.Fatal("redacted event presentation changed")
			}
		})
	}
}

func TestSenderMailboxConsumers(t *testing.T) {
	for _, server := range []string{"alpha.example", "beta.example"} {
		for _, mode := range []string{"wait-text", "wait-json", "inbox-text", "inbox-json", "saved"} {
			t.Run(server+"/"+mode, func(t *testing.T) {
				e, room := senderWorld(t, server)
				e.as("alice", "inbox").ok(t)
				cases := senderCases()
				var ids []string
				for _, tc := range cases {
					ids = append(ids, e.hs.inject(room, tc.raw, toMsg("@claimed:origin.example", "question", "bounded body")))
				}
				var text string
				switch mode {
				case "wait-text":
					text = e.as("alice", "wait", "--timeout", "2s").ok(t).stdout
				case "wait-json":
					text = e.as("alice", "wait", "--timeout", "2s", "--json").ok(t).stdout
				case "inbox-text":
					text = e.as("alice", "inbox").ok(t).stdout
				case "inbox-json":
					text = e.as("alice", "inbox", "--json").ok(t).stdout
				case "saved":
					path := filepath.Join(outputTestDir(t), "frame")
					text = e.as("alice", "inbox", "--out", path, "--summary").ok(t).stdout
					records, err := readSavedInbox(path)
					if err != nil {
						t.Fatal(err)
					}
					assertSenderRecords(t, records, ids, cases)
					raw, err := os.ReadFile(path)
					if err != nil || bytes.Contains(raw, []byte("\u202e")) {
						t.Fatal("saved sender format control is not inert")
					}
					for i, tc := range cases {
						if !strings.Contains(text, "\ncount sender="+tc.text+" n=1\n") || !strings.Contains(text, fmt.Sprintf("\nevent %d sender=%s kind=", i+1, tc.text)) {
							t.Fatalf("summary sender changed: %s", tc.name)
						}
						before := len(e.hs.urls())
						show := e.as("alice", "inbox", "show", ids[i], "--from", path).ok(t).stdout
						if !strings.HasPrefix(show, inboxBegin+"\n--- event 1/1: from="+tc.text+" room=") || len(e.hs.urls()) != before {
							t.Fatalf("saved show sender changed or contacted server: %s", tc.name)
						}
					}
				}
				if strings.HasSuffix(mode, "json") {
					assertSenderRecords(t, decodeRecords(t, text), ids, cases)
				} else if mode != "saved" {
					var want strings.Builder
					if mode == "inbox-text" {
						want.WriteString(inboxBegin + "\n")
					}
					for i, tc := range cases {
						if mode == "wait-text" {
							fmt.Fprintf(&want, "to #proj %s from=%s kind=question topic=alpha: bounded body\n", ids[i], tc.text)
						} else {
							fmt.Fprintf(&want, "--- event %d/%d: to from=%s room=#proj kind=question id=%s topic=alpha\n  | bounded body\n", i+1, len(ids), tc.text, ids[i])
						}
					}
					if mode == "inbox-text" {
						want.WriteString(inboxEnd + "\n")
					}
					if text != want.String() {
						t.Fatalf("mailbox sender frame changed: got %q want %q", text, want.String())
					}
				}
				for _, id := range ids {
					if e.received("alice", id) != (strings.HasPrefix(mode, "inbox") || mode == "saved") {
						t.Fatal("sender rendering changed receipt semantics")
					}
				}
			})
		}
	}
}

// Attachment sender columns and newly generated archive blocks use the event
// sender too. Header From and attachment metadata cannot supply the identity.
func TestSenderFilesArchiveConsumers(t *testing.T) {
	for _, server := range []string{"alpha.example", "beta.example"} {
		t.Run(server, func(t *testing.T) {
			e, room := senderWorld(t, server)
			cases := senderCases()
			var ids []string
			for _, tc := range cases {
				content := toMsg("@claimed:origin.example", "question", "bounded body")
				h := content[ContentKey].(Header)
				h.Attachment = &Attachment{Name: "fixture.txt", Size: 1, SHA256: strings.Repeat("a", 64), MXC: "mxc://alpha.example/fixture"}
				content[ContentKey] = h
				ids = append(ids, e.hs.inject(room, tc.raw, content))
			}
			lines := strings.Split(strings.TrimSuffix(e.as("alice", "files", room).ok(t).stdout, "\n"), "\n")
			if len(lines) != len(ids) {
				t.Fatal("file selection changed")
			}
			for i, line := range lines {
				fields := strings.Split(line, "\t")
				if len(fields) != 6 || fields[0] != ids[i] || fields[1] != cases[i].text || decodedSender(t, fields[1]) != cases[i].raw {
					t.Fatalf("file sender changed: %q", line)
				}
			}
			assertSenderRecords(t, decodeRecords(t, e.as("alice", "files", room, "--json").ok(t).stdout), ids, cases)
			dir := filepath.Join(t.TempDir(), "archive")
			e.as("alice", "archive", room, "--dir", dir).ok(t)
			archive := readFileStr(t, filepath.Join(dir, "proj", RoomFileName))
			if len(blockHeaders(archive)) != len(ids) {
				t.Fatal("archive frame count changed")
			}
			for i, tc := range cases {
				ev, _ := e.hs.eventByID(ids[i])
				want := "\n## " + ids[i] + "\n- " + time.UnixMilli(ev.OriginServerTS).UTC().Format(time.RFC3339) + " " + tc.text + " kind="
				if !strings.Contains(archive, want) {
					t.Fatalf("archive sender changed: %s", tc.name)
				}
			}
		})
	}
}

func TestSenderWatchConsumers(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		t.Run(fmt.Sprint(asJSON), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			const sender = "@Same\n\u202e:alpha.example"
			payload := strings.Replace(waitValidEvent, `"@bob:test.local"`, strconv.Quote(sender), 1)
			a, p := watchRetryApp(t, func(r *http.Request) (*http.Response, error) { return waitResponse(r, 200, payload) })
			a.afterPrint = func() error { cancel(); return nil }
			args := []string{"--as", "alice", "watch"}
			if asJSON {
				args = append(args, "--json")
			}
			if code := a.Run(ctx, args); code != ExitOK {
				t.Fatalf("watch failed: %d %v", code, a.Stderr)
			}
			text := a.Stdout.(*bytes.Buffer).String()
			if asJSON {
				assertSenderRecords(t, decodeRecords(t, text), []string{"$neighbor"}, []senderCase{{raw: sender}})
			} else if text != "mention #proj $neighbor from=\"@Same\\n\\u202e:alpha.example\": @alice:test.local recovered\n" {
				t.Fatalf("watch sender changed: %q", text)
			}
			mb, err := config.OpenMailbox(p, "alice")
			if err != nil || mb.Received("$neighbor") {
				t.Fatal("watch receipted event")
			}
		})
	}
}

// Repeated event IDs retain the existing mailbox behavior; distinct events
// from one account remain separate. Exact own-sender exclusion is unchanged.
func TestSenderMailboxIdentityNeighbors(t *testing.T) {
	e, room := world(t)
	e.as("alice", "inbox").ok(t)
	first := e.hs.inject(room, "@same:test.local", toMsg("label-one", "question", "one"))
	second := e.hs.inject(room, "@same:test.local", toMsg("label-two", "question", "two"))
	e.hs.inject(room, "@alice:test.local", toMsg("other", "question", "own"))
	e.hs.mu.Lock()
	for _, ev := range e.hs.events {
		if ev.EventID == first {
			e.hs.events = append(e.hs.events, ev)
			break
		}
	}
	e.hs.mu.Unlock()
	records := decodeRecords(t, e.as("alice", "inbox", "--json").ok(t).stdout)
	assertSenderRecords(t, records, []string{first, second}, []senderCase{{raw: "@same:test.local"}, {raw: "@same:test.local"}})
}

// Direct record coverage keeps format escaping reversible even when printed
// NDJSON is displayed as text, independently of the safe body transformation.
func TestSenderRecordFormatEscaping(t *testing.T) {
	ev := matrix.Event{Type: "m.room.message", Sender: "@same\u202e:alpha.example", Content: json.RawMessage(`{"msgtype":"m.text","body":"body"}`)}
	data, err := json.Marshal(eventRecord("message", "#proj", "!room:alpha.example", ev, "alpha.example", false))
	if err != nil || bytes.Contains(data, []byte("\u202e")) || !bytes.Contains(data, []byte(`\u202e`)) {
		t.Fatal("sender format escaping changed")
	}
	var rec EventRecord
	if json.Unmarshal(data, &rec) != nil || rec.Sender != ev.Sender {
		t.Fatal("sender JSON decoding is lossy")
	}
}

// Dedicated long-sender assertions let the CI clipping controls prove their
// intended failure without an earlier quoting failure masking the bound.
func TestSenderLongInboxConsumers(t *testing.T) {
	for _, mode := range []string{"inbox", "saved"} {
		t.Run(mode, func(t *testing.T) {
			e, room := world(t)
			e.as("alice", "inbox").ok(t)
			sender := "@" + strings.Repeat("a", 230) + ":alpha.example"
			id := e.hs.inject(room, sender, toMsg("@claimed:origin.example", "question", "body"))
			if mode == "inbox" {
				out := e.as("alice", "inbox").ok(t).stdout
				if !strings.HasPrefix(out, inboxBegin+"\n--- event 1/1: to from="+sender+" room=") {
					t.Fatal("long inbox sender changed")
				}
			} else {
				path := filepath.Join(outputTestDir(t), "frame")
				e.as("alice", "inbox", "--out", path).ok(t)
				out := e.as("alice", "inbox", "show", id, "--from", path).ok(t).stdout
				if !strings.HasPrefix(out, inboxBegin+"\n--- event 1/1: from="+sender+" room=") {
					t.Fatal("long saved show sender changed")
				}
			}
		})
	}
}
