package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const ExplicitThreadPolicyVersion = 1

// ThreadFollow is explicit intent belonging to the identity whose state holds it.
// Room and Root are canonical Matrix IDs; action and scope make intent inspectable.
type ThreadFollow struct {
	Room   string `json:"room"`
	Root   string `json:"root"`
	Action string `json:"action"`
	Scope  string `json:"scope"`
}

// FollowsThread never uses participation, cursors, receipts or generation.
func (s State) FollowsThread(room, root string) bool {
	for _, f := range s.ThreadFollows {
		if f.Room == room && f.Root == root && f.Action == "follow" && f.Scope == "thread" {
			return true
		}
	}
	return false
}

// ThreadMigration is an immutable before-cutover inventory plus a checkpoint.
// Unknown legacy roots are deliberately not called implicit or explicit. Implicit
// records only local participation whose origin the current writer actually knows.
// Pending records the existing legacy tail, not permission to receipt or delete mail.
type ThreadMigration struct {
	Version             int                 `json:"version"`
	Phase               string              `json:"phase"`
	Identity            string              `json:"identity"`
	Explicit            []ThreadFollow      `json:"explicit"`
	Subscriptions       json.RawMessage     `json:"explicit_subscriptions"`
	Implicit            []string            `json:"participation_only"`
	Unknown             []string            `json:"unknown_legacy_roots"`
	Mutes               []string            `json:"mutes"`
	Pending             []ThreadPending     `json:"pending_legacy_tail"`
	Received            []string            `json:"received"`
	LegacyPending       json.RawMessage     `json:"legacy_pending,omitempty"`
	LegacyAcked         []string            `json:"legacy_acked,omitempty"`
	LegacySubscriptions map[string][]string `json:"legacy_subscriptions,omitempty"`
	MailboxSHA256       string              `json:"mailbox_sha256"`
	MailboxBytes        int                 `json:"mailbox_bytes"`
	Result              string              `json:"result"`
}

type ThreadPending struct {
	MailEntry
	WasShown bool `json:"shown"`
}

// MigrateThreadFollows must run under UpdateState's lock. The inventory is saved
// and directory-synced before cutover. Completion is another atomic state write:
// either the old state, inventoried checkpoint or complete policy survives a crash.
// A retry uses CURRENT intent, never restores follows/mutes from the inventory.
// The mailbox is read only; no pending, receipt, shown or wake state is changed.
func MigrateThreadFollows(p Paths, name string, s *State, subscriptions json.RawMessage) error {
	return migrateThreadFollows(p, name, s, subscriptions, nil)
}

// afterInventory injects a bounded failure at the durable checkpoint in tests.
func migrateThreadFollows(p Paths, name string, s *State, subscriptions json.RawMessage, afterInventory func() error) error {
	if err := ValidateName(name); err != nil {
		return err
	}
	if s.ThreadPolicyVersion == ExplicitThreadPolicyVersion {
		if s.ThreadMigration == nil || s.ThreadMigration.Version != ExplicitThreadPolicyVersion || s.ThreadMigration.Phase != "complete" || s.ThreadMigration.Identity != name {
			return fmt.Errorf("invalid thread follow migration completion")
		}
		return nil
	}
	if s.ThreadPolicyVersion != 0 {
		return fmt.Errorf("unsupported thread follow policy version %d", s.ThreadPolicyVersion)
	}
	if s.ThreadMigration == nil {
		mb, err := OpenMailbox(p, name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p.MailboxFile(name))
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		sum := sha256.Sum256(data)
		m := &ThreadMigration{Version: ExplicitThreadPolicyVersion, Phase: "inventoried", Identity: name,
			Explicit: append([]ThreadFollow{}, s.ThreadFollows...), Subscriptions: append(json.RawMessage(nil), subscriptions...),
			Implicit: append([]string{}, s.Participation...), Unknown: append([]string{}, s.Threads...),
			Mutes: append([]string{}, s.Muted...), Pending: []ThreadPending{}, Received: append([]string{}, mb.received...),
			LegacySubscriptions: s.LegacySubscriptions, LegacyPending: append(json.RawMessage(nil), s.LegacyPending...), LegacyAcked: append([]string(nil), s.LegacyAcked...),
			MailboxSHA256: hex.EncodeToString(sum[:]), MailboxBytes: len(data),
			Result: "inventory durable; cutover pending; existing pending obligations retained"}
		for _, e := range mb.Pending() {
			m.Pending = append(m.Pending, ThreadPending{MailEntry: e, WasShown: e.Shown})
		}
		s.ThreadMigration = m
		if err := saveThreadCheckpoint(p, name, *s); err != nil {
			return err
		}
	}
	m := s.ThreadMigration
	if m.Version != ExplicitThreadPolicyVersion || m.Phase != "inventoried" || m.Identity != name {
		return fmt.Errorf("invalid thread follow migration checkpoint")
	}
	if afterInventory != nil {
		if err := afterInventory(); err != nil {
			return err
		}
	}
	// Retire unknown future eligibility without touching genuine explicit intent.
	s.Threads = nil
	s.ThreadPolicyVersion = ExplicitThreadPolicyVersion
	m.Phase = "complete"
	m.Result = "unknown legacy roots excluded; participation grants no eligibility; explicit follows and mutes preserved; existing pending obligations retained"
	return saveThreadCheckpoint(p, name, *s)
}

func saveThreadCheckpoint(p Paths, name string, s State) error {
	if err := SaveState(p, name, s); err != nil {
		return err
	}
	// SaveState fsyncs the file before rename; sync the directory for checkpoint
	// durability across a host crash, in addition to ordinary process interruption.
	dir, err := os.Open(filepath.Dir(p.StateFile(name)))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
