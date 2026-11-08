package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// These are outgoing targets, not the broader grammar of received member IDs.
func TestRecipientExactGrammar(t *testing.T) {
	maxID := "@" + strings.Repeat("a", 255-len("@:a")) + ":a"
	for _, tc := range []struct {
		target, want string
		explicit     bool
	}{
		{"@same:alpha.example", "@same:alpha.example", true},
		{"@same:beta.example", "@same:beta.example", true},
		{"@alice+bot:alpha.example", "@alice+bot:alpha.example", true},
		{"@bridge/user:alpha.example", "@bridge/user:alpha.example", true},
		{"@a._=-09:host.example:00080", "@a._=-09:host.example:00080", true},
		{"@same:192.0.2.1:99999", "@same:192.0.2.1:99999", true},
		{"@same:[2001:db8::a]:08448", "@same:[2001:db8::a]:08448", true},
		{maxID, maxID, true},
		{"Same", "@same:alpha.example", false},
		{"@Same", "@same:alpha.example", false},
	} {
		t.Run(tc.target, func(t *testing.T) {
			got, explicit, err := messageRecipient(tc.target, "alpha.example")
			if err != nil || got != tc.want || explicit != tc.explicit {
				t.Fatalf("exact grammar: got %q explicit=%v err=%v, want %q explicit=%v", got, explicit, err, tc.want, tc.explicit)
			}
		})
	}
	for _, tc := range recipientRefusals() {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := messageRecipient(tc.target, "alpha.example")
			if got != "" || err == nil {
				t.Fatalf("recipient refusal: accepted %q as %q", tc.target, got)
			}
			if tc.lower != "" && !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.lower)) {
				t.Fatalf("recipient refusal: missing lowercase suggestion %q", tc.lower)
			}
			if tc.lower == "" && strings.Contains(err.Error(), "; use ") {
				t.Fatalf("recipient refusal: invalid lowercase suggestion: %v", err)
			}
		})
	}
}

type recipientRefusal struct{ name, target, lower string }

func recipientRefusals() []recipientRefusal {
	return []recipientRefusal{
		{"uppercase-local", "@Same:alpha.example", "@same:alpha.example"},
		{"uppercase-plus", "@Alice+Bot:alpha.example", "@alice+bot:alpha.example"},
		{"uppercase-slash", "@Bridge/User:alpha.example", "@bridge/user:alpha.example"},
		{"uppercase-server", "@same:Alpha.Example:08448", "@same:alpha.example:08448"},
		{"uppercase-ipv6", "@same:[2001:DB8::A]:8448", "@same:[2001:db8::a]:8448"},
		{"empty-server", "@same:", ""},
		{"missing-at", "same:alpha.example", ""},
		{"missing-at-empty-server", "same:", ""},
		{"empty-local", "@:alpha.example", ""},
		{"historical-local", "@old!name:alpha.example", ""},
		{"unicode-local", "@é:alpha.example", ""},
		{"unicode-uppercase", "@É:alpha.example", ""},
		{"unicode-kelvin", "@K:alpha.example", ""},
		{"uppercase-invalid-local", "@Old!name:alpha.example", ""},
		{"uppercase-invalid-server", "@Same:bad host", ""},
		{"uppercase-overlong", "@A" + strings.Repeat("a", 256-len("@A:a")) + ":a", ""},
		{"local-space", "@old name:alpha.example", ""},
		{"server-path", "@same:alpha.example/path", ""},
		{"server-url", "@same:https://alpha.example", ""},
		{"server-query", "@same:alpha.example?x=y", ""},
		{"server-userinfo", "@same:user@alpha.example", ""},
		{"server-unicode", "@same:é.example", ""},
		{"server-underscore", "@same:alpha_example", ""},
		{"ipv6-unbracketed", "@same:2001:db8::1", ""},
		{"ipv6-unclosed", "@same:[2001:db8::1", ""},
		{"ipv6-zone", "@same:[fe80::1%eth0]", ""},
		{"ipv4-bracketed", "@same:[192.0.2.1]", ""},
		{"port-empty", "@same:alpha.example:", ""},
		{"port-alpha", "@same:alpha.example:wrong", ""},
		{"port-long", "@same:alpha.example:123456", ""},
		{"port-sign", "@same:alpha.example:+80", ""},
		{"overlong", "@" + strings.Repeat("a", 256-len("@:a")) + ":a", ""},
	}
}

func recipientPublishArgs(t *testing.T, command, room, root string) []string {
	t.Helper()
	if command == "topic" {
		return []string{"topic", "new", room, "addressed", "Addressed", "--body", "recipient body"}
	}
	args := []string{command, room, "--topic", root}
	if command == "attach" {
		args = append(args, writeFile(t, t.TempDir(), "notes.txt", []byte("recipient fixture\n")))
	}
	return append(args, "recipient body")
}

// Observe real fake-server content and receipts from all three publishers.
// A foreign member with the same localpart cannot redirect local shorthand.
func TestRecipientExactWire(t *testing.T) {
	for _, server := range []string{"alpha.example", "beta.example"} {
		for _, command := range []string{"post", "topic", "attach"} {
			for _, tc := range []struct {
				name            string
				to              []string
				names, mentions []string
				warn            bool
			}{
				{"explicit-local", []string{"@same:" + server}, []string{"@same:" + server}, []string{"@same:" + server}, false},
				{"both-servers", []string{"@same:alpha.example", "@same:beta.example"}, []string{"@same:alpha.example", "@same:beta.example"}, []string{"@same:alpha.example", "@same:beta.example"}, false},
				{"modern-localparts", []string{"@alice+bot:" + server, "@bridge/user:" + server}, []string{"@alice+bot:" + server, "@bridge/user:" + server}, []string{"@alice+bot:" + server, "@bridge/user:" + server}, false},
				{"ports", []string{"@a._=-09:host.example:00080,@same:[2001:db8::a]:08448"}, []string{"@a._=-09:host.example:00080", "@same:[2001:db8::a]:08448"}, []string{"@a._=-09:host.example:00080", "@same:[2001:db8::a]:08448"}, false},
				{"bare", []string{"Same"}, []string{"same"}, []string{"@same:" + server}, true},
				{"at-shorthand", []string{"@Same"}, []string{"same"}, []string{"@same:" + server}, true},
				{"mixed-duplicates", []string{" ,Same,@same:alpha.example", "@same:beta.example,Same", "@same:" + server + ", "}, []string{"same", "@same:alpha.example", "@same:beta.example", "same", "@same:" + server}, []string{"@same:" + server, "@same:alpha.example", "@same:beta.example", "@same:" + server, "@same:" + server}, true},
				{"all", []string{"all"}, []string{"all"}, nil, false},
				{"default", nil, []string{"all"}, nil, false},
				{"empty-segments", []string{" , ", ""}, []string{"all"}, nil, false},
			} {
				t.Run(server+"/"+command+"/"+tc.name, func(t *testing.T) {
					e := newEnv(t)
					e.hs.server = server
					alice, same := e.agent("alice"), e.agent("same")
					foreign := "@same:foreign.example"
					room := e.hs.addRoom("proj", alice, same, foreign)
					root := strings.TrimSpace(e.as("alice", "topic", "new", room, "root", "Root").ok(t).stdout)
					args := recipientPublishArgs(t, command, room, root)
					for _, target := range tc.to {
						args = append(args, "--to", target)
					}
					idPath := filepath.Join(outputTestDir(t), "event-id")
					args = append(args, "--json", "--id-file", idPath)
					const send = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
					before := e.hs.CallCount(send)
					r := e.as("alice", args...).ok(t)
					ev := e.hs.lastEvent()
					m := ev.Message()
					h, ok := decodeHeader(m.A2A)
					if !ok || !slices.Equal(h.To, tc.names) || m.Mentions == nil || !slices.Equal(m.Mentions.UserIDs, tc.mentions) {
						t.Fatalf("recipient identity mismatch: header=%+v mentions=%+v, want %v / %v", h, m.Mentions, tc.names, tc.mentions)
					}
					// Topic openers keep their existing frame-only body.
					// The post/attach protocol body must agree with Header.To.
					if command != "topic" && !strings.Contains(m.Body, "- to: "+strings.Join(tc.names, ", ")+"\n") {
						t.Fatalf("recipient identity mismatch: visible header %q", m.Body)
					}
					wantWarning := ""
					if tc.warn {
						wantWarning = fmt.Sprintf("a2a: warning [LOCAL_RECIPIENT]: --to shorthand resolves on selected server %q; use full @name:server IDs for cross-server recipients\n", server)
					}
					if r.stderr != wantWarning {
						t.Fatalf("warning mismatch: %q", r.stderr)
					}
					wantRoot, wantReply := root, root
					if command == "topic" {
						wantRoot, wantReply = ev.EventID, ""
					}
					want := fmt.Sprintf("{\"event_id\":%q,\"room_id\":%q,\"topic_root\":%q,\"reply_to\":%q}\n", ev.EventID, room, wantRoot, wantReply)
					var receipt PublishResult
					if json.Unmarshal([]byte(r.stdout), &receipt) != nil || r.stdout != want || readFileStr(t, idPath) != ev.EventID+"\n" || e.hs.CallCount(send) != before+1 {
						t.Fatal("recipient receipt changed or not exactly one send")
					}
					wantUploads := 0
					if command == "attach" {
						wantUploads = 1
					}
					if e.hs.CallCount(uploadRoute) != wantUploads {
						t.Fatal("wrong upload count")
					}
				})
			}
		}
	}
}

// Every invalid family is checked before, between and after valid recipients.
// A transport tripwire also proves there is no media or recipient lookup HTTP.
func TestRecipientExactRefusal(t *testing.T) {
	for _, command := range []string{"post", "topic", "attach"} {
		for _, bad := range recipientRefusals() {
			for position := 0; position < 3; position++ {
				t.Run(fmt.Sprintf("%s/%s/%d", command, bad.name, position), func(t *testing.T) {
					e, room := world(t)
					root := strings.TrimSpace(e.as("alice", "topic", "new", room, "root", "Root").ok(t).stdout)
					args := recipientPublishArgs(t, command, room, root)
					targets := []string{"Same", "@same:alpha.example"}
					targets = slices.Insert(targets, position, bad.target)
					for _, target := range targets {
						args = append(args, "--to", target)
					}
					idPath := filepath.Join(outputTestDir(t), "event-id")
					args = append(args, "--json", "--id-file", idPath)
					calls := 0
					e.configure = func(a *App) {
						a.HTTP = &http.Client{Transport: memberReadTransport(func(r *http.Request) (*http.Response, error) {
							calls++
							return nil, fmt.Errorf("unexpected request")
						})}
					}
					r := e.as("alice", args...)
					if r.code != ExitUsage || r.stdout != "" || !strings.Contains(r.stderr, "invalid user") || strings.Contains(r.stderr, "LOCAL_RECIPIENT") || calls != 0 {
						t.Fatalf("recipient refusal: code=%d stdout=%q stderr=%q HTTP=%d", r.code, r.stdout, r.stderr, calls)
					}
					if bad.lower != "" && !strings.Contains(r.stderr, fmt.Sprintf("%q", bad.lower)) {
						t.Fatalf("recipient refusal: missing lowercase suggestion %q", bad.lower)
					}
					if bad.lower == "" && strings.Contains(r.stderr, "; use ") {
						t.Fatalf("recipient refusal: invalid lowercase suggestion: %q", r.stderr)
					}
					if _, err := os.Lstat(idPath); !os.IsNotExist(err) {
						t.Fatalf("recipient refusal: id-file must be absent, stat error=%v", err)
					}
				})
			}
		}
	}
}
