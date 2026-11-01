package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/relux-works/skill-go-testing-tools/tuitestkit"
)

// Both commands must preserve the same ordered recipient list in the actual
// header, readable body and mentions. Defaults, duplicates and empty segments
// retain comma-list semantics; these fixtures use only a fake homeserver.
func TestRecipientFlags(t *testing.T) {
	cases := []struct {
		name, names, mentions             string
		beforeRoom, beforeBody, afterBody []string
		literalBody                       bool
	}{
		{name: "default", names: "all"},
		{name: "single", beforeBody: []string{"--to", "alice"}, names: "alice", mentions: "@alice:test.local"},
		{name: "comma-list", beforeBody: []string{"--to", "alice,@carol,@remote:else.local"}, names: "alice,carol,@remote:else.local", mentions: "@alice:test.local,@carol:test.local,@remote:else.local"},
		{name: "repeated", beforeBody: []string{"--to", "alice", "--to", "@carol"}, names: "alice,carol", mentions: "@alice:test.local,@carol:test.local"},
		{name: "mixed-comma-equals", beforeBody: []string{"--to= alice, @remote:else.local ", "--to", "@carol:test.local,bob"}, names: "alice,@remote:else.local,carol,bob", mentions: "@alice:test.local,@remote:else.local,@carol:test.local,@bob:test.local"},
		{name: "duplicates", beforeBody: []string{"--to", "alice,alice", "--to=@alice:test.local"}, names: "alice,alice,alice", mentions: "@alice:test.local,@alice:test.local,@alice:test.local"},
		{name: "all-and-named", beforeBody: []string{"--to=all", "--to", "alice"}, names: "all,alice", mentions: "@alice:test.local"},
		{name: "explicit-empty", beforeBody: []string{"--to="}, names: "all"},
		{name: "only-empty-segments", beforeBody: []string{"--to", " , ", "--to="}, names: "all"},
		{name: "empty-first", beforeBody: []string{"--to=", "--to=alice"}, names: "alice", mentions: "@alice:test.local"},
		{name: "empty-last", beforeBody: []string{"--to=alice", "--to= , "}, names: "alice", mentions: "@alice:test.local"},
		{name: "interspersed", beforeRoom: []string{"--to=alice"}, beforeBody: []string{"--to", "@carol"}, afterBody: []string{"--to=@remote:else.local"}, names: "alice,carol,@remote:else.local", mentions: "@alice:test.local,@carol:test.local,@remote:else.local"},
		{name: "end-of-options", beforeBody: []string{"--to=alice", "--to=@carol", "--"}, names: "alice,carol", mentions: "@alice:test.local,@carol:test.local", literalBody: true},
	}
	for _, command := range []string{"post", "attach"} {
		t.Run(command, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					e, _ := world(t)
					root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "recipients", "Recipients").ok(t).stdout)
					args := append([]string{command}, tc.beforeRoom...)
					args = append(args, "proj", "--topic", root)
					if command == "attach" {
						args = append(args, writeFile(t, t.TempDir(), "notes.txt", []byte("recipient fixture\n")))
					}
					args = append(args, tc.beforeBody...)
					body := "recipient body"
					if tc.literalBody {
						body = "--to=literal-body"
					}
					args = append(args, body)
					args = append(args, tc.afterBody...)
					before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
					r := e.as("bob", args...).ok(t)
					ev, ok := e.hs.eventByID(strings.TrimSpace(r.stdout))
					if !ok {
						t.Fatalf("returned event not sent: %q", r.stdout)
					}
					m := ev.Message()
					h, ok := decodeHeader(m.A2A)
					if !ok || !slices.Equal(h.To, strings.Split(tc.names, ",")) || h.Topic != root || ev.ThreadRoot() != root {
						t.Errorf("header = %+v, want To %q and root %q", h, tc.names, root)
					}
					var wantMentions []string
					if tc.mentions != "" {
						wantMentions = strings.Split(tc.mentions, ",")
					}
					if m.Mentions == nil || !slices.Equal(m.Mentions.UserIDs, wantMentions) {
						t.Errorf("mentions = %+v, want %v", m.Mentions, wantMentions)
					}
					tuitestkit.ContainsStr(t, m.Body, "- to: "+strings.ReplaceAll(tc.names, ",", ", ")+"\n")
					tuitestkit.ContainsStr(t, m.Body, "\n\n"+body)
					tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}", before+1)
					uploads := 0
					if command == "attach" {
						uploads = 1
					}
					tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, uploadRoute, uploads)
				})
			}
		})
	}
}

// A malformed occurrence anywhere must refuse before send or media access,
// even when a later occurrence is valid. A separate valid corpus above proves
// that rejecting every invocation cannot satisfy these expectations.
func TestRecipientFlagsRefuseInvalidOccurrence(t *testing.T) {
	cases := []struct {
		name  string
		flags []string
	}{
		{"first", []string{"--to", "bad user", "--to", "alice"}},
		{"middle", []string{"--to=alice", "--to=bad user", "--to=@carol"}},
		{"comma-in-first", []string{"--to", "alice,bad user", "--to=bob"}},
		{"last", []string{"--to=alice", "--to=bad user"}},
		{"only", []string{"--to=bad user"}},
	}
	for _, command := range []string{"post", "attach"} {
		t.Run(command, func(t *testing.T) {
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					e, _ := world(t)
					root := strings.TrimSpace(e.as("alice", "topic", "new", "proj", "recipients", "Recipients").ok(t).stdout)
					args := []string{command, "proj", "--topic", root}
					if command == "attach" {
						args = append(args, writeFile(t, t.TempDir(), "notes.txt", []byte("recipient fixture\n")))
					}
					args = append(args, tc.flags...)
					args = append(args, "recipient body")
					before := e.hs.CallCount("PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}")
					r := e.as("bob", args...)
					if r.code != ExitUsage {
						t.Errorf("exit = %d, want usage refusal %d; stderr %q", r.code, ExitUsage, r.stderr)
					}
					if r.stdout != "" {
						t.Errorf("refusal published an event id: %q", r.stdout)
					}
					tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, "PUT /_matrix/client/v3/rooms/{room}/send/{type}/{txn}", before)
					tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, uploadRoute, 0)
					tuitestkit.AssertCalledN(t, &e.hs.MockCallRecorder, mediaConfigRoute, 0)
					tuitestkit.ContainsStr(t, r.stderr, "invalid user")
				})
			}
		})
	}
}
