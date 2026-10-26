//go:build unix

package cli

import (
	"bytes"
	"errors"
	"sync"
	"testing"
	"time"
)

// A held state lock cannot stall new diagnostic reporting or alter authority.
// The unlocked control persists the same fixed code. Only fixture locks are held.
func TestBindDiagnosticLockedState(t *testing.T) {
	e, a, s := bindDiagnosticEnv(t)
	var log bytes.Buffer
	a.Stderr = &log
	before := readFile(t, e.paths().StateFile("alice"))
	release := sync.OnceFunc(e.holdStateLock("alice"))
	defer release()
	done := make(chan struct{})
	go func() {
		a.recordTerminalFailure(s, "diagnostic-generation", "state", errors.New(bindDiagnosticMarker))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("diagnostic waited behind failed state lock")
	}
	if readFile(t, e.paths().StateFile("alice")) != before || bytes.Contains(log.Bytes(), []byte(bindDiagnosticMarker)) {
		t.Fatal("held-lock report wrote/leaked")
	}
	release()
	a.recordTerminalFailure(s, "diagnostic-generation", "state", errors.New(bindDiagnosticMarker))
	if d := e.state("alice").Terminal.Diagnostic; d == nil || d.Phase != "state" || d.Class != "unavailable" {
		t.Fatal(d)
	}
}
