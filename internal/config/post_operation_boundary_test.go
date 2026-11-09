//go:build darwin || linux

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Claim: 0755 state roots work unchanged while writable roots/public journal components refuse; local mode fixtures only.
func TestPostJournalStateRootBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		baseMode   os.FileMode
		unsafePart string
	}{
		{"public_state_root", 0755, ""},
		{"writable_state_root", 0775, ""},
		{"public_journal_root", 0755, "post-operations"},
		{"public_journal_class", 0755, "post-operations/agents"},
		{"public_journal_identity", 0755, "post-operations/agents/alice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(base, tc.baseMode); err != nil {
				t.Fatal(err)
			}
			p := Paths{Base: base}
			if tc.unsafePart != "" {
				j, err := OpenPostJournal(p, "alice", false, true)
				if err != nil {
					t.Fatal(err)
				}
				j.Close()
				if err := os.Chmod(filepath.Join(base, tc.unsafePart), 0755); err != nil {
					t.Fatal(err)
				}
			}
			for _, create := range []bool{true, false} {
				j, err := OpenPostJournal(p, "alice", false, create)
				if tc.baseMode == 0775 || tc.unsafePart != "" {
					if j != nil {
						j.Close()
					}
					if err == nil {
						t.Fatalf("unsafe path accepted (create=%v)", create)
					}
					continue
				}
				if err != nil {
					t.Fatalf("0755 state root refused (create=%v): %v", create, err)
				}
				if create {
					r := mustPrepareOperation(t, j, operationCandidate("boundary", 1))
					r, err = j.ReserveAttempt(r)
					if err == nil {
						_, err = j.RecordACK(r, "$ack", 200, 1)
					}
					if err != nil {
						t.Fatal(err)
					}
				} else if r, ok, err := j.Read("boundary"); err != nil || !ok || r.Outcome != "acknowledged" {
					t.Fatalf("durable ACK missing: ok=%v err=%v", ok, err)
				}
				j.Close()
			}
			st, err := os.Stat(base)
			if err != nil || st.Mode().Perm() != tc.baseMode {
				t.Fatal("state root permissions changed")
			}
			if tc.baseMode == 0775 {
				if _, err := os.Lstat(filepath.Join(base, "post-operations")); !os.IsNotExist(err) {
					t.Fatal("refused state root acquired a journal")
				}
			}
		})
	}
}

// Claim: A writable state ancestor prevents journal open; isolated mode change, not hostile replacement.
func TestPostJournalUnsafeStateAncestor(t *testing.T) {
	ancestor, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(ancestor, "state")
	if err := os.Mkdir(base, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(ancestor, 0775); err != nil {
		t.Fatal(err)
	}
	if j, err := OpenPostJournal(Paths{Base: base}, "alice", false, true); err == nil {
		j.Close()
		t.Fatal("writable state ancestor accepted")
	}
}

type operationModeInfo struct {
	os.FileInfo
	mode os.FileMode
}

func (s operationModeInfo) Mode() os.FileMode { return s.mode }
func (s operationModeInfo) IsDir() bool       { return s.mode.IsDir() }

// Single-UID sandboxes cannot chown to another user. Inject precisely the
// metadata used by the production predicate; this is not real-chown evidence.
// Claim: own private journal entries and safe root ancestors pass; foreign or special-mode entries fail under the listed metadata cases.
func TestPostJournalBoundaryOwnerAndSpecialModesSynthetic(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(base)
	if err != nil {
		t.Fatal(err)
	}
	info := func(uid uint32, mode os.FileMode) os.FileInfo {
		foreign := operationForeignInfo{FileInfo: st, stat: *st.Sys().(*syscall.Stat_t)}
		foreign.stat.Uid = uid
		return operationModeInfo{FileInfo: foreign, mode: mode}
	}
	own := uint32(os.Geteuid())
	foreign := own + 1
	for _, tc := range []struct {
		name   string
		uid    uint32
		mode   os.FileMode
		strict bool
		want   bool
	}{
		{"own_public_base", own, os.ModeDir | 0755, false, true},
		{"foreign_base", foreign, os.ModeDir | 0755, false, false},
		{"foreign_private_base", foreign, os.ModeDir | 0700, false, false},
		{"root_base", 0, os.ModeDir | 0755, false, true},
		{"writable_base", own, os.ModeDir | 0775, false, false},
		{"root_sticky_ancestor", 0, os.ModeDir | os.ModeSticky | 0777, false, true},
		{"root_nonsticky_ancestor", 0, os.ModeDir | 0777, false, false},
		{"foreign_sticky_ancestor", foreign, os.ModeDir | os.ModeSticky | 0777, false, false},
		{"private_journal_dir", own, os.ModeDir | 0700, true, true},
		{"public_journal_dir", own, os.ModeDir | 0755, true, false},
		{"foreign_journal_dir", foreign, os.ModeDir | 0700, true, false},
		{"setuid_journal_dir", own, os.ModeDir | os.ModeSetuid | 0700, true, false},
		{"setgid_journal_dir", own, os.ModeDir | os.ModeSetgid | 0700, true, false},
		{"sticky_journal_dir", own, os.ModeDir | os.ModeSticky | 0700, true, false},
		{"private_journal_file", own, 0600, true, true},
		{"public_journal_file", own, 0644, true, false},
		{"foreign_journal_file", foreign, 0600, true, false},
		{"setuid_journal_file", own, os.ModeSetuid | 0600, true, false},
		{"setgid_journal_file", own, os.ModeSetgid | 0600, true, false},
		{"sticky_journal_file", own, os.ModeSticky | 0600, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := postOperationOwner(info(tc.uid, tc.mode), tc.strict); got != tc.want {
				t.Fatalf("owner/mode admission=%v want=%v", got, tc.want)
			}
		})
	}
}
