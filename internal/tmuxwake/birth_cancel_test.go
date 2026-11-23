//go:build darwin || linux

package tmuxwake

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pipeHoldingPS puts a fake ps first on PATH. It starts a background child
// that inherits stdout and holds it for holdSeconds, publishes its PID by
// atomic rename, then waits itself, so killing ps alone does not close the
// pipe Birth reads.
func pipeHoldingPS(t *testing.T, holdSeconds string) string {
	t.Helper()
	dir := t.TempDir()
	childPIDFile := filepath.Join(dir, "child.pid")
	script := "#!/bin/sh\nsleep " + holdSeconds + " &\nchild=$!\n" +
		"if ! kill -0 \"$child\" 2>/dev/null; then exit 1; fi\n" +
		"pidfile=\"$TMUXWAKE_TEST_CHILD_PID_FILE\"\n" +
		"tmpfile=\"${pidfile}.tmp.$$\"\n" +
		"printf '%s\\n' \"$child\" > \"$tmpfile\"\n" +
		"mv \"$tmpfile\" \"$pidfile\"\nwait\n"
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMUXWAKE_TEST_CHILD_PID_FILE", childPIDFile)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return childPIDFile
}

func waitForPipeHolder(t *testing.T, childPIDFile string) {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		data, err := os.ReadFile(childPIDFile)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr != nil || pid <= 0 {
				t.Fatalf("fake ps wrote invalid child PID %q: %v", strings.TrimSpace(string(data)), parseErr)
			}
			if err := syscall.Kill(pid, 0); err == nil {
				return
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("read fake ps child PID: %v", err)
		}

		select {
		case <-ticker.C:
		case <-timeout.C:
			t.Fatal("fake ps did not start a live pipe-holding child")
		}
	}
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

// Proves: with no WaitDelay, cancellation waits at least 1.5s after the fake
// ps has published a live child PID, showing that child already holds stdout.
// Limit: a shell-script ps and sleep child stand in for a real pipe-inheriting
// descendant.
func TestBirthCancellationBoundedWithInheritedPipeCalibration(t *testing.T) {
	old := birthWaitDelay
	birthWaitDelay = 0
	t.Cleanup(func() { birthWaitDelay = old })
	childPIDFile := pipeHoldingPS(t, "4")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type birthResult struct {
		err        error
		returnedAt time.Time
	}
	done := make(chan birthResult, 1)
	go func() {
		_, err := Birth(ctx, os.Getpid())
		done <- birthResult{err: err, returnedAt: time.Now()}
	}()
	waitForPipeHolder(t, childPIDFile)
	cancelledAt := time.Now()
	cancel()
	result := <-done
	if result.err == nil {
		t.Fatal("a cancelled Birth through the fake ps must fail")
	}
	if elapsed := result.returnedAt.Sub(cancelledAt); elapsed < 1500*time.Millisecond {
		t.Fatalf("without WaitDelay the cancelled Birth returned in %s; the witness cannot see the hang", elapsed)
	}
}
