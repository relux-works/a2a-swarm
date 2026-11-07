package cli

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Exercise the wire mentions as well as stderr: a shorthand never discovers a
// same-named account on another server, even if that account is in the list.
func TestRecipientServerWarning(t *testing.T) {
	for _, server := range []string{"alpha.example", "beta.example"} {
		for _, command := range []string{"post", "topic", "attach"} {
			for _, tc := range []struct {
				name, to string
				warn     bool
				mentions []string
			}{
				{"bare", "bob", true, []string{"@bob:" + server}},
				{"at-shorthand", "@bob", true, []string{"@bob:" + server}},
				{"empty-server", "bob:", true, []string{"@bob:" + server}},
				{"mixed", "bob,@bob:remote.example,@carol", true, []string{"@bob:" + server, "@bob:remote.example", "@carol:" + server}},
				{"explicit-local", "@bob:" + server, false, []string{"@bob:" + server}},
				{"explicit-remote", "@bob:remote.example", false, []string{"@bob:remote.example"}},
				{"all", "all", false, nil},
				{"default", "", false, nil},
				{"invalid", "bob,bad user", false, nil},
			} {
				t.Run(server+"/"+command+"/"+tc.name, func(t *testing.T) {
					e := newEnv(t)
					e.hs.server = server
					alice, bob := e.agent("alice"), e.agent("bob")
					room := e.hs.addRoom("proj", alice, bob)
					root := strings.TrimSpace(e.as("alice", "topic", "new", room, "root", "Root").ok(t).stdout)
					args := []string{command, room, "--topic", root}
					if command == "topic" {
						args = []string{"topic", "new", room, "addressed", "Addressed"}
					} else {
						if command == "attach" {
							args = append(args, writeFile(t, t.TempDir(), "notes.txt", []byte("fixture\n")))
						}
						args = append(args, "body")
					}
					args = append(args, "--to", tc.to)
					const send = "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}"
					before := e.hs.CallCount(send)
					r := e.as("alice", args...)
					warning := "a2a: warning [LOCAL_RECIPIENT]: --to shorthand resolves on selected server \"" + server + "\"; use full @name:server IDs for cross-server recipients\n"
					wantWarning := ""
					if tc.warn {
						wantWarning = warning
					}
					if tc.name == "invalid" {
						if r.code != ExitUsage || r.stdout != "" || !strings.Contains(r.stderr, "invalid user") || strings.Contains(r.stderr, "LOCAL_RECIPIENT") || e.hs.CallCount(send) != before || e.hs.CallCount(uploadRoute) != 0 {
							t.Fatal("invalid recipient did not refuse without warning or publication")
						}
						return
					}
					r.ok(t)
					if r.stderr != wantWarning {
						t.Errorf("stderr = %q, want %q", r.stderr, wantWarning)
					}
					ev := e.hs.lastEvent()
					if r.stdout != ev.EventID+"\n" || e.hs.CallCount(send) != before+1 {
						t.Fatal("event ID did not identify exactly one publication")
					}
					m := ev.Message()
					if m.Mentions == nil || !slices.Equal(m.Mentions.UserIDs, tc.mentions) {
						t.Errorf("mentions = %+v, want %v", m.Mentions, tc.mentions)
					}
				})
			}
		}
	}
}

// For each publisher, shorthand emits exactly one stderr warning without
// contaminating the single JSON receipt on stdout or the id-only file.
func TestRecipientServerWarningJSON(t *testing.T) {
	for _, command := range []string{"post", "topic", "attach"} {
		t.Run(command, func(t *testing.T) {
			e, room := world(t)
			root := strings.TrimSuffix(e.as("alice", "topic", "new", room, "root", "Root").ok(t).stdout, "\n")
			args := []string{command, room, "--topic", root}
			if command == "topic" {
				args = []string{"topic", "new", room, "addressed", "Addressed"}
			} else {
				if command == "attach" {
					args = append(args, writeFile(t, t.TempDir(), "notes.txt", []byte("fixture\n")))
				}
				args = append(args, "body")
			}
			idPath := filepath.Join(outputTestDir(t), "event-id")
			args = append(args, "--to", "bob", "--json", "--id-file", idPath)
			r := e.as("alice", args...).ok(t)
			ev := e.hs.lastEvent()
			wantRoot, wantReply := root, root
			if command == "topic" {
				wantRoot, wantReply = ev.EventID, ""
			}
			want := fmt.Sprintf("{\"event_id\":%q,\"room_id\":%q,\"topic_root\":%q,\"reply_to\":%q}\n", ev.EventID, room, wantRoot, wantReply)
			if r.stdout != want {
				t.Errorf("stdout = %q, want %q", r.stdout, want)
			}
			var receipt PublishResult
			// Unmarshal consumes the whole input, rejecting trailing JSON values or text.
			if err := json.Unmarshal([]byte(r.stdout), &receipt); err != nil {
				t.Fatalf("stdout is not one JSON receipt: %v", err)
			}
			warning := "a2a: warning [LOCAL_RECIPIENT]: --to shorthand resolves on selected server \"" + testServer + "\"; use full @name:server IDs for cross-server recipients\n"
			if r.stderr != warning {
				t.Errorf("stderr = %q, want %q", r.stderr, warning)
			}
			if got := readFileStr(t, idPath); got != ev.EventID+"\n" {
				t.Errorf("id file = %q, want %q", got, ev.EventID+"\n")
			}
		})
	}
}
