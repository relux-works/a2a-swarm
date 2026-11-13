//go:build linux || darwin

package wakemute_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/relux-works/a2a-swarm/internal/wakemute"
	"github.com/relux-works/a2a-swarm/internal/wakemute/fixture"
)

// Complete bytes (including newline) fit exactly or refuse before either copy changes.
// The adjacent valid history uses the public owner-confirmed mutation path.
func TestReviewStateCapacityBoundary(t *testing.T) {
	for _, targetSize := range []int{wakemute.MaxBytes - 1, wakemute.MaxBytes} {
		t.Run(fmt.Sprintf("encoded-size-%d", targetSize), func(t *testing.T) {
			root := t.TempDir()
			scope := fixture.Scope(root)
			fixture.Enroll(t, scope)
			approved := fixture.Confirm(t, scope, "capacity-current", "mute")
			markerPath := filepath.Join(root, wakemute.Marker)
			markerData, err := os.ReadFile(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			var enrollment wakemute.Enrollment
			if err := json.Unmarshal(markerData, &enrollment); err != nil {
				t.Fatal(err)
			}
			history := approved
			history.ID = fmt.Sprintf("%032x", 1)
			history.Run = "history-00000000"
			history.State = "closed"
			historyJSON, err := json.Marshal(history)
			if err != nil {
				t.Fatal(err)
			}
			predicted := wakemute.Store{Version: 1, Generation: 2, ConnectionGenerations: map[string]uint64{".": 2}, Operations: []wakemute.Operation{approved}}
			enrollment.Snapshot = &predicted
			initialJSON, err := json.Marshal(enrollment)
			if err != nil {
				t.Fatal(err)
			}
			count := (targetSize - len(initialJSON)) / (len(historyJSON) + 1)
			operations := make([]wakemute.Operation, count, count+1)
			for index := range operations {
				operations[index] = history
				operations[index].ID = fmt.Sprintf("%032x", index+1)
				operations[index].Run = fmt.Sprintf("history-%08d", index)
			}
			predicted.Operations = append(operations, approved)
			predictedJSON, err := json.Marshal(enrollment)
			if err != nil {
				t.Fatal(err)
			}
			remaining := targetSize - len(predictedJSON)
			for index := 0; remaining > 0 && index < len(operations); index++ {
				padding := min(remaining, 128-len(operations[index].Run))
				operations[index].Run += strings.Repeat("x", padding)
				remaining -= padding
			}
			predictedJSON, err = json.Marshal(enrollment)
			if err != nil || len(predictedJSON) != targetSize {
				t.Fatalf("bad boundary setup: length=%d target=%d err=%v", len(predictedJSON), targetSize, err)
			}
			initialStore := wakemute.Store{Version: 1, Generation: 1, ConnectionGenerations: map[string]uint64{".": 1}, Operations: operations}
			enrollment.Snapshot = &initialStore
			markerData, err = json.Marshal(enrollment)
			if err != nil {
				t.Fatal(err)
			}
			ledgerData, err := json.Marshal(initialStore)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(markerPath, append(markerData, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, wakemute.Ledger), append(ledgerData, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			before, err := wakemute.Status(ctx, scope)
			if err != nil || before.Status != "unmuted" {
				t.Fatalf("valid history rejected: %v", err)
			}
			beforeMarker, _ := os.ReadFile(markerPath)
			beforeLedger, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
			result, mutationErr := wakemute.Mute(ctx, approved)
			info, err := os.Stat(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			after, statusErr := wakemute.Status(ctx, scope)
			if info.Size() > wakemute.MaxBytes {
				t.Fatalf("writer published an unreadable oversized control record: bytes=%d limit=%d mutation=%v result=%s status=%s status_error=%v discoverable_operations=%d", info.Size(), wakemute.MaxBytes, mutationErr, result.Status, after.Status, statusErr, len(after.Roots[0].Operations))
			}
			if targetSize == wakemute.MaxBytes {
				currentMarker, _ := os.ReadFile(markerPath)
				currentLedger, _ := os.ReadFile(filepath.Join(root, wakemute.Ledger))
				if mutationErr == nil || statusErr != nil || after.Status != "unmuted" || string(beforeMarker) != string(currentMarker) || string(beforeLedger) != string(currentLedger) {
					t.Fatalf("capacity refusal changed readable control state: mutation=%v status=%v result=%s", mutationErr, statusErr, after.Status)
				}
				return
			}
			if mutationErr != nil || statusErr != nil || after.Status != "muted" {
				t.Fatalf("adjacent capacity positive failed: mutation=%v status=%v result=%s", mutationErr, statusErr, after.Status)
			}
		})
	}
}
