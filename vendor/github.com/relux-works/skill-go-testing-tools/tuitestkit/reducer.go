package tuitestkit

import (
	"fmt"
	"testing"
)

// ReducerTest defines a single table-driven test case for a pure reducer function.
// S is the state type, A is the action type.
type ReducerTest[S any, A any] struct {
	Name    string
	Initial S
	Action  A
	Assert  func(t *testing.T, got S)
}

// Step defines one step in a multi-action reducer sequence.
// Assert is optional — if nil, the step is applied without per-step validation.
type Step[S any, A any] struct {
	Name   string
	Action A
	Assert func(t *testing.T, got S)
}

// ReducerSequence defines a multi-step test scenario for a reducer.
// Actions are applied sequentially starting from Initial.
// Final is the required assertion on the end state.
type ReducerSequence[S, A any] struct {
	Name    string
	Initial S
	Steps   []Step[S, A]
	Final   func(t *testing.T, got S)
}

// Invariant defines a property that must hold for any state produced by the reducer.
type Invariant[S any] struct {
	Name  string
	Check func(s S) error
}

// InvariantChecker holds a set of invariants and validates state against all of them.
type InvariantChecker[S any] struct {
	invariants []Invariant[S]
}

// NewInvariantChecker creates an InvariantChecker with the given invariants.
func NewInvariantChecker[S any](invariants ...Invariant[S]) *InvariantChecker[S] {
	return &InvariantChecker[S]{invariants: invariants}
}

// Check validates the given state against all registered invariants.
// Returns a combined error if any invariant is violated, nil otherwise.
func (ic *InvariantChecker[S]) Check(s S) error {
	for _, inv := range ic.invariants {
		if err := inv.Check(s); err != nil {
			return fmt.Errorf("invariant %q violated: %w", inv.Name, err)
		}
	}
	return nil
}

// RunReducerTests executes a slice of table-driven reducer test cases.
// Each test is run as a subtest via t.Run.
func RunReducerTests[S, A any](t *testing.T, reduce func(S, A) S, tests []ReducerTest[S, A]) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.Name, func(t *testing.T) {
			t.Helper()
			got := reduce(tt.Initial, tt.Action)
			tt.Assert(t, got)
		})
	}
}

// RunReducerSequences executes a slice of multi-step reducer sequence tests.
// For each sequence, actions are applied in order. Per-step Assert (if non-nil)
// runs after each step. Final assertion runs on the end state.
func RunReducerSequences[S, A any](t *testing.T, reduce func(S, A) S, sequences []ReducerSequence[S, A]) {
	t.Helper()
	for _, seq := range sequences {
		t.Run(seq.Name, func(t *testing.T) {
			t.Helper()
			state := seq.Initial
			for i, step := range seq.Steps {
				state = reduce(state, step.Action)
				if step.Assert != nil {
					name := step.Name
					if name == "" {
						name = fmt.Sprintf("step-%d", i)
					}
					t.Run(name, func(t *testing.T) {
						t.Helper()
						step.Assert(t, state)
					})
				}
			}
			if seq.Final != nil {
				seq.Final(t, state)
			}
		})
	}
}

// WrapWithInvariants wraps a reducer function with invariant checking.
// After every reduce call, all invariants are checked. If any invariant
// is violated, t.Fatalf is called with the violation details.
func WrapWithInvariants[S, A any](t *testing.T, reduce func(S, A) S, checker *InvariantChecker[S]) func(S, A) S {
	t.Helper()
	return func(s S, a A) S {
		result := reduce(s, a)
		if err := checker.Check(result); err != nil {
			t.Fatalf("invariant check failed after reduce: %v", err)
		}
		return result
	}
}
