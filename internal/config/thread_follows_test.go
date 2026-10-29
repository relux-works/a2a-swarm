package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/codexwake"
)

// Proves local participation and unknown legacy roots grant no follow intent;
// explicit follow deduplicates, clears mute, and mute removes only intent.
func TestExplicitThreadIntent(t *testing.T) {
	s := State{Threads: []string{"$legacy"}}
	s.AddThread("$posted")
	if s.FollowsThread("!r", "$legacy") || s.FollowsThread("!r", "$posted") {
		t.Fatal("participation became follow intent")
	}
	if !s.Follow("!r", "$posted") || s.Follow("!r", "$posted") || s.Follow("", "$bad") || s.Follow("!r", "") {
		t.Fatal("follow intent validation/dedup failed")
	}
	if !s.FollowsThread("!r", "$posted") || s.FollowsThread("!other", "$posted") {
		t.Fatal("canonical room scope ignored")
	}
	if !s.Mute("$posted") || s.Mute("$posted") || s.FollowsThread("!r", "$posted") {
		t.Fatal("mute did not remove follow")
	}
	s.AddThread("$posted")
	if !s.IsMuted("$posted") || s.FollowsThread("!r", "$posted") {
		t.Fatal("post lifted mute")
	}
	if !s.Follow("!r", "$posted") || s.IsMuted("$posted") || !s.FollowsThread("!r", "$posted") {
		t.Fatal("explicit follow did not clear mute")
	}
}

func seedThreadMigration(t *testing.T) (Paths, []byte) {
	t.Helper()
	p := testPaths(t)
	s := State{NextBatch: "cursor", Threads: []string{"$unknown", "$possibly-explicit"}, Participation: []string{"$known-post"},
		Muted: []string{"$muted"}, LegacyPending: json.RawMessage(`{"events":["$opaque-pending"]}`), LegacyAcked: []string{"$legacy-receipt"},
		HookSessions: []HookSession{{ID: "hook", Blocks: 2, Rung: []string{"$old"}}}, HooksPaused: true,
		Wake:         codexwake.Ledger{Attempt: &codexwake.Attempt{ID: "attempt", Generation: "generation", IDs: []string{"$unshown"}, Output: "reserved notice", Started: time.Unix(80, 0).UTC()}, Rung: []string{"$already-rung"}, Reservations: []time.Time{time.Unix(80, 0).UTC()}, LastMailIDs: []string{"$unshown"}, LastTurn: "turn", LastTurnStatus: "inProgress", Outcome: "uncertain"},
		Terminal:     &TerminalBinding{Generation: "terminal-generation", AttemptIDs: []string{"$shown"}, Rung: []string{"$terminal-rung"}, Typed: true},
		Notification: Notification{Version: 1, NotBefore: time.Unix(100, 0).UTC()},
	}
	s.Follow("!r", "$verified")
	if err := SaveState(p, "a", s); err != nil {
		t.Fatal(err)
	}
	_, err := UpdateState(p, "a", func(_ *State) error {
		mb, err := OpenMailbox(p, "a")
		if err != nil {
			return err
		}
		if _, err := mb.Append([]MailEntry{
			{ID: "$unshown", Kind: MailEvent, Room: "!r", Rel: "thread", Event: json.RawMessage(`{"body":"retain"}`)},
			{ID: "$shown", Kind: MailEvent, Room: "!r", Rel: "thread"},
			{ID: "$received", Kind: MailEvent, Room: "!r", Rel: "to"},
		}); err != nil {
			return err
		}
		if err := mb.MarkShown([]string{"$shown"}); err != nil {
			return err
		}
		_, err = mb.Receive([]string{"$received"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(p.MailboxFile("a"))
	if err != nil {
		t.Fatal(err)
	}
	return p, data
}

func migrateThreadFixture(p Paths, after func() error) (State, error) {
	return UpdateState(p, "a", func(s *State) error {
		return migrateThreadFollows(p, "a", s, json.RawMessage(`{"v":1,"subs":[{"id":"explicit-kind","room":"!r","kinds":["decision"]}]}`), after)
	})
}

func unchangedMailbox(t *testing.T, p Paths, want []byte) {
	t.Helper()
	got, err := os.ReadFile(p.MailboxFile("a"))
	if err != nil || !bytes.Equal(want, got) {
		t.Fatalf("migration changed durable mailbox: %v", err)
	}
	mb, err := OpenMailbox(p, "a")
	if err != nil {
		t.Fatal(err)
	}
	if len(mb.Pending()) != 2 || mb.Pending()[0].Shown || !mb.Pending()[1].Shown || !mb.Received("$received") || mb.Received("$shown") {
		t.Fatal("pending/receipt/shown state changed")
	}
}

// Proves readable provenance inventory precedes cutover and repeated migration
// leaves actual durable mailbox bytes, pending/shown/receipts and other state intact.
func TestThreadMigrationInventoryAndPending(t *testing.T) {
	p, mailbox := seedThreadMigration(t)
	before, _ := LoadState(p, "a")
	s, err := migrateThreadFixture(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := s.ThreadMigration
	if s.ThreadPolicyVersion != 1 || m == nil || m.Phase != "complete" || m.Identity != "a" || len(m.Explicit) != 1 || len(m.Unknown) != 2 || len(m.Implicit) != 1 || len(m.Mutes) != 1 || len(m.Pending) != 2 || !m.Pending[1].WasShown || len(m.Received) != 1 || m.MailboxBytes != len(mailbox) || m.MailboxSHA256 == "" || !bytes.Contains(m.Subscriptions, []byte("explicit-kind")) {
		t.Fatalf("incomplete inventory: %+v", m)
	}
	if len(s.Threads) != 0 || s.FollowsThread("!r", "$unknown") || !s.FollowsThread("!r", "$verified") || !s.IsMuted("$muted") {
		t.Fatal("wrong cutover eligibility")
	}
	// Removing just migration outputs must recover the complete pre-cutover state.
	probe := s
	probe.Threads, probe.ThreadMigration, probe.ThreadPolicyVersion = before.Threads, nil, 0
	if !reflect.DeepEqual(before, probe) {
		t.Fatalf("unrelated state changed: before=%+v after=%+v", before, probe)
	}
	unchangedMailbox(t, p, mailbox)
	stateBytes, _ := os.ReadFile(p.StateFile("a"))
	if _, err := migrateThreadFixture(p, nil); err != nil {
		t.Fatal(err)
	}
	repeated, _ := os.ReadFile(p.StateFile("a"))
	if !bytes.Equal(stateBytes, repeated) {
		t.Fatal("repeat changed completed inventory")
	}
	unchangedMailbox(t, p, mailbox)
}

// Proves a process interruption after inventory or after completed cutover is
// restartable; concurrent follow/mute changes survive and old pending stays byte-exact.
func TestThreadMigrationInterruptedAndConcurrentIntent(t *testing.T) {
	for _, phase := range []string{"inventory", "complete"} {
		t.Run(phase, func(t *testing.T) {
			p, mailbox := seedThreadMigration(t)
			crash := errors.New("injected process interruption")
			_, err := UpdateState(p, "a", func(s *State) error {
				err := migrateThreadFollows(p, "a", s, json.RawMessage(`{"v":1,"subs":[]}`), func() error {
					if phase == "inventory" {
						return crash
					}
					return nil
				})
				if err != nil {
					return err
				}
				return crash
			})
			if !errors.Is(err, crash) {
				t.Fatalf("expected injected failure, got %v", err)
			}
			checkpoint, _ := LoadState(p, "a")
			if phase == "inventory" && (checkpoint.ThreadPolicyVersion != 0 || checkpoint.ThreadMigration.Phase != "inventoried" || len(checkpoint.Threads) != 2) {
				t.Fatal("inventory falsely completed")
			}
			done := make(chan error, 3)
			for _, fn := range []func(*State){
				func(s *State) { s.Follow("!r", "$concurrent") },
				func(s *State) { s.Mute("$verified") },
				func(s *State) { s.AddThread("$post-during-restart") },
			} {
				go func(fn func(*State)) {
					_, err := UpdateState(p, "a", func(s *State) error { fn(s); return nil })
					done <- err
				}(fn)
			}
			for i := 0; i < 3; i++ {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			s, err := migrateThreadFixture(p, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !s.FollowsThread("!r", "$concurrent") || s.FollowsThread("!r", "$verified") || !s.IsMuted("$verified") || s.FollowsThread("!r", "$post-during-restart") || s.ThreadMigration.Phase != "complete" {
				t.Fatalf("restart lost current intent: %+v", s)
			}
			unchangedMailbox(t, p, mailbox)
		})
	}
}

// Proves unsupported/malformed checkpoints and a corrupt durable mailbox refuse
// cutover without rewriting either file; valid migration is covered beside these.
func TestThreadMigrationRefusals(t *testing.T) {
	for _, kind := range []string{"future-version", "bad-checkpoint", "bad-completion", "corrupt-mailbox"} {
		t.Run(kind, func(t *testing.T) {
			p, _ := seedThreadMigration(t)
			s, _ := LoadState(p, "a")
			switch kind {
			case "future-version":
				s.ThreadPolicyVersion = 99
			case "bad-checkpoint":
				s.ThreadMigration = &ThreadMigration{Version: 1, Identity: "foreign", Phase: "inventoried"}
			case "bad-completion":
				s.ThreadPolicyVersion = 1
			case "corrupt-mailbox":
				if err := os.WriteFile(p.MailboxFile("a"), []byte("not-json\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveState(p, "a", s); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(p.StateFile("a"))
			mail, _ := os.ReadFile(p.MailboxFile("a"))
			wantError := map[string]string{
				"future-version":  "unsupported thread follow policy version 99",
				"bad-checkpoint":  "invalid thread follow migration checkpoint",
				"bad-completion":  "invalid thread follow migration completion",
				"corrupt-mailbox": "line 1 is not valid JSON",
			}[kind]
			if _, err := migrateThreadFixture(p, nil); err == nil || !strings.Contains(err.Error(), wantError) {
				t.Fatalf("want intended rejection %q, got %v", wantError, err)
			}
			after, _ := os.ReadFile(p.StateFile("a"))
			mailAfter, _ := os.ReadFile(p.MailboxFile("a"))
			if !bytes.Equal(before, after) || !bytes.Equal(mail, mailAfter) {
				t.Fatal("refused migration changed durable state")
			}
		})
	}
}

// Proves a writer arriving while migration owns the state lock cannot lose its
// explicit follow to the cutover, including when it re-follows an unknown root.
func TestThreadMigrationSerializesConcurrentFollow(t *testing.T) {
	p, mailbox := seedThreadMigration(t)
	inventoried := make(chan struct{})
	release := make(chan struct{})
	migration := make(chan error, 1)
	go func() {
		_, err := migrateThreadFixture(p, func() error {
			close(inventoried)
			select {
			case <-release:
				return nil
			case <-time.After(10 * time.Second):
				return errors.New("fixture release missing")
			}
		})
		migration <- err
	}()
	<-inventoried
	writerStarted := make(chan struct{})
	writer := make(chan error, 1)
	go func() {
		close(writerStarted)
		_, err := UpdateState(p, "a", func(s *State) error { s.Follow("!r", "$unknown"); return nil })
		writer <- err
	}()
	<-writerStarted
	close(release)
	if err := <-migration; err != nil {
		t.Fatal(err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
	s, err := migrateThreadFixture(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !s.FollowsThread("!r", "$unknown") || !s.FollowsThread("!r", "$verified") || !s.IsMuted("$muted") || len(s.ThreadMigration.Explicit) != 1 {
		t.Fatal("concurrent follow lost or historical inventory rewritten")
	}
	unchangedMailbox(t, p, mailbox)
}

// Proves persisted cursor and synthetic binding-generation replacement retain
// explicit intent and cannot turn participation into consent. This exercises
// the state boundary only, not a deployed native session-host rebind.
func TestThreadIntentAcrossGenerationReplacement(t *testing.T) {
	p := testPaths(t)
	s := State{Participation: []string{"$posted"}, NextBatch: "before", Terminal: &TerminalBinding{Generation: "before"}}
	s.Follow("!r", "$explicit")
	if err := SaveState(p, "a", s); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []string{"replacement", "restart"} {
		if _, err := UpdateState(p, "a", func(s *State) error {
			s.NextBatch = generation
			s.Terminal = &TerminalBinding{Generation: generation, AttemptIDs: []string{"$pending"}}
			s.Wake = codexwake.Ledger{Rung: []string{"$handled"}, LastMailIDs: []string{"$pending"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		got, err := LoadState(p, "a")
		if err != nil || !got.FollowsThread("!r", "$explicit") || got.FollowsThread("!r", "$posted") || got.FollowsThread("!r", "$handled") || got.FollowsThread("!r", "$pending") {
			t.Fatalf("generation changed follow intent: generation=%s err=%v", generation, err)
		}
	}
}
