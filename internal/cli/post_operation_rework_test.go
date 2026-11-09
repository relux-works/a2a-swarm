package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/relux-works/a2a-swarm/internal/config"
)

// Claim: Retained prepared/UNKNOWN posts exit 1 and ACK posts exit 0 without provider/HTTP work; local synthetic recovery only.
func TestDurablePostRetainedExitCodes(t *testing.T) {
	for _, state := range []string{"not_submitted", "unknown", "acknowledged"} {
		t.Run(state, func(t *testing.T) {
			w := operationWorldNew(t)
			a, _, _ := w.app()
			if state != "acknowledged" {
				stop := "after_prepare"
				if state == "unknown" {
					stop = "after_reservation"
				}
				a.postOperationFault = func(stage string) error {
					if stage == stop {
						return errors.New("synthetic stop")
					}
					return nil
				}
			}
			initial := 1
			if state == "acknowledged" {
				initial = 0
			}
			if code := a.Run(context.Background(), operationArgs()); code != initial {
				t.Fatal(code)
			}
			if w.record().Outcome != state {
				t.Fatal("fixture phase")
			}
			calls, sends, providers := w.calls.Load(), w.sends.Load(), w.store.reads.Load()
			for range 3 {
				post, out, diag := w.app()
				want := 1
				if state == "acknowledged" {
					want = 0
				}
				if code := post.Run(context.Background(), operationArgs()); code != want {
					t.Fatalf("post=%d %s", code, diag)
				}
				if got := operationResult(t, out); got.Outcome != state || got.Reason != "retained_local_evidence" {
					t.Fatal(got)
				}
				status, out, diag := w.app()
				want = 0
				if state == "unknown" {
					want = 1
				} // existing documented status contract
				if code := status.Run(context.Background(), []string{"post-operation", "status", "op", "--json"}); code != want {
					t.Fatalf("status=%d %s", code, diag)
				}
				if got := operationResult(t, out); got.Outcome != state {
					t.Fatal(got)
				}
			}
			if w.calls.Load() != calls || w.sends.Load() != sends || w.store.reads.Load() != providers {
				t.Fatal("retained invocation sent or opened a session/provider")
			}
		})
	}
}

// Write a coherent replacement without flock. Public hashes are not secrets;
// recomputing them cannot authorize different bytes for a prepared operation.
func replaceOperationFixture(t *testing.T, w *operationWorld, r config.PostOperation) {
	t.Helper()
	r.RecordDigest = ""
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	r.RecordDigest = config.PostOperationDigest(raw)
	raw, err = json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(w.paths.Base, "post-operations", "agents", "alice")
	path := filepath.Join(dir, "replacement.fixture")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, filepath.Join(dir, "op.json")); err != nil {
		t.Fatal(err)
	}
}

// Claim: Coherent or corrupt replacements refuse at reservation/ACK gates without replay or leaked IDs; before-gate synthetic swaps only.
func TestDurablePostRequestSwapRefusesAtBothGates(t *testing.T) {
	for _, stage := range []string{"after_prepare", "after_reservation", "before_ack_persist"} {
		for _, field := range []string{"request_bytes", "request_length", "transaction", "binding", "event_type", "record_digest"} {
			t.Run(stage+"/"+field, func(t *testing.T) {
				w := operationWorldNew(t)
				a, out, diag := w.app()
				var verifiedRequest []byte
				a.postOperationFault = func(s string) error {
					if s != stage {
						return nil
					}
					r := w.record()
					verifiedRequest = bytes.Clone(r.Request)
					switch field {
					case "request_bytes":
						r.Request = bytes.ReplaceAll(r.Request, []byte("synthetic body"), []byte("different body"))
						r.RequestLength = len(r.Request)
						r.ContentDigest = config.PostOperationDigest(r.Request)
					case "request_length":
						r.Request = bytes.ReplaceAll(r.Request, []byte("synthetic body"), []byte("different body with extra bytes"))
						r.RequestLength = len(r.Request)
						r.ContentDigest = config.PostOperationDigest(r.Request)
					case "transaction":
						r.TransactionID = "different_txn"
					case "binding":
						r.Binding.DeviceID = "different-device"
					case "event_type":
						r.EventType = "m.room.other"
					case "record_digest":
						// A corrupt digest is a refusal alongside coherent swaps.
						path := filepath.Join(w.paths.Base, "post-operations", "agents", "alice", "op.json")
						r.RecordDigest = strings.Repeat("0", 64)
						raw, err := json.Marshal(r)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, raw, 0600); err != nil {
							t.Fatal(err)
						}
						return nil
					}
					replaceOperationFixture(t, w, r)
					return nil
				}
				if code := a.Run(context.Background(), operationArgs()); code != 1 {
					t.Fatalf("swap exit=%d %s", code, diag)
				}
				if strings.Contains(out.String()+diag.String(), "$accepted") || strings.Contains(out.String()+diag.String(), "synthetic body") || strings.Contains(out.String()+diag.String(), w.paths.Base) {
					t.Fatal("failure leaked event/content/path")
				}
				if stage == "after_prepare" {
					if w.sends.Load() != 0 || out.Len() != 0 {
						t.Fatal("reservation mismatch sent or claimed result")
					}
				} else {
					if w.sends.Load() != 1 || !bytes.Equal(w.requests[0], verifiedRequest) {
						t.Fatal("sent bytes differ from verified reservation")
					}
					got := operationResult(t, out)
					if got.Outcome != "unknown" || got.Reason != "ack_persist_failed" || got.EventID != "" || !strings.Contains(diag.String(), "do not repost") {
						t.Fatal("ACK mismatch falsely acknowledged", got)
					}
				}
				calls, providers, sends := w.calls.Load(), w.store.reads.Load(), w.sends.Load()
				next, _, _ := w.app()
				_ = next.Run(context.Background(), operationArgs())
				if w.calls.Load() != calls || w.store.reads.Load() != providers || w.sends.Load() != sends {
					t.Fatal("mismatched retained ID reopened session or resent")
				}
			})
		}
	}
}

// Claim: ACK persistence failure emits only closed UNKNOWN and never replays; inert transport, not power-loss durability.
func TestDurablePostACKFailureClosedProjection(t *testing.T) {
	w := operationWorldNew(t)
	a, out, diag := w.app()
	a.postOperationFault = func(stage string) error {
		if stage == "before_ack_persist" {
			return errors.New("synthetic detail must not escape")
		}
		return nil
	}
	if a.Run(context.Background(), operationArgs()) != 1 {
		t.Fatal("undurable ACK exit")
	}
	var got map[string]any
	if json.Unmarshal(out.Bytes(), &got) != nil || len(got) != 5 || got["outcome"] != "unknown" || got["reason"] != "ack_persist_failed" {
		t.Fatal("unclosed UNKNOWN projection", out)
	}
	for _, hidden := range []string{"$accepted", "event_id", "synthetic body", "synthetic detail", "REDACTION_PLACEHOLDER", w.paths.Base} {
		if strings.Contains(out.String()+diag.String(), hidden) {
			t.Fatal("undurable ACK exposed forbidden detail")
		}
	}
	if !strings.Contains(diag.String(), "do not repost") {
		t.Fatal("missing no-repost instruction")
	}
	calls, providers := w.calls.Load(), w.store.reads.Load()
	for range 3 {
		next, output, diagnostics := w.app()
		if next.Run(context.Background(), operationArgs()) != 1 {
			t.Fatal("retained UNKNOWN exit")
		}
		if strings.Contains(output.String()+diagnostics.String(), "$accepted") {
			t.Fatal("later invocation exposed undurable event")
		}
	}
	if w.sends.Load() != 1 || w.calls.Load() != calls || w.store.reads.Load() != providers {
		t.Fatal("ACK failure caused replay")
	}
}
