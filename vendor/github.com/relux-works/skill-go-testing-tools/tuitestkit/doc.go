// Package tuitestkit provides test utilities for bubbletea TUI applications.
//
// Built on the Elm architecture insight: pure reducers enable comprehensive
// testing without I/O, network, or UI rendering dependencies.
//
// Core components:
//   - Message builders: Key(), Keys(), WindowSize(), MouseClick(), MouseScroll()
//   - Reducer test harness: table-driven tests for pure reducers with invariant checking
//   - Mock executor: building blocks for mocking CLI executor interfaces
//   - View assertions: ANSI-aware helpers for asserting on View() output
package tuitestkit
