package cli

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
)

// TestOwnBirthIsReadOnce proves a process reads its own start time through
// the `ps` helper at most once: after one successful read the same value is
// returned even when no helper can be started. Control: with the same empty
// PATH a direct read of the start time fails, so the second answer cannot have
// come from a helper. The memo holds this process only; another PID is always
// read afresh by pidBirth.
func TestOwnBirthIsReadOnce(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("start times are read through ps on darwin only")
	}
	a := &App{}
	first, err := a.ownBirth(context.Background())
	if err != nil {
		t.Fatalf("first own start-time read: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := a.pidBirth(context.Background(), os.Getpid()); err == nil {
		t.Fatal("control: a direct start-time read succeeded without a ps helper on PATH")
	}
	again, err := a.ownBirth(context.Background())
	if err != nil || again != first {
		t.Fatalf("second own start-time read = %d, %v; want the remembered %d without a helper", again, err, first)
	}
}

// TestOwnBirthAsksTestSeamEveryTime proves the memo never answers for an App
// whose start-time reader is replaced: fixtures that model a reused PID by
// changing the reader's answer see every change, and an error is not
// remembered.
func TestOwnBirthAsksTestSeamEveryTime(t *testing.T) {
	answers := []int64{11, 22}
	failed := errors.New("reader unavailable")
	calls := 0
	a := &App{leaseBirth: func(context.Context, int) (int64, error) {
		calls++
		if calls > len(answers) {
			return 0, failed
		}
		return answers[calls-1], nil
	}}
	for i, want := range answers {
		if got, err := a.ownBirth(context.Background()); err != nil || got != want {
			t.Fatalf("read %d = %d, %v; want %d from the replaced reader", i+1, got, err, want)
		}
	}
	if _, err := a.ownBirth(context.Background()); !errors.Is(err, failed) {
		t.Fatalf("failing reader = %v, want its error", err)
	}
	if calls != 3 {
		t.Fatalf("replaced reader was asked %d times, want 3", calls)
	}
}
