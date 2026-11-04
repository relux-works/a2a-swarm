//go:build n1_fixture && darwin

package n1fixture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"syscall"
	"time"
)

var ErrCustodyEvidence = errors.New("fixture custody evidence unavailable")

func custodyError(stage string, e error) error {
	if e == nil {
		return nil
	}
	return fmt.Errorf("%w: %s: %w", ErrCustodyEvidence, stage, e)
}

const waitKind = "exec.Cmd.Wait returned"
const clientBirthImage = "not sealed; never enumerated or signalled"

type waitOutcome struct {
	Exit   int    `json:"exit"`
	Signal string `json:"signal"`
}
type waitReceipt struct {
	PID        int         `json:"pid"`
	Parent     int         `json:"parent"`
	Birth      string      `json:"birth"`
	Nonce      string      `json:"nonce"`
	Generation string      `json:"generation"`
	Outcome    waitOutcome `json:"outcome"`
	Kind       string      `json:"kind"`
	At         time.Time   `json:"at"`
	Exit       int         `json:"exit"`
	Signal     string      `json:"signal"`
}
type clientReceipt struct {
	PID        int         `json:"pid"`
	Parent     int         `json:"parent"`
	Nonce      string      `json:"nonce"`
	Outcome    waitOutcome `json:"outcome"`
	Kind       string      `json:"kind"`
	BirthImage string      `json:"birth_image"`
}

func actualOutcome(e error) waitOutcome {
	r := waitOutcome{}
	if e != nil {
		r.Exit = -1
		if ee, ok := e.(*exec.ExitError); ok {
			r.Exit = ee.ExitCode()
			if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				r.Signal = ws.Signal().String()
			}
		}
	}
	return r
}
func waitName(pid int) string   { return fmt.Sprintf("wait-%d.json", pid) }
func clientName(pid int) string { return fmt.Sprintf("client-wait-%d.json", pid) }

// Actual reaping and its outcome are facts even when their store fails. Neither
// an instrumentation error nor a missing receipt changes those in-memory facts.
func completeChild(c *Child, result waitOutcome, actualErr error, g *completionGroup) {
	c.Err = actualErr
	c.Reaped = true
	c.Outcome = result
	r := c.Row // The launch seal remains immutable while other owners read it.
	r.Reaped = true
	r.Exit = result.Exit
	r.Signal = result.Signal
	c.EvidenceErr = errors.Join(custodyError("seal child", c.sealErr), recordWait(r, result))
	g.completed(completion{row: &r, err: c.EvidenceErr})
	close(c.Done)
}
func recordWait(r Row, result waitOutcome) error {
	if !r.Sealed {
		return custodyError("persist unsealed child Wait", errors.New("launch seal unavailable"))
	}
	ledgerErr := ledger(func(rows *[]Row) error {
		for i := len(*rows) - 1; i >= 0; i-- {
			old := (*rows)[i]
			if old.PID == r.PID && old.PPID == r.PPID && old.Birth == r.Birth && old.Nonce == r.Nonce && old.Generation == r.Generation {
				(*rows)[i].Reaped = true
				(*rows)[i].Exit = result.Exit
				(*rows)[i].Signal = result.Signal
				return nil
			}
		}
		return errors.New("matching sealed Wait row unavailable")
	})
	receipt := waitReceipt{PID: r.PID, Parent: r.PPID, Birth: r.Birth, Nonce: r.Nonce, Generation: r.Generation, Outcome: result, Exit: result.Exit, Signal: result.Signal, Kind: waitKind, At: time.Now().UTC()}
	return errors.Join(custodyError("persist reaped ledger", ledgerErr), custodyError("persist wait receipt", Write(waitName(r.PID), receipt)))
}
func VerifyWait(r Row) error {
	var v waitReceipt
	if e := Read(waitName(r.PID), &v); e != nil {
		return custodyError("verify wait receipt read", e)
	}
	if v.PID != r.PID || v.Parent != r.PPID || v.Birth != r.Birth || v.Nonce != r.Nonce || v.Generation != r.Generation || v.Kind != waitKind || v.Outcome.Exit != v.Exit || v.Outcome.Signal != v.Signal || (r.Reaped && (v.Exit != r.Exit || v.Signal != r.Signal)) {
		return custodyError("verify wait receipt identity/outcome", errors.New("PID/parent/birth/nonce/generation/kind/exit/signal mismatch"))
	}
	return nil
}
func childCompletionError(c *Child) error {
	if !c.Reaped {
		return custodyError("verify child completion", errors.New("actual Wait has not returned"))
	}
	r := c.Row
	r.Reaped, r.Exit, r.Signal = true, c.Outcome.Exit, c.Outcome.Signal
	return errors.Join(custodyError("seal child", c.sealErr), c.EvidenceErr, VerifyWait(r))
}
func verifyClient(r clientReceipt) error {
	var got clientReceipt
	if e := Read(clientName(r.PID), &got); e != nil {
		return custodyError("verify client receipt read", e)
	}
	if !reflect.DeepEqual(got, r) {
		return custodyError("verify client receipt identity/outcome", errors.New("client PID/parent/nonce/kind/outcome mismatch"))
	}
	return nil
}
func completeClient(r clientReceipt, actualErr error, g *completionGroup) error {
	evidenceErr := custodyError("persist client receipt", Write(clientName(r.PID), r))
	g.completed(completion{client: &r, err: evidenceErr})
	return errors.Join(actualErr, evidenceErr)
}

type completion struct {
	row    *Row
	client *clientReceipt
	err    error
}
type completionGroup struct {
	sync.WaitGroup
	mu      sync.Mutex
	results []completion
}

func (g *completionGroup) completed(r completion) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.results = append(g.results, r)
}

// WaitAll means completed, persisted custody, not successful child App exits.
// Actual exit/signal remain in Child and receipts, distinct from evidence errors.
func (g *completionGroup) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { g.WaitGroup.Wait(); close(done) }()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
	}
	g.mu.Lock()
	rows := append([]completion(nil), g.results...)
	g.mu.Unlock()
	var failures []error
	for _, r := range rows {
		failures = append(failures, r.err)
		if r.row != nil {
			failures = append(failures, VerifyWait(*r.row))
		}
		if r.client != nil {
			failures = append(failures, verifyClient(*r.client))
		}
	}
	return errors.Join(failures...)
}

// Called after stop attempts, including rows that were already Reaped. It has
// no process operations. The pane retains its explicitly labelled alternative.
func VerifyCompletedRows() error {
	rows, e := Rows()
	if e != nil {
		return custodyError("verify completed ledger", e)
	}
	var failures []error
	for _, r := range rows {
		if r.Role == "driver" {
			continue
		}
		if !r.Reaped {
			failures = append(failures, custodyError("verify completed ledger", fmt.Errorf("PID %d lacks persisted completed custody", r.PID)))
			continue
		}
		if r.Role == "pane-supervisor" {
			var v struct {
				PID         int    `json:"pid"`
				Nonce       string `json:"nonce"`
				RuntimeWait bool   `json:"runtime_wait_returned"`
			}
			e := Read("pane-exit.json", &v)
			if e == nil && (v.PID != r.PID || v.Nonce != r.Nonce || !v.RuntimeWait) {
				e = errors.New("pane exit identity/Wait mismatch")
			}
			failures = append(failures, custodyError("verify explicit pane exit", e))
			continue
		}
		failures = append(failures, VerifyWait(r))
	}
	return errors.Join(failures...)
}

// The caller receives an error even if writing cleanup/STOP is impossible. The
// attempt is always retained; no failed store is the sole channel for its error.
func FinishCleanup(prior error) error {
	evidenceErr := errors.Join(prior, VerifyCompletedRows())
	e := Write("cleanup.json", map[string]any{"all_owned_waits_or_explicit_pane_exit_verified": evidenceErr == nil, "directory_retained": true})
	return errors.Join(evidenceErr, custodyError("persist final cleanup", e))
}

// Keep actual client parent acquisition outside pure completion controls.
func newClientReceipt(pid int, result waitOutcome) clientReceipt {
	return clientReceipt{PID: pid, Parent: os.Getpid(), Nonce: Nonce(), Outcome: result, Kind: waitKind, BirthImage: clientBirthImage}
}
