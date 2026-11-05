package tuitestkit

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// --- Call recording ---
//
// Mock building blocks are designed for composition. Embed MockCallRecorder and
// MockResponseMap into your project-specific mock struct:
//
//	type MyExecutorMock struct {
//	    tuitestkit.MockCallRecorder
//	    Responses *tuitestkit.MockResponseMap
//	}
//
//	func NewMyExecutorMock() *MyExecutorMock {
//	    return &MyExecutorMock{Responses: tuitestkit.NewMockResponseMap()}
//	}
//
//	func (m *MyExecutorMock) TreeJSON() ([]byte, error) {
//	    m.Record("TreeJSON")
//	    return m.Responses.Get("TreeJSON")
//	}
//
//	func (m *MyExecutorMock) Execute(cmd string, args ...string) ([]byte, error) {
//	    m.Record("Execute", cmd, args)
//	    return m.Responses.Get("Execute:" + cmd)
//	}
//
// In tests:
//
//	mock := NewMyExecutorMock()
//	mock.Responses.Set("TreeJSON", []byte(`{"root":{}}`), nil)
//	// ... run your bubbletea model with mock as the executor ...
//	tuitestkit.AssertCalled(t, &mock.MockCallRecorder, "TreeJSON")
//	tuitestkit.AssertCalledWith(t, &mock.MockCallRecorder, "Execute", "ls", []string{"-la"})

// MockCall represents a single recorded method invocation.
type MockCall struct {
	Method string
	Args   []any
}

// MockCallRecorder provides thread-safe recording of method calls.
// Embed it into project-specific mock structs to track invocations.
type MockCallRecorder struct {
	mu    sync.Mutex
	calls []MockCall
}

// Record records a method call with the given arguments.
// Safe to call from multiple goroutines.
func (r *MockCallRecorder) Record(method string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, MockCall{Method: method, Args: args})
}

// CallCount returns the number of times the named method was called.
func (r *MockCallRecorder) CallCount(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// Calls returns all recorded calls in order.
// Returns a copy — the caller cannot mutate the recorder's internal state.
func (r *MockCallRecorder) Calls() []MockCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]MockCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// CallsFor returns all recorded calls for the named method, in order.
func (r *MockCallRecorder) CallsFor(method string) []MockCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []MockCall
	for _, c := range r.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// Reset clears all recorded calls.
func (r *MockCallRecorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// --- Canned responses ---

// MockResponse holds a canned response for a mocked method.
type MockResponse struct {
	Data  []byte
	Error error
}

// MockResponseMap provides thread-safe storage and lookup of canned responses.
// Use it to configure what your mock returns for specific method keys.
type MockResponseMap struct {
	mu        sync.Mutex
	responses map[string]MockResponse
}

// NewMockResponseMap creates an empty MockResponseMap ready for use.
func NewMockResponseMap() *MockResponseMap {
	return &MockResponseMap{
		responses: make(map[string]MockResponse),
	}
}

// Set stores a canned response (data + error) for the given key.
func (m *MockResponseMap) Set(key string, data []byte, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.responses[key] = MockResponse{Data: data, Error: err}
}

// Get retrieves the canned response for the given key.
// If the key is not found, returns (nil, nil).
func (m *MockResponseMap) Get(key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	resp, ok := m.responses[key]
	if !ok {
		return nil, nil
	}
	return resp.Data, resp.Error
}

// SetError stores a canned error-only response for the given key.
// Convenience wrapper: equivalent to Set(key, nil, err).
func (m *MockResponseMap) SetError(key string, err error) {
	m.Set(key, nil, err)
}

// --- Test assertion helpers ---

// AssertCalled fails the test if the named method was never called.
func AssertCalled(t testing.TB, r *MockCallRecorder, method string) {
	t.Helper()
	if r.CallCount(method) == 0 {
		t.Errorf("expected %q to be called, but it was not", method)
	}
}

// AssertNotCalled fails the test if the named method was called at least once.
func AssertNotCalled(t testing.TB, r *MockCallRecorder, method string) {
	t.Helper()
	if n := r.CallCount(method); n > 0 {
		t.Errorf("expected %q not to be called, but it was called %d time(s)", method, n)
	}
}

// AssertCalledN fails the test if the named method was not called exactly n times.
func AssertCalledN(t testing.TB, r *MockCallRecorder, method string, n int) {
	t.Helper()
	if got := r.CallCount(method); got != n {
		t.Errorf("expected %q to be called %d time(s), got %d", method, n, got)
	}
}

// AssertCalledWith fails the test if the named method was never called with the given arguments.
// Argument comparison uses reflect.DeepEqual.
func AssertCalledWith(t testing.TB, r *MockCallRecorder, method string, args ...any) {
	t.Helper()
	calls := r.CallsFor(method)
	if len(calls) == 0 {
		t.Errorf("expected %q to be called with %v, but it was never called", method, args)
		return
	}
	for _, c := range calls {
		if reflect.DeepEqual(c.Args, args) {
			return
		}
	}
	t.Errorf("expected %q to be called with %v, but no matching call found.\nRecorded calls for %q:", method, args, method)
	for i, c := range calls {
		fmt.Fprintf(fmtWriter{t}, "  [%d] %v\n", i, c.Args)
	}
}

// fmtWriter adapts testing.TB to io.Writer for fmt.Fprintf usage in assertions.
type fmtWriter struct {
	t testing.TB
}

func (w fmtWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(string(p))
	return len(p), nil
}
