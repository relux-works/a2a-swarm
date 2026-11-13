//go:build linux || darwin

package wakemute_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// A reader-validated 499-operation history reaches 998. All four selected
// connection counters and the root then cross 999 -> 1000 at cleanup.
// The finite reserved envelope includes newline, maximum uint64 decimal widths
// and installing (the longest nonclosed state, including recovery unions).
// Fixtures synthesize retained history; they are not 499 live owner operations.
func cleanupCapacityFixture(t *testing.T, target int, reserved bool, overlapping ...bool) (wakemute.Scope, wakemute.Operation) {
	t.Helper()
	root := t.TempDir()
	connections := make([]wakemute.Connection, 4)
	for i := range connections {
		name := strings.Repeat(string(rune('a'+i)), 63) + fmt.Sprint(i)
		connections[i] = wakemute.Connection{Name: name, Path: "connections/" + name}
	}
	// Lengthen only private fixture paths so 499 bounded run references can
	// reach the byte boundary without unbounded or invalid operation fields.
	prototype, err := wakemute.NewOperation(wakemute.Scope{{Root: root, Connections: connections}}, "history-00000000")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(prototype)
	extra := (wakemute.MaxBytes-32000)/500 - len(raw)
	for extra > 1 {
		n := min(extra-1, 180)
		root = filepath.Join(root, strings.Repeat("r", n))
		extra -= n + 1
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	scope := wakemute.Scope{{Root: root, Connections: connections}}
	fixture.Enroll(t, scope)
	approved := fixture.Confirm(t, scope, "capacity-cleanup-current", "mute")
	marker, err := os.ReadFile(filepath.Join(root, wakemute.Marker))
	if err != nil {
		t.Fatal(err)
	}
	var enrollment wakemute.Enrollment
	if err := json.Unmarshal(marker, &enrollment); err != nil {
		t.Fatal(err)
	}
	history := make([]wakemute.Operation, 499)
	for i := range history {
		history[i] = approved
		history[i].ID = fmt.Sprintf("%032x", i+1)
		history[i].Run = fmt.Sprintf("history-%08d", i)
		history[i].State = "closed"
	}
	hasOverlap := len(overlapping) > 0 && overlapping[0]
	if hasOverlap {
		history[0].State = "active"
	}
	predicted := wakemute.Store{Version: 1, Generation: 999, ConnectionGenerations: map[string]uint64{}, Operations: append(append([]wakemute.Operation{}, history...), approved)}
	for _, c := range connections {
		predicted.ConnectionGenerations[c.Path] = 999
	}
	if reserved {
		for i := range predicted.Operations {
			if predicted.Operations[i].State != "closed" {
				predicted.Operations[i].State = "installing"
			}
		}
		predicted.Generation = math.MaxUint64
		for path := range predicted.ConnectionGenerations {
			predicted.ConnectionGenerations[path] = math.MaxUint64
		}
	} else {
		// Reproduce the reported final-closed-size boundary (before reservation).
		predicted.Generation = 1000
		for path := range predicted.ConnectionGenerations {
			predicted.ConnectionGenerations[path] = 1000
		}
		predicted.Operations[len(history)].State = "closed"
	}
	enrollment.Snapshot = &predicted
	encoded, _ := json.Marshal(enrollment)
	remaining := target - len(encoded) - 1
	if remaining < 0 {
		t.Fatalf("fixture overshot capacity: remaining=%d", remaining)
	}
	for i := range history {
		pad := min(remaining, 128-len(history[i].Run))
		history[i].Run += strings.Repeat("x", pad)
		remaining -= pad
		predicted.Operations[i] = history[i]
		if reserved && predicted.Operations[i].State != "closed" {
			predicted.Operations[i].State = "installing"
		}
	}
	encoded, _ = json.Marshal(enrollment)
	if remaining != 0 || len(encoded)+1 != target {
		t.Fatalf("fixture boundary mismatch: bytes=%d target=%d remaining=%d", len(encoded)+1, target, remaining)
	}
	initial := wakemute.Store{Version: 1, Generation: 998, ConnectionGenerations: map[string]uint64{}, Operations: history}
	for _, c := range connections {
		initial.ConnectionGenerations[c.Path] = 998
	}
	enrollment.Snapshot = &initial
	encoded, _ = json.Marshal(enrollment)
	ledger, _ := json.Marshal(initial)
	if err := os.WriteFile(filepath.Join(root, wakemute.Marker), append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, wakemute.Ledger), append(ledger, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := wakemute.Status(fixture.Context(t), scope)
	if err != nil || before.Status != map[bool]string{false: "unmuted", true: "muted"}[hasOverlap] {
		t.Fatalf("reader rejected fixture history: %v", err)
	}
	return scope, approved
}

// The one-byte-under and exact reservation bounds admit and close; one byte
// over refuses before either copy changes, even though the active row fits.
func TestR2CapacityCleanupRollover(t *testing.T) {
	for _, delta := range []int{-1, 0, 1} {
		t.Run(fmt.Sprintf("reserved-bytes-%d", wakemute.MaxBytes+delta), func(t *testing.T) {
			scope, approved := cleanupCapacityFixture(t, wakemute.MaxBytes+delta, true)
			root := scope[0].Root
			beforeMarker, _ := os.ReadFile(filepath.Join(root, wakemute.Marker))
			beforeLedger, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
			_, err := wakemute.Mute(fixture.Context(t), approved)
			if delta > 0 {
				afterMarker, _ := os.ReadFile(filepath.Join(root, wakemute.Marker))
				afterLedger, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
				if !errors.Is(err, wakemute.ErrUnsafe) || !bytes.Equal(beforeMarker, afterMarker) || !bytes.Equal(beforeLedger, afterLedger) {
					t.Fatalf("cleanup capacity not reserved before activation: err=%v changed=%t/%t", err, !bytes.Equal(beforeMarker, afterMarker), !bytes.Equal(beforeLedger, afterLedger))
				}
				if err := wakemute.Admission(fixture.Context(t), root, scope[0].Connections[0].Path, func(uint64) error { return nil }); err != nil {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatalf("safe adjacent reservation refused: %v", err)
			}
			if _, err := wakemute.Unmute(fixture.Context(t), scope, approved.Run, approved.ID); err != nil {
				t.Fatalf("authorized cleanup stranded by generation growth: %v", err)
			}
			status, err := wakemute.Status(fixture.Context(t), scope)
			if err != nil || status.Status != "unmuted" || status.Roots[0].Generation != 1000 {
				t.Fatalf("cleanup failed: %+v %v", status, err)
			}
			for _, epoch := range status.Roots[0].ConnectionGenerations {
				if epoch != 1000 {
					t.Fatal("selected connection generation did not roll over")
				}
			}
			if len(status.Roots[0].Operations) != 500 {
				t.Fatal("cleanup discarded replay history")
			}
			if err := wakemute.Admission(fixture.Context(t), root, scope[0].Connections[0].Path, func(uint64) error { return nil }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The review's original one-byte-over cleanup projection must now refuse up
// front, with both copies unchanged and the namespace still admitted/unmuted.
func TestR2CapacityOriginalCleanupShortageRefuses(t *testing.T) {
	scope, approved := cleanupCapacityFixture(t, wakemute.MaxBytes+1, false)
	root := scope[0].Root
	beforeMarker, _ := os.ReadFile(filepath.Join(root, wakemute.Marker))
	beforeLedger, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
	_, err := wakemute.Mute(fixture.Context(t), approved)
	if err == nil {
		_, first := wakemute.Unmute(fixture.Context(t), scope, approved.Run, approved.ID)
		_, second := wakemute.Unmute(fixture.Context(t), scope, approved.Run, approved.ID)
		t.Fatalf("original cleanup shortage admitted; authorized cleanup stranded by generation growth: %v / retry %v", first, second)
	}
	afterMarker, _ := os.ReadFile(filepath.Join(root, wakemute.Marker))
	afterLedger, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
	if !errors.Is(err, wakemute.ErrUnsafe) || !bytes.Equal(beforeMarker, afterMarker) || !bytes.Equal(beforeLedger, afterLedger) {
		t.Fatalf("original cleanup shortage admitted or changed control state: %v", err)
	}
	if err := wakemute.Admission(fixture.Context(t), root, scope[0].Connections[0].Path, func(uint64) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// Pending overlaps retain their state-width reservation; closing one, a lost
// close acknowledgement and exact retry cannot spend another operation's space.
func TestR3CapacityOverlapAndRetry(t *testing.T) {
	scope, approved := cleanupCapacityFixture(t, wakemute.MaxBytes, true, true)
	root := scope[0].Root
	// Repeated validation and durable writes of near-1-MiB, 500-row copies
	// can exceed the generic fixture's 3s under hosted -race CI. Bound this
	// entire admission/cleanup sequence to 2m; retries share, not reset, it.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if _, err := wakemute.Mute(ctx, approved); err != nil {
		t.Fatal(err)
	}
	status, err := wakemute.Status(ctx, scope)
	if err != nil {
		t.Fatal(err)
	}
	old := status.Roots[0].Operations[0]
	if _, err := wakemute.Unmute(ctx, scope, old.Run, old.ID); err != nil {
		t.Fatal(err)
	}
	if err := wakemute.Admission(ctx, root, scope[0].Connections[0].Path, func(uint64) error { return nil }); !errors.Is(err, wakemute.ErrMuted) {
		t.Fatalf("overlap lost: %v", err)
	}
	calls := 0
	_, err = wakemute.FaultUnmuteForTest(ctx, scope, approved.Run, approved.ID, func(p string) error {
		if p == "after-rename" {
			calls++
			if calls == 3 {
				return errors.New("fixture lost final-copy acknowledgement")
			}
		}
		return nil
	})
	if !errors.Is(err, wakemute.ErrUncertain) || calls != 3 {
		t.Fatalf("close fault not witnessed: %v calls=%d", err, calls)
	}
	if _, err := wakemute.Unmute(ctx, scope, approved.Run, approved.ID); err != nil {
		t.Fatalf("reserved retry stranded: %v", err)
	}
	status, err = wakemute.Status(ctx, scope)
	if err != nil || status.Status != "unmuted" || len(status.Roots[0].Operations) != 500 {
		t.Fatalf("retry lost history or cleanup: %v", err)
	}
}

// A known capacity refusal in a later root must not publish an earlier root's
// coordinator intent; actual I/O faults retain the existing partial semantics.
func TestR3CapacityPreflightAllRoots(t *testing.T) {
	first := fixture.Scope(t.TempDir())
	fixture.Enroll(t, first)
	last, _ := cleanupCapacityFixture(t, wakemute.MaxBytes+1, true)
	scope, err := wakemute.Normalize(append(first, last...))
	if err != nil {
		t.Fatal(err)
	}
	if scope[0].Root != first[0].Root {
		t.Fatal("fixture must put valid root first")
	}
	approved := fixture.Confirm(t, scope, "all-root-capacity", "mute")
	before := map[string][]byte{}
	for _, r := range scope {
		for _, name := range []string{wakemute.Marker, wakemute.Ledger} {
			path := filepath.Join(r.Root, name)
			before[path], err = os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := wakemute.Mute(fixture.Context(t), approved); !errors.Is(err, wakemute.ErrUnsafe) {
		t.Fatalf("known capacity refusal missing: %v", err)
	}
	for path, b := range before {
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, after) {
			t.Fatalf("capacity refusal partially wrote %s: %v", path, err)
		}
	}
}
