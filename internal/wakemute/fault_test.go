//go:build linux || darwin

package wakemute_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// Each fault is injected at a named durability boundary, not a setup failure.
// Every possible commit remains discoverable under its original public run;
// partial roots never produce complete-scope success. Exact undo is exercised.
func TestScopeMuteDurabilityFaultsAndPartialUndo(t *testing.T) {
	for _, point := range []string{"before-create", "before-file-sync", "after-file-sync", "before-rename", "after-rename", "after-directory-sync"} {
		for _, write := range []int{1, 2, 3, 4, 5, 6, 7, 8} {
			t.Run(point+string(rune('0'+write)), func(t *testing.T) {
				scope := append(fixture.Scope(t.TempDir()), fixture.Scope(t.TempDir())...)
				scope, _ = wakemute.Normalize(scope)
				fixture.Enroll(t, scope)
				op := fixture.Confirm(t, scope, "fault", "mute")
				hit := 0
				planted := errors.New("injected persistence boundary")
				result, err := wakemute.FaultMuteForTest(fixture.Context(t), op, func(p string) error {
					if p == point {
						hit++
						if hit == write {
							return planted
						}
					}
					return nil
				})
				if !errors.Is(err, wakemute.ErrUncertain) || hit != write || result.Status == "muted" || len(result.Roots) != 2 {
					t.Fatalf("fault not witnessed: %v %+v hits=%d", err, result, hit)
				}
				status, e := wakemute.Status(fixture.Context(t), scope)
				if e != nil && !errors.Is(e, wakemute.ErrUncertain) && !errors.Is(e, wakemute.ErrUnsafe) {
					t.Fatal(e)
				}
				active := false
				for _, root := range status.Roots {
					for _, operation := range root.Operations {
						if operation.ID == op.ID {
							active = true
							if operation.Run != op.Run {
								t.Fatal("recovery run lost")
							}
						}
					}
					// Atomic replacement leaves no incomplete JSON and scratch is removed.
					entries, _ := os.ReadDir(root.Root)
					if len(entries) != 2 {
						t.Fatal("temporary files leaked")
					}
				}
				expectedCommit := write > 1 || point == "after-rename" || point == "after-directory-sync"
				if active != expectedCommit {
					t.Fatalf("wrong exact durable poststate: active=%t", active)
				}
				if active {
					if _, e = wakemute.Unmute(fixture.Context(t), scope, op.Run, op.ID); e != nil {
						t.Fatal(e)
					}
				}
				for _, root := range scope {
					if e = admission(t, root.Root); e != nil {
						t.Fatal(e)
					}
				}
				good := fixture.Mute(t, scope, "adjacent-success")
				fixture.Unmute(t, good)
			})
		}
	}
}

// Losing stdout is caller-side loss, not an invisible operation or rollback.
// A newly constructed status reader finds the same committed run and exact ID.
func TestScopeMuteLostResultRecovery(t *testing.T) {
	scope := fixture.Scope(t.TempDir())
	fixture.Enroll(t, scope)
	approved := fixture.Confirm(t, scope, "lost-result", "mute")
	if _, err := wakemute.Mute(fixture.Context(t), approved); err != nil {
		t.Fatal(err)
	} // Deliberately discard result.
	status, err := wakemute.Status(fixture.Context(t), scope)
	if err != nil || len(status.Roots) != 1 || len(status.Roots[0].Operations) != 1 || status.Roots[0].Operations[0].ID != approved.ID || status.Roots[0].Operations[0].Run != "lost-result" || status.Quiesced {
		t.Fatal("lost caller result stranded invisible mute")
	}
	fixture.Unmute(t, status.Roots[0].Operations[0])
}

// Faults in closing one operation never lift a concurrent operation, including
// after some roots commit their thaw. A retry finishes only the recorded undo.
func TestScopeMuteUnmuteFaultsRetainOverlap(t *testing.T) {
	for _, point := range []string{"before-create", "after-file-sync", "after-rename", "after-directory-sync"} {
		t.Run(point, func(t *testing.T) {
			scope := append(fixture.Scope(t.TempDir()), fixture.Scope(t.TempDir())...)
			scope, _ = wakemute.Normalize(scope)
			fixture.Enroll(t, scope)
			a := fixture.Mute(t, scope, "A")
			b := fixture.Mute(t, scope, "B")
			calls := 0
			_, err := wakemute.FaultUnmuteForTest(fixture.Context(t), scope, a.Run, a.ID, func(p string) error {
				if p == point {
					calls++
					if calls == 3 {
						return errors.New("injected close")
					}
				}
				return nil
			})
			if !errors.Is(err, wakemute.ErrUncertain) || calls != 3 {
				t.Fatal("close fault did not fire")
			}
			for _, root := range scope {
				called := false
				refused := wakemute.Admission(fixture.Context(t), root.Root, ".", func(uint64) error { called = true; return nil })
				if called || (!errors.Is(refused, wakemute.ErrMuted) && !errors.Is(refused, wakemute.ErrUncertain)) {
					t.Fatalf("close fault lifted overlapping B: %v", refused)
				}
				state, _ := wakemute.Status(fixture.Context(t), scope)
				found := false
				for _, row := range state.Roots {
					if row.Root == root.Root {
						for _, op := range row.Operations {
							if op.ID == b.ID && op.State == "active" {
								found = true
							}
						}
					}
				}
				if !found {
					t.Fatal("close fault erased overlapping B recovery record")
				}
			}
			fixture.Unmute(t, a)
			fixture.Unmute(t, b)
		})
	}
}

// Owner/type/mode failures are rejected before creating even a temporary entry.
// A read-only scope report does not repair missing or unreadable enrollment.
func TestScopeMuteEnrollmentAbsenceSafety(t *testing.T) {
	root := t.TempDir()
	scope := fixture.Scope(root)
	fixture.Enroll(t, scope)
	os.Chmod(filepath.Join(root, wakemute.Ledger), 0000)
	_, err := wakemute.Status(fixture.Context(t), scope)
	if err == nil {
		t.Fatal("unreadable admitted state accepted")
	}
	info, _ := os.Stat(filepath.Join(root, wakemute.Ledger))
	if info.Mode().Perm() != 0 {
		t.Fatal("status repaired permissions")
	}
}
