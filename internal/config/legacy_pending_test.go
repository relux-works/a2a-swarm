package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Proves supported old batches deduplicate exact IDs and reject unsupported
// shapes/invalid IDs with the intended recovery error, without interpreting bodies.
func TestLegacyPendingIDs(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		valid bool
		count int
	}{
		{`{"next_batch":"unused","events":["$one","$one","$two"]}`, true, 2},
		{`{"events":[]}`, true, 0}, {`null`, true, 0}, {``, true, 0},
		{`{}`, false, 0}, {`{"events":null}`, false, 0}, {`{"events":["$lost"],"events":[]}`, false, 0}, {`{"events":["not-an-event"]}`, false, 0},
		{`{"events":["$bad\n"]}`, false, 0}, {`{"events":[7]}`, false, 0}, {`{"events":[],"body":"unsupported"}`, false, 0},
	} {
		ids, err := LegacyPendingIDs(json.RawMessage(tc.raw))
		if tc.valid {
			if err != nil || len(ids) != tc.count {
				t.Fatalf("valid batch %q: %v %v", tc.raw, ids, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "legacy pending recovery:") {
			t.Fatalf("wanted specific recovery refusal for %q: %v", tc.raw, err)
		}
	}
}

// Proves inventory duplicates neither pending body, retains metadata, and leaves
// real pending recoverable. Normal ack+compaction removes only received payload.
func TestThreadMigrationMetadataAfterCompaction(t *testing.T) {
	p := testPaths(t)
	const first = "fixture received body must disappear"
	const second = "fixture still pending body must survive"
	if err := SaveState(p, "a", State{NextBatch: "cursor"}); err != nil {
		t.Fatal(err)
	}
	_, err := UpdateState(p, "a", func(s *State) error {
		mb, err := OpenMailbox(p, "a")
		if err != nil {
			return err
		}
		_, err = mb.Append([]MailEntry{{ID: "$first", Kind: MailEvent, Room: "!r", Event: json.RawMessage(`{"body":"` + first + `"}`)}, {ID: "$second", Kind: MailEvent, Room: "!r", Event: json.RawMessage(`{"body":"` + second + `"}`)}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p.MailboxFile("a"))
	st, err := UpdateState(p, "a", func(s *State) error { return MigrateThreadFollows(p, "a", s, json.RawMessage(`{"subs":[]}`)) })
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p.MailboxFile("a"))
	inventory, _ := json.Marshal(st.ThreadMigration)
	if !bytes.Equal(before, after) || bytes.Contains(inventory, []byte(first)) || bytes.Contains(inventory, []byte(second)) || !bytes.Contains(inventory, []byte("$second")) {
		t.Fatal("inventory copied bodies or changed durable mailbox")
	}
	_, err = UpdateState(p, "a", func(s *State) error {
		mb, err := OpenMailbox(p, "a")
		if err != nil {
			return err
		}
		if _, err = mb.Receive([]string{"$first"}); err != nil {
			return err
		}
		return mb.compact()
	})
	if err != nil {
		t.Fatal(err)
	}
	compacted, _ := os.ReadFile(p.MailboxFile("a"))
	mb, err := OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(compacted, []byte(first)) || !bytes.Contains(compacted, []byte(second)) || !mb.Received("$first") || len(mb.Pending()) != 1 || mb.Pending()[0].ID != "$second" {
		t.Fatal("ack/compaction changed outstanding obligation or retained received body")
	}
	stateBytes, _ := os.ReadFile(p.StateFile("a"))
	if bytes.Contains(stateBytes, []byte(first)) || bytes.Contains(stateBytes, []byte(second)) {
		t.Fatal("received payload outlived compaction in inventory")
	}
}

// Proves follow clears every duplicate mute found in hand-edited legacy state,
// while preserving a different root's mute and delivering valid explicit intent.
func TestThreadFollowClearsDuplicateMutes(t *testing.T) {
	s := State{Muted: []string{"$root", "$other", "$root"}}
	if !s.Follow("!r", "$root") || s.IsMuted("$root") || !s.IsMuted("$other") || !s.FollowsThread("!r", "$root") {
		t.Fatal("follow reported clear while duplicate mute remained")
	}
}

// Proves an unresolved old pending ID with no recovery cursor refuses cutover
// before inventory writes; an already-durable ID with no cursor remains safe.
func TestThreadMigrationLegacyMissingCursor(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "unresolved", true: "already-durable"}[durable], func(t *testing.T) {
			p := testPaths(t)
			if err := SaveState(p, "a", State{Threads: []string{"$root"}, LegacyPending: json.RawMessage(`{"events":["$pending"]}`)}); err != nil {
				t.Fatal(err)
			}
			if durable {
				if _, err := UpdateState(p, "a", func(s *State) error {
					mb, err := OpenMailbox(p, "a")
					if err != nil {
						return err
					}
					_, err = mb.Append([]MailEntry{{ID: "$pending", Kind: MailEvent, Room: "!r"}})
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(p.StateFile("a"))
			_, err := UpdateState(p, "a", func(s *State) error { return MigrateThreadFollows(p, "a", s, json.RawMessage(`{"subs":[]}`)) })
			if durable {
				if err != nil {
					t.Fatalf("durable control refused: %v", err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "missing sync cursor for unresolved event") {
					t.Fatalf("wrong missing-cursor refusal: %v", err)
				}
				after, _ := os.ReadFile(p.StateFile("a"))
				if !bytes.Equal(before, after) {
					t.Fatal("unrecoverable initial state was changed")
				}
			}
		})
	}
}

// Proves an authorized local receipt between the inventory checkpoint and retry
// survives resumed cutover; resumption writes neither mail nor another receipt.
func TestThreadMigrationOfflineReceiptDuringRestart(t *testing.T) {
	p, _ := seedThreadMigration(t)
	stop := fmt.Errorf("inventory checkpoint interruption")
	if _, err := migrateThreadFixture(p, func() error { return stop }); !errors.Is(err, stop) {
		t.Fatalf("wrong injected failure: %v", err)
	}
	_, err := UpdateState(p, "a", func(s *State) error {
		mb, err := OpenMailbox(p, "a")
		if err != nil {
			return err
		}
		_, err = mb.Receive([]string{"$unshown"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p.MailboxFile("a"))
	if _, err = migrateThreadFixture(p, nil); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(p.MailboxFile("a"))
	mb, err := OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !mb.Received("$unshown") || mb.Received("$shown") || len(mb.Pending()) != 1 {
		t.Fatal("resumed migration rewrote local disposition")
	}
}
