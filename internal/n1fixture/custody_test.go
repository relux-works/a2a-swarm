//go:build n1_fixture && darwin

package n1fixture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func receiptFixture(t *testing.T) (Row, *completionGroup) {
	t.Helper()
	t.Setenv("N1_DIR", t.TempDir())
	r := Row{PID: 41001, PPID: 41000, UID: 501, Name: "synthetic-child", Birth: "synthetic-birth", Image: "synthetic-image", Nonce: "synthetic-nonce", Generation: "synthetic-generation", Role: "consumer", Sealed: true}
	if e := Write("ledger.json", []Row{r}); e != nil {
		t.Fatal(e)
	}
	return r, &completionGroup{}
}
func modelCompletion(r Row, g *completionGroup, result waitOutcome, actualErr error) *Child {
	c := &Child{Row: r, Done: make(chan struct{})}
	g.Add(1)
	completeChild(c, result, actualErr, g)
	g.Done()
	return c
}
func requireCustody(t *testing.T, e error, stage string) {
	t.Helper()
	if !errors.Is(e, ErrCustodyEvidence) || !strings.Contains(e.Error(), stage) {
		t.Fatalf("expected custody failure %q, got %v", stage, e)
	}
}
func requireRetainedVerdict(t *testing.T) {
	t.Helper()
	var v struct {
		Clean    bool `json:"all_owned_waits_or_explicit_pane_exit_verified"`
		Retained bool `json:"directory_retained"`
	}
	if e := Read("cleanup.json", &v); e != nil {
		t.Fatal(e)
	}
	if v.Clean || !v.Retained {
		t.Fatal("custody failure produced a clean cleanup verdict")
	}
}

// Models the exact completion helper used after cmd.Wait, using only synthetic
// rows/private files. It proves instrument propagation, NOT actual Wait/lifecycle.
func TestCustodyChildCompletion(t *testing.T) {
	for _, mode := range []string{"valid", "receipt-write-failure", "store-unavailable"} {
		t.Run(mode, func(t *testing.T) {
			r, g := receiptFixture(t)
			actualErr := errors.New("synthetic actual Wait error")
			result := waitOutcome{Exit: 17, Signal: "synthetic-signal"}
			if mode == "receipt-write-failure" {
				if e := os.Mkdir(filepath.Join(Dir(), waitName(r.PID)), 0700); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "store-unavailable" {
				p := filepath.Join(Dir(), "unavailable")
				if e := os.WriteFile(p, []byte("synthetic unavailable store"), 0600); e != nil {
					t.Fatal(e)
				}
				t.Setenv("N1_DIR", p)
			}
			c := modelCompletion(r, g, result, actualErr)
			if !c.Reaped || c.Err != actualErr || c.Outcome != result {
				t.Fatal("custody error overwrote actual modeled reap/exit/signal")
			}
			closeErr := CloseChild(context.Background(), c)
			groupErr := g.Wait(context.Background())
			if mode == "valid" {
				if c.EvidenceErr != nil || closeErr != nil || groupErr != nil {
					t.Fatal("valid custody refused", c.EvidenceErr, closeErr, groupErr)
				}
				rows, e := Rows()
				if e != nil || !rows[0].Reaped || rows[0].Exit != 17 || rows[0].Signal != result.Signal {
					t.Fatal("modeled outcome not retained", rows, e)
				}
				if e = FinishCleanup(nil); e != nil {
					t.Fatal(e)
				}
				var v map[string]any
				if e = Read("cleanup.json", &v); e != nil || v["all_owned_waits_or_explicit_pane_exit_verified"] != true {
					t.Fatal("valid cleanup failed", e)
				}
				return
			}
			requireCustody(t, c.EvidenceErr, "persist")
			requireCustody(t, closeErr, "persist")
			requireCustody(t, groupErr, "persist")
			e := FinishCleanup(groupErr)
			requireCustody(t, e, "persist")
			if mode == "receipt-write-failure" {
				var link *os.LinkError
				if !errors.As(c.EvidenceErr, &link) || link.New != filepath.Join(Dir(), waitName(r.PID)) {
					t.Fatal("did not fail at intended receipt rename", c.EvidenceErr)
				}
				rows, e := Rows()
				if e != nil || !rows[0].Reaped {
					t.Fatal("actually modeled reaped child relabelled running", e)
				}
				requireRetainedVerdict(t)
			} else if !strings.Contains(e.Error(), "persist final cleanup") {
				t.Fatal("unwritable final/STOP store failure was swallowed", e)
			}
			if mode == "store-unavailable" {
				if e := Write("STOP.json", true); e == nil {
					t.Fatal("control did not block STOP persistence")
				}
			}
		})
	}
}

// Closed Done and persisted Reaped must not bypass a fresh exact receipt check.
// Each mismatch is independent of process setup, probes, signals or a timeout.
func TestCustodyCompletedReceipt(t *testing.T) {
	cases := []string{"valid", "missing", "pid", "parent", "birth", "nonce", "generation", "exit", "signal", "kind"}
	for _, field := range cases {
		t.Run(field, func(t *testing.T) {
			r, g := receiptFixture(t)
			c := modelCompletion(r, g, waitOutcome{}, nil)
			if field == "missing" {
				if e := os.Remove(filepath.Join(Dir(), waitName(r.PID))); e != nil {
					t.Fatal(e)
				}
			} else if field != "valid" {
				var data map[string]any
				if e := Read(waitName(r.PID), &data); e != nil {
					t.Fatal(e)
				}
				if field == "pid" || field == "parent" || field == "exit" {
					data[field] = 99999
				} else {
					data[field] = "mismatched-synthetic-value"
				}
				if e := Write(waitName(r.PID), data); e != nil {
					t.Fatal(e)
				}
			}
			closeErr := CloseChild(context.Background(), c)
			aggregateErr := g.Wait(context.Background())
			rowsErr := VerifyCompletedRows()
			if field == "valid" {
				if errors.Join(closeErr, aggregateErr, rowsErr) != nil {
					t.Fatal(closeErr, aggregateErr, rowsErr)
				}
				return
			}
			stage := "verify wait receipt"
			requireCustody(t, closeErr, stage)
			requireCustody(t, aggregateErr, stage)
			requireCustody(t, rowsErr, stage)
			if !c.Reaped || c.Err != nil || c.Outcome != (waitOutcome{}) {
				t.Fatal("receipt corruption changed actual modeled outcome")
			}
			requireCustody(t, FinishCleanup(aggregateErr), stage)
			requireRetainedVerdict(t)
		})
	}
}

// Client completion uses the same pure function called after its real Wait.
// A client write failure remains separate from the original modeled Wait error
// and is sticky in aggregate completion; no client process is launched here.
func TestCustodyClientCompletion(t *testing.T) {
	for _, mode := range []string{"valid", "write-failure", "missing", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			childRow, g := receiptFixture(t)
			modelCompletion(childRow, g, waitOutcome{}, nil)
			r := clientReceipt{PID: 42001, Parent: 42000, Nonce: "client-nonce", Outcome: waitOutcome{Exit: 19, Signal: "model-signal"}, Kind: waitKind, BirthImage: clientBirthImage}
			if mode == "write-failure" {
				if e := os.Mkdir(filepath.Join(Dir(), clientName(r.PID)), 0700); e != nil {
					t.Fatal(e)
				}
			}
			actualErr := errors.New("synthetic client Wait error")
			e := completeClient(r, actualErr, g)
			if !errors.Is(e, actualErr) {
				t.Fatal("client actual Wait outcome was overwritten", e)
			}
			if mode == "missing" {
				if e := os.Remove(filepath.Join(Dir(), clientName(r.PID))); e != nil {
					t.Fatal(e)
				}
			}
			if mode == "mismatch" {
				bad := r
				bad.Nonce = "other-nonce"
				if e := Write(clientName(r.PID), bad); e != nil {
					t.Fatal(e)
				}
			}
			aggregate := g.Wait(context.Background())
			if mode == "valid" {
				if errors.Is(e, ErrCustodyEvidence) || aggregate != nil {
					t.Fatal("valid client custody refused", e, aggregate)
				}
				return
			}
			if mode == "write-failure" {
				requireCustody(t, e, "persist client receipt")
			}
			requireCustody(t, aggregate, "client receipt")
			requireCustody(t, FinishCleanup(aggregate), "client receipt")
			requireRetainedVerdict(t)
		})
	}
}

// Final cleanup persistence itself is a required gate, even if all actual
// modeled Waits/receipts are valid and another STOP file cannot be written.
func TestCustodyFinalCleanup(t *testing.T) {
	for _, mode := range []string{"valid", "final-write-failure", "prior-failure"} {
		t.Run(mode, func(t *testing.T) {
			r, g := receiptFixture(t)
			c := modelCompletion(r, g, waitOutcome{}, nil)
			if e := g.Wait(context.Background()); e != nil {
				t.Fatal(e)
			}
			var prior error
			if mode == "prior-failure" {
				prior = errors.New("specific earlier cleanup failure")
			}
			if mode == "final-write-failure" {
				if e := os.Mkdir(filepath.Join(Dir(), "cleanup.json"), 0700); e != nil {
					t.Fatal(e)
				}
			}
			e := FinishCleanup(prior)
			switch mode {
			case "valid":
				if e != nil {
					t.Fatal(e)
				}
			case "prior-failure":
				if !errors.Is(e, prior) {
					t.Fatal("exact prior error lost", e)
				}
				requireRetainedVerdict(t)
			case "final-write-failure":
				requireCustody(t, e, "persist final cleanup")
				var link *os.LinkError
				if !errors.As(e, &link) || link.New != filepath.Join(Dir(), "cleanup.json") {
					t.Fatal("wrong failure", e)
				}
			}
			if !c.Reaped || !reflect.DeepEqual(c.Outcome, waitOutcome{}) || c.Err != nil {
				t.Fatal("cleanup failure changed modeled actual Wait")
			}
		})
	}
}
