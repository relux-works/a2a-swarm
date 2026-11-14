//go:build darwin || linux

package tmuxwake

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pipeHoldingPS puts a fake ps first on PATH. It starts a background child
// that inherits stdout and holds it for holdSeconds, then waits itself, so
// killing ps alone does not close the pipe Birth reads.
func pipeHoldingPS(t *testing.T, holdSeconds string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep " + holdSeconds + " &\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func cancelledBirthElapsed(t *testing.T) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := Birth(ctx, os.Getpid()); err == nil {
		t.Fatal("a cancelled Birth through the fake ps must fail")
	}
	return time.Since(start)
}

// Proves: after cancellation, Birth returns within the WaitDelay bound
// (here under 1.5s) even when a descendant of ps still holds its stdout
// pipe open for 4s. Limit: a shell-script ps on PATH stands in for a real
// pipe-inheriting descendant.
func TestBirthCancellationBoundedWithInheritedPipe(t *testing.T) {
	pipeHoldingPS(t, "4")
	if elapsed := cancelledBirthElapsed(t); elapsed > 1500*time.Millisecond {
		t.Fatalf("cancelled Birth took %s with an inherited pipe; want under 1.5s", elapsed)
	}
}

// Calibration: with no WaitDelay, the same cancelled Birth waits for the
// pipe holder (about 2s here), so the bound above is what the delay buys.
func TestBirthCancellationBoundedWithInheritedPipeCalibration(t *testing.T) {
	old := birthWaitDelay
	birthWaitDelay = 0
	t.Cleanup(func() { birthWaitDelay = old })
	pipeHoldingPS(t, "2")
	if elapsed := cancelledBirthElapsed(t); elapsed < 1500*time.Millisecond {
		t.Fatalf("without WaitDelay the cancelled Birth returned in %s; the witness cannot see the hang", elapsed)
	}
}
